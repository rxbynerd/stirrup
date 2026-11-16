package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes/fake"
	utilexec "k8s.io/client-go/util/exec"
	"k8s.io/utils/ptr"
)

const (
	gvisorProcVersion = "Linux version 4.19.0-gvisor #1 SMP Sun Jan 10 15:06:54 PST 2016"
	hostProcVersion   = "Linux version 7.1.3-200.fc44.aarch64 (mockbuild@b5898cfcc2f345a484e8f3c69c910796) (gcc (GCC) 16.1.1 20260515 (Red Hat 16.1.1-2), GNU ld version 2.46-3.fc44) #1 SMP PREEMPT_DYNAMIC Sat Jul  4 19:23:52 UTC 2026"
	sentryMeminfo     = "Alloc:              4202 kB\nTotalAlloc:        11985 kB\n"
)

func readDiagnosticsFixture(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("testdata", name))
	require.New(t).NoError(err)
	return string(content)
}

func TestRunK8sRuntimeDiagnostics_GVisorGuestPasses(t *testing.T) {
	must := require.New(t)
	core, security, calls := fakeDiagnosticsCore(t, readDiagnosticsFixture(t, "runtime-diagnostics-gvisor-pod.txt"), nil)

	start := time.Now()
	err := runK8sRuntimeDiagnostics(context.Background(), "gvisor", k8sRuntimeProfiles["gvisor"], core)
	must.NoError(err)

	must.Len(*calls, 1, "every diagnostic file is read in one exec")
	call := (*calls)[0]
	must.Equal([]string{"/bin/sh", "-c", guestFileDumpScript, "stirrup-diag", procVersionPath, gvisorSentryMeminfoPath}, call.command)
	must.False(call.stdinSet)
	must.WithinDuration(start.Add(k8sFileIOTimeout), call.deadline, 5*time.Second, "the exec is bounded by k8sFileIOTimeout")

	must.Equal([]runtimeDiagnosticEvent{{
		runtime:  "gvisor",
		passed:   true,
		observed: map[string]string{"kernelVersion": gvisorProcVersion, "sentryMeminfo": "present"},
	}}, security.runtimeDiagnostics)
}

// The release string differs between gVisor versions and configurations;
// only the build stamp after it is fixed.
func TestRunK8sRuntimeDiagnostics_GVisorAcceptsEveryRelease(t *testing.T) {
	for _, version := range []string{
		"Linux version 4.4.0 #1 SMP Sun Jan 10 15:06:54 PST 2016",
		gvisorProcVersion,
		"Linux version 5.15.0-gvisor #1 SMP Sun Jan 10 15:06:54 PST 2016",
	} {
		t.Run(version, func(t *testing.T) {
			must := require.New(t)
			core, security, _ := fakeDiagnosticsCore(t, gvisorDump(ptr.To(version+"\n"), ptr.To("")), nil)
			must.NoError(runK8sRuntimeDiagnostics(context.Background(), "gvisor", k8sRuntimeProfiles["gvisor"], core))
			must.Len(security.runtimeDiagnostics, 1)
			must.Equal(map[string]string{"kernelVersion": version, "sentryMeminfo": "present"}, security.runtimeDiagnostics[0].observed)
		})
	}
}

func TestRunK8sRuntimeDiagnostics_GVisorFailures(t *testing.T) {
	cases := []struct {
		name         string
		stdout       string
		execErr      error
		wantCheck    string
		wantReason   string
		wantObserved map[string]string
	}{
		{
			name:         "gvisor class whose handler is runc",
			stdout:       readDiagnosticsFixture(t, "runtime-diagnostics-gvisor-class-on-runc.txt"),
			wantCheck:    "kernel_version",
			wantReason:   `does not end in gVisor's build stamp "#1 SMP Sun Jan 10 15:06:54 PST 2016", so the Pod is not running under gVisor`,
			wantObserved: map[string]string{"kernelVersion": hostProcVersion, "sentryMeminfo": "absent"},
		},
		{
			name:         "gvisor build stamp without the sentry file",
			stdout:       gvisorDump(ptr.To(gvisorProcVersion+"\n"), nil),
			wantCheck:    "sentry_meminfo",
			wantReason:   "/proc/sentry-meminfo is absent, unreadable or not a regular file, so the Pod is not running under gVisor",
			wantObserved: map[string]string{"kernelVersion": gvisorProcVersion, "sentryMeminfo": "absent"},
		},
		{
			name:         "sentry file with a host kernel version",
			stdout:       gvisorDump(ptr.To(hostProcVersion+"\n"), ptr.To(sentryMeminfo)),
			wantCheck:    "kernel_version",
			wantReason:   "does not end in gVisor's build stamp",
			wantObserved: map[string]string{"kernelVersion": hostProcVersion, "sentryMeminfo": "present"},
		},
		{
			name:         "build stamp without the Linux version prefix",
			stdout:       gvisorDump(ptr.To("4.19.0-gvisor #1 SMP Sun Jan 10 15:06:54 PST 2016\n"), ptr.To(sentryMeminfo)),
			wantCheck:    "kernel_version",
			wantReason:   `kernel version "4.19.0-gvisor #1 SMP Sun Jan 10 15:06:54 PST 2016" does not end in`,
			wantObserved: map[string]string{"kernelVersion": "4.19.0-gvisor #1 SMP Sun Jan 10 15:06:54 PST 2016", "sentryMeminfo": "present"},
		},
		{
			name:         "build stamp followed by more text",
			stdout:       gvisorDump(ptr.To(gvisorProcVersion+" extra\n"), ptr.To(sentryMeminfo)),
			wantCheck:    "kernel_version",
			wantReason:   "does not end in gVisor's build stamp",
			wantObserved: map[string]string{"kernelVersion": gvisorProcVersion + " extra", "sentryMeminfo": "present"},
		},
		{
			name:         "missing kernel version",
			stdout:       gvisorDump(nil, ptr.To(sentryMeminfo)),
			wantCheck:    "kernel_version",
			wantReason:   "/proc/version is absent, unreadable or not a regular file",
			wantObserved: map[string]string{"sentryMeminfo": "present"},
		},
		{
			name:         "empty kernel version",
			stdout:       gvisorDump(ptr.To(""), ptr.To(sentryMeminfo)),
			wantCheck:    "kernel_version",
			wantReason:   `kernel version "" does not end in`,
			wantObserved: map[string]string{"kernelVersion": "", "sentryMeminfo": "present"},
		},
		{
			name:         "multi-line kernel version",
			stdout:       gvisorDump(ptr.To(gvisorProcVersion+"\n"+gvisorProcVersion+"\n"), ptr.To(sentryMeminfo)),
			wantCheck:    "diagnostic_output",
			wantReason:   "/proc/version has 2 non-empty lines; the kernel prints one",
			wantObserved: map[string]string{"sentryMeminfo": "present"},
		},
		{
			name:       "malformed framing",
			stdout:     gvisorProcVersion + "\n",
			wantCheck:  "diagnostic_output",
			wantReason: "where the report for /proc/version should start",
		},
		{
			name:       "nitro paths answered instead",
			stdout:     isolatedGuestDump,
			wantCheck:  "diagnostic_output",
			wantReason: "where the report for /proc/version should start",
		},
		{
			name:       "truncated report",
			stdout:     strings.TrimSuffix(gvisorGuestDump, "end /proc/sentry-meminfo\n"),
			wantCheck:  "diagnostic_output",
			wantReason: "the report for /proc/sentry-meminfo is not terminated",
		},
		{
			name:       "oversized output",
			stdout:     gvisorGuestDump + strings.Repeat("x", guestFileDumpOutputLimit),
			wantCheck:  "diagnostic_output",
			wantReason: fmt.Sprintf("more than %d bytes", guestFileDumpOutputLimit),
		},
		{
			name:       "no output",
			wantCheck:  "diagnostic_output",
			wantReason: "guest file read produced no output",
		},
		{
			name:       "exec transport error",
			execErr:    errors.New("error dialing backend: connection refused"),
			wantCheck:  "diagnostic_exec",
			wantReason: "guest file read failed: error dialing backend: connection refused",
		},
		{
			name:       "non-zero exit",
			execErr:    utilexec.CodeExitError{Err: errors.New("command terminated with exit code 127"), Code: 127},
			wantCheck:  "diagnostic_exec",
			wantReason: "guest file read exited 127",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			core, security, _ := fakeDiagnosticsCore(t, tc.stdout, tc.execErr)

			err := runK8sRuntimeDiagnostics(context.Background(), "gvisor", k8sRuntimeProfiles["gvisor"], core)
			must.ErrorContains(err, `runtime diagnostics for executor.runtime "gvisor" failed: `+tc.wantCheck+": ")
			must.ErrorContains(err, tc.wantReason)
			if tc.execErr != nil {
				must.ErrorIs(err, tc.execErr)
			}

			must.Len(security.runtimeDiagnostics, 1)
			event := security.runtimeDiagnostics[0]
			check.Equal("gvisor", event.runtime)
			check.False(event.passed)
			check.Equal(tc.wantCheck, event.check)
			check.Contains(event.reason, tc.wantReason)
			if tc.wantObserved == nil {
				check.Empty(event.observed)
			} else {
				check.Equal(tc.wantObserved, event.observed)
			}
		})
	}
}

func TestRunK8sRuntimeDiagnostics_GVisorBoundsTheKernelVersion(t *testing.T) {
	must := require.New(t)
	long := "Linux version " + strings.Repeat("x", 4*k8sClusterTextMax) + "\x1b[31m" + gvisorKernelBuildStamp
	core, security, _ := fakeDiagnosticsCore(t, gvisorDump(ptr.To(long+"\n"), ptr.To(sentryMeminfo)), nil)

	must.NoError(runK8sRuntimeDiagnostics(context.Background(), "gvisor", k8sRuntimeProfiles["gvisor"], core))
	must.Len(security.runtimeDiagnostics, 1)
	observed := security.runtimeDiagnostics[0].observed["kernelVersion"]
	must.LessOrEqual(len([]rune(observed)), k8sClusterTextMax+1, "the observed kernel version is cut to the cluster text bound")
	requireNoControlCharacters(t, observed)
}

func TestNewK8sExecutor_GVisorDiagnosticsPassReturnsExecutor(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset()
	admitPods(cs, nil)
	cfg := constructorCfg("gvisor")
	recorder := &recordingSecurityEmitter{}
	cfg.Security = recorder
	calls := 0

	exec, err := newFakeK8sExecutorWithExec(context.Background(), cs, cfg, guestExec(readDiagnosticsFixture(t, "runtime-diagnostics-gvisor-pod.txt"), nil, &calls))

	must.NoError(err)
	must.Equal(1, calls, "the diagnostics read the guest in one exec")
	must.Equal([]runtimeDiagnosticEvent{{
		runtime:  "gvisor",
		passed:   true,
		observed: map[string]string{"kernelVersion": gvisorProcVersion, "sentryMeminfo": "present"},
	}}, recorder.runtimeDiagnostics)
	must.Empty(podDeletes(cs), "a passing run keeps its Pod")
	must.NoError(exec.Close())
}

func TestNewK8sExecutor_GVisorDiagnosticsFailureTearsDown(t *testing.T) {
	cases := []struct {
		name      string
		stdout    string
		execErr   error
		wantCheck string
	}{
		{name: "gvisor class whose handler is runc", stdout: readDiagnosticsFixture(t, "runtime-diagnostics-gvisor-class-on-runc.txt"), wantCheck: "kernel_version"},
		{name: "no sentry file", stdout: gvisorDump(ptr.To(gvisorProcVersion+"\n"), nil), wantCheck: "sentry_meminfo"},
		{name: "exec error", execErr: errors.New("upgrade request failed"), wantCheck: "diagnostic_exec"},
		{name: "malformed output", stdout: "garbage\n", wantCheck: "diagnostic_output"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			cs := fake.NewClientset()
			admitPods(cs, nil)
			lingering := lingerOnDelete(t, cs, 3)
			cfg := constructorCfg("gvisor")
			recorder := &recordingSecurityEmitter{}
			cfg.Security = recorder
			calls := 0

			exec, err := newFakeK8sExecutorWithExec(context.Background(), cs, cfg, guestExec(tc.stdout, tc.execErr, &calls))

			must.Nil(exec)
			must.ErrorContains(err, `runtime diagnostics for executor.runtime "gvisor" failed: `+tc.wantCheck+": ")
			must.Equal(1, calls)
			requireTornDown(t, cs)
			requireRemovedAfterRelease(t, lingering)
			must.Len(recorder.runtimeDiagnostics, 1)
			must.False(recorder.runtimeDiagnostics[0].passed)
			must.Equal(tc.wantCheck, recorder.runtimeDiagnostics[0].check)
		})
	}
}

func TestRunK8sRuntimeDiagnostics_GVisorSkipsBlankLinesAroundTheVersion(t *testing.T) {
	must := require.New(t)
	core, security, _ := fakeDiagnosticsCore(t, gvisorDump(ptr.To("\n"+gvisorProcVersion+"\n\n"), ptr.To(sentryMeminfo)), nil)

	must.NoError(runK8sRuntimeDiagnostics(context.Background(), "gvisor", k8sRuntimeProfiles["gvisor"], core))
	must.Len(security.runtimeDiagnostics, 1)
	must.Equal(map[string]string{"kernelVersion": gvisorProcVersion, "sentryMeminfo": "present"}, security.runtimeDiagnostics[0].observed)
}
