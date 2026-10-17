package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rxbynerd/stirrup/types"
)

type ledgerUsageEmitter interface {
	TraceEmitter
	LedgerUsageRecorder
}

// ledgerUsageEmitterBuilders returns a constructor for each top-level
// production emitter; sub-agent (nested) emitters never record ledger
// usage.
func ledgerUsageEmitterBuilders(t *testing.T) map[string]func(t *testing.T) ledgerUsageEmitter {
	gcsSrv := httptest.NewServer(newGCSCaptureServer().handler())
	t.Cleanup(gcsSrv.Close)

	return map[string]func(t *testing.T) ledgerUsageEmitter{
		"jsonl": func(*testing.T) ledgerUsageEmitter {
			return NewJSONLTraceEmitter(&bytes.Buffer{}, false)
		},
		"otel": func(t *testing.T) ledgerUsageEmitter {
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(tracetest.NewInMemoryExporter()))
			t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
			return NewOTelTraceEmitterForTest(tp)
		},
		"gcs": func(t *testing.T) ledgerUsageEmitter {
			e, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
				Bucket:           "b",
				CredentialSource: &staticBearerSource{token: "t"},
				EndpointBaseURL:  gcsSrv.URL,
			})
			if err != nil {
				t.Fatalf("NewGCSTraceEmitter: %v", err)
			}
			return e
		},
	}
}

func TestEmitters_LedgerUsageJoinsTokenUsage(t *testing.T) {
	turn := types.TokenUsage{Input: 100, Output: 20, CacheRead: 40}
	aux := types.TokenUsage{Input: 30, Output: 5, Reasoning: 2}
	external := types.TokenUsage{Input: 70, Output: 1}
	for name, build := range ledgerUsageEmitterBuilders(t) {
		t.Run(name, func(t *testing.T) {
			e := build(t)

			e.Start("run-1", &types.RunConfig{RunID: "run-1"})
			e.RecordTurn(types.TurnTrace{Turn: 0, Tokens: turn})
			e.RecordLedgerUsage(aux, external)
			got, err := e.Finish(context.Background(), "success")
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}
			want := types.TokenUsage{Input: 130, Output: 25, CacheRead: 40, Reasoning: 2}
			if got.TokenUsage != want {
				t.Errorf("TokenUsage = %+v, want turns plus auxiliary %+v", got.TokenUsage, want)
			}
			if got.AuxiliaryTokens != aux {
				t.Errorf("AuxiliaryTokens = %+v, want %+v", got.AuxiliaryTokens, aux)
			}
			if got.ExternalGuardTokens != external {
				t.Errorf("ExternalGuardTokens = %+v, want %+v", got.ExternalGuardTokens, external)
			}

			e.Start("run-2", &types.RunConfig{RunID: "run-2"})
			got, err = e.Finish(context.Background(), "success")
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if got.TokenUsage != (types.TokenUsage{}) || got.AuxiliaryTokens != (types.TokenUsage{}) || got.ExternalGuardTokens != (types.TokenUsage{}) {
				t.Errorf("after restart TokenUsage=%+v AuxiliaryTokens=%+v ExternalGuardTokens=%+v, want all zero",
					got.TokenUsage, got.AuxiliaryTokens, got.ExternalGuardTokens)
			}
		})
	}
}

func TestJSONLTraceEmitter_RunFinishedCarriesLedgerUsage(t *testing.T) {
	var buf bytes.Buffer
	e := NewJSONLTraceEmitter(&buf, false)
	e.Start("run-1", &types.RunConfig{RunID: "run-1"})
	e.RecordLedgerUsage(types.TokenUsage{Input: 3, Output: 4}, types.TokenUsage{Input: 9})
	if _, err := e.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	var ev struct {
		Trace map[string]json.RawMessage `json:"trace"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &ev); err != nil {
		t.Fatalf("unmarshal run_finished: %v", err)
	}
	if got := string(ev.Trace["auxiliaryTokens"]); got != `{"input":3,"output":4}` {
		t.Errorf("auxiliaryTokens = %s", got)
	}
	if got := string(ev.Trace["externalGuardTokens"]); got != `{"input":9,"output":0}` {
		t.Errorf("externalGuardTokens = %s", got)
	}
}

func TestJSONLTraceEmitter_ZeroLedgerUsageIsOmitted(t *testing.T) {
	var buf bytes.Buffer
	e := NewJSONLTraceEmitter(&buf, false)
	e.Start("run-1", &types.RunConfig{RunID: "run-1"})
	if _, err := e.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if out := buf.String(); strings.Contains(out, "auxiliaryTokens") || strings.Contains(out, "externalGuardTokens") {
		t.Errorf("run_finished carries zero ledger usage fields:\n%s", out)
	}
}

func TestEmitters_ParentTotalIncludesForwardedChildTurnsAndAuxiliary(t *testing.T) {
	parent := NewJSONLTraceEmitter(&bytes.Buffer{}, false)
	parent.Start("parent-run", &types.RunConfig{RunID: "parent-run"})
	parent.RecordTurn(types.TurnTrace{Turn: 0, Tokens: types.TokenUsage{Input: 10}})

	child := NewNestedJSONLEmitter(parent, "parent-run", context.Background())
	child.Start("child-run", &types.RunConfig{RunID: "child-run"})
	child.RecordTurn(types.TurnTrace{Turn: 0, Tokens: types.TokenUsage{Input: 90}})
	childTrace, err := child.Finish(context.Background(), "success")
	if err != nil {
		t.Fatalf("child Finish: %v", err)
	}
	if childTrace.TokenUsage.Total() != 90 {
		t.Errorf("child TokenUsage total = %d, want its own 90", childTrace.TokenUsage.Total())
	}

	parent.RecordLedgerUsage(types.TokenUsage{Input: 5}, types.TokenUsage{})
	got, err := parent.Finish(context.Background(), "success")
	if err != nil {
		t.Fatalf("parent Finish: %v", err)
	}
	if got.TokenUsage.Total() != 105 {
		t.Errorf("parent TokenUsage total = %d, want 10 own + 90 child + 5 auxiliary", got.TokenUsage.Total())
	}
}
