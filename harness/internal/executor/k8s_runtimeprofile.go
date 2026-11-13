package executor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"

	"github.com/rxbynerd/stirrup/types"
)

// k8sRuntimeProfile declares how the k8s executor adapts a sandbox Pod to
// one executor.runtime value. The requirement rules for a runtime live in
// types.K8sRuntimeRequirementErrors, not here.
type k8sRuntimeProfile struct {
	// runtimeClassName is the Pod RuntimeClassName; empty leaves it unset.
	runtimeClassName string
	// podAnnotations are set on every sandbox Pod for this runtime.
	podAnnotations map[string]string
	// noNetworkAnnotations are added when network.mode is "none".
	noNetworkAnnotations map[string]string
	// strictAdmission adds the whole-Pod admission checks
	// (strictAdmissionChecks) at creation and during the readiness wait,
	// and reports an admission failure as a runtime diagnostic event.
	strictAdmission bool
	// requiredOverheadResources must each be a key of the admitted Pod's
	// spec.overhead, and requiredNodeSelector entries must be in its
	// spec.nodeSelector with exactly these values: the marks the platform's
	// own RuntimeClass sets, which a same-named class elsewhere lacks.
	// Checked only under strictAdmission.
	requiredOverheadResources []corev1.ResourceName
	requiredNodeSelector      map[string]string
	// verify runs guest diagnostics after readiness and returns what the
	// guest reported; nil means the runtime has none. A failed check is a
	// *k8sRuntimeDiagnosticError.
	verify func(ctx context.Context, core *podExecCore) (map[string]string, error)
}

// k8sRuntimeProfiles has one entry per accepted k8s executor.runtime value,
// including "". The shared maps are read-only; callers copy them.
var k8sRuntimeProfiles = map[string]k8sRuntimeProfile{
	"":          {},
	"runc":      {runtimeClassName: "runc"},
	"gvisor":    {runtimeClassName: "gvisor"},
	"kata-qemu": {runtimeClassName: "kata-qemu"},
	"kata-fc":   {runtimeClassName: "kata-fc"},
	"kata-clh":  {runtimeClassName: "kata-clh"},
	"nitro-isolated": {
		runtimeClassName: "isolated-sandbox",
		// The default 2M log throttle also throttles exec stdout, the only
		// path results leave an offline guest by.
		podAnnotations:            map[string]string{"sandbox.amazonaws.com/log-bandwidth": "20M"},
		noNetworkAnnotations:      map[string]string{"sandbox.amazonaws.com/network-isolated": "true"},
		strictAdmission:           true,
		requiredOverheadResources: []corev1.ResourceName{"eks.amazonaws.com/nitro-sandbox"},
		requiredNodeSelector:      map[string]string{"eks.amazonaws.com/nitro-sandbox": "true"},
		verify:                    verifyNitroIsolatedGuest,
	},
}

// k8sRuntimeProfileFor returns the profile for an executor.runtime value.
// A value without a profile is an error rather than a fallback, so a caller
// that bypasses ValidateRunConfig cannot run under assumptions the executor
// does not hold for that runtime.
func k8sRuntimeProfileFor(runtime string) (k8sRuntimeProfile, error) {
	profile, ok := k8sRuntimeProfiles[runtime]
	if !ok {
		return k8sRuntimeProfile{}, fmt.Errorf(
			"k8s executor: unsupported executor.runtime %q (accepted: %s, or empty)",
			runtime, strings.Join(types.ValidK8sExecutorRuntimeValues(), ", "))
	}
	return profile, nil
}

// k8sRuntimeDiagnosticError is a failed guest diagnostic. check names the
// failing check for the sandbox_runtime_diagnostic_failed event.
type k8sRuntimeDiagnosticError struct {
	check  string
	reason string
	err    error
}

func (e *k8sRuntimeDiagnosticError) Error() string { return e.check + ": " + e.reason }

func (e *k8sRuntimeDiagnosticError) Unwrap() error { return e.err }

// runK8sRuntimeDiagnostics runs the profile's guest diagnostics over core's
// exec path and reports the outcome as a sandbox runtime diagnostic event.
// A profile without diagnostics passes without an event.
func runK8sRuntimeDiagnostics(ctx context.Context, runtime string, profile k8sRuntimeProfile, core *podExecCore) error {
	if profile.verify == nil {
		return nil
	}
	observed, err := profile.verify(ctx, core)
	if err == nil {
		if core.Security != nil {
			core.Security.SandboxRuntimeDiagnostic(runtime, true, "", "", observed)
		}
		return nil
	}
	var diagErr *k8sRuntimeDiagnosticError
	if !errors.As(err, &diagErr) {
		diagErr = &k8sRuntimeDiagnosticError{check: diagnosticCheckExec, reason: err.Error(), err: err}
	}
	if core.Security != nil {
		core.Security.SandboxRuntimeDiagnostic(runtime, false, diagErr.check, diagErr.reason, observed)
	}
	return fmt.Errorf("runtime diagnostics for executor.runtime %q failed: %w", runtime, diagErr)
}

const (
	nitroDMIProductNamePath = "/sys/class/dmi/id/product_name"
	procNetDevPath          = "/proc/net/dev"
	procNetRoutePath        = "/proc/net/route"

	// nitroGuestDMISuffix ends the DMI product name a Nitro Sandbox guest
	// reports (for example "m5d.enclave").
	nitroGuestDMISuffix = ".enclave"

	diagnosticCheckAdmission  = "admitted_pod"
	diagnosticCheckExec       = "diagnostic_exec"
	diagnosticCheckOutput     = "diagnostic_output"
	diagnosticCheckDMI        = "dmi_product_name"
	diagnosticCheckInterfaces = "network_interfaces"
	diagnosticCheckRoutes     = "route_table"

	// guestFileDumpOutputLimit bounds the diagnostic exec's stdout and
	// stderr; the files read are a few hundred bytes on a sandbox guest.
	guestFileDumpOutputLimit = 64 * 1024
	// diagnosticValueMaxBytes bounds a guest-reported value quoted in an
	// observed entry or a failure reason.
	diagnosticValueMaxBytes = 128
)

// nitroIsolatedAllowedInterfaces are the interfaces a network-isolated Nitro
// Sandbox guest lists in /proc/net/dev. A guest kernel that adds another
// device fails the diagnostic until this set is updated.
var nitroIsolatedAllowedInterfaces = map[string]bool{"lo": true, "dummy0": true}

// guestFileDumpScript prints each file named in its arguments using only
// shell builtins, since the image contract does not include cat. A readable
// file is framed by "begin <path>" and "end <path>" with every content line
// prefixed by one space, so content can never be read as a frame line; the
// "|| [ -n ...]" keeps a last line that has no trailing newline. A path that
// is absent, unreadable or not a regular file prints "missing <path>".
const guestFileDumpScript = `for f in "$@"; do
  if [ -f "$f" ] && [ -r "$f" ]; then
    printf 'begin %s\n' "$f"
    while IFS= read -r l || [ -n "$l" ]; do printf ' %s\n' "$l"; done < "$f"
    printf 'end %s\n' "$f"
  else
    printf 'missing %s\n' "$f"
  fi
done`

// guestFileDumpCommand is the argv that reads paths inside the sandbox.
func guestFileDumpCommand(paths ...string) []string {
	return append([]string{"/bin/sh", "-c", guestFileDumpScript, "stirrup-diag"}, paths...)
}

// guestFile is one file guestFileDumpScript reported.
type guestFile struct {
	present bool
	lines   []string
}

// readGuestFiles reads paths inside the sandbox in one exec bounded by
// k8sFileIOTimeout. An exec, exit or framing failure is a
// diagnostic_exec or diagnostic_output *k8sRuntimeDiagnosticError.
func readGuestFiles(ctx context.Context, core *podExecCore, paths ...string) (map[string]guestFile, error) {
	ctx, cancel := context.WithTimeout(ctx, k8sFileIOTimeout)
	defer cancel()

	stdout := writeCapBuffer{limit: guestFileDumpOutputLimit}
	stderr := writeCapBuffer{limit: guestFileDumpOutputLimit}
	execErr := core.streamExec(ctx, guestFileDumpCommand(paths...), nil, &stdout, &stderr)
	if stdout.exceeded || stderr.exceeded {
		return nil, &k8sRuntimeDiagnosticError{
			check:  diagnosticCheckOutput,
			reason: fmt.Sprintf("guest file read produced more than %d bytes", guestFileDumpOutputLimit),
		}
	}
	if execErr != nil {
		if code, ok := extractExitCode(execErr); ok && code != 0 {
			return nil, &k8sRuntimeDiagnosticError{
				check:  diagnosticCheckExec,
				reason: fmt.Sprintf("guest file read exited %d: %s", code, boundDiagnosticValue(strings.TrimSpace(stderr.String()))),
				err:    execErr,
			}
		}
		return nil, &k8sRuntimeDiagnosticError{
			check:  diagnosticCheckExec,
			reason: fmt.Sprintf("guest file read failed: %v", execErr),
			err:    execErr,
		}
	}
	files, err := parseGuestFileDump(stdout.Bytes(), paths)
	if err != nil {
		return nil, &k8sRuntimeDiagnosticError{check: diagnosticCheckOutput, reason: err.Error(), err: err}
	}
	return files, nil
}

// parseGuestFileDump parses guestFileDumpScript's output and requires a
// report for exactly the requested paths, in order.
func parseGuestFileDump(out []byte, paths []string) (map[string]guestFile, error) {
	files := make(map[string]guestFile, len(paths))
	next := 0
	section := ""
	var current guestFile
	text := strings.TrimSuffix(string(out), "\n")
	if text == "" {
		return nil, errors.New("guest file read produced no output")
	}
	for _, line := range strings.Split(text, "\n") {
		if section != "" {
			switch {
			case strings.HasPrefix(line, " "):
				current.lines = append(current.lines, line[1:])
			case line == "end "+section:
				files[section] = current
				section = ""
			default:
				return nil, fmt.Errorf("unexpected line %q inside the report for %s", boundDiagnosticValue(line), section)
			}
			continue
		}
		if next == len(paths) {
			return nil, fmt.Errorf("unexpected line %q after the last report", boundDiagnosticValue(line))
		}
		want := paths[next]
		switch line {
		case "begin " + want:
			section = want
			current = guestFile{present: true}
		case "missing " + want:
			files[want] = guestFile{}
		default:
			return nil, fmt.Errorf("unexpected line %q where the report for %s should start", boundDiagnosticValue(line), want)
		}
		next++
	}
	if section != "" {
		return nil, fmt.Errorf("the report for %s is not terminated", section)
	}
	if next != len(paths) {
		return nil, fmt.Errorf("no report for %s", paths[next])
	}
	return files, nil
}

// verifyNitroIsolatedGuest checks, from inside the guest, that the Pod runs
// in a Nitro Sandbox with no guest IP network: the DMI product name ends in
// ".enclave", /proc/net/dev lists no interface beyond lo and dummy0, and
// /proc/net/route has no entries. /proc/net follows the reading process's
// network namespace, which /sys/class/net does not. The image supplies the
// shell, so this is a configuration diagnostic, not attestation. Every
// check is evaluated so observed is complete; the first failure is returned.
func verifyNitroIsolatedGuest(ctx context.Context, core *podExecCore) (map[string]string, error) {
	files, err := readGuestFiles(ctx, core, nitroDMIProductNamePath, procNetDevPath, procNetRoutePath)
	if err != nil {
		return nil, err
	}
	observed := map[string]string{}
	var failures []*k8sRuntimeDiagnosticError

	if failure := checkNitroDMIProductName(files[nitroDMIProductNamePath], observed); failure != nil {
		failures = append(failures, failure)
	}
	if failure := checkIsolatedInterfaces(files[procNetDevPath], observed); failure != nil {
		failures = append(failures, failure)
	}
	if failure := checkEmptyRouteTable(files[procNetRoutePath], observed); failure != nil {
		failures = append(failures, failure)
	}
	if len(failures) > 0 {
		return observed, failures[0]
	}
	return observed, nil
}

func checkNitroDMIProductName(file guestFile, observed map[string]string) *k8sRuntimeDiagnosticError {
	if !file.present {
		return &k8sRuntimeDiagnosticError{check: diagnosticCheckDMI, reason: nitroDMIProductNamePath + " is absent, unreadable or not a regular file"}
	}
	name := strings.TrimSpace(strings.Join(file.lines, "\n"))
	observed["dmiProductName"] = boundDiagnosticValue(name)
	if !strings.HasSuffix(name, nitroGuestDMISuffix) {
		return &k8sRuntimeDiagnosticError{
			check:  diagnosticCheckDMI,
			reason: fmt.Sprintf("DMI product name %q does not end in %q, so the Pod is not running in a Nitro Sandbox", boundDiagnosticValue(name), nitroGuestDMISuffix),
		}
	}
	return nil
}

func checkIsolatedInterfaces(file guestFile, observed map[string]string) *k8sRuntimeDiagnosticError {
	if !file.present {
		return &k8sRuntimeDiagnosticError{check: diagnosticCheckInterfaces, reason: procNetDevPath + " is absent, unreadable or not a regular file"}
	}
	names, err := parseProcNetDevInterfaces(file.lines)
	if err != nil {
		return &k8sRuntimeDiagnosticError{check: diagnosticCheckInterfaces, reason: err.Error(), err: err}
	}
	observed["interfaces"] = boundDiagnosticValue(strings.Join(names, ","))
	var unexpected []string
	for _, name := range names {
		if !nitroIsolatedAllowedInterfaces[name] {
			unexpected = append(unexpected, name)
		}
	}
	if len(unexpected) > 0 {
		return &k8sRuntimeDiagnosticError{
			check: diagnosticCheckInterfaces,
			reason: fmt.Sprintf("the guest lists network interfaces %s beyond the allowed %s",
				boundDiagnosticValue(strings.Join(unexpected, ",")), strings.Join(sortedSetMembers(nitroIsolatedAllowedInterfaces), ",")),
		}
	}
	return nil
}

func checkEmptyRouteTable(file guestFile, observed map[string]string) *k8sRuntimeDiagnosticError {
	if !file.present {
		return &k8sRuntimeDiagnosticError{check: diagnosticCheckRoutes, reason: procNetRoutePath + " is absent, unreadable or not a regular file"}
	}
	routes, err := parseProcNetRouteEntries(file.lines)
	if err != nil {
		return &k8sRuntimeDiagnosticError{check: diagnosticCheckRoutes, reason: err.Error(), err: err}
	}
	observed["routeEntries"] = strconv.Itoa(len(routes))
	if len(routes) > 0 {
		return &k8sRuntimeDiagnosticError{
			check:  diagnosticCheckRoutes,
			reason: fmt.Sprintf("the guest has %d IPv4 route entries (first: %s)", len(routes), boundDiagnosticValue(routes[0])),
		}
	}
	return nil
}

// parseProcNetDevInterfaces returns the interface names in /proc/net/dev,
// in file order. The two header lines are required, and every other line
// must be "<name>: <16 counters>".
func parseProcNetDevInterfaces(lines []string) ([]string, error) {
	lines = trimTrailingBlankLines(lines)
	if len(lines) < 2 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "Inter-|") || !strings.HasPrefix(strings.TrimSpace(lines[1]), "face |") {
		return nil, errors.New(procNetDevPath + " does not start with its two header lines")
	}
	var names []string
	for _, line := range lines[2:] {
		name, counters, ok := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		fields := strings.Fields(counters)
		if !ok || name == "" || len(fields) != 16 || !allUnsigned(fields) {
			return nil, fmt.Errorf("unrecognised %s line %q", procNetDevPath, boundDiagnosticValue(line))
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, errors.New(procNetDevPath + " lists no interfaces")
	}
	return names, nil
}

// parseProcNetRouteEntries returns the "<iface> <destination>" of each
// entry in /proc/net/route. The header line is required; the kernel prints
// it even when the table is empty.
func parseProcNetRouteEntries(lines []string) ([]string, error) {
	lines = trimTrailingBlankLines(lines)
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "Iface") {
		return nil, errors.New(procNetRoutePath + " does not start with its header line")
	}
	var entries []string
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("unrecognised %s line %q", procNetRoutePath, boundDiagnosticValue(line))
		}
		entries = append(entries, fields[0]+" "+fields[1])
	}
	return entries, nil
}

func trimTrailingBlankLines(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func allUnsigned(fields []string) bool {
	for _, f := range fields {
		if _, err := strconv.ParseUint(f, 10, 64); err != nil {
			return false
		}
	}
	return true
}

func sortedSetMembers(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// boundDiagnosticValue cuts a guest-reported value to
// diagnosticValueMaxBytes on a rune boundary.
func boundDiagnosticValue(s string) string {
	if len(s) <= diagnosticValueMaxBytes {
		return s
	}
	const marker = "…"
	cut := diagnosticValueMaxBytes - len(marker)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + marker
}
