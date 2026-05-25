package cmd

import "errors"

// CLI exit-code scheme (issue #253, #240). A single closed set of codes
// applied across `harness`, `job`, and `run-config` so a wrapper script
// can branch on the failure class without parsing stderr:
//
//	0  success
//	1  validation failed (ValidateRunConfig / run-config --validate)
//	2  parse error (malformed JSON on stdin or in --config)
//	3  I/O error (stdin/stdout/file read/write)
//	4  usage error (an invalid flag combination — e.g. a --dry-run probe
//	   gate supplied without --dry-run)
//
// Code 0 is never carried by an exitError — a nil error from a command's
// RunE is the success path, and Execute() exits 0 implicitly. The
// constants below are the non-zero classes only.
const (
	exitValidation = 1
	exitParse      = 2
	exitIO         = 3
	exitUsage      = 4
)

// exitError wraps a command error with the CLI exit code its failure
// class maps to. Execute() unwraps it via errors.As and exits with the
// carried code; any error NOT wrapped in an exitError preserves the
// historical default-1 behaviour so nothing previously classified
// silently changes its exit status.
//
// The wrapper is transparent: Error() and Unwrap() defer to the
// underlying error so existing errors.Is / message-matching tests and
// the stderr text an operator sees are unchanged. Only the process exit
// code is affected.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }

func (e *exitError) Unwrap() error { return e.err }

// parseError tags err as a malformed-input failure (exit 2): JSON that
// failed to decode from --config or piped stdin. A nil err returns nil
// so call sites can wrap unconditionally without manufacturing a
// phantom failure.
func parseError(err error) error {
	if err == nil {
		return nil
	}
	return &exitError{code: exitParse, err: err}
}

// ioError tags err as an I/O failure (exit 3): a --config / --prompt-file
// path that could not be opened, stat'd, or read, or an output write
// that failed. A nil err returns nil.
func ioError(err error) error {
	if err == nil {
		return nil
	}
	return &exitError{code: exitIO, err: err}
}

// validationError tags err as a configuration-validation failure
// (exit 1): a RunConfig that parsed cleanly but failed
// ValidateRunConfig (or run-config --validate). A nil err returns nil.
//
// Exit 1 is also the untyped-error default, so wrapping here is mostly
// documentary today; it keeps the validation class explicit so a future
// renumbering of the scheme touches one site rather than relying on the
// default.
func validationError(err error) error {
	if err == nil {
		return nil
	}
	return &exitError{code: exitValidation, err: err}
}

// usageError tags err as an invalid flag-combination failure (exit 4):
// a flag was supplied in a context where it has no meaning — e.g. a
// --dry-run probe gate (--no-probe-provider) or --dry-run-timeout
// without --dry-run. A nil err returns nil so call sites can wrap
// unconditionally. Distinct from validationError (exit 1, a structurally
// invalid RunConfig) because the config itself is fine; only the
// command-line combination is incoherent.
func usageError(err error) error {
	if err == nil {
		return nil
	}
	return &exitError{code: exitUsage, err: err}
}

// classifyExitCode maps a command error to its process exit code. It is
// the testable core of Execute()'s os.Exit decision: a nil error is the
// success path (0), an error that unwraps to an *exitError carries its
// code, and any other error preserves the historical default of 1 so
// no previously-unclassified failure changes its exit status.
func classifyExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return 1
}
