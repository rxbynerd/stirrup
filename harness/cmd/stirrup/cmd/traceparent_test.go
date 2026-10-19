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
	for _, name := range []string{"traceparent", "tracestate"} {
		t.Run(name, func(t *testing.T) {
			require.Nil(t, runConfigCmd.Flags().Lookup(name), "run-config must not register --%s; it stays harness-only", name)
			require.NotNil(t, harnessCmd.Flags().Lookup(name), "harness must register --%s", name)
		})
	}

	cmd := newTestRunConfigCommand()
	err := cmd.ParseFlags([]string{"--traceparent", cliTraceparent})
	require.ErrorContains(t, err, "unknown flag: --traceparent")
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
	ctx := upstreamContext(t, context.Background(), [][2]string{
		{"traceparent", cliTraceparent},
		{"tracestate", "vendor=value"},
	})
	sc := oteltrace.SpanContextFromContext(ctx)
	require.True(t, sc.IsValid() && sc.IsRemote() && sc.IsSampled(), "span context = %+v, want a valid, remote, sampled context", sc)
	require.Equal(t, cliTraceID, sc.TraceID().String(), "trace ID")
	require.Equal(t, cliSpanID, sc.SpanID().String(), "span ID")
	require.Equal(t, "vendor=value", sc.TraceState().String(), "tracestate")
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
			base := context.Background()
			ctx := upstreamContext(t, base, flags)
			require.Equal(t, base, ctx, "context was replaced")
			require.False(t, oteltrace.SpanContextFromContext(ctx).IsValid(), "span context became valid")
		})
	}
}

// unsetEnv removes key for the test and restores its prior state after.
// An empty value is not the same as unset: the SDK reads an empty
// OTEL_TRACES_SAMPLER as an unsupported sampler.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	require.NoError(t, os.Unsetenv(key), "unset %s", key)
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
	require.NoError(t, runErr, "runHarness stdout: %s", stdout)
	for _, line := range strings.Split(stdout, "\n") {
		payload, ok := strings.CutPrefix(line, "STIRRUP_RESULT ")
		if !ok {
			continue
		}
		var result types.RunResult
		require.NoError(t, json.Unmarshal([]byte(payload), &result), "STIRRUP_RESULT does not parse: %s", payload)
		return result, receiver
	}
	require.FailNow(t, "no STIRRUP_RESULT line in stdout", "stdout: %q", stdout)
	return types.RunResult{}, nil
}

func TestRunHarness_TraceParentParentsExportedRunSpan(t *testing.T) {
	result, receiver := runHarnessWithTraceParent(t, [][2]string{
		{"traceparent", cliTraceparent},
		{"tracestate", "vendor=value"},
	})

	require.NotNil(t, result.TraceReference, "RunResult.traceReference")
	require.Equal(t, cliTraceID, result.TraceReference.TraceID, "traceReference.traceID")
	require.True(t, result.TraceReference.Sampled, "traceReference.sampled under a sampled parent")

	root, seen := receiver.span("invoke_agent stirrup")
	require.NotNil(t, root, "no exported invoke_agent stirrup span; received %s", strings.Join(seen, ", "))
	require.Equal(t, cliTraceID, hex.EncodeToString(root.GetTraceId()), "run span trace ID")
	require.Equal(t, cliSpanID, hex.EncodeToString(root.GetParentSpanId()), "run span parent, the flag's span")
	require.Equal(t, hex.EncodeToString(root.GetSpanId()), result.TraceReference.RunSpanID, "traceReference.runSpanId")
}

func TestRunHarness_InvalidTraceParentStartsNewRoot(t *testing.T) {
	result, receiver := runHarnessWithTraceParent(t, [][2]string{
		{"traceparent", "ff-" + cliTraceID + "-" + cliSpanID + "-01"},
	})

	root, seen := receiver.span("invoke_agent stirrup")
	require.NotNil(t, root, "no exported invoke_agent stirrup span; received %s", strings.Join(seen, ", "))
	require.Empty(t, root.GetParentSpanId(), "run span should be a new root")
	require.NotEqual(t, cliTraceID, hex.EncodeToString(root.GetTraceId()), "run span joined the invalid traceparent's trace")
	require.NotNil(t, result.TraceReference, "RunResult.traceReference")
	require.NotEmpty(t, result.TraceReference.TraceID, "freshly minted trace ID")
	require.NotEqual(t, cliTraceID, result.TraceReference.TraceID, "freshly minted trace ID")
}

func TestRunHarness_UnsampledTraceParentExportsNothing(t *testing.T) {
	result, receiver := runHarnessWithTraceParent(t, [][2]string{
		{"traceparent", "00-" + cliTraceID + "-" + cliSpanID + "-00"},
	})

	require.NotNil(t, result.TraceReference, "RunResult.traceReference")
	require.Equal(t, cliTraceID, result.TraceReference.TraceID, "traceReference.traceID")
	require.False(t, result.TraceReference.Sampled, "traceReference.sampled under an unsampled parent")
	if n := receiver.count(); n != 0 {
		_, names := receiver.span("")
		require.Equal(t, 0, n, "OTLP receiver spans under an unsampled parent: %v", names)
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
			logs := captureSlog(t)
			warnIgnoredTraceParent(tc.cfg, tc.traceParent, tc.traceState)
			got := logs.String()
			if tc.wantWarning == "" {
				require.Empty(t, got, "unexpected log output")
			} else {
				require.Contains(t, got, tc.wantWarning, "log output")
			}
		})
	}
}

func TestRunHarness_InvalidTraceParentLogsWarning(t *testing.T) {
	logs := captureSlog(t)
	runHarnessWithTraceParent(t, [][2]string{{"traceparent", "not-a-traceparent"}})
	require.Contains(t, logs.String(), "--traceparent ignored", "log output should carry the ignored-traceparent warning")
}

func TestRunHarness_OutputRunConfigIgnoresTraceParentSilently(t *testing.T) {
	logs := captureSlog(t)
	cmd := newTestHarnessCommand()
	setFlags(t, cmd.Flags(), [][2]string{
		{"prompt", "p"},
		{"traceparent", "not-a-traceparent"},
		{"output-runconfig", t.TempDir() + "/rc.json"},
	})
	require.NoError(t, runHarness(cmd, nil))
	require.NotContains(t, logs.String(), "traceparent", "--output-runconfig should not log a trace parent warning")
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
		sr := srv.waitDone(t, what)
		require.Equal(t, "success", sr, "%s stop_reason", what)
	}
	select {
	case err := <-errCh:
		_ = stdoutDone()
		require.NoError(t, err, "runHarness")
	case <-time.After(15 * time.Second):
		require.FailNow(t, "runHarness did not return after cancel")
	}

	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	var roots []*otlptrace.Span
	for _, sp := range receiver.spans {
		if sp.GetName() == "invoke_agent stirrup" {
			roots = append(roots, sp)
		}
	}
	require.Len(t, roots, 2, "run spans exported for the primary and follow-up runs")
	byTrace := map[string]*otlptrace.Span{}
	for _, sp := range roots {
		byTrace[hex.EncodeToString(sp.GetTraceId())] = sp
	}
	require.Len(t, byTrace, 2, "primary and follow-up share a trace; trace IDs = %v", byTrace)
	primary := byTrace[cliTraceID]
	require.NotNil(t, primary, "no run span on the flag's trace %s", cliTraceID)
	require.Equal(t, cliSpanID, hex.EncodeToString(primary.GetParentSpanId()), "primary run span parent, the flag's span")
	for id, sp := range byTrace {
		if id != cliTraceID {
			require.Empty(t, sp.GetParentSpanId(), "follow-up run span parent for trace %s, want a new root", id)
		}
	}
}
