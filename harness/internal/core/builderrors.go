package core

import (
	"errors"
	"fmt"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/types"
)

// BuildError is every error BuildLoop returns: the failure's class and the
// construction step in progress when it failed. Error is the wrapped
// error's text unchanged.
type BuildError struct {
	// Reason is a types.BuildFailure* value.
	Reason string
	// Step is the types.SessionStep* value in progress, empty outside the
	// named steps.
	Step string
	Err  error
}

func (e *BuildError) Error() string { return e.Err.Error() }

func (e *BuildError) Unwrap() error { return e.Err }

// Failure is the error's class as a "constructing" session phase.
func (e *BuildError) Failure() *types.BuildFailure {
	return &types.BuildFailure{
		Reason: e.Reason,
		Phase:  types.SessionPhase{Phase: types.SessionPhaseConstructing, Step: e.Step},
	}
}

// classifiedBuildError marks a construction error with its class at the
// point it is raised.
type classifiedBuildError struct {
	reason string
	err    error
}

func (e *classifiedBuildError) Error() string { return e.err.Error() }

func (e *classifiedBuildError) Unwrap() error { return e.err }

func markBuildFailure(reason string, err error) error {
	return &classifiedBuildError{reason: reason, err: err}
}

// classifyBuildError wraps a construction error in a BuildError. A host
// file failure wins over any other class, since its message is reduced to
// the field for the control plane; then the class marked where the error
// was raised; then the class an executor gave its startup failure.
func classifyBuildError(err error, step string) error {
	var built *BuildError
	if errors.As(err, &built) {
		return err
	}
	reason := types.BuildFailureUnclassified
	var hostFile *HostFileError
	var marked *classifiedBuildError
	var startup *executor.StartupError
	switch {
	case errors.As(err, &hostFile):
		reason = types.BuildFailureHostFile
	case errors.As(err, &marked):
		reason = marked.reason
	case errors.As(err, &startup):
		reason = startup.Reason
	}
	return &BuildError{Reason: reason, Step: step, Err: err}
}

// HostFileError reports a file named in the RunConfig that the harness
// could not use from its own filesystem. Error carries the detail for the
// harness log; Redacted names only the field, so a control plane cannot
// use the field to probe the harness host.
type HostFileError struct {
	// Field is the RunConfig path of the field that named the file, such
	// as "provider.replayFile" or "providers[alt].replayFile".
	Field string
	Err   error
}

func (e *HostFileError) Error() string {
	return fmt.Sprintf("%s: %v", e.Field, e.Err)
}

func (e *HostFileError) Unwrap() error { return e.Err }

// Redacted is the error text safe to send to a control plane.
func (e *HostFileError) Redacted() string {
	return fmt.Sprintf("%s unavailable on the harness host", e.Field)
}

// APIKeyRefError reports a provider apiKeyRef that did not resolve to a
// key, so a caller can point the operator at the field that names it.
type APIKeyRefError struct {
	Ref string
	Err error
}

func (e *APIKeyRefError) Error() string {
	return fmt.Sprintf("apiKeyRef %q: %v", e.Ref, e.Err)
}

func (e *APIKeyRefError) Unwrap() error { return e.Err }
