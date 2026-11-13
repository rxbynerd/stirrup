package executor

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes/fake"
	utilexec "k8s.io/client-go/util/exec"
)

// errKubeletUnreachable is the pods/exec error an API server returns while a
// new node's kubelet has no serving certificate.
var errKubeletUnreachable = errors.New("unable to upgrade connection: error dialing backend: remote error: tls: internal error")

// execReachableCore returns a core whose pods/exec calls are served by execFn.
func execReachableCore(execFn podExecFunc) *podExecCore {
	return &podExecCore{namespace: testK8sNamespace, podName: testK8sPodName, execFn: execFn}
}

// scriptedExec serves call n with outcomes[n-1], and the last outcome for
// every later call.
func scriptedExec(calls *int, outcomes ...error) podExecFunc {
	return func(context.Context, []string, io.Reader, io.Writer, io.Writer) error {
		*calls++
		return outcomes[min(*calls, len(outcomes))-1]
	}
}

func TestWaitForExecReachable_RetriesWhileTheKubeletIsUnreachable(t *testing.T) {
	must := require.New(t)
	logger, logs := testLogger()
	calls := 0
	core := execReachableCore(scriptedExec(&calls, errKubeletUnreachable, errKubeletUnreachable, nil))

	err := waitForExecReachable(context.Background(), core, fastK8sTiming, logger)

	must.NoError(err)
	must.Equal(3, calls)
	must.Equal(1, strings.Count(logs.String(), "k8s executor waiting for exec to reach the sandbox pod"), "the wait is logged once")
}

func TestWaitForExecReachable_RunsTheProbeCommandWithoutStdin(t *testing.T) {
	must := require.New(t)
	logger, _ := testLogger()
	var command []string
	var stdin io.Reader = errReader{}
	core := execReachableCore(func(_ context.Context, c []string, in io.Reader, _, _ io.Writer) error {
		command, stdin = c, in
		return nil
	})

	must.NoError(waitForExecReachable(context.Background(), core, fastK8sTiming, logger))

	must.Equal([]string{"/bin/sh", "-c", ":"}, command)
	must.Nil(stdin)
}

func TestWaitForExecReachable_LeavesOtherFailuresToTheFirstExec(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "the probe exits non-zero", err: utilexec.CodeExitError{Err: errors.New("exit 127"), Code: 127}},
		{name: "exec is forbidden", err: errors.New(`unable to upgrade connection: Forbidden (user=system:serviceaccount:ns:sa, verb=create, resource=pods, subresource=exec)`)},
		{name: "the container is gone", err: errors.New("container not found (\"agent\")")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			logger, _ := testLogger()
			calls := 0
			core := execReachableCore(scriptedExec(&calls, tc.err))

			err := waitForExecReachable(context.Background(), core, slowExecReachableTiming, logger)

			must.NoError(err)
			must.Equal(1, calls, "a failure that waiting cannot cure is not retried")
		})
	}
}

func TestWaitForExecReachable_FailsWhenTheKubeletStaysUnreachable(t *testing.T) {
	must := require.New(t)
	logger, _ := testLogger()
	calls := 0
	core := execReachableCore(scriptedExec(&calls, errKubeletUnreachable))
	timing := k8sTiming{pollPeriod: time.Millisecond, execReachableTimeout: 50 * time.Millisecond}

	err := waitForExecReachable(context.Background(), core, timing, logger)

	must.ErrorIs(err, errExecUnreachable)
	must.Equal("is not reachable for exec within 50ms: "+errKubeletUnreachable.Error(), err.Error())
	must.Greater(calls, 1)
}

func TestWaitForExecReachable_SanitisesClusterTextInTheErrorAndTheLog(t *testing.T) {
	cases := []struct {
		name string
		// retried selects the retried dial failure; otherwise the error
		// is one the wait leaves to the next exec.
		retried bool
	}{
		{name: "retried failure", retried: true},
		{name: "failure left to the next exec"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			logger, logs := testLogger()
			text := "container \x1b[31mgone\u202e\u009b"
			if tc.retried {
				text = "error dialing backend: \x1b[31mtls\u202e\u009b: internal error"
			}
			calls := 0
			core := execReachableCore(scriptedExec(&calls, errors.New(text)))
			timing := k8sTiming{pollPeriod: time.Millisecond, execReachableTimeout: 20 * time.Millisecond}

			err := waitForExecReachable(context.Background(), core, timing, logger)

			if tc.retried {
				must.Equal("is not reachable for exec within 20ms: error dialing backend: [31mtls: internal error", err.Error())
			} else {
				must.NoError(err)
			}
			for _, r := range logs.String() {
				must.False(isUnprintableRune(r) && r != '\n', "log line carries unprintable rune %U", r)
			}
			must.Contains(logs.String(), "[31m", "the failure text is logged")
		})
	}
}

func TestWaitForExecReachable_HungAttemptFailsAtTheBound(t *testing.T) {
	must := require.New(t)
	logger, _ := testLogger()
	calls := 0
	core := execReachableCore(func(ctx context.Context, _ []string, _ io.Reader, _, _ io.Writer) error {
		calls++
		<-ctx.Done()
		return ctx.Err()
	})
	// The bound ends the first attempt before the per-attempt timeout does.
	timing := k8sTiming{pollPeriod: time.Millisecond, execReachableTimeout: 50 * time.Millisecond}

	err := waitForExecReachable(context.Background(), core, timing, logger)

	must.ErrorIs(err, errExecUnreachable)
	must.Equal(1, calls)
}

func TestWaitForExecReachable_RetriesAnAttemptThatTimesOut(t *testing.T) {
	must := require.New(t)
	logger, logs := testLogger()
	calls := 0
	core := execReachableCore(func(ctx context.Context, _ []string, _ io.Reader, _, _ io.Writer) error {
		calls++
		if calls == 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	})
	timing := k8sTiming{pollPeriod: time.Millisecond, execReachableTimeout: 30 * time.Second, execProbeTimeout: 20 * time.Millisecond}
	started := time.Now()

	err := waitForExecReachable(context.Background(), core, timing, logger)

	must.NoError(err)
	must.Equal(2, calls)
	must.Less(time.Since(started), 5*time.Second, "the attempt ends at its own timeout, not the default")
	must.Contains(logs.String(), "k8s executor exec reached the sandbox pod")
}

func TestWaitForExecReachable_ReportsTheDialFailureWhenTheBoundCutsTheLastAttempt(t *testing.T) {
	must := require.New(t)
	logger, _ := testLogger()
	calls := 0
	core := execReachableCore(func(ctx context.Context, _ []string, _ io.Reader, _, _ io.Writer) error {
		calls++
		if calls == 1 {
			return errKubeletUnreachable
		}
		<-ctx.Done()
		return ctx.Err()
	})
	timing := k8sTiming{pollPeriod: time.Millisecond, execReachableTimeout: 50 * time.Millisecond}

	err := waitForExecReachable(context.Background(), core, timing, logger)

	must.ErrorIs(err, errExecUnreachable)
	must.ErrorContains(err, errKubeletUnreachable.Error(), "the bound cutting an attempt short does not replace the cause")
	must.Equal(2, calls)
}

func TestWaitForExecReachable_CallerCancellationIsAnInterruption(t *testing.T) {
	must := require.New(t)
	logger, _ := testLogger()
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	core := execReachableCore(func(context.Context, []string, io.Reader, io.Writer, io.Writer) error {
		calls++
		cancel()
		return errKubeletUnreachable
	})

	err := waitForExecReachable(ctx, core, slowExecReachableTiming, logger)

	must.ErrorIs(err, context.Canceled)
	must.NotErrorIs(err, errExecUnreachable)
	must.Equal(1, calls)
}

func TestNewK8sExecutor_WaitsForExecBeforeReturning(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset()
	admitPods(cs, nil)
	calls := 0

	exec, err := newFakeK8sExecutorWithExec(context.Background(), cs, constructorCfg(""), scriptedExec(&calls, errKubeletUnreachable, nil))

	must.NoError(err)
	must.Equal(2, calls)
	must.Empty(podDeletes(cs))
	must.NoError(exec.Close())
}

func TestNewK8sExecutor_UnreachableExecTearsDown(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset()
	admitPods(cs, nil)
	calls := 0

	exec, err := newFakeK8sExecutorWithExec(context.Background(), cs, constructorCfg(""), scriptedExec(&calls, errKubeletUnreachable))

	must.Nil(exec)
	must.ErrorIs(err, errExecUnreachable)
	must.ErrorContains(err, "pod "+testK8sNamespace+"/")
	must.Len(podDeletes(cs), 1, "the Pod is deleted when exec never becomes reachable")
}

func TestNewK8sExecutor_NitroDiagnosticsStillRunAfterAProbeFailure(t *testing.T) {
	must := require.New(t)
	ordinaryPod, err := os.ReadFile(filepath.Join("testdata", "runtime-diagnostics-ordinary-pod.txt"))
	must.NoError(err)
	cs := fake.NewClientset()
	admitPods(cs, nil)
	cfg, recorder := nitroConstructorCfg()
	execFn := func(_ context.Context, command []string, _ io.Reader, out, _ io.Writer) error {
		if isExecProbe(command) {
			return errors.New("unable to upgrade connection: Forbidden")
		}
		_, writeErr := out.Write(ordinaryPod)
		return writeErr
	}

	exec, err := newFakeK8sExecutorWithExec(context.Background(), cs, cfg, execFn)

	must.Nil(exec)
	must.ErrorContains(err, "runtime diagnostics for executor.runtime \"nitro-isolated\" failed", "a probe failure the wait does not retry leaves the diagnostics in force")
	must.Len(recorder.runtimeDiagnostics, 1)
	must.False(recorder.runtimeDiagnostics[0].passed)
	must.Len(podDeletes(cs), 1)
}

func TestNewK8sExecutor_NitroDiagnosticsRunAfterExecIsReachable(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset()
	admitPods(cs, nil)
	cfg, recorder := nitroConstructorCfg()
	var commands [][]string
	execFn := func(_ context.Context, command []string, _ io.Reader, out, _ io.Writer) error {
		commands = append(commands, command)
		switch len(commands) {
		case 1:
			return errKubeletUnreachable
		case 2:
			return nil
		default:
			_, err := io.WriteString(out, isolatedGuestDump)
			return err
		}
	}

	exec, err := newFakeK8sExecutorWithExec(context.Background(), cs, cfg, execFn)

	must.NoError(err)
	must.Len(commands, 3)
	must.True(isExecProbe(commands[0]))
	must.True(isExecProbe(commands[1]))
	must.False(isExecProbe(commands[2]), "the diagnostics follow the probe")
	must.Len(recorder.runtimeDiagnostics, 1)
	must.True(recorder.runtimeDiagnostics[0].passed)
	must.NoError(exec.Close())
}

// slowExecReachableTiming has a bound long enough that a test returning
// quickly proves the wait ended without retrying.
var slowExecReachableTiming = k8sTiming{pollPeriod: time.Millisecond, execReachableTimeout: 30 * time.Second}

// errReader is a non-nil reader used to prove a value was overwritten.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("unexpected read") }
