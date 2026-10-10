package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// cacheScriptProvider answers the first toolTurns calls with a thinking
// block and one test_tool call, then ends the run. It records each request
// as one JSON string per message, so later mutation of the loop's history
// cannot change what was sent.
type cacheScriptProvider struct {
	mu        sync.Mutex
	toolTurns int
	requests  [][]string
}

func (p *cacheScriptProvider) Stream(_ context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	encoded := make([]string, len(params.Messages))
	for i, msg := range params.Messages {
		b, err := json.Marshal(msg)
		if err != nil {
			return nil, err
		}
		encoded[i] = string(b)
	}
	p.requests = append(p.requests, encoded)

	call := len(p.requests)
	ch := make(chan types.StreamEvent, 3)
	if call > p.toolTurns {
		ch <- types.StreamEvent{Type: "text_delta", Text: "done"}
		ch <- types.StreamEvent{Type: "message_complete", StopReason: "end_turn"}
	} else {
		ch <- types.StreamEvent{Type: "thinking", Text: "checking the next file", ThoughtSignature: fmt.Sprintf("sig-%d", call)}
		ch <- types.StreamEvent{Type: "tool_call", ID: fmt.Sprintf("tc_%d", call), Name: "test_tool", Input: map[string]any{"file": call}}
		ch <- types.StreamEvent{Type: "message_complete", StopReason: "tool_use"}
	}
	close(ch)
	return ch, nil
}

// promptCacheUsage models an automatic prompt cache over a run's requests:
// each request reads the longest run of leading messages it shares with
// the previous request and writes the rest. System prompt and tool
// definitions are identical on every request and left out.
type promptCacheUsage struct {
	read, written int
	missedTurns   int
}

func simulatePromptCache(requests [][]string) promptCacheUsage {
	var usage promptCacheUsage
	for k, req := range requests {
		shared := 0
		if k > 0 {
			prev := requests[k-1]
			for shared < len(prev) && shared < len(req) && prev[shared] == req[shared] {
				shared++
			}
			if shared < len(prev) {
				usage.missedTurns++
			}
		}
		for i, msg := range req {
			tokens := len(msg) / tokenEstimationDivisor
			if i < shared {
				usage.read += tokens
			} else {
				usage.written += tokens
			}
		}
	}
	return usage
}

func (u promptCacheUsage) readShare() float64 {
	return float64(u.read) / float64(u.read+u.written)
}

// cost is the input cost in base-rate tokens: cache reads bill at a tenth
// of the base input rate and cache writes at 1.25 times (docs/providers.md).
func (u promptCacheUsage) cost() float64 {
	return float64(u.read)*0.1 + float64(u.written)*1.25
}

// runCacheScript runs 40 tool turns in a read-only mode against a loop with
// no executor, under strategy and a 16k-token window.
func runCacheScript(t *testing.T, strategy contextpkg.ContextStrategy) (*cacheScriptProvider, *recordingStrategy) {
	t.Helper()
	const toolTurns = 40
	prov := &cacheScriptProvider{toolTurns: toolTurns}
	rec := &recordingStrategy{inner: strategy}
	loop := buildTestLoop(nil)
	registerLargeTestTool(t, loop)
	loop.Provider = prov
	loop.Context = rec
	loop.Executor = nil

	cfg := buildTestConfig()
	cfg.Mode = "planning"
	cfg.PermissionPolicy = types.PermissionPolicyConfig{Type: "deny-side-effects"}
	cfg.MaxTurns = toolTurns + 5
	cfg.ContextStrategy.MaxTokens = 16_000
	runTrace, err := loop.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("outcome = %q, want success", runTrace.Outcome)
	}
	if len(prov.requests) != toolTurns+1 {
		t.Fatalf("provider saw %d requests, want %d", len(prov.requests), toolTurns+1)
	}
	return prov, rec
}

// TestLoop_ClearToolResultsKeepsRequestsStableBetweenBatches drives 40 tool
// results through a read-only run with no executor. Every request between
// two clearing batches must start with the previous request byte for
// byte, no request may be truncated by the fit check, and thinking is
// stripped from the first batch onward. It also logs the prompt-cache
// usage this gives against the sliding window over the same script.
func TestLoop_ClearToolResultsKeepsRequestsStableBetweenBatches(t *testing.T) {
	prov, rec := runCacheScript(t, contextpkg.NewClearToolResultsStrategy(contextpkg.ClearToolResultsConfig{Keep: contextpkg.DefaultClearKeep}))

	batches, firstBatch := 0, -1
	for k, compacted := range rec.compacted {
		if compacted {
			batches++
			if firstBatch < 0 {
				firstBatch = k
			}
		}
	}
	if batches < 2 {
		t.Fatalf("%d clearing batches in the run; the script must cross at least two", batches)
	}
	for k, req := range prov.requests {
		joined := strings.Join(req, "\n")
		if strings.Contains(joined, "[...truncated") {
			t.Fatalf("request %d was truncated by the fit check", k)
		}
		if !rec.compacted[k] && k > 0 {
			prev := prov.requests[k-1]
			if len(req) != len(prev)+2 {
				t.Fatalf("request %d has %d messages, want the previous %d plus one exchange", k, len(req), len(prev))
			}
			for i := range prev {
				if req[i] != prev[i] {
					t.Fatalf("request %d changed message %d of the previous request between batches", k, i)
				}
			}
		}
		if hasThinking := strings.Contains(joined, `"type":"thinking"`); hasThinking != (k > 0 && k < firstBatch) {
			t.Errorf("request %d: thinking present = %v, first batch at request %d", k, hasThinking, firstBatch)
		}
	}

	clearing := simulatePromptCache(prov.requests)
	slidingProv, _ := runCacheScript(t, contextpkg.NewSlidingWindowStrategy())
	sliding := simulatePromptCache(slidingProv.requests)
	t.Logf("clear-tool-results: %d batches, cache read %d, written %d (%.0f%% read), %d turns missed the cache, cost %.0f",
		batches, clearing.read, clearing.written, 100*clearing.readShare(), clearing.missedTurns, clearing.cost())
	t.Logf("sliding-window:     cache read %d, written %d (%.0f%% read), %d turns missed the cache, cost %.0f",
		sliding.read, sliding.written, 100*sliding.readShare(), sliding.missedTurns, sliding.cost())
	if clearing.missedTurns != batches {
		t.Errorf("%d requests missed the cache, want one per batch (%d)", clearing.missedTurns, batches)
	}
	if clearing.cost() >= sliding.cost() {
		t.Errorf("clear-tool-results input cost %.0f is not below the sliding window's %.0f", clearing.cost(), sliding.cost())
	}
}

func TestBuildContextStrategy_ClearToolResults(t *testing.T) {
	keep, clearAtLeast := 0, 500
	cs := buildContextStrategy(types.ContextStrategyConfig{
		Type: "clear-tool-results", Keep: &keep, ClearAtLeast: &clearAtLeast, ExcludeTools: []string{"run_command"},
	}, nil, "", 0, nil, nil)
	if _, ok := cs.(*contextpkg.ClearToolResultsStrategy); !ok {
		t.Fatalf("expected ClearToolResultsStrategy, got %T", cs)
	}
	if cs := buildContextStrategy(types.ContextStrategyConfig{Type: "clear-tool-results"}, nil, "", 0, nil, nil); cs == nil {
		t.Fatal("clear-tool-results with default settings built no strategy")
	}
}

func TestWarnUnknownExcludedTools(t *testing.T) {
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{Name: "run_command", InputSchema: json.RawMessage(`{"type":"object"}`)})
	profile, _ := tool.ProfileFor("coding-classic")
	presenter, err := tool.NewPresenter(registry, profile)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	warnUnknownExcludedTools([]string{"run_command", "bash", "no_such_tool"}, presenter, slog.New(slog.NewTextHandler(&logs, nil)))
	out := logs.String()
	if strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, "tool=no_such_tool") {
		t.Errorf("want one warning naming no_such_tool, got:\n%s", out)
	}
}

// TestBuildLoop_ClearToolResultsInReadOnlyModeWithoutWritableExecutor pins
// that a read-only run on an executor that cannot write accepts the
// strategy end to end through validation and the factory.
func TestBuildLoop_ClearToolResultsInReadOnlyModeWithoutWritableExecutor(t *testing.T) {
	t.Setenv("TEST_ANTHROPIC_KEY", "test-key")
	keep := 2
	config := buildTestConfig()
	config.Provider = types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://TEST_ANTHROPIC_KEY"}
	config.Mode = "research"
	config.Executor = types.ExecutorConfig{Type: "none"}
	config.PermissionPolicy = types.PermissionPolicyConfig{Type: "deny-side-effects"}
	config.Tools = types.ToolsConfig{BuiltIn: []string{"web_fetch"}}
	config.RuleOfTwo = disableRuleOfTwo()
	config.ContextStrategy = types.ContextStrategyConfig{
		Type: "clear-tool-results", MaxTokens: 100_000, Keep: &keep, ExcludeTools: []string{"web_fetch"},
	}
	if err := types.ValidateRunConfig(config); err != nil {
		t.Fatalf("ValidateRunConfig: %v", err)
	}
	loop, err := BuildLoopWithTransport(context.Background(), config, transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}))
	if err != nil {
		t.Fatalf("BuildLoopWithTransport: %v", err)
	}
	defer func() { _ = loop.Close() }()
	if _, err := loop.Context.Prepare(context.Background(), []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "task"}}}},
		contextpkg.TokenBudget{MaxTokens: 100_000, CurrentTokens: 10, ReserveForResponse: 1000}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
}

// TestBuildLoop_ClearToolResultsResolvesProfileAliases pins that the
// factory-built strategy matches excludeTools against the presented alias
// a tool_use block records under a toolset profile.
func TestBuildLoop_ClearToolResultsResolvesProfileAliases(t *testing.T) {
	t.Setenv("TEST_ANTHROPIC_KEY", "test-key")
	keep := 0
	config := buildTestConfig()
	config.Provider = types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://TEST_ANTHROPIC_KEY"}
	config.Executor = types.ExecutorConfig{Type: "local", Workspace: t.TempDir()}
	config.Tools = types.ToolsConfig{BuiltIn: []string{"run_command", "read_file"}, Profile: "coding-classic"}
	config.RuleOfTwo = disableRuleOfTwo()
	config.ContextStrategy = types.ContextStrategyConfig{
		Type: "clear-tool-results", MaxTokens: 100_000, Keep: &keep, ExcludeTools: []string{"run_command"},
	}
	if err := types.ValidateRunConfig(config); err != nil {
		t.Fatalf("ValidateRunConfig: %v", err)
	}
	loop, err := BuildLoopWithTransport(context.Background(), config, transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}))
	if err != nil {
		t.Fatalf("BuildLoopWithTransport: %v", err)
	}
	defer func() { _ = loop.Close() }()

	result := strings.Repeat("output\n", 400)
	messages := []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "task"}}}}
	for i, name := range []string{"bash", "read_file", "bash"} {
		id := fmt.Sprintf("c%d", i)
		messages = append(messages,
			types.Message{Role: "assistant", Content: []types.ContentBlock{{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(`{}`)}}},
			types.Message{Role: "user", Content: []types.ContentBlock{{Type: "tool_result", ToolUseID: id, Content: result}}},
		)
	}
	const reserve = 500
	current := estimateCurrentTokens(messages)
	trigger := current - 300
	out, err := loop.Context.Prepare(context.Background(), messages, contextpkg.TokenBudget{
		MaxTokens: reserve + trigger*100/90 + 1, CurrentTokens: current, ReserveForResponse: reserve,
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if out[2].Content[0].Content != result || out[4].Content[0].Content != contextpkg.ClearedToolResultPlaceholder {
		t.Errorf("want the bash result kept and the read_file result cleared, got %.20q and %.20q",
			out[2].Content[0].Content, out[4].Content[0].Content)
	}
}
