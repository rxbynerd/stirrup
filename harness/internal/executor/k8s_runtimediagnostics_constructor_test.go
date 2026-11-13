package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes/fake"
	utilexec "k8s.io/client-go/util/exec"
	"k8s.io/utils/ptr"

	"github.com/rxbynerd/stirrup/harness/internal/security"
)

// guestExec serves every pods/exec call with stdout and err, counting calls.
func guestExec(stdout string, err error, calls *int) podExecFunc {
	return func(_ context.Context, _ []string, _ io.Reader, out, _ io.Writer) error {
		*calls++
		if _, writeErr := io.WriteString(out, stdout); writeErr != nil {
			return writeErr
		}
		return err
	}
}

func TestNewK8sExecutor_NitroDiagnosticsPassReturnsExecutor(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset()
	admitPods(cs, nil)
	cfg, recorder := nitroConstructorCfg()
	calls := 0

	exec, err := newFakeK8sExecutorWithExec(context.Background(), cs, cfg, guestExec(isolatedGuestDump, nil, &calls))

	must.NoError(err)
	must.Equal(1, calls, "the diagnostics read the guest in one exec")
	must.Equal([]runtimeDiagnosticEvent{{
		runtime:  "nitro-isolated",
		passed:   true,
		observed: map[string]string{"dmiProductName": "m5d.enclave", "interfaces": "lo,dummy0", "routeEntries": "0"},
	}}, recorder.runtimeDiagnostics)
	must.Empty(podDeletes(cs), "a passing run keeps its Pod")
	must.NoError(exec.Close())
}

func TestNewK8sExecutor_NitroDiagnosticsFailureTearsDown(t *testing.T) {
	must := require.New(t)
	ordinaryPod, err := os.ReadFile(filepath.Join("testdata", "runtime-diagnostics-ordinary-pod.txt"))
	must.NoError(err)

	cases := []struct {
		name      string
		stdout    string
		execErr   error
		wantCheck string
	}{
		{name: "dmi mismatch", stdout: string(ordinaryPod), wantCheck: "dmi_product_name"},
		{name: "unexpected interface", stdout: guestDump(
			ptr.To("m5d.enclave\n"), ptr.To(procNetDevHeader+procNetDevLo+procNetDevDummy0+procNetDevEth0), ptr.To(procNetRouteHeader),
		), wantCheck: "network_interfaces"},
		{name: "route entry", stdout: guestDump(
			ptr.To("m5d.enclave\n"), ptr.To(procNetDevHeader+procNetDevLo+procNetDevDummy0), ptr.To(procNetRouteHeader+procNetRouteDflt),
		), wantCheck: "route_table"},
		{name: "exec error", execErr: errors.New("upgrade request failed"), wantCheck: "diagnostic_exec"},
		{name: "non-zero exit", execErr: utilexec.CodeExitError{Err: errors.New("exit 2"), Code: 2}, wantCheck: "diagnostic_exec"},
		{name: "malformed output", stdout: "garbage\n", wantCheck: "diagnostic_output"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			cs := fake.NewClientset()
			admitPods(cs, nil)
			lingering := lingerOnDelete(t, cs, 3)
			cfg, recorder := nitroConstructorCfg()
			calls := 0

			exec, err := newFakeK8sExecutorWithExec(context.Background(), cs, cfg, guestExec(tc.stdout, tc.execErr, &calls))

			must.Nil(exec)
			must.ErrorContains(err, `runtime diagnostics for executor.runtime "nitro-isolated" failed: `+tc.wantCheck+": ")
			must.Equal(1, calls)
			requireTornDown(t, cs)
			requireRemovedAfterRelease(t, lingering)
			must.Len(recorder.runtimeDiagnostics, 1)
			must.False(recorder.runtimeDiagnostics[0].passed)
			must.Equal(tc.wantCheck, recorder.runtimeDiagnostics[0].check)
		})
	}
}

// blockingGuestExec blocks every pods/exec call until its ctx ends, after
// calling onStart, and returns the ctx error.
func blockingGuestExec(onStart func()) podExecFunc {
	return func(ctx context.Context, _ []string, _ io.Reader, _, _ io.Writer) error {
		if onStart != nil {
			onStart()
		}
		<-ctx.Done()
		return ctx.Err()
	}
}

func TestNewK8sExecutor_InterruptedDiagnosticsReportNoFailure(t *testing.T) {
	cases := []struct {
		name    string
		ctx     func() (context.Context, context.CancelFunc)
		cancel  bool
		wantErr error
	}{
		{name: "caller cancels", ctx: func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }, cancel: true, wantErr: context.Canceled},
		{name: "caller deadline passes", ctx: func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 500*time.Millisecond)
		}, wantErr: context.DeadlineExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			cs := fake.NewClientset()
			admitPods(cs, nil)
			cfg, recorder := nitroConstructorCfg()
			ctx, cancel := tc.ctx()
			defer cancel()
			var onStart func()
			if tc.cancel {
				onStart = cancel
			}

			exec, err := newFakeK8sExecutorWithExec(ctx, cs, cfg, blockingGuestExec(onStart))

			must.Nil(exec)
			must.ErrorIs(err, tc.wantErr)
			must.ErrorContains(err, `runtime diagnostics for executor.runtime "nitro-isolated" interrupted`)
			must.Empty(recorder.runtimeDiagnostics, "an interrupted run is not a runtime diagnostic failure")
			requireTornDown(t, cs)
		})
	}
}

func TestNewK8sExecutor_ContentFailureAsTheCallerCancelsIsAFailure(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset()
	admitPods(cs, nil)
	cfg, recorder := nitroConstructorCfg()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dump := guestDump(ptr.To("m5d.xlarge\n"), ptr.To(procNetDevHeader+procNetDevLo+procNetDevEth0), ptr.To(procNetRouteHeader+procNetRouteDflt))
	execFn := func(_ context.Context, _ []string, _ io.Reader, out, _ io.Writer) error {
		cancel()
		_, err := io.WriteString(out, dump)
		return err
	}

	exec, err := newFakeK8sExecutorWithExec(ctx, cs, cfg, execFn)

	must.Nil(exec)
	must.ErrorContains(err, `runtime diagnostics for executor.runtime "nitro-isolated" failed: dmi_product_name: `)
	must.NotErrorIs(err, context.Canceled)
	must.Len(recorder.runtimeDiagnostics, 1)
	must.Equal("dmi_product_name", recorder.runtimeDiagnostics[0].check)
	requireTornDown(t, cs)
}

func TestNewK8sExecutor_DiagnosticsTimeoutWithLiveCallerIsAFailure(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset()
	admitPods(cs, nil)
	cfg, recorder := nitroConstructorCfg()
	profile, proxyEnv, err := prepareK8sExecutor(cfg)
	must.NoError(err)
	timing := fastK8sTiming
	timing.fileIOTimeout = 50 * time.Millisecond
	logger, _ := testLogger()

	exec, err := newK8sExecutor(context.Background(), cfg, profile, proxyEnv, cs, nil, blockingGuestExec(nil), timing, logger)

	must.Nil(exec)
	must.ErrorContains(err, `runtime diagnostics for executor.runtime "nitro-isolated" failed: diagnostic_exec: `)
	must.ErrorIs(err, context.DeadlineExceeded)
	must.Len(recorder.runtimeDiagnostics, 1)
	must.Equal("diagnostic_exec", recorder.runtimeDiagnostics[0].check)
	requireTornDown(t, cs)
}

func TestNewK8sExecutor_RuntimeWithoutDiagnosticsRunsNoExec(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset()
	admitPods(cs, nil)
	cfg := constructorCfg("gvisor")
	recorder := &recordingSecurityEmitter{}
	cfg.Security = recorder
	calls := 0

	exec, err := newFakeK8sExecutorWithExec(context.Background(), cs, cfg, guestExec("", nil, &calls))

	must.NoError(err)
	must.Zero(calls)
	must.Empty(recorder.runtimeDiagnostics)
	must.NoError(exec.Close())
}

// A guest-reported value reaches the security event stream scrubbed.
func TestNewK8sExecutor_NitroDiagnosticsRedactSecretShapedGuestValues(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset()
	admitPods(cs, nil)
	var events bytes.Buffer
	cfg := nitroCfg()
	cfg.Security = security.NewSecurityLogger(&events, testK8sRunID)
	calls := 0
	dump := guestDump(ptr.To("sk-ant-abc123_DEF-456\n"), ptr.To(procNetDevHeader+procNetDevLo), ptr.To(procNetRouteHeader))

	_, err := newFakeK8sExecutorWithExec(context.Background(), cs, cfg, guestExec(dump, nil, &calls))

	must.ErrorContains(err, "dmi_product_name")
	must.NotContains(err.Error(), "sk-ant-", "the returned error carries the unredacted guest value")
	must.NotContains(events.String(), "sk-ant-abc123_DEF-456")
	var event security.SecurityEvent
	must.NoError(json.Unmarshal(bytes.TrimSpace(events.Bytes()), &event))
	must.Equal("sandbox_runtime_diagnostic_failed", event.Event)
	must.Equal("error", event.Level)
	observed, ok := event.Data["observed"].(map[string]any)
	must.True(ok, "observed is a JSON object: %#v", event.Data["observed"])
	must.Contains(observed["dmiProductName"], "[REDACTED")
}
