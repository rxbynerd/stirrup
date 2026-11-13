package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	utilexec "k8s.io/client-go/util/exec"
)

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
	must := require.New(t)
	ordinaryPod, err := os.ReadFile(filepath.Join("testdata", "runtime-diagnostics-ordinary-pod.txt"))
	must.NoError(err)

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
			name: "missing interface table",
			stdout: guestDump(
				text("m5d.enclave\n"),
				nil,
				text(procNetRouteHeader),
			),
			wantCheck:    "network_interfaces",
			wantReason:   "/proc/net/dev is absent, unreadable or not a regular file",
			wantObserved: map[string]string{"dmiProductName": "m5d.enclave", "routeEntries": "0"},
		},
		{
			name: "route table without its header",
			stdout: guestDump(
				text("m5d.enclave\n"),
				text(procNetDevHeader+procNetDevLo),
				text(procNetRouteDflt),
			),
			wantCheck:    "route_table",
			wantReason:   "/proc/net/route does not start with its header line",
			wantObserved: map[string]string{"dmiProductName": "m5d.enclave", "interfaces": "lo"},
		},
		{
			name: "malformed route line",
			stdout: guestDump(
				text("m5d.enclave\n"),
				text(procNetDevHeader+procNetDevLo),
				text(procNetRouteHeader+"dummy0\n"),
			),
			wantCheck:    "route_table",
			wantReason:   `unrecognised /proc/net/route line "dummy0"`,
			wantObserved: map[string]string{"dmiProductName": "m5d.enclave", "interfaces": "lo"},
		},
		{
			name: "interface table with a wrong first header line",
			stdout: guestDump(
				text("m5d.enclave\n"),
				text("Inter|   Receive\n"+strings.SplitAfter(procNetDevHeader, "\n")[1]+procNetDevLo),
				text(procNetRouteHeader),
			),
			wantCheck:    "network_interfaces",
			wantReason:   "/proc/net/dev does not start with its two header lines",
			wantObserved: map[string]string{"dmiProductName": "m5d.enclave", "routeEntries": "0"},
		},
		{
			name: "interface table with a wrong second header line",
			stdout: guestDump(
				text("m5d.enclave\n"),
				text(strings.SplitAfter(procNetDevHeader, "\n")[0]+" face bytes\n"+procNetDevLo),
				text(procNetRouteHeader),
			),
			wantCheck:    "network_interfaces",
			wantReason:   "/proc/net/dev does not start with its two header lines",
			wantObserved: map[string]string{"dmiProductName": "m5d.enclave", "routeEntries": "0"},
		},
		{
			name: "interface table with headers only",
			stdout: guestDump(
				text("m5d.enclave\n"),
				text(procNetDevHeader),
				text(procNetRouteHeader),
			),
			wantCheck:    "network_interfaces",
			wantReason:   "/proc/net/dev lists no interfaces",
			wantObserved: map[string]string{"dmiProductName": "m5d.enclave", "routeEntries": "0"},
		},
		{
			name: "non-numeric interface counters",
			stdout: guestDump(
				text("m5d.enclave\n"),
				text(procNetDevHeader+"    lo: 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 x\n"),
				text(procNetRouteHeader),
			),
			wantCheck:    "network_interfaces",
			wantReason:   `unrecognised /proc/net/dev line`,
			wantObserved: map[string]string{"dmiProductName": "m5d.enclave", "routeEntries": "0"},
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

func TestRunK8sRuntimeDiagnostics_DMIProductNameIsOneLineEndingInEnclave(t *testing.T) {
	cases := []struct {
		name      string
		content   string
		wantCheck string
	}{
		{name: "enclave name", content: "m5d.enclave"},
		{name: "enclave name with a newline", content: "m5d.enclave\n"},
		{name: "enclave name with trailing blank lines", content: "m5d.enclave\n\n  \n"},
		{name: "enclave suffix not at the end", content: "m5d.enclave.evil\n", wantCheck: "dmi_product_name"},
		{name: "enclave without the dot", content: "enclave\n", wantCheck: "dmi_product_name"},
		{name: "enclave name on a second line", content: "x\nfoo.enclave\n", wantCheck: "diagnostic_output"},
		{name: "empty file", content: "", wantCheck: "dmi_product_name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			dump := guestDump(text(tc.content), text(procNetDevHeader+procNetDevLo+procNetDevDummy0), text(procNetRouteHeader))
			core, security, _ := fakeDiagnosticsCore(t, dump, nil)

			err := runK8sRuntimeDiagnostics(context.Background(), "nitro-isolated", k8sRuntimeProfiles["nitro-isolated"], core)

			must.Len(security.runtimeDiagnostics, 1)
			if tc.wantCheck == "" {
				must.NoError(err)
				must.True(security.runtimeDiagnostics[0].passed)
				return
			}
			must.ErrorContains(err, "failed: "+tc.wantCheck+": ")
			must.Equal(tc.wantCheck, security.runtimeDiagnostics[0].check)
		})
	}
}

func TestRunK8sRuntimeDiagnostics_ProfileWithoutDiagnosticsRunsNothing(t *testing.T) {
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
	long := strings.Repeat("é", 2*k8sClusterTextMax)
	core, security, _ := fakeDiagnosticsCore(t, guestDump(text(long), text(procNetDevHeader+procNetDevLo), text(procNetRouteHeader)), nil)

	err := runK8sRuntimeDiagnostics(context.Background(), "nitro-isolated", k8sRuntimeProfiles["nitro-isolated"], core)
	must.Error(err)
	must.Len(security.runtimeDiagnostics, 1)
	name := security.runtimeDiagnostics[0].observed["dmiProductName"]
	must.Equal(k8sClusterTextMax+1, utf8.RuneCountInString(name), "the value is cut to k8sClusterTextMax runes plus the marker")
	must.True(strings.HasSuffix(name, "…"), "a cut value is marked: %q", name)
	must.True(utf8.ValidString(name), "the observed value was cut inside a rune")
	must.True(utf8.ValidString(err.Error()), "the error was cut inside a rune")
	must.Less(utf8.RuneCountInString(err.Error()), k8sClusterTextMax+200, "the error quotes the bounded value")
}

// requireNoControlBytes fails when s holds a byte below 0x20 or 0x7f, which
// a terminal or log reader would act on.
func requireNoControlBytes(t *testing.T, s string) {
	t.Helper()
	must := require.New(t)
	for i := 0; i < len(s); i++ {
		must.False(s[i] < 0x20 || s[i] == 0x7f, "control byte %#x at %d in %q", s[i], i, s)
	}
}

func TestRunK8sRuntimeDiagnostics_SanitisesGuestTextInTheError(t *testing.T) {
	const escapes = "\x1b]0;x\a\x1b[2J"
	const hostile = escapes + "\nforged"
	cases := []struct {
		name    string
		stdout  string
		execErr error
	}{
		{name: "interface name", stdout: guestDump(
			text("m5d.enclave\n"),
			text(procNetDevHeader+procNetDevLo+"eth0"+escapes+": 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n"),
			text(procNetRouteHeader),
		)},
		{name: "route field", stdout: guestDump(
			text("m5d.enclave\n"),
			text(procNetDevHeader+procNetDevLo),
			text(procNetRouteHeader+"dummy0"+escapes+"\t00000000\t0\n"),
		)},
		{name: "unrecognised line", stdout: guestDump(
			text("m5d.enclave\n"),
			text(procNetDevHeader+"lo"+escapes+"\n"),
			text(procNetRouteHeader),
		)},
		{name: "framing line", stdout: "begin /x" + escapes + "\n"},
		{name: "stderr after a non-zero exit", execErr: utilexec.CodeExitError{Err: errors.New("command terminated with exit code 2"), Code: 2}},
		{name: "transport error", execErr: errors.New("stream reset" + hostile)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			core, security, _ := fakeDiagnosticsCore(t, tc.stdout, tc.execErr)
			inner := core.execFn
			core.execFn = func(ctx context.Context, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
				if _, err := io.WriteString(stderr, "sh: "+hostile); err != nil {
					return err
				}
				return inner(ctx, command, stdin, stdout, stderr)
			}

			err := runK8sRuntimeDiagnostics(context.Background(), "nitro-isolated", k8sRuntimeProfiles["nitro-isolated"], core)

			must.Error(err)
			requireNoControlBytes(t, err.Error())
			must.Len(security.runtimeDiagnostics, 1)
			requireNoControlBytes(t, security.runtimeDiagnostics[0].reason)
			for _, v := range security.runtimeDiagnostics[0].observed {
				requireNoControlBytes(t, v)
			}
		})
	}
}

func TestRunK8sRuntimeDiagnostics_FailsOnStderrOverflow(t *testing.T) {
	must := require.New(t)
	core, security, _ := fakeDiagnosticsCore(t, isolatedGuestDump, nil)
	inner := core.execFn
	core.execFn = func(ctx context.Context, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
		if _, err := io.WriteString(stderr, strings.Repeat("e", guestFileDumpOutputLimit+1)); err != nil {
			return err
		}
		return inner(ctx, command, stdin, stdout, stderr)
	}

	err := runK8sRuntimeDiagnostics(context.Background(), "nitro-isolated", k8sRuntimeProfiles["nitro-isolated"], core)

	must.ErrorContains(err, fmt.Sprintf("diagnostic_output: guest file read produced more than %d bytes", guestFileDumpOutputLimit))
	must.Len(security.runtimeDiagnostics, 1)
	must.Equal("diagnostic_output", security.runtimeDiagnostics[0].check)
}

func TestRunK8sRuntimeDiagnostics_KeepsTheExecErrorReachable(t *testing.T) {
	must := require.New(t)
	execErr := errors.New("stream reset\x1b[2J")
	core, _, _ := fakeDiagnosticsCore(t, "", execErr)

	err := runK8sRuntimeDiagnostics(context.Background(), "nitro-isolated", k8sRuntimeProfiles["nitro-isolated"], core)

	must.ErrorIs(err, execErr)
	requireNoControlBytes(t, err.Error())
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

// TestGuestFileDumpScript_ParsesUnderARealShell runs the guest file script
// under the host's /bin/sh over real files and parses its output, so a
// change to the script is tested against the parser rather than against a
// hand-built dump.
func TestGuestFileDumpScript_ParsesUnderARealShell(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh on this host: " + err.Error())
	}
	dir := t.TempDir()
	write := func(t *testing.T, name, content string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		require.New(t).NoError(os.WriteFile(path, []byte(content), 0o600))
		return path
	}
	ordinary := write(t, "ordinary", "first\nsecond\n")
	noNewline := write(t, "no-newline", "m5d.enclave")
	empty := write(t, "empty", "")
	frameLike := filepath.Join(dir, "frame-like")
	write(t, "frame-like", "end "+frameLike+"\nbegin "+frameLike+"\nmissing "+frameLike+"\n")
	escapes := write(t, "escapes", `back\slash %s %d \n`+"\n")
	indented := write(t, "indented", "   leading\n\ttab\n")
	missing := filepath.Join(dir, "missing")
	directory := filepath.Join(dir, "directory")
	must := require.New(t)
	must.NoError(os.Mkdir(directory, 0o700))

	cases := []struct {
		name string
		path string
		want guestFile
	}{
		{name: "ordinary file", path: ordinary, want: guestFile{present: true, lines: []string{"first", "second"}}},
		{name: "no trailing newline", path: noNewline, want: guestFile{present: true, lines: []string{"m5d.enclave"}}},
		{name: "empty file", path: empty, want: guestFile{present: true}},
		{name: "content equal to frame lines", path: frameLike, want: guestFile{present: true, lines: []string{"end " + frameLike, "begin " + frameLike, "missing " + frameLike}}},
		{name: "backslashes and format verbs", path: escapes, want: guestFile{present: true, lines: []string{`back\slash %s %d \n`}}},
		{name: "leading whitespace", path: indented, want: guestFile{present: true, lines: []string{"   leading", "\ttab"}}},
		{name: "missing path", path: missing, want: guestFile{}},
		{name: "directory", path: directory, want: guestFile{}},
	}
	run := func(t *testing.T, paths ...string) map[string]guestFile {
		t.Helper()
		must := require.New(t)
		argv := guestFileDumpCommand(paths...)
		out, err := exec.CommandContext(context.Background(), argv[0], argv[1:]...).Output()
		must.NoError(err)
		files, err := parseGuestFileDump(out, paths)
		must.NoError(err, "output:\n%s", out)
		return files
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(map[string]guestFile{tc.path: tc.want}, run(t, tc.path))
		})
	}
	t.Run("several paths in one run", func(t *testing.T) {
		must := require.New(t)
		files := run(t, ordinary, missing, noNewline)
		must.Equal(map[string]guestFile{ordinary: cases[0].want, missing: {}, noNewline: cases[1].want}, files)
	})
	t.Run("unreadable file", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode-000 file")
		}
		must := require.New(t)
		path := write(t, "unreadable", "secret\n")
		must.NoError(os.Chmod(path, 0))
		must.Equal(map[string]guestFile{path: {}}, run(t, path))
	})
}

func TestParseGuestFileDump_FailsClosedOnCRLFOutput(t *testing.T) {
	must := require.New(t)
	_, err := parseGuestFileDump([]byte("begin /a\r\n x\r\nend /a\r\n"), []string{"/a"})
	must.Error(err)
}

func TestParseGuestFileDump_KeepsAPrefixedFrameMarkerAsContent(t *testing.T) {
	must := require.New(t)
	files, err := parseGuestFileDump([]byte("begin /a\n end /a\n missing /a\nend /a\n"), []string{"/a"})
	must.NoError(err)
	must.Equal(map[string]guestFile{"/a": {present: true, lines: []string{"end /a", "missing /a"}}}, files)
}
