package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// auxiliaryUsageServer is an openai-compatible endpoint that answers the
// loop's turns, summary requests, llm-judge requests and cloud-judge
// requests, each with a fixed usage per category, and counts them.
type auxiliaryUsageServer struct {
	mu     sync.Mutex
	counts map[string]int
	turns  []string
}

var (
	judgeNonceRe = regexp.MustCompile(`Its \\"nonce\\" member must be exactly \\"([^"\\]+)\\"`)
	guardNonceRe = regexp.MustCompile(`The nonce member must be exactly \\"([^"\\]+)\\"`)
)

var auxiliaryUsage = map[string]types.TokenUsage{
	"turn":    {Input: 1000, Output: 10},
	"summary": {Input: 300, Output: 20},
	"judge":   {Input: 200, Output: 15},
	"guard":   {Input: 50, Output: 5},
}

func (s *auxiliaryUsageServer) handler(t *testing.T) http.HandlerFunc {
	check := assert.New(t)
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if !check.NoError(err, "read request") {
			return
		}
		body := string(raw)
		var category, content, toolCall string
		switch {
		case strings.Contains(body, "You are a precise summariser"):
			category, content = "summary", "condensed history"
		case strings.Contains(body, "You are a verification judge"):
			m := judgeNonceRe.FindStringSubmatch(body)
			if !check.NotNil(m, "judge request carries no nonce instruction") {
				return
			}
			category, content = "judge", fmt.Sprintf(`{"reasoning":"ok","nonce":%q,"passed":true,"feedback":"meets criteria"}`, m[1])
		case strings.Contains(body, "You are a safety classifier"):
			m := guardNonceRe.FindStringSubmatch(body)
			if !check.NotNil(m, "guard request carries no nonce instruction") {
				return
			}
			category, content = "guard", fmt.Sprintf(`{"nonce":%q,"verdict":"allow","reason":"benign"}`, m[1])
		default:
			category = "turn"
		}

		s.mu.Lock()
		n := s.counts[category]
		s.counts[category]++
		s.mu.Unlock()

		if category == "turn" {
			if n < len(s.turns) {
				toolCall = s.turns[n]
			} else {
				content = "done"
			}
		}

		u := auxiliaryUsage[category]
		usage := fmt.Sprintf(`"usage":{"prompt_tokens":%d,"completion_tokens":%d}`, u.Input, u.Output)
		var sse string
		if toolCall != "" {
			sse = openAIChunk(fmt.Sprintf(`{"id":"r","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_%d","type":"function","function":{"name":"read_file","arguments":%q}}]},"finish_reason":null}]}`, n, toolCall)) +
				openAIChunk(`{"id":"r","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],`+usage+`}`)
		} else {
			encoded, _ := json.Marshal(content)
			sse = openAIChunk(fmt.Sprintf(`{"id":"r","choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`, encoded)) +
				openAIChunk(`{"id":"r","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],`+usage+`}`)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse+"data: [DONE]\n\n")
	}
}

func (s *auxiliaryUsageServer) count(category string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[category]
}

func scaled(u types.TokenUsage, n int) types.TokenUsage {
	return types.TokenUsage{Input: u.Input * n, Output: u.Output * n}
}

// TestBuildLoop_AuxiliaryCallsChargeTheRunLedger drives a factory-built
// loop whose summary, llm-judge and cloud-judge calls go to the run's
// provider, plus a granite-guardian stage on its own endpoint. Provider
// calls are charged and reported as auxiliary usage; the granite usage is
// reported only and never ends the run, even past maxTokenBudget.
func TestBuildLoop_AuxiliaryCallsChargeTheRunLedger(t *testing.T) {
	must := require.New(t)
	// A first run with an ample window records the loop's context
	// estimates, from which the second run's window is chosen so the
	// summarise strategy first compacts on the fifth model call.
	calibration := runAuxiliaryLedgerLoop(t, 100_000, true)
	maxTokens := 0
	for m := maxTokensCompactingFrom(t, calibration.estimates, 4); m > 0; m -= 25 {
		if trial := runAuxiliaryLedgerLoop(t, m, true); firstCompaction(trial.compacted) == 4 {
			maxTokens = m
			break
		}
	}
	must.NotZero(maxTokens, "no maxTokens makes the summarise strategy first compact on the fifth call")

	run := runAuxiliaryLedgerLoop(t, maxTokens, false)
	turns := run.srv.count("turn")
	summaries := run.srv.count("summary")
	judges := run.srv.count("judge")
	guards := run.srv.count("guard")
	must.Positive(summaries, "summary calls")
	must.Positive(judges, "judge calls")
	must.Positive(guards, "guard calls")

	wantAux := scaled(auxiliaryUsage["summary"], summaries)
	wantAux.Add(scaled(auxiliaryUsage["judge"], judges))
	wantAux.Add(scaled(auxiliaryUsage["guard"], guards))
	must.Equal(wantAux, run.trace.AuxiliaryTokens, "auxiliary usage: summary=%d judge=%d guard=%d", summaries, judges, guards)

	wantTotal := scaled(auxiliaryUsage["turn"], turns)
	wantTotal.Add(wantAux)
	must.Equal(wantTotal, run.trace.TokenUsage, "total usage: turns plus auxiliary")

	wantExternal := types.TokenUsage{Input: 1000000 * run.graniteCalls, Output: run.graniteCalls}
	must.NotZero(run.graniteCalls, "the granite-guardian stage was never called")
	must.Equal(wantExternal, run.trace.ExternalGuardTokens)

	must.Greater(run.trace.TokenUsage.Total()+wantExternal.Total(), run.budget,
		"test setup: external usage must push the run past the token budget")
}

type auxiliaryLedgerRun struct {
	trace        *types.RunTrace
	srv          *auxiliaryUsageServer
	graniteCalls int
	budget       int
	estimates    []int
	compacted    []bool
}

// runAuxiliaryLedgerLoop runs the factory-built loop once against fresh
// endpoints. recordEstimates wraps the context strategy to capture the
// per-call context estimates.
func runAuxiliaryLedgerLoop(t *testing.T, maxTokens int, recordEstimates bool) auxiliaryLedgerRun {
	t.Helper()
	must := require.New(t)
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	workspace := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt"} {
		content := []byte(strings.Repeat("file contents line\n", 150))
		must.NoError(os.WriteFile(filepath.Join(workspace, name), content, 0o644), "seed %s", name)
	}

	srv := &auxiliaryUsageServer{
		counts: map[string]int{},
		turns:  []string{`{"path":"a.txt"}`, `{"path":"b.txt"}`, `{"path":"c.txt"}`, `{"path":"d.txt"}`, `{"path":"e.txt"}`},
	}
	providerSrv := httptest.NewServer(srv.handler(t))
	defer providerSrv.Close()

	var graniteCalls int
	var graniteMu sync.Mutex
	graniteSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		graniteMu.Lock()
		graniteCalls++
		graniteMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"<score>no</score>"}}],"usage":{"prompt_tokens":1000000,"completion_tokens":1}}`)
	}))
	defer graniteSrv.Close()

	timeout := 60
	enforce := false
	budget := 1_000_000
	config := &types.RunConfig{
		RunID:            "ledger-aux",
		Mode:             "execution",
		Prompt:           "Read the five files and report.",
		Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: providerSrv.URL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "gpt-4o-mini"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "summarise", MaxTokens: maxTokens},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: workspace},
		EditStrategy:     types.EditStrategyConfig{Type: "whole-file"},
		Verifier:         types.VerifierConfig{Type: "llm-judge", Criteria: "The files were read."},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"read_file"}},
		GuardRail: &types.GuardRailConfig{
			Type: "composite",
			Stages: []types.GuardRailConfig{
				{Type: "cloud-judge", Model: "gpt-4o-mini"},
				{Type: "granite-guardian", Endpoint: graniteSrv.URL},
			},
		},
		RuleOfTwo:      &types.RuleOfTwoConfig{Enforce: &enforce},
		MaxTurns:       8,
		MaxTokenBudget: &budget,
		Timeout:        &timeout,
	}

	loop, err := BuildLoopWithTransport(context.Background(), config, &recordingTransport{})
	must.NoError(err, "BuildLoopWithTransport")
	defer func() { _ = loop.Close() }()

	var rec *recordingStrategy
	if recordEstimates {
		rec = &recordingStrategy{inner: loop.Context}
		loop.Context = rec
	}

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err, "loop.Run")
	must.Equal("success", runTrace.Outcome)
	graniteMu.Lock()
	calls := graniteCalls
	graniteMu.Unlock()
	out := auxiliaryLedgerRun{trace: runTrace, srv: srv, graniteCalls: calls, budget: budget}
	if rec != nil {
		out.estimates = rec.currentTokens
		out.compacted = rec.compacted
	}
	return out
}
