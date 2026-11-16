package executor

import "fmt"

// SandboxLostError reports that an operation failed because the executor's
// sandbox has ended for good: no later command or file operation can
// succeed. State describes the terminal state read from the sandbox's
// runtime; Err is the operation's own failure.
type SandboxLostError struct {
	State string
	Err   error
}

func (e *SandboxLostError) Error() string {
	return fmt.Sprintf("sandbox lost: %s (%v)", e.State, e.Err)
}

func (e *SandboxLostError) Unwrap() error { return e.Err }

// SandboxLossReporter is the optional capability of an executor whose
// sandbox can end during a run. SandboxLost returns the *SandboxLostError
// recorded when an operation first found the sandbox gone, and nil while
// the sandbox is usable. Once it returns an error, every later operation
// fails with that error without contacting the sandbox.
type SandboxLossReporter interface {
	SandboxLost() error
}
