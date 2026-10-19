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
	require.Less(t, len(got), n)
	require.True(t, strings.HasPrefix(got, strings.Repeat(c, verificationTextMaxBytes)),
		"got %d bytes, want a %d-byte prefix", len(got), verificationTextMaxBytes)
	require.False(t, strings.HasPrefix(got, strings.Repeat(c, verificationTextMaxBytes+1)))
}

func TestRun_VerifierErrorIsScrubbedOnEventsTraceAndLog(t *testing.T) {
	const marker = "VERIFIER-INFRA-MARKER-662"
	loop, events, traceBuf, logBuf := verificationTestLoop(&erroringVerifier{
		err: fmt.Errorf("exec failed: %s token=%s", marker, fakeVerificationSecret),
	})

	rt, err := loop.Run(context.Background(), verifierConfig("test-runner"))
	require.NoError(t, err)
	require.Equal(t, "verification_error", rt.Outcome)

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
			require.Contains(t, o.out, marker)
			require.NotContains(t, o.out, fakeVerificationSecret)
		})
	}

	require.Contains(t, events.String(), `"type":"warning"`)
	require.Contains(t, logBuf.String(), "verifier error")
	require.Contains(t, logBuf.String(), "level=WARN")

	require.Len(t, rt.VerificationResults, 1)
	got := rt.VerificationResults[0]
	require.Equal(t, 1, got.Attempt)
	require.False(t, got.Passed)
	require.Contains(t, got.Error, marker)
	require.Contains(t, got.Error, "[REDACTED]")
}

func TestRun_ExhaustedRetriesRecordEveryAttemptIncludingTheLast(t *testing.T) {
	var script []types.VerificationResult
	for i := 1; i <= maxVerificationRetries+1; i++ {
		script = append(script, types.VerificationResult{Feedback: fmt.Sprintf("attempt-%d failure", i)})
	}
	loop, _, traceBuf, _ := verificationTestLoop(&resultScriptVerifier{results: script})

	rt, err := loop.Run(context.Background(), verifierConfig("test-runner"))
	require.NoError(t, err)
	require.Equal(t, "verification_failed", rt.Outcome)
	require.Len(t, rt.VerificationResults, len(script))

	for i, r := range rt.VerificationResults {
		t.Run(fmt.Sprintf("attempt %d", i+1), func(t *testing.T) {
			require.Equal(t, i+1, r.Attempt)
			require.False(t, r.Passed)
			require.Equal(t, script[i].Feedback, r.Feedback)
		})
	}

	last := script[len(script)-1].Feedback
	require.Contains(t, traceBuf.String(), last)
}

func TestRun_PassingVerifierRecordsScrubbedCappedDetails(t *testing.T) {
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
	require.NoError(t, err)
	require.Equal(t, "success", rt.Outcome)
	require.Len(t, rt.VerificationResults, 1)

	got := rt.VerificationResults[0]
	require.True(t, got.Passed)
	require.Equal(t, 1, got.Attempt)
	require.NotContains(t, fmt.Sprintf("%v", got.Details), fakeVerificationSecret)

	cmd, _ := got.Details["command"].(string)
	require.True(t, strings.HasPrefix(cmd, "go test ./..."), "command detail is %q", cmd)
	require.Contains(t, cmd, "[REDACTED]")

	require.Equal(t, 0, got.Details["exitCode"])

	out, _ := got.Details["output"].(string)
	requireCappedRepeat(t, out, "x", len(long))
}

func TestRun_VerifierFeedbackIsCappedPerAttempt(t *testing.T) {
	long := strings.Repeat("y", 3*verificationTextMaxBytes)
	loop, _, _, _ := verificationTestLoop(&resultScriptVerifier{results: []types.VerificationResult{
		{Feedback: long},
		{Passed: true},
	}})

	rt, err := loop.Run(context.Background(), verifierConfig("test-runner"))
	require.NoError(t, err)
	require.Len(t, rt.VerificationResults, 2)

	requireCappedRepeat(t, rt.VerificationResults[0].Feedback, "y", len(long))
}

func TestRun_FailThenPassIsEvalPassed(t *testing.T) {
	loop, _, _, _ := verificationTestLoop(&passAfterVerifier{failures: 1})

	rt, err := loop.Run(context.Background(), verifierConfig("test-runner"))
	require.NoError(t, err)
	require.Equal(t, "success", rt.Outcome)

	require.Len(t, rt.VerificationResults, 2)
	require.False(t, rt.VerificationResults[0].Passed)
	require.True(t, rt.VerificationResults[1].Passed)
	require.Equal(t, types.EvalPassed, types.EvalOutcomeFor(*rt))
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
			loop, _, _, _ := verificationTestLoop(verifier.NewNoneVerifier())

			rt, err := loop.Run(context.Background(), verifierConfig(tc.verifierType))
			require.NoError(t, err)
			require.Equal(t, "success", rt.Outcome)
			require.Empty(t, rt.VerificationResults)
		})
	}
}

func TestBoundVerificationValue_NonJSONNativeValues(t *testing.T) {
	t.Run("func becomes placeholder", func(t *testing.T) {
		got := boundVerificationValue(func() {})
		s, ok := got.(string)
		require.True(t, ok, "got %#v, want a string placeholder", got)
		require.True(t, strings.HasPrefix(s, "[unrecordable "), "got %q", s)
	})

	t.Run("struct token is scrubbed", func(t *testing.T) {
		type report struct{ Token string }
		got := boundVerificationValue(report{Token: fakeVerificationSecret})
		normalised, ok := got.(map[string]any)
		require.True(t, ok, "got %#v, want a map", got)
		require.Equal(t, "[REDACTED]", normalised["Token"])
	})
}
