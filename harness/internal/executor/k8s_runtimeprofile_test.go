package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/rxbynerd/stirrup/types"
)

// TestK8sRuntimeProfiles_MatchTypesRuntimeSet pins the profile registry to
// the k8s runtime values ValidateRunConfig accepts, in both directions, so
// an accepted value always has a profile and no profile is unreachable.
func TestK8sRuntimeProfiles_MatchTypesRuntimeSet(t *testing.T) {
	accepted := append([]string{""}, types.ValidK8sExecutorRuntimeValues()...)
	for _, runtime := range accepted {
		t.Run(fmt.Sprintf("accepted %q has a profile", runtime), func(t *testing.T) {
			must := require.New(t)
			must.Contains(k8sRuntimeProfiles, runtime)
		})
	}
	for runtime := range k8sRuntimeProfiles {
		t.Run(fmt.Sprintf("profile %q is accepted", runtime), func(t *testing.T) {
			must := require.New(t)
			must.Contains(accepted, runtime)
		})
	}
}

func TestK8sRuntimeProfiles_RuntimeClassNames(t *testing.T) {
	t.Run("empty runtime leaves the class unset", func(t *testing.T) {
		must := require.New(t)
		must.Empty(k8sRuntimeProfiles[""].runtimeClassName)
	})
	for _, runtime := range types.ValidK8sExecutorRuntimeValues() {
		if runtime == "nitro-isolated" {
			continue
		}
		t.Run(runtime+" is its own class", func(t *testing.T) {
			must := require.New(t)
			profile := k8sRuntimeProfiles[runtime]
			must.Equal(runtime, profile.runtimeClassName)
			must.Empty(profile.podAnnotations)
			must.Empty(profile.noNetworkAnnotations)
			must.Nil(profile.verify)
		})
	}
	t.Run("nitro-isolated selects isolated-sandbox", func(t *testing.T) {
		must := require.New(t)
		profile := k8sRuntimeProfiles["nitro-isolated"]
		must.Equal("isolated-sandbox", profile.runtimeClassName)
		must.Equal(map[string]string{"sandbox.amazonaws.com/log-bandwidth": "20M"}, profile.podAnnotations)
		must.Equal(map[string]string{"sandbox.amazonaws.com/network-isolated": "true"}, profile.noNetworkAnnotations)
		must.NotNil(profile.verify)
	})
}

func TestK8sRuntimeProfileFor_RejectsUnknownRuntime(t *testing.T) {
	for _, runtime := range []string{"isolated-sandbox", "confidential-sandbox", "runsc", "kata"} {
		t.Run(runtime, func(t *testing.T) {
			must := require.New(t)
			_, err := k8sRuntimeProfileFor(runtime)
			must.ErrorContains(err, fmt.Sprintf("unsupported executor.runtime %q", runtime))
		})
	}
	t.Run("known runtime resolves", func(t *testing.T) {
		must := require.New(t)
		profile, err := k8sRuntimeProfileFor("gvisor")
		must.NoError(err)
		must.Equal("gvisor", profile.runtimeClassName)
	})
}

// procNetDevHeader is the two header lines Linux prints in /proc/net/dev.
const procNetDevHeader = "Inter-|   Receive                                                |  Transmit\n" +
	" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n"

// procNetRouteHeader is the header line Linux prints in /proc/net/route,
// including its trailing padding.
const procNetRouteHeader = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT                                                       \n"

const (
	procNetDevLo     = "    lo:       0       0    0    0    0     0          0         0        0       0    0    0    0     0       0          0\n"
	procNetDevDummy0 = "dummy0:       0       0    0    0    0     0          0         0        0       0    0    0    0     0       0          0\n"
	procNetDevEth0   = "  eth0:   91214     167    0    0    0     0          0         0    22914     183    0    0    0     0       0          0\n"
	procNetRouteDflt = "eth0\t00000000\t0100570A\t0003\t0\t0\t0\t00000000\t0\t0\t0                                                                               \n"
)

// guestDump frames file contents the way guestFileDumpScript prints them.
// A nil content reports the file as missing.
func guestDump(files ...*string) string {
	paths := []string{nitroDMIProductNamePath, procNetDevPath, procNetRoutePath}
	var b strings.Builder
	for i, content := range files {
		if content == nil {
			b.WriteString("missing " + paths[i] + "\n")
			continue
		}
		b.WriteString("begin " + paths[i] + "\n")
		if *content != "" {
			for _, line := range strings.Split(strings.TrimSuffix(*content, "\n"), "\n") {
				b.WriteString(" " + line + "\n")
			}
		}
		b.WriteString("end " + paths[i] + "\n")
	}
	return b.String()
}

func text(s string) *string { return &s }

// isolatedGuestDump is a network-isolated Nitro Sandbox guest: the
// .enclave product name, only lo and dummy0, and an empty route table.
var isolatedGuestDump = guestDump(
	text("m5d.enclave\n"),
	text(procNetDevHeader+procNetDevLo+procNetDevDummy0),
	text(procNetRouteHeader),
)

// fakeDiagnosticsCore returns a core whose exec returns stdout/err and
// records the argv, stdin and deadline of the call.
func fakeDiagnosticsCore(t *testing.T, stdout string, err error) (*podExecCore, *recordingSecurityEmitter, *[]diagnosticExecCall) {
	t.Helper()
	security := &recordingSecurityEmitter{}
	calls := &[]diagnosticExecCall{}
	core := &podExecCore{Security: security}
	core.execFn = func(ctx context.Context, command []string, stdin io.Reader, out, _ io.Writer) error {
		deadline, _ := ctx.Deadline()
		*calls = append(*calls, diagnosticExecCall{command: command, stdinSet: stdin != nil, deadline: deadline})
		if _, writeErr := io.WriteString(out, stdout); writeErr != nil {
			return writeErr
		}
		return err
	}
	return core, security, calls
}

type diagnosticExecCall struct {
	command  []string
	stdinSet bool
	deadline time.Time
}

func TestRunK8sRuntimeDiagnostics_NitroIsolatedGuestPasses(t *testing.T) {
	must := require.New(t)
	core, security, calls := fakeDiagnosticsCore(t, isolatedGuestDump, nil)

	start := time.Now()
	err := runK8sRuntimeDiagnostics(context.Background(), "nitro-isolated", k8sRuntimeProfiles["nitro-isolated"], core)
	must.NoError(err)

	must.Len(*calls, 1, "every diagnostic file is read in one exec")
	call := (*calls)[0]
	must.Equal([]string{"/bin/sh", "-c", guestFileDumpScript, "stirrup-diag", nitroDMIProductNamePath, procNetDevPath, procNetRoutePath}, call.command)
	must.False(call.stdinSet)
	must.WithinDuration(start.Add(k8sFileIOTimeout), call.deadline, 5*time.Second, "the exec is bounded by k8sFileIOTimeout")

	must.Equal([]runtimeDiagnosticEvent{{
		runtime:  "nitro-isolated",
		passed:   true,
		observed: map[string]string{"dmiProductName": "m5d.enclave", "interfaces": "lo,dummy0", "routeEntries": "0"},
	}}, security.runtimeDiagnostics)
}

func TestRunK8sRuntimeDiagnostics_NitroIsolatedFailures(t *testing.T) {
	ordinaryPod, err := os.ReadFile(filepath.Join("testdata", "runtime-diagnostics-ordinary-pod.txt"))
	require.NoError(t, err)

	cases := []struct {
		name         string
		stdout       string
		execErr      error
		wantCheck    string
		wantReason   string
		wantObserved map[string]string
	}{
		{
			name:         "ordinary pod captured from kind",
			stdout:       string(ordinaryPod),
			wantCheck:    "dmi_product_name",
			wantReason:   `DMI product name "kind" does not end in ".enclave"`,
			wantObserved: map[string]string{"dmiProductName": "kind", "interfaces": "lo,eth0", "routeEntries": "3"},
		},
		{
			name: "networked nitro guest",
			stdout: guestDump(
				text("m5d.enclave\n"),
				text(procNetDevHeader+procNetDevLo+procNetDevDummy0+procNetDevEth0),
				text(procNetRouteHeader+procNetRouteDflt),
			),
			wantCheck:    "network_interfaces",
			wantReason:   "network interfaces eth0 beyond the allowed dummy0,lo",
			wantObserved: map[string]string{"dmiProductName": "m5d.enclave", "interfaces": "lo,dummy0,eth0", "routeEntries": "1"},
		},
		{
			name: "dmi mismatch on an otherwise isolated guest",
			stdout: guestDump(
				text("m5d.xlarge\n"),
				text(procNetDevHeader+procNetDevLo+procNetDevDummy0),
				text(procNetRouteHeader),
			),
			wantCheck:    "dmi_product_name",
			wantReason:   `DMI product name "m5d.xlarge" does not end in ".enclave"`,
			wantObserved: map[string]string{"dmiProductName": "m5d.xlarge", "interfaces": "lo,dummy0", "routeEntries": "0"},
		},
		{
			name: "route entry with only allowed interfaces",
			stdout: guestDump(
				text("m5d.enclave\n"),
				text(procNetDevHeader+procNetDevLo+procNetDevDummy0),
				text(procNetRouteHeader+"dummy0\t00000000\t00000000\t0001\t0\t0\t0\t00000000\t0\t0\t0\n"),
			),
			wantCheck:    "route_table",
			wantReason:   "1 IPv4 route entries (first: dummy0 00000000)",
			wantObserved: map[string]string{"dmiProductName": "m5d.enclave", "interfaces": "lo,dummy0", "routeEntries": "1"},
		},
		{
			name: "missing dmi file",
			stdout: guestDump(
				nil,
				text(procNetDevHeader+procNetDevLo+procNetDevDummy0),
				text(procNetRouteHeader),
			),
			wantCheck:    "dmi_product_name",
			wantReason:   "/sys/class/dmi/id/product_name is absent, unreadable or not a regular file",
			wantObserved: map[string]string{"interfaces": "lo,dummy0", "routeEntries": "0"},
		},
		{
			name: "missing route table",
			stdout: guestDump(
				text("m5d.enclave"),
				text(procNetDevHeader+procNetDevLo),
				nil,
			),
			wantCheck:    "route_table",
			wantReason:   "/proc/net/route is absent, unreadable or not a regular file",
			wantObserved: map[string]string{"dmiProductName": "m5d.enclave", "interfaces": "lo"},
		},
		{
			name: "malformed interface line",
			stdout: guestDump(
				text("m5d.enclave\n"),
				text(procNetDevHeader+"    lo:       0       0\n"),
				text(procNetRouteHeader),
			),
			wantCheck:    "network_interfaces",
			wantReason:   `unrecognised /proc/net/dev line`,
			wantObserved: map[string]string{"dmiProductName": "m5d.enclave", "routeEntries": "0"},
		},
		{
			name: "interface table without headers",
			stdout: guestDump(
				text("m5d.enclave\n"),
				text(procNetDevLo),
				text(procNetRouteHeader),
			),
			wantCheck:    "network_interfaces",
			wantReason:   "/proc/net/dev does not start with its two header lines",
			wantObserved: map[string]string{"dmiProductName": "m5d.enclave", "routeEntries": "0"},
		},
		{
			name: "empty dmi file",
			stdout: guestDump(
				text(""),
				text(procNetDevHeader+procNetDevLo+procNetDevDummy0),
				text(procNetRouteHeader),
			),
			wantCheck:    "dmi_product_name",
			wantReason:   `DMI product name "" does not end in ".enclave"`,
			wantObserved: map[string]string{"dmiProductName": "", "interfaces": "lo,dummy0", "routeEntries": "0"},
		},
		{
			name:       "malformed framing",
			stdout:     "m5d.enclave\n",
			wantCheck:  "diagnostic_output",
			wantReason: `unexpected line "m5d.enclave" where the report for /sys/class/dmi/id/product_name should start`,
		},
		{
			name:       "truncated report",
			stdout:     strings.TrimSuffix(isolatedGuestDump, "end /proc/net/route\n"),
			wantCheck:  "diagnostic_output",
			wantReason: "the report for /proc/net/route is not terminated",
		},
		{
			name:       "no output",
			stdout:     "",
			wantCheck:  "diagnostic_output",
			wantReason: "guest file read produced no output",
		},
		{
			name:       "oversized output",
			stdout:     isolatedGuestDump + strings.Repeat("x", guestFileDumpOutputLimit),
			wantCheck:  "diagnostic_output",
			wantReason: fmt.Sprintf("more than %d bytes", guestFileDumpOutputLimit),
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

			err := runK8sRuntimeDiagnostics(context.Background(), "nitro-isolated", k8sRuntimeProfiles["nitro-isolated"], core)
			must.ErrorContains(err, `runtime diagnostics for executor.runtime "nitro-isolated" failed: `+tc.wantCheck+": ")
			must.ErrorContains(err, tc.wantReason)
			var diagErr *k8sRuntimeDiagnosticError
			must.ErrorAs(err, &diagErr)
			if tc.execErr != nil {
				must.ErrorIs(err, tc.execErr)
			}

			must.Len(security.runtimeDiagnostics, 1)
			event := security.runtimeDiagnostics[0]
			check.Equal("nitro-isolated", event.runtime)
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

func TestRunK8sRuntimeDiagnostics_ProfileWithoutVerifyRunsNothing(t *testing.T) {
	must := require.New(t)
	core, security, calls := fakeDiagnosticsCore(t, "", nil)
	must.NoError(runK8sRuntimeDiagnostics(context.Background(), "gvisor", k8sRuntimeProfiles["gvisor"], core))
	must.Empty(*calls)
	must.Empty(security.runtimeDiagnostics)
}

func TestRunK8sRuntimeDiagnostics_NilSecurityStillFailsClosed(t *testing.T) {
	must := require.New(t)
	core, _, _ := fakeDiagnosticsCore(t, guestDump(text("kind\n"), text(procNetDevHeader+procNetDevLo), text(procNetRouteHeader)), nil)
	core.Security = nil
	err := runK8sRuntimeDiagnostics(context.Background(), "nitro-isolated", k8sRuntimeProfiles["nitro-isolated"], core)
	must.ErrorContains(err, "dmi_product_name")
}

func TestRunK8sRuntimeDiagnostics_BoundsGuestValues(t *testing.T) {
	must := require.New(t)
	long := strings.Repeat("é", diagnosticValueMaxBytes)
	core, security, _ := fakeDiagnosticsCore(t, guestDump(text(long), text(procNetDevHeader+procNetDevLo), text(procNetRouteHeader)), nil)

	err := runK8sRuntimeDiagnostics(context.Background(), "nitro-isolated", k8sRuntimeProfiles["nitro-isolated"], core)
	must.Error(err)
	must.Len(security.runtimeDiagnostics, 1)
	name := security.runtimeDiagnostics[0].observed["dmiProductName"]
	must.LessOrEqual(len(name), diagnosticValueMaxBytes)
	must.True(strings.HasSuffix(name, "…"), "a cut value is marked: %q", name)
	must.Less(len(err.Error()), 2*diagnosticValueMaxBytes+200, "the error quotes the bounded value")
}

func TestParseGuestFileDump_RequiresEachPathInOrder(t *testing.T) {
	paths := []string{"/a", "/b"}
	cases := []struct {
		name    string
		out     string
		wantErr string
	}{
		{name: "out of order", out: "missing /b\nmissing /a\n", wantErr: `unexpected line "missing /b" where the report for /a should start`},
		{name: "report missing", out: "missing /a\n", wantErr: "no report for /b"},
		{name: "extra report", out: "missing /a\nmissing /b\nmissing /c\n", wantErr: `unexpected line "missing /c" after the last report`},
		{name: "unprefixed content", out: "begin /a\nx\nend /a\nmissing /b\n", wantErr: `unexpected line "x" inside the report for /a`},
		{name: "mismatched end", out: "begin /a\nend /b\n", wantErr: `unexpected line "end /b" inside the report for /a`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			_, err := parseGuestFileDump([]byte(tc.out), paths)
			must.EqualError(err, tc.wantErr)
		})
	}
	t.Run("content keeps leading whitespace and a line without a newline", func(t *testing.T) {
		must := require.New(t)
		files, err := parseGuestFileDump([]byte("begin /a\n   indented\n last\nend /a\nmissing /b\n"), paths)
		must.NoError(err)
		must.Equal(map[string]guestFile{
			"/a": {present: true, lines: []string{"  indented", "last"}},
			"/b": {},
		}, files)
	})
}
