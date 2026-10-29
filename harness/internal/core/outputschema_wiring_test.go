package core

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	tracepkg "github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// verifierPromptNonce matches the llm-judge conversation fence so a test
// double can echo the nonce the verdict must carry.
var verifierPromptNonce = regexp.MustCompile(`<<<CONVERSATION_([0-9a-f]+)>>>`)

// passingJudgeProvider answers every llm-judge prompt with a passing
// verdict carrying the prompt's nonce, and records the last params.
type passingJudgeProvider struct {
	lastParams types.StreamParams
}

func (p *passingJudgeProvider) Stream(_ context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	p.lastParams = params
	nonce := ""
	for _, m := range params.Messages {
		for _, b := range m.Content {
			if sm := verifierPromptNonce.FindStringSubmatch(b.Text); sm != nil {
				nonce = sm[1]
			}
		}
	}
	ch := make(chan types.StreamEvent, 2)
	ch <- types.StreamEvent{Type: "text_delta", Text: fmt.Sprintf(`{"reasoning":"r","nonce":%q,"passed":true,"feedback":"ok"}`, nonce)}
	ch <- types.StreamEvent{Type: "message_complete", StopReason: "end_turn"}
	close(ch)
	return ch, nil
}

// TestBuildVerifier_LLMJudge_ThreadsOutputSchemaSupport pins that the
// factory's capability answer reaches an llm-judge at the top level and
// nested in a composite, resolved for the judge's own model including the
// default, and lands on the result's structuredOutput detail.
func TestBuildVerifier_LLMJudge_ThreadsOutputSchemaSupport(t *testing.T) {
	grantHaiku := func(model string) bool { return model == "claude-haiku-5-5" }

	t.Run("top level with default model records json_schema", func(t *testing.T) {
		must := require.New(t)
		prov := &passingJudgeProvider{}
		v := buildVerifier(types.VerifierConfig{Type: "llm-judge", Criteria: "c"}, prov, nil, grantHaiku)
		result, err := v.Verify(context.Background(), verifierVerifyContext())
		must.NoError(err)
		must.NotNil(prov.lastParams.OutputSchema)
		must.Equal(types.JudgeStructuredJSONSchema, result.Details["structuredOutput"]) //nolint:testifylint // a mode name, not a JSON document
	})

	t.Run("top level with an ungranted model records prompt_only", func(t *testing.T) {
		must := require.New(t)
		v := buildVerifier(types.VerifierConfig{Type: "llm-judge", Criteria: "c", Model: "other-model"}, &passingJudgeProvider{}, nil, grantHaiku)
		result, err := v.Verify(context.Background(), verifierVerifyContext())
		must.NoError(err)
		must.Equal(types.JudgeStructuredPromptOnly, result.Details["structuredOutput"])
	})

	t.Run("nil support records prompt_only", func(t *testing.T) {
		must := require.New(t)
		v := buildVerifier(types.VerifierConfig{Type: "llm-judge", Criteria: "c"}, &passingJudgeProvider{}, nil, nil)
		result, err := v.Verify(context.Background(), verifierVerifyContext())
		must.NoError(err)
		must.Equal(types.JudgeStructuredPromptOnly, result.Details["structuredOutput"])
	})

	t.Run("composite threads the answer to the nested judge", func(t *testing.T) {
		must := require.New(t)
		cfg := types.VerifierConfig{Type: "composite", Verifiers: []types.VerifierConfig{
			{Type: "none"},
			{Type: "llm-judge", Criteria: "c"},
		}}
		v := buildVerifier(cfg, &passingJudgeProvider{}, nil, grantHaiku)
		result, err := v.Verify(context.Background(), verifierVerifyContext())
		must.NoError(err)
		nested, ok := result.Details["verifier_1"].(map[string]any)
		must.True(ok, "details: %+v", result.Details)
		must.Equal(types.JudgeStructuredJSONSchema, nested["structuredOutput"]) //nolint:testifylint // a mode name, not a JSON document
	})
}

// TestBuildGuardRail_CloudJudge_ThreadsOutputSchemaSupport pins that the
// factory's capability answer reaches a cloud-judge at the top level and
// nested in a composite and lands on the Decision.
func TestBuildGuardRail_CloudJudge_ThreadsOutputSchemaSupport(t *testing.T) {
	ctx := context.Background()
	grantHaiku := func(model string) bool { return model == "claude-haiku-5-5" }
	prov := &nonceEchoJudgeProvider{verdict: "allow", stopReason: "end_turn"}

	t.Run("top level with default model records json_schema", func(t *testing.T) {
		must := require.New(t)
		g, err := buildGuardRail(ctx, &types.GuardRailConfig{Type: "cloud-judge"}, nil, prov, nil, nil, grantHaiku)
		must.NoError(err)
		d, err := g.Check(ctx, guard.Input{Phase: guard.PhasePostTurn, Content: "x"})
		must.NoError(err)
		must.Equal(types.JudgeStructuredJSONSchema, d.StructuredOutput) //nolint:testifylint // a mode name, not a JSON document
	})

	t.Run("top level with an ungranted model records prompt_only", func(t *testing.T) {
		must := require.New(t)
		g, err := buildGuardRail(ctx, &types.GuardRailConfig{Type: "cloud-judge", Model: "other-model"}, nil, prov, nil, nil, grantHaiku)
		must.NoError(err)
		d, err := g.Check(ctx, guard.Input{Phase: guard.PhasePostTurn, Content: "x"})
		must.NoError(err)
		must.Equal(types.JudgeStructuredPromptOnly, d.StructuredOutput)
	})

	t.Run("composite threads the answer to the nested judge", func(t *testing.T) {
		must := require.New(t)
		cfg := &types.GuardRailConfig{Type: "composite", Stages: []types.GuardRailConfig{{Type: "cloud-judge"}}}
		g, err := buildGuardRail(ctx, cfg, nil, prov, nil, nil, grantHaiku)
		must.NoError(err)
		d, err := g.Check(ctx, guard.Input{Phase: guard.PhasePostTurn, Content: "x"})
		must.NoError(err)
		must.Equal(types.JudgeStructuredJSONSchema, d.StructuredOutput) //nolint:testifylint // a mode name, not a JSON document
	})
}

// modeGuard returns a fixed Decision or a guard.Error carrying a
// structured-output mode.
type modeGuard struct {
	decision *guard.Decision
	err      error
}

func (g *modeGuard) Check(context.Context, guard.Input) (*guard.Decision, error) {
	return g.decision, g.err
}

// TestGuardCheck_RecordsStructuredOutputSpanAttribute pins that the guard
// span carries guard.structured_output on a decision and on a failed
// check alike, that the fail-open decision keeps the mode, and that a
// guard with no mode adds no attribute.
func TestGuardCheck_RecordsStructuredOutputSpanAttribute(t *testing.T) {
	newLoop := func(t *testing.T, g guard.GuardRail) (*AgenticLoop, *tracetest.InMemoryExporter) {
		t.Helper()
		exporter := tracetest.NewInMemoryExporter()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
		t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
		return &AgenticLoop{
			Tracer:    tracepkg.NewOTelTraceEmitterForTest(tp).Tracer(),
			Metrics:   observability.NewNoopMetrics(),
			GuardRail: g,
		}, exporter
	}
	spanAttr := func(t *testing.T, exporter *tracetest.InMemoryExporter, key string) (string, bool) {
		t.Helper()
		spans := exporter.GetSpans()
		require.Len(t, spans, 1)
		for _, kv := range spans[0].Attributes {
			if string(kv.Key) == key {
				return kv.Value.AsString(), true
			}
		}
		return "", false
	}
	in := guard.Input{Phase: guard.PhasePreTurn, Content: "x"}

	t.Run("decision mode is recorded", func(t *testing.T) {
		must := require.New(t)
		loop, exporter := newLoop(t, &modeGuard{decision: &guard.Decision{Verdict: guard.VerdictAllow, GuardID: "cloud-judge", StructuredOutput: types.JudgeStructuredJSONSchema}})
		allowed, _, _ := loop.guardCheck(context.Background(), in, false)
		must.True(allowed)
		got, ok := spanAttr(t, exporter, "guard.structured_output")
		must.True(ok)
		must.Equal(types.JudgeStructuredJSONSchema, got) //nolint:testifylint // a mode name, not a JSON document
	})

	t.Run("failed check records the mode and keeps it on the fail-open decision", func(t *testing.T) {
		must := require.New(t)
		loop, exporter := newLoop(t, &modeGuard{err: &guard.Error{GuardID: "cloud-judge", StructuredOutput: types.JudgeStructuredPromptOnly, Err: errors.New("no verdict")}})
		allowed, d, _ := loop.guardCheck(context.Background(), in, true)
		must.True(allowed)
		must.Equal(types.JudgeStructuredPromptOnly, d.StructuredOutput)
		got, ok := spanAttr(t, exporter, "guard.structured_output")
		must.True(ok)
		must.Equal(types.JudgeStructuredPromptOnly, got)
	})

	t.Run("guard without a mode adds no attribute", func(t *testing.T) {
		must := require.New(t)
		loop, exporter := newLoop(t, &modeGuard{decision: &guard.Decision{Verdict: guard.VerdictAllow, GuardID: "none"}})
		loop.guardCheck(context.Background(), in, false)
		_, ok := spanAttr(t, exporter, "guard.structured_output")
		must.False(ok)
		_, ok = spanAttr(t, exporter, string(attribute.Key("guard.id")))
		must.True(ok)
	})
}

func verifierVerifyContext() verifier.VerifyContext {
	return verifier.VerifyContext{Messages: []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "do it"}}}}}
}
