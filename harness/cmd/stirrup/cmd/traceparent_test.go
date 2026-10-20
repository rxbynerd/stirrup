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
	"github.com/stretchr/testify/require"
	oteltrace "go.opentelemetry.io/otel/trace"
	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

func TestRunConfigCommand_RejectsTraceParentFlags(t *testing.T) {
	must := require.New(t)
	for _, name := range []string{"traceparent", "tracestate"} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			must.Nil(runConfigCmd.Flags().Lookup(name), "run-config must not register --%s; it stays harness-only", name)
			must.NotNil(harnessCmd.Flags().Lookup(name), "harness must register --%s", name)
		})
	}

	cmd := newTestRunConfigCommand()
	err := cmd.ParseFlags([]string{"--traceparent", cliTraceparent})
	must.ErrorContains(err, "unknown flag: --traceparent")
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

func TestTraceParentFlags_ProduceRemoteSpanContext(t *testing.T) {
	must := require.New(t)
	ctx := upstreamContext(t, context.Background(), [][2]string{
		{"traceparent", cliTraceparent},
		{"tracestate", "vendor=value"},
	})
	sc := oteltrace.SpanContextFromContext(ctx)
	must.True(sc.IsValid() && sc.IsRemote() && sc.IsSampled(), "span context = %+v, want a valid, remote, sampled context", sc)
	must.Equal(cliTraceID, sc.TraceID().String(), "trace ID")
	must.Equal(cliSpanID, sc.SpanID().String(), "span ID")
	must.Equal("vendor=value", sc.TraceState().String(), "tracestate")
}

func TestTraceParentFlags_LeaveContextUnchangedWhenInvalid(t *testing.T) {
	cases := map[string][][2]string{
		"no flags":                  nil,
		"wrong length":              {{"traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7"}},
		"bad version":               {{"traceparent", "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}},
		"tracestate without parent": {{"tracestate", "vendor=value"}},
	}
	for name, flags := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			base := context.Background()
			ctx := upstreamContext(t, base, flags)
			must.Equal(base, ctx, "context was replaced")
			must.False(oteltrace.SpanContextFromContext(ctx).IsValid(), "span context became valid")
		})
	}
}

// unsetEnv removes key for the test and restores its prior state after.
// An empty value is not the same as unset: the SDK reads an empty
// OTEL_TRACES_SAMPLER as an unsupported sampler.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	must := require.New(t)
	t.Setenv(key, "")
	must.NoError(os.Unsetenv(key), "unset %s", key)
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
	must := require.New(t)
	cmd, receiver := traceParentCommand(t, flags)

	stdoutDone := captureStdout(t)
	runErr := runHarness(cmd, nil)
	stdout := stdoutDone()
	must.NoError(runErr, "runHarness stdout: %s", stdout)
	for _, line := range strings.Split(stdout, "\n") {
		payload, ok := strings.CutPrefix(line, "STIRRUP_RESULT ")
		if !ok {
			continue
		}
		var result types.RunResult
		must.NoError(json.Unmarshal([]byte(payload), &result), "STIRRUP_RESULT does not parse: %s", payload)
		return result, receiver
	}
	must.FailNow("no STIRRUP_RESULT line in stdout", "stdout: %q", stdout)
	return types.RunResult{}, nil
}

func TestRunHarness_TraceParentParentsExportedRunSpan(t *testing.T) {
	must := require.New(t)
	result, receiver := runHarnessWithTraceParent(t, [][2]string{
		{"traceparent", cliTraceparent},
		{"tracestate", "vendor=value"},
	})

	must.NotNil(result.TraceReference, "RunResult.traceReference")
	must.Equal(cliTraceID, result.TraceReference.TraceID, "traceReference.traceID")
	must.True(result.TraceReference.Sampled, "traceReference.sampled under a sampled parent")

	root, seen := receiver.span("invoke_agent stirrup")
	must.NotNil(root, "no exported invoke_agent stirrup span; received %s", strings.Join(seen, ", "))
	must.Equal(cliTraceID, hex.EncodeToString(root.GetTraceId()), "run span trace ID")
	must.Equal(cliSpanID, hex.EncodeToString(root.GetParentSpanId()), "run span parent, the flag's span")
	must.Equal(hex.EncodeToString(root.GetSpanId()), result.TraceReference.RunSpanID, "traceReference.runSpanId")
}

func TestRunHarness_InvalidTraceParentStartsNewRoot(t *testing.T) {
	must := require.New(t)
	result, receiver := runHarnessWithTraceParent(t, [][2]string{
		{"traceparent", "ff-" + cliTraceID + "-" + cliSpanID + "-01"},
	})

	root, seen := receiver.span("invoke_agent stirrup")
	must.NotNil(root, "no exported invoke_agent stirrup span; received %s", strings.Join(seen, ", "))
	must.Empty(root.GetParentSpanId(), "run span should be a new root")
	must.NotEqual(cliTraceID, hex.EncodeToString(root.GetTraceId()), "run span joined the invalid traceparent's trace")
	must.NotNil(result.TraceReference, "RunResult.traceReference")
	must.NotEmpty(result.TraceReference.TraceID, "freshly minted trace ID")
	must.NotEqual(cliTraceID, result.TraceReference.TraceID, "freshly minted trace ID")
}

func TestRunHarness_UnsampledTraceParentExportsNothing(t *testing.T) {
	must := require.New(t)
	result, receiver := runHarnessWithTraceParent(t, [][2]string{
		{"traceparent", "00-" + cliTraceID + "-" + cliSpanID + "-00"},
	})

	must.NotNil(result.TraceReference, "RunResult.traceReference")
	must.Equal(cliTraceID, result.TraceReference.TraceID, "traceReference.traceID")
	must.False(result.TraceReference.Sampled, "traceReference.sampled under an unsampled parent")
	if n := receiver.count(); n != 0 {
		_, names := receiver.span("")
		must.Equal(0, n, "OTLP receiver spans under an unsampled parent: %v", names)
	}
}

func TestWarnIgnoredTraceParent_LogsOnlyWhenFlagsIgnored(t *testing.T) {
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
			must := require.New(t)
			logs := captureSlog(t)
			warnIgnoredTraceParent(tc.cfg, tc.traceParent, tc.traceState)
			got := logs.String()
			if tc.wantWarning == "" {
				must.Empty(got, "unexpected log output")
			} else {
				must.Contains(got, tc.wantWarning, "log output")
			}
		})
	}
}

func TestRunHarness_InvalidTraceParentLogsWarning(t *testing.T) {
	must := require.New(t)
	logs := captureSlog(t)
	runHarnessWithTraceParent(t, [][2]string{{"traceparent", "not-a-traceparent"}})
	must.Contains(logs.String(), "--traceparent ignored", "log output should carry the ignored-traceparent warning")
}

func TestRunHarness_OutputRunConfigIgnoresTraceParentSilently(t *testing.T) {
	must := require.New(t)
	logs := captureSlog(t)
	cmd := newTestHarnessCommand()
	setFlags(t, cmd.Flags(), [][2]string{
		{"prompt", "p"},
		{"traceparent", "not-a-traceparent"},
		{"output-runconfig", t.TempDir() + "/rc.json"},
	})
	must.NoError(runHarness(cmd, nil))
	must.NotContains(logs.String(), "traceparent", "--output-runconfig should not log a trace parent warning")
}

func TestRunHarness_FollowUpAfterTraceParentStartsNewTrace(t *testing.T) {
	must := require.New(t)
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
		sr := srv.waitDone(t, what)
		must.Equal("success", sr, "%s stop_reason", what)
	}
	select {
	case err := <-errCh:
		_ = stdoutDone()
		must.NoError(err, "runHarness")
	case <-time.After(15 * time.Second):
		must.FailNow("runHarness did not return after cancel")
	}

	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	var roots []*otlptrace.Span
	for _, sp := range receiver.spans {
		if sp.GetName() == "invoke_agent stirrup" {
			roots = append(roots, sp)
		}
	}
	must.Len(roots, 2, "run spans exported for the primary and follow-up runs")
	byTrace := map[string]*otlptrace.Span{}
	for _, sp := range roots {
		byTrace[hex.EncodeToString(sp.GetTraceId())] = sp
	}
	must.Len(byTrace, 2, "primary and follow-up share a trace; trace IDs = %v", byTrace)
	primary := byTrace[cliTraceID]
	must.NotNil(primary, "no run span on the flag's trace %s", cliTraceID)
	must.Equal(cliSpanID, hex.EncodeToString(primary.GetParentSpanId()), "primary run span parent, the flag's span")
	for id, sp := range byTrace {
		if id != cliTraceID {
			must.Empty(sp.GetParentSpanId(), "follow-up run span parent for trace %s, want a new root", id)
		}
	}
}
