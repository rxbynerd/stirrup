package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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
			must := require.New(t)
			e, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
				Bucket:           "b",
				CredentialSource: &staticBearerSource{token: "t"},
				EndpointBaseURL:  gcsSrv.URL,
			})
			must.NoError(err)
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
			must := require.New(t)
			e := build(t)

			e.Start("run-1", &types.RunConfig{RunID: "run-1"})
			e.RecordTurn(types.TurnTrace{Turn: 0, Tokens: turn})
			e.RecordLedgerUsage(aux, external)
			got, err := e.Finish(context.Background(), "success")
			must.NoError(err)

			want := types.TokenUsage{Input: 130, Output: 25, CacheRead: 40, Reasoning: 2}
			must.Equal(want, got.TokenUsage, "turn usage plus auxiliary usage")
			must.Equal(aux, got.AuxiliaryTokens)
			must.Equal(external, got.ExternalGuardTokens)

			e.Start("run-2", &types.RunConfig{RunID: "run-2"})
			got, err = e.Finish(context.Background(), "success")
			must.NoError(err)
			must.Equal(types.TokenUsage{}, got.TokenUsage, "turn usage after restart")
			must.Equal(types.TokenUsage{}, got.AuxiliaryTokens, "auxiliary usage after restart")
			must.Equal(types.TokenUsage{}, got.ExternalGuardTokens, "external guard usage after restart")
		})
	}
}

func TestJSONLTraceEmitter_RunFinishedCarriesLedgerUsage(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	e := NewJSONLTraceEmitter(&buf, false)
	e.Start("run-1", &types.RunConfig{RunID: "run-1"})
	e.RecordLedgerUsage(types.TokenUsage{Input: 3, Output: 4}, types.TokenUsage{Input: 9})
	_, err := e.Finish(context.Background(), "success")
	must.NoError(err)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	var ev struct {
		Trace map[string]json.RawMessage `json:"trace"`
	}
	must.NoError(json.Unmarshal([]byte(lines[len(lines)-1]), &ev))
	must.Equal(`{"input":3,"output":4}`, string(ev.Trace["auxiliaryTokens"]), "auxiliaryTokens")
	must.Equal(`{"input":9,"output":0}`, string(ev.Trace["externalGuardTokens"]), "externalGuardTokens")
}

func TestJSONLTraceEmitter_ZeroLedgerUsageIsOmitted(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	e := NewJSONLTraceEmitter(&buf, false)
	e.Start("run-1", &types.RunConfig{RunID: "run-1"})
	_, err := e.Finish(context.Background(), "success")
	must.NoError(err)

	out := buf.String()
	must.NotContains(out, "auxiliaryTokens")
	must.NotContains(out, "externalGuardTokens")
}

func TestEmitters_ParentTotalIncludesForwardedChildTurnsAndAuxiliary(t *testing.T) {
	must := require.New(t)
	parent := NewJSONLTraceEmitter(&bytes.Buffer{}, false)
	parent.Start("parent-run", &types.RunConfig{RunID: "parent-run"})
	parent.RecordTurn(types.TurnTrace{Turn: 0, Tokens: types.TokenUsage{Input: 10}})

	child := NewNestedJSONLEmitter(parent, "parent-run", context.Background())
	child.Start("child-run", &types.RunConfig{RunID: "child-run"})
	child.RecordTurn(types.TurnTrace{Turn: 0, Tokens: types.TokenUsage{Input: 90}})
	childTrace, err := child.Finish(context.Background(), "success")
	must.NoError(err)
	must.Equal(90, childTrace.TokenUsage.Total(), "child keeps its own turn usage")

	parent.RecordLedgerUsage(types.TokenUsage{Input: 5}, types.TokenUsage{})
	got, err := parent.Finish(context.Background(), "success")
	must.NoError(err)
	must.Equal(105, got.TokenUsage.Total(), "10 own turns + 90 child turns + 5 auxiliary")
}
