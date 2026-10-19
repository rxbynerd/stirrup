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
			e, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
				Bucket:           "b",
				CredentialSource: &staticBearerSource{token: "t"},
				EndpointBaseURL:  gcsSrv.URL,
			})
			require.NoError(t, err)
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
			require.NoError(t, err)

			want := types.TokenUsage{Input: 130, Output: 25, CacheRead: 40, Reasoning: 2}
			require.Equal(t, want, got.TokenUsage, "turn usage plus auxiliary usage")
			require.Equal(t, aux, got.AuxiliaryTokens)
			require.Equal(t, external, got.ExternalGuardTokens)

			e.Start("run-2", &types.RunConfig{RunID: "run-2"})
			got, err = e.Finish(context.Background(), "success")
			require.NoError(t, err)
			require.Equal(t, types.TokenUsage{}, got.TokenUsage, "turn usage after restart")
			require.Equal(t, types.TokenUsage{}, got.AuxiliaryTokens, "auxiliary usage after restart")
			require.Equal(t, types.TokenUsage{}, got.ExternalGuardTokens, "external guard usage after restart")
		})
	}
}

func TestJSONLTraceEmitter_RunFinishedCarriesLedgerUsage(t *testing.T) {
	var buf bytes.Buffer
	e := NewJSONLTraceEmitter(&buf, false)
	e.Start("run-1", &types.RunConfig{RunID: "run-1"})
	e.RecordLedgerUsage(types.TokenUsage{Input: 3, Output: 4}, types.TokenUsage{Input: 9})
	_, err := e.Finish(context.Background(), "success")
	require.NoError(t, err)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	var ev struct {
		Trace map[string]json.RawMessage `json:"trace"`
	}
	require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &ev))
	require.Equal(t, `{"input":3,"output":4}`, string(ev.Trace["auxiliaryTokens"]), "auxiliaryTokens")
	require.Equal(t, `{"input":9,"output":0}`, string(ev.Trace["externalGuardTokens"]), "externalGuardTokens")
}

func TestJSONLTraceEmitter_ZeroLedgerUsageIsOmitted(t *testing.T) {
	var buf bytes.Buffer
	e := NewJSONLTraceEmitter(&buf, false)
	e.Start("run-1", &types.RunConfig{RunID: "run-1"})
	_, err := e.Finish(context.Background(), "success")
	require.NoError(t, err)

	out := buf.String()
	require.NotContains(t, out, "auxiliaryTokens")
	require.NotContains(t, out, "externalGuardTokens")
}

func TestEmitters_ParentTotalIncludesForwardedChildTurnsAndAuxiliary(t *testing.T) {
	parent := NewJSONLTraceEmitter(&bytes.Buffer{}, false)
	parent.Start("parent-run", &types.RunConfig{RunID: "parent-run"})
	parent.RecordTurn(types.TurnTrace{Turn: 0, Tokens: types.TokenUsage{Input: 10}})

	child := NewNestedJSONLEmitter(parent, "parent-run", context.Background())
	child.Start("child-run", &types.RunConfig{RunID: "child-run"})
	child.RecordTurn(types.TurnTrace{Turn: 0, Tokens: types.TokenUsage{Input: 90}})
	childTrace, err := child.Finish(context.Background(), "success")
	require.NoError(t, err)
	require.Equal(t, 90, childTrace.TokenUsage.Total(), "child keeps its own turn usage")

	parent.RecordLedgerUsage(types.TokenUsage{Input: 5}, types.TokenUsage{})
	got, err := parent.Finish(context.Background(), "success")
	require.NoError(t, err)
	require.Equal(t, 105, got.TokenUsage.Total(), "10 own turns + 90 child turns + 5 auxiliary")
}
