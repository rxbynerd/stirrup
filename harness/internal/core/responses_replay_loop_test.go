package core

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/types"
)

// These tests drive the real openai-responses adapter through the loop
// against httptest servers. The SSE bodies are synthetic (documented
// shapes, not probed).

const replayLoopModel = "gpt-5.6-sol"

func responsesSSEEvent(name, data string) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", name, data)
}

// responsesToolTurnSSE is turn n of a reasoning run: an encrypted
// reasoning item and one test_tool call, whose ids and arguments all carry
// n so the loop's stall detector sees distinct calls.
func responsesToolTurnSSE(n int) string {
	args := fmt.Sprintf(`{\"n\":%d}`, n)
	reasoning := fmt.Sprintf(`{"type":"reasoning","id":"rs_%d","summary":[],"encrypted_content":"enc-turn-%d"}`, n, n)
	call := fmt.Sprintf(`{"type":"function_call","id":"fc_%d","call_id":"call_%d","name":"test_tool","arguments":"%s","status":"completed"}`, n, n, args)
	return responsesSSEEvent("response.output_item.done", `{"output_index":0,"item":`+reasoning+`}`) +
		responsesSSEEvent("response.output_item.added", fmt.Sprintf(`{"output_index":1,"item":{"type":"function_call","id":"fc_%d","call_id":"call_%d","name":"test_tool","arguments":"","status":"in_progress"}}`, n, n)) +
		responsesSSEEvent("response.function_call_arguments.delta", fmt.Sprintf(`{"item_id":"fc_%d","output_index":1,"delta":"%s"}`, n, args)) +
		responsesSSEEvent("response.function_call_arguments.done", fmt.Sprintf(`{"item_id":"fc_%d","output_index":1,"arguments":"%s"}`, n, args)) +
		responsesSSEEvent("response.output_item.done", `{"output_index":1,"item":`+call+`}`) +
		responsesSSEEvent("response.completed", `{"response":{"status":"completed","output":[`+reasoning+`,`+call+`],"usage":{"input_tokens":10,"output_tokens":5}}}`)
}

// responsesFinalTurnSSE is a closing text-only turn.
func responsesFinalTurnSSE() string {
	message := `{"type":"message","id":"msg_final","role":"assistant","status":"completed","content":[{"type":"output_text","text":"done"}]}`
	return responsesSSEEvent("response.output_text.delta", `{"item_id":"msg_final","output_index":0,"delta":"done"}`) +
		responsesSSEEvent("response.completed", `{"response":{"status":"completed","output":[`+message+`],"usage":{"input_tokens":10,"output_tokens":5}}}`)
}

// responsesTurnServer answers each request with the next scripted SSE body
// and records every request body.
type responsesTurnServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	turns  []string
	bodies []string
}

func newResponsesTurnServer(t *testing.T, turns ...string) *responsesTurnServer {
	t.Helper()
	s := &responsesTurnServer{turns: turns}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		s.mu.Lock()
		s.bodies = append(s.bodies, string(body))
		if len(s.turns) == 0 {
			s.mu.Unlock()
			t.Errorf("unscripted request: %s", body)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		reply := s.turns[0]
		s.turns = s.turns[1:]
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *responsesTurnServer) adapter() *provider.OpenAIResponsesAdapter {
	return provider.NewOpenAIResponsesAdapter(func(context.Context) (string, error) { return "test-key", nil }, s.srv.URL, provider.OpenAIAuthConfig{})
}

func (s *responsesTurnServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

func responsesLoopConfig() *types.RunConfig {
	config := buildTestConfig()
	config.Provider = types.ProviderConfig{Type: "openai-responses", APIKeyRef: "secret://TEST"}
	config.ModelRouter = types.ModelRouterConfig{Type: "static", Provider: "responses-a", Model: replayLoopModel}
	return config
}

func runResponsesLoop(t *testing.T, loop *AgenticLoop, config *types.RunConfig) {
	t.Helper()
	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("outcome = %q, want success", runTrace.Outcome)
	}
}

// TestLoop_ResponsesReplayFollowsItsEndpoint pins the loop-level replay
// contract across a router switch: a stored reasoning turn replays to the
// provider that produced it, and a turn routed to another named provider
// (same model, another endpoint) is reconstructed without item ids or
// encrypted reasoning, with its tool call still paired.
func TestLoop_ResponsesReplayFollowsItsEndpoint(t *testing.T) {
	t.Run("same provider replays", func(t *testing.T) {
		a := newResponsesTurnServer(t, responsesToolTurnSSE(0), responsesFinalTurnSSE())
		loop := buildTestLoop(nil)
		loop.Provider = nil
		loop.Providers = map[string]provider.ProviderAdapter{"responses-a": a.adapter()}
		loop.Router = router.NewStaticRouter("responses-a", replayLoopModel)
		runResponsesLoop(t, loop, responsesLoopConfig())

		bodies := a.requests()
		if len(bodies) != 2 {
			t.Fatalf("provider A saw %d requests, want 2", len(bodies))
		}
		if !strings.Contains(bodies[1], "enc-turn-0") || !strings.Contains(bodies[1], `"id":"fc_0"`) {
			t.Errorf("second request did not replay the stored turn: %s", bodies[1])
		}
	})

	t.Run("router switch reconstructs", func(t *testing.T) {
		a := newResponsesTurnServer(t, responsesToolTurnSSE(0))
		b := newResponsesTurnServer(t, responsesFinalTurnSSE())
		loop := buildTestLoop(nil)
		loop.Provider = nil
		loop.Providers = map[string]provider.ProviderAdapter{
			"responses-a": a.adapter(),
			"responses-b": b.adapter(),
		}
		loop.Router = router.NewDynamicRouter(router.DynamicRouterConfig{
			DefaultSelection:       router.ModelSelection{Provider: "responses-a", Model: replayLoopModel},
			ExpensiveSelection:     router.ModelSelection{Provider: "responses-b", Model: replayLoopModel},
			ExpensiveTurnThreshold: 1,
		})
		runResponsesLoop(t, loop, responsesLoopConfig())

		if n := len(a.requests()); n != 1 {
			t.Fatalf("provider A saw %d requests, want 1", n)
		}
		bodies := b.requests()
		if len(bodies) != 1 {
			t.Fatalf("provider B saw %d requests, want 1", len(bodies))
		}
		body := bodies[0]
		for _, leak := range []string{"enc-turn-0", `"id":`} {
			if strings.Contains(body, leak) {
				t.Errorf("request to another endpoint carries %s: %s", leak, body)
			}
		}
		if strings.Count(body, `"call_id":"call_0"`) != 2 {
			t.Errorf("call_0 is not paired with its output: %s", body)
		}
	})
}

// staticSummaryProvider answers every summary request with fixed text.
type staticSummaryProvider struct{}

func (staticSummaryProvider) Stream(context.Context, types.StreamParams) (<-chan types.StreamEvent, error) {
	ch := make(chan types.StreamEvent, 2)
	ch <- types.StreamEvent{Type: "text_delta", Text: "Earlier turns called test_tool."}
	ch <- types.StreamEvent{Type: "message_complete", StopReason: "end_turn"}
	close(ch)
	return ch, nil
}

// TestLoop_ResponsesReplaySurvivesSummarisation pins that a context
// rewrite over a Responses history carrying stored items never resends the
// rewritten turns' items: once the summariser folds the first reasoning
// turn into a summary, later requests carry neither its encrypted
// reasoning nor its item ids, while the turns kept verbatim still replay.
// The control run without summarisation replays every turn.
func TestLoop_ResponsesReplaySurvivesSummarisation(t *testing.T) {
	script := []string{
		responsesToolTurnSSE(0), responsesToolTurnSSE(1), responsesToolTurnSSE(2),
		responsesToolTurnSSE(3), responsesFinalTurnSSE(),
	}
	cases := []struct {
		name           string
		strategy       contextpkg.ContextStrategy
		maxTokens      int
		wantFirstTurn  bool
		wantSummarised bool
	}{
		{"summarised", contextpkg.NewSummariseStrategy(staticSummaryProvider{}, "summary-model"), 100, false, true},
		{"control", contextpkg.NewSlidingWindowStrategy(), 200000, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newResponsesTurnServer(t, script...)
			loop := buildTestLoop(nil)
			loop.Provider = nil
			loop.Providers = map[string]provider.ProviderAdapter{"responses-a": srv.adapter()}
			loop.Router = router.NewStaticRouter("responses-a", replayLoopModel)
			loop.Context = tc.strategy
			config := responsesLoopConfig()
			config.ContextStrategy.MaxTokens = tc.maxTokens
			runResponsesLoop(t, loop, config)

			bodies := srv.requests()
			if len(bodies) != len(script) {
				t.Fatalf("server saw %d requests, want %d", len(bodies), len(script))
			}
			last := bodies[len(bodies)-1]
			if got := strings.Contains(last, "conversation_summary"); got != tc.wantSummarised {
				t.Fatalf("last request summarised = %v, want %v: %s", got, tc.wantSummarised, last)
			}
			for _, marker := range []string{"enc-turn-0", `"id":"rs_0"`, `"id":"fc_0"`} {
				if got := strings.Contains(last, marker); got != tc.wantFirstTurn {
					t.Errorf("last request carries %s = %v, want %v: %s", marker, got, tc.wantFirstTurn, last)
				}
			}
			if !strings.Contains(last, "enc-turn-3") || !strings.Contains(last, `"id":"fc_3"`) {
				t.Errorf("last request did not replay the most recent stored turn: %s", last)
			}
		})
	}
}
