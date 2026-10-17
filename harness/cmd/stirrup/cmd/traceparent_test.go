package cmd

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	oteltrace "go.opentelemetry.io/otel/trace"
	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

func TestRunConfigCommandRejectsTraceParentFlags(t *testing.T) {
	for _, name := range []string{"traceparent", "tracestate"} {
		if runConfigCmd.Flags().Lookup(name) != nil {
			t.Errorf("run-config registers --%s; it must stay harness-only", name)
		}
		if harnessCmd.Flags().Lookup(name) == nil {
			t.Errorf("harness does not register --%s", name)
		}
	}

	cmd := newTestRunConfigCommand()
	err := cmd.ParseFlags([]string{"--traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"})
	if err == nil || !strings.Contains(err.Error(), "unknown flag: --traceparent") {
		t.Fatalf("ParseFlags error = %v, want an unknown flag error", err)
	}
}

const (
	cliTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	cliTraceID     = "4bf92f3577b34da6a3ce929d0e0e4736"
	cliSpanID      = "00f067aa0ba902b7"
)

// upstreamContext applies the harness command's parent flags to a base
// context the way runHarness hands them to runWithConfig.
func upstreamContext(t *testing.T, base context.Context, flags [][2]string) context.Context {
	t.Helper()
	cmd := newTestHarnessCommand()
	setFlags(t, cmd.Flags(), flags)
	traceParent, _ := cmd.Flags().GetString("traceparent")
	traceState, _ := cmd.Flags().GetString("tracestate")
	return observability.ExtractTraceContext(base, traceParent, traceState)
}

func TestTraceParentFlagsProduceRemoteSpanContext(t *testing.T) {
	ctx := upstreamContext(t, context.Background(), [][2]string{
		{"traceparent", cliTraceparent},
		{"tracestate", "vendor=value"},
	})
	sc := oteltrace.SpanContextFromContext(ctx)
	if !sc.IsValid() || !sc.IsRemote() || !sc.IsSampled() {
		t.Fatalf("span context = %+v, want a valid, remote, sampled context", sc)
	}
	if sc.TraceID().String() != cliTraceID || sc.SpanID().String() != cliSpanID {
		t.Errorf("IDs = %s/%s, want %s/%s", sc.TraceID(), sc.SpanID(), cliTraceID, cliSpanID)
	}
	if got := sc.TraceState().String(); got != "vendor=value" {
		t.Errorf("tracestate = %q, want vendor=value", got)
	}
}

func TestTraceParentFlagsLeaveContextUnchangedWhenInvalid(t *testing.T) {
	cases := map[string][][2]string{
		"no flags":                  nil,
		"wrong length":              {{"traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7"}},
		"bad version":               {{"traceparent", "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}},
		"tracestate without parent": {{"tracestate", "vendor=value"}},
	}
	for name, flags := range cases {
		t.Run(name, func(t *testing.T) {
			base := context.Background()
			ctx := upstreamContext(t, base, flags)
			if ctx != base {
				t.Error("context was replaced")
			}
			if oteltrace.SpanContextFromContext(ctx).IsValid() {
				t.Error("span context became valid")
			}
		})
	}
}

// unsetEnv removes key for the test and restores its prior state after.
// An empty value is not the same as unset: the SDK reads an empty
// OTEL_TRACES_SAMPLER as an unsupported sampler.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
}

// captureSlog routes the default slog logger to a buffer for the test.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return &buf
}

// traceParentCommand builds a harness command for a one-turn run that
// exports spans over OTLP/HTTP to a fake receiver. The sampler env var is
// unset so the SDK's parent-based default applies.
func traceParentCommand(t *testing.T, flags [][2]string) (*cobra.Command, *otlpSpanReceiver) {
	t.Helper()
	unsetEnv(t, "OTEL_TRACES_SAMPLER")
	t.Setenv("TEST_TRACEPARENT_KEY", "unused-fake-key")
	provider := openAICompatibleSuccessServer(t)
	t.Cleanup(provider.Close)
	receiver := &otlpSpanReceiver{}
	otlp := httptest.NewServer(receiver)
	t.Cleanup(otlp.Close)

	cmd := newTestHarnessCommand()
	setFlags(t, cmd.Flags(), append([][2]string{
		{"prompt", "Say hello."},
		{"mode", "planning"},
		{"provider", "openai-compatible"},
		{"api-key-ref", "secret://TEST_TRACEPARENT_KEY"},
		{"base-url", provider.URL},
		{"model", "gpt-4o-mini"},
		{"executor", "local"},
		{"workspace", t.TempDir()},
		{"trace-emitter", "otel"},
		{"otel-endpoint", otlp.URL},
		{"otel-protocol", "http/protobuf"},
		{"output", "json"},
	}, flags...))
	return cmd, receiver
}

// runHarnessWithTraceParent drives runHarness end to end and returns the
// STIRRUP_RESULT payload.
func runHarnessWithTraceParent(t *testing.T, flags [][2]string) (types.RunResult, *otlpSpanReceiver) {
	t.Helper()
	cmd, receiver := traceParentCommand(t, flags)

	stdoutDone := captureStdout(t)
	runErr := runHarness(cmd, nil)
	stdout := stdoutDone()
	if runErr != nil {
		t.Fatalf("runHarness: %v\nstdout: %s", runErr, stdout)
	}
	for _, line := range strings.Split(stdout, "\n") {
		payload, ok := strings.CutPrefix(line, "STIRRUP_RESULT ")
		if !ok {
			continue
		}
		var result types.RunResult
		if err := json.Unmarshal([]byte(payload), &result); err != nil {
			t.Fatalf("STIRRUP_RESULT does not parse: %v\n%s", err, payload)
		}
		return result, receiver
	}
	t.Fatalf("no STIRRUP_RESULT line in stdout: %q", stdout)
	return types.RunResult{}, nil
}

func TestRunHarness_TraceParentParentsExportedRunSpan(t *testing.T) {
	result, receiver := runHarnessWithTraceParent(t, [][2]string{
		{"traceparent", cliTraceparent},
		{"tracestate", "vendor=value"},
	})

	if result.TraceReference == nil || result.TraceReference.TraceID != cliTraceID {
		t.Fatalf("RunResult.traceReference = %+v, want trace ID %s", result.TraceReference, cliTraceID)
	}
	if !result.TraceReference.Sampled {
		t.Error("traceReference.sampled = false, want true under a sampled parent")
	}

	root, seen := receiver.span("invoke_agent stirrup")
	if root == nil {
		t.Fatalf("no exported invoke_agent stirrup span; received %s", strings.Join(seen, ", "))
	}
	wantTrace, _ := hex.DecodeString(cliTraceID)
	wantParent, _ := hex.DecodeString(cliSpanID)
	if !bytes.Equal(root.GetTraceId(), wantTrace) {
		t.Errorf("run span trace ID = %x, want %s", root.GetTraceId(), cliTraceID)
	}
	if !bytes.Equal(root.GetParentSpanId(), wantParent) {
		t.Errorf("run span parent = %x, want the flag's span %s", root.GetParentSpanId(), cliSpanID)
	}
	if got := hex.EncodeToString(root.GetSpanId()); got != result.TraceReference.RunSpanID {
		t.Errorf("traceReference.runSpanId = %s, want the exported run span %s", result.TraceReference.RunSpanID, got)
	}
}

func TestRunHarness_InvalidTraceParentStartsNewRoot(t *testing.T) {
	result, receiver := runHarnessWithTraceParent(t, [][2]string{
		{"traceparent", "ff-" + cliTraceID + "-" + cliSpanID + "-01"},
	})

	root, seen := receiver.span("invoke_agent stirrup")
	if root == nil {
		t.Fatalf("no exported invoke_agent stirrup span; received %s", strings.Join(seen, ", "))
	}
	if len(root.GetParentSpanId()) != 0 {
		t.Errorf("run span parent = %x, want a new root", root.GetParentSpanId())
	}
	if hex.EncodeToString(root.GetTraceId()) == cliTraceID {
		t.Error("run span joined the invalid traceparent's trace")
	}
	if result.TraceReference == nil || result.TraceReference.TraceID == cliTraceID || result.TraceReference.TraceID == "" {
		t.Errorf("RunResult.traceReference = %+v, want a freshly minted trace ID", result.TraceReference)
	}
}

func TestRunHarness_UnsampledTraceParentExportsNothing(t *testing.T) {
	result, receiver := runHarnessWithTraceParent(t, [][2]string{
		{"traceparent", "00-" + cliTraceID + "-" + cliSpanID + "-00"},
	})

	if result.TraceReference == nil || result.TraceReference.TraceID != cliTraceID || result.TraceReference.Sampled {
		t.Errorf("RunResult.traceReference = %+v, want trace ID %s with sampled false", result.TraceReference, cliTraceID)
	}
	if n := receiver.count(); n != 0 {
		_, names := receiver.span("")
		t.Errorf("OTLP receiver got %d spans under an unsampled parent, want none: %v", n, names)
	}
}

func TestWarnIgnoredTraceParent(t *testing.T) {
	otel := &types.RunConfig{TraceEmitter: types.TraceEmitterConfig{Type: "otel"}}
	jsonl := &types.RunConfig{TraceEmitter: types.TraceEmitterConfig{Type: "jsonl"}}
	cases := []struct {
		name                    string
		cfg                     *types.RunConfig
		traceParent, traceState string
		wantWarning             string
	}{
		{"valid parent with otel is silent", otel, cliTraceparent, "vendor=value", ""},
		{"no flags is silent", jsonl, "", "", ""},
		{"invalid traceparent", otel, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7", "", "--traceparent ignored"},
		{"valid parent without spans", jsonl, cliTraceparent, "", "--traceparent has no effect"},
		{"tracestate without traceparent", otel, "", "vendor=value", "--tracestate ignored"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureSlog(t)
			warnIgnoredTraceParent(tc.cfg, tc.traceParent, tc.traceState)
			got := logs.String()
			if tc.wantWarning == "" && got != "" {
				t.Errorf("unexpected log output: %s", got)
			}
			if tc.wantWarning != "" && !strings.Contains(got, tc.wantWarning) {
				t.Errorf("log output = %q, want it to contain %q", got, tc.wantWarning)
			}
		})
	}
}

func TestRunHarness_InvalidTraceParentLogsWarning(t *testing.T) {
	logs := captureSlog(t)
	runHarnessWithTraceParent(t, [][2]string{{"traceparent", "not-a-traceparent"}})
	if !strings.Contains(logs.String(), "--traceparent ignored") {
		t.Errorf("log output = %q, want the ignored-traceparent warning", logs.String())
	}
}

func TestRunHarness_OutputRunConfigIgnoresTraceParentSilently(t *testing.T) {
	logs := captureSlog(t)
	cmd := newTestHarnessCommand()
	setFlags(t, cmd.Flags(), [][2]string{
		{"prompt", "p"},
		{"traceparent", "not-a-traceparent"},
		{"output-runconfig", t.TempDir() + "/rc.json"},
	})
	if err := runHarness(cmd, nil); err != nil {
		t.Fatalf("runHarness: %v", err)
	}
	if strings.Contains(logs.String(), "traceparent") {
		t.Errorf("--output-runconfig logged %q, want no trace parent warning", logs.String())
	}
}

func TestRunHarness_FollowUpAfterTraceParentStartsNewTrace(t *testing.T) {
	srv := newScriptedControlPlane(&pb.RunConfig{}, func(n int) *pb.ControlEvent {
		if n == 0 {
			return &pb.ControlEvent{Type: "user_response", UserResponse: "And again."}
		}
		return &pb.ControlEvent{Type: "cancel"}
	})
	serveControlPlane(t, srv)

	cmd, receiver := traceParentCommand(t, [][2]string{
		{"traceparent", cliTraceparent},
		{"transport", "grpc"},
		{"transport-addr", os.Getenv("CONTROL_PLANE_ADDR")},
		{"followup-grace", "30"},
	})
	stdoutDone := captureStdout(t)
	errCh := make(chan error, 1)
	go func() { errCh <- runHarness(cmd, nil) }()

	for _, what := range []string{"primary run", "follow-up run"} {
		if sr := srv.waitDone(t, what); sr != "success" {
			t.Fatalf("%s done stop_reason = %q, want success", what, sr)
		}
	}
	select {
	case err := <-errCh:
		_ = stdoutDone()
		if err != nil {
			t.Fatalf("runHarness: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runHarness did not return after cancel")
	}

	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	var roots []*otlptrace.Span
	for _, sp := range receiver.spans {
		if sp.GetName() == "invoke_agent stirrup" {
			roots = append(roots, sp)
		}
	}
	if len(roots) != 2 {
		t.Fatalf("exported %d run spans, want 2 (primary and follow-up)", len(roots))
	}
	byTrace := map[string]*otlptrace.Span{}
	for _, sp := range roots {
		byTrace[hex.EncodeToString(sp.GetTraceId())] = sp
	}
	if len(byTrace) != 2 {
		t.Fatalf("primary and follow-up share a trace; trace IDs = %v", byTrace)
	}
	primary := byTrace[cliTraceID]
	if primary == nil || hex.EncodeToString(primary.GetParentSpanId()) != cliSpanID {
		t.Errorf("no run span parented on the flag's span %s/%s", cliTraceID, cliSpanID)
	}
	for id, sp := range byTrace {
		if id != cliTraceID && len(sp.GetParentSpanId()) != 0 {
			t.Errorf("follow-up run span parent = %x, want a new root", sp.GetParentSpanId())
		}
	}
}
