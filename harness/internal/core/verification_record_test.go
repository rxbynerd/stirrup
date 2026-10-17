package core

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

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

func TestRun_VerifierErrorIsScrubbedOnEventsTraceAndLog(t *testing.T) {
	const marker = "VERIFIER-INFRA-MARKER-662"
	loop, events, traceBuf, logBuf := verificationTestLoop(&erroringVerifier{
		err: fmt.Errorf("exec failed: %s token=%s", marker, fakeVerificationSecret),
	})

	rt, err := loop.Run(context.Background(), verifierConfig("test-runner"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rt.Outcome != "verification_error" {
		t.Fatalf("outcome = %q, want verification_error", rt.Outcome)
	}

	for name, out := range map[string]string{
		"stdio events": events.String(),
		"jsonl trace":  traceBuf.String(),
		"log":          logBuf.String(),
	} {
		if !strings.Contains(out, marker) {
			t.Errorf("%s lacks the verifier error marker:\n%s", name, out)
		}
		if strings.Contains(out, fakeVerificationSecret) {
			t.Errorf("%s carries the unscrubbed secret", name)
		}
	}
	if !strings.Contains(events.String(), `"type":"warning"`) {
		t.Errorf("no warning event emitted:\n%s", events.String())
	}
	if !strings.Contains(logBuf.String(), "verifier error") || !strings.Contains(logBuf.String(), "level=WARN") {
		t.Errorf("no warn-level verifier error log:\n%s", logBuf.String())
	}

	if len(rt.VerificationResults) != 1 {
		t.Fatalf("VerificationResults = %+v, want one entry", rt.VerificationResults)
	}
	got := rt.VerificationResults[0]
	if got.Attempt != 1 || got.Passed || !strings.Contains(got.Error, marker) || !strings.Contains(got.Error, "[REDACTED]") {
		t.Errorf("recorded attempt = %+v, want attempt 1, failed, scrubbed error with marker", got)
	}
}

func TestRun_ExhaustedRetriesRecordEveryAttemptIncludingTheLast(t *testing.T) {
	var script []types.VerificationResult
	for i := 1; i <= maxVerificationRetries+1; i++ {
		script = append(script, types.VerificationResult{Feedback: fmt.Sprintf("attempt-%d failure", i)})
	}
	loop, _, traceBuf, _ := verificationTestLoop(&resultScriptVerifier{results: script})

	rt, err := loop.Run(context.Background(), verifierConfig("test-runner"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rt.Outcome != "verification_failed" {
		t.Fatalf("outcome = %q, want verification_failed", rt.Outcome)
	}
	if len(rt.VerificationResults) != len(script) {
		t.Fatalf("recorded %d attempts, want %d: %+v", len(rt.VerificationResults), len(script), rt.VerificationResults)
	}
	for i, r := range rt.VerificationResults {
		if r.Attempt != i+1 || r.Passed || r.Feedback != script[i].Feedback {
			t.Errorf("attempt %d = %+v, want attempt %d with feedback %q", i, r, i+1, script[i].Feedback)
		}
	}
	last := script[len(script)-1].Feedback
	if !strings.Contains(traceBuf.String(), last) {
		t.Errorf("JSONL trace lacks the deciding attempt's feedback %q", last)
	}
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
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rt.Outcome != "success" || len(rt.VerificationResults) != 1 {
		t.Fatalf("outcome=%q results=%+v, want success with one result", rt.Outcome, rt.VerificationResults)
	}
	got := rt.VerificationResults[0]
	if !got.Passed || got.Attempt != 1 {
		t.Errorf("result = %+v, want attempt 1 passed", got)
	}
	if strings.Contains(fmt.Sprintf("%v", got.Details), fakeVerificationSecret) {
		t.Error("Details carry the unscrubbed secret")
	}
	if cmd, _ := got.Details["command"].(string); !strings.HasPrefix(cmd, "go test ./...") || !strings.Contains(cmd, "[REDACTED]") {
		t.Errorf("command detail = %q, want the scrubbed command", cmd)
	}
	if got.Details["exitCode"] != 0 {
		t.Errorf("exitCode detail = %v, want 0", got.Details["exitCode"])
	}
	out, _ := got.Details["output"].(string)
	if len(out) >= len(long) || !strings.HasPrefix(out, strings.Repeat("x", verificationTextMaxBytes)) || strings.HasPrefix(out, strings.Repeat("x", verificationTextMaxBytes+1)) {
		t.Errorf("output detail is %d bytes, want a %d-byte prefix plus the truncation marker", len(out), verificationTextMaxBytes)
	}
}

func TestRun_VerifierFeedbackIsCappedPerAttempt(t *testing.T) {
	long := strings.Repeat("y", 3*verificationTextMaxBytes)
	loop, _, _, _ := verificationTestLoop(&resultScriptVerifier{results: []types.VerificationResult{
		{Feedback: long},
		{Passed: true},
	}})

	rt, err := loop.Run(context.Background(), verifierConfig("test-runner"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rt.VerificationResults) != 2 {
		t.Fatalf("VerificationResults = %+v, want two entries", rt.VerificationResults)
	}
	fb := rt.VerificationResults[0].Feedback
	if len(fb) >= len(long) || !strings.HasPrefix(fb, strings.Repeat("y", verificationTextMaxBytes)) || strings.HasPrefix(fb, strings.Repeat("y", verificationTextMaxBytes+1)) {
		t.Errorf("recorded feedback is %d bytes, want a %d-byte prefix plus the truncation marker", len(fb), verificationTextMaxBytes)
	}
}

func TestRun_FailThenPassIsEvalPassed(t *testing.T) {
	loop, _, _, _ := verificationTestLoop(&passAfterVerifier{failures: 1})

	rt, err := loop.Run(context.Background(), verifierConfig("test-runner"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rt.Outcome != "success" {
		t.Fatalf("outcome = %q, want success", rt.Outcome)
	}
	if len(rt.VerificationResults) != 2 || rt.VerificationResults[0].Passed || !rt.VerificationResults[1].Passed {
		t.Fatalf("VerificationResults = %+v, want a failed then a passing attempt", rt.VerificationResults)
	}
	if got := types.EvalOutcomeFor(*rt); got != types.EvalPassed {
		t.Errorf("EvalOutcomeFor = %q, want passed", got)
	}
}

func TestRun_NoneVerifierRecordsNothing(t *testing.T) {
	for _, verifierType := range []string{"none", ""} {
		t.Run("type="+verifierType, func(t *testing.T) {
			loop, _, _, _ := verificationTestLoop(verifier.NewNoneVerifier())

			rt, err := loop.Run(context.Background(), verifierConfig(verifierType))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if rt.Outcome != "success" {
				t.Fatalf("outcome = %q, want success", rt.Outcome)
			}
			if len(rt.VerificationResults) != 0 {
				t.Errorf("VerificationResults = %+v, want empty for the none verifier", rt.VerificationResults)
			}
		})
	}
}

func TestBoundVerificationValue_NonJSONNativeValues(t *testing.T) {
	got := boundVerificationValue(func() {})
	if s, ok := got.(string); !ok || !strings.HasPrefix(s, "[unrecordable ") {
		t.Errorf("boundVerificationValue(func) = %#v, want an unrecordable placeholder", got)
	}

	type report struct{ Token string }
	normalised, ok := boundVerificationValue(report{Token: fakeVerificationSecret}).(map[string]any)
	if !ok || normalised["Token"] != "[REDACTED]" {
		t.Errorf("boundVerificationValue(struct) = %#v, want a map with the token scrubbed", normalised)
	}
}
