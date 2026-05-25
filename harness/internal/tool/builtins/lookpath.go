package builtins

import "os/exec"

// execLookPath is a thin alias for os/exec.LookPath so the search.go probe
// can be stubbed in tests via the `lookPath` package variable without
// importing os/exec into the test file (which would force every test in
// the package to deal with PATH state).
func execLookPath(name string) (string, error) {
	return exec.LookPath(name)
}
