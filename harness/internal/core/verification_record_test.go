package core

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// fakeVerificationSecret is secret-shaped for the log scrubber's
// github_pat pattern; built at runtime so no literal token sits in source.
var fakeVerificationSecret = "ghp_" + strings.Repeat("Ab3", 12)

type erroringVerifier struct{ err error }

func (v *erroringVerifier) Verify(context.Context, verifier.VerifyContext) (*types.VerificationResult, error) {
	return nil, v.err
}

// resultScriptVerifier returns results[i] on the i-th call and repeats the
// last entry once the script runs out.
type resultScriptVerifier struct {
	results []types.VerificationResult
	calls   int
}

func (v *resultScriptVerifier) Verify(context.Context, verifier.VerifyContext) (*types.VerificationResult, error) {
	i := min(v.calls, len(v.results)-1)
	v.calls++
	r := v.results[i]
	return &r, nil
}

// verificationTestLoop wires a loop whose transport, JSONL trace and
// slog output land in the returned buffers. Tracing stays on the noop
// provider, so nothing reaches OpenTelemetry.
func verificationTestLoop(v verifier.Verifier) (loop *AgenticLoop, events, traceBuf, logBuf *bytes.Buffer) {
	events, traceBuf, logBuf = &bytes.Buffer{}, &bytes.Buffer{}, &bytes.Buffer{}
	loop = buildTestLoop(&mockProvider{events: []types.StreamEvent{
		{Type: "text_delta", Text: "Done."},
		{Type: "message_complete", StopReason: "end_turn"},
	}})
	loop.Verifier = v
	loop.Transport = transport.NewStdioTransport(events, &bytes.Buffer{})
	loop.Trace = trace.NewJSONLTraceEmitter(traceBuf, false)
	loop.Logger = slog.New(slog.NewTextHandler(logBuf, nil))
	return loop, events, traceBuf, logBuf
}

func verifierConfig(verifierType string) *types.RunConfig {
	cfg := buildTestConfig()
	cfg.Verifier = types.VerifierConfig{Type: verifierType, Command: "go test ./..."}
	return cfg
}

// requireCappedRepeat asserts that got keeps the first verificationTextMaxBytes
// bytes of a run of c that was n bytes long, and nothing more.
func requireCappedRepeat(t *testing.T, got, c string, n int) {
	t.Helper()
	must := require.New(t)
	must.Less(len(got), n)
	must.True(strings.HasPrefix(got, strings.Repeat(c, verificationTextMaxBytes)),
		"got %d bytes, want a %d-byte prefix", len(got), verificationTextMaxBytes)
	must.False(strings.HasPrefix(got, strings.Repeat(c, verificationTextMaxBytes+1)))
}

func TestRun_VerifierErrorIsScrubbedOnEventsTraceAndLog(t *testing.T) {
	must := require.New(t)
	const marker = "VERIFIER-INFRA-MARKER-662"
	loop, events, traceBuf, logBuf := verificationTestLoop(&erroringVerifier{
		err: fmt.Errorf("exec failed: %s token=%s", marker, fakeVerificationSecret),
	})

	rt, err := loop.Run(context.Background(), verifierConfig("test-runner"))
	must.NoError(err)
	must.Equal("verification_error", rt.Outcome)

	outputs := []struct {
		name string
		out  string
	}{
		{name: "stdio events", out: events.String()},
		{name: "jsonl trace", out: traceBuf.String()},
		{name: "log", out: logBuf.String()},
	}
	for _, o := range outputs {
		t.Run(o.name, func(t *testing.T) {
			must := require.New(t)
			must.Contains(o.out, marker)
			must.NotContains(o.out, fakeVerificationSecret)
		})
	}

	must.Contains(events.String(), `"type":"warning"`)
	must.Contains(logBuf.String(), "verifier error")
	must.Contains(logBuf.String(), "level=WARN")

	must.Len(rt.VerificationResults, 1)
	got := rt.VerificationResults[0]
	must.Equal(1, got.Attempt)
	must.False(got.Passed)
	must.Contains(got.Error, marker)
	must.Contains(got.Error, "[REDACTED]")
}

func TestRun_ExhaustedRetriesRecordEveryAttemptIncludingTheLast(t *testing.T) {
	must := require.New(t)
	var script []types.VerificationResult
	for i := 1; i <= maxVerificationRetries+1; i++ {
		script = append(script, types.VerificationResult{Feedback: fmt.Sprintf("attempt-%d failure", i)})
	}
	loop, _, traceBuf, _ := verificationTestLoop(&resultScriptVerifier{results: script})

	rt, err := loop.Run(context.Background(), verifierConfig("test-runner"))
	must.NoError(err)
	must.Equal("verification_failed", rt.Outcome)
	must.Len(rt.VerificationResults, len(script))

	for i, r := range rt.VerificationResults {
		t.Run(fmt.Sprintf("attempt %d", i+1), func(t *testing.T) {
			must := require.New(t)
			must.Equal(i+1, r.Attempt)
			must.False(r.Passed)
			must.Equal(script[i].Feedback, r.Feedback)
		})
	}

	last := script[len(script)-1].Feedback
	must.Contains(traceBuf.String(), last)
}

func TestRun_PassingVerifierRecordsScrubbedCappedDetails(t *testing.T) {
	must := require.New(t)
	long := strings.Repeat("x", 3*verificationTextMaxBytes)
	loop, _, _, _ := verificationTestLoop(&resultScriptVerifier{results: []types.VerificationResult{{
		Passed: true,
		Details: map[string]any{
			"command":  "go test ./... # " + fakeVerificationSecret,
			"exitCode": 0,
			"output":   long,
			"nested":   map[string]any{"token": fakeVerificationSecret},
			"list":     []any{fakeVerificationSecret, 3},
			"typed":    []string{fakeVerificationSecret},
		},
	}}})

	rt, err := loop.Run(context.Background(), verifierConfig("test-runner"))
	must.NoError(err)
	must.Equal("success", rt.Outcome)
	must.Len(rt.VerificationResults, 1)

	got := rt.VerificationResults[0]
	must.True(got.Passed)
	must.Equal(1, got.Attempt)
	must.NotContains(fmt.Sprintf("%v", got.Details), fakeVerificationSecret)

	cmd, _ := got.Details["command"].(string)
	must.True(strings.HasPrefix(cmd, "go test ./..."), "command detail is %q", cmd)
	must.Contains(cmd, "[REDACTED]")

	must.Equal(0, got.Details["exitCode"])

	out, _ := got.Details["output"].(string)
	requireCappedRepeat(t, out, "x", len(long))
}

func TestRun_VerifierFeedbackIsCappedPerAttempt(t *testing.T) {
	must := require.New(t)
	long := strings.Repeat("y", 3*verificationTextMaxBytes)
	loop, _, _, _ := verificationTestLoop(&resultScriptVerifier{results: []types.VerificationResult{
		{Feedback: long},
		{Passed: true},
	}})

	rt, err := loop.Run(context.Background(), verifierConfig("test-runner"))
	must.NoError(err)
	must.Len(rt.VerificationResults, 2)

	requireCappedRepeat(t, rt.VerificationResults[0].Feedback, "y", len(long))
}

func TestRun_FailThenPassIsEvalPassed(t *testing.T) {
	must := require.New(t)
	loop, _, _, _ := verificationTestLoop(&passAfterVerifier{failures: 1})

	rt, err := loop.Run(context.Background(), verifierConfig("test-runner"))
	must.NoError(err)
	must.Equal("success", rt.Outcome)

	must.Len(rt.VerificationResults, 2)
	must.False(rt.VerificationResults[0].Passed)
	must.True(rt.VerificationResults[1].Passed)
	must.Equal(types.EvalPassed, types.EvalOutcomeFor(*rt))
}

func TestRun_NoneVerifierRecordsNothing(t *testing.T) {
	cases := []struct {
		name         string
		verifierType string
	}{
		{name: "none type", verifierType: "none"},
		{name: "empty type", verifierType: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			loop, _, _, _ := verificationTestLoop(verifier.NewNoneVerifier())

			rt, err := loop.Run(context.Background(), verifierConfig(tc.verifierType))
			must.NoError(err)
			must.Equal("success", rt.Outcome)
			must.Empty(rt.VerificationResults)
		})
	}
}

func TestBoundVerificationValue_NonJSONNativeValues(t *testing.T) {
	t.Run("func becomes placeholder", func(t *testing.T) {
		must := require.New(t)
		got := boundVerificationValue(func() {})
		s, ok := got.(string)
		must.True(ok, "got %#v, want a string placeholder", got)
		must.True(strings.HasPrefix(s, "[unrecordable "), "got %q", s)
	})

	t.Run("struct token is scrubbed", func(t *testing.T) {
		must := require.New(t)
		type report struct{ Token string }
		got := boundVerificationValue(report{Token: fakeVerificationSecret})
		normalised, ok := got.(map[string]any)
		must.True(ok, "got %#v, want a map", got)
		must.Equal("[REDACTED]", normalised["Token"])
	})
}
