package executor

import (
	"errors"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/rxbynerd/stirrup/types"
)

// StartupError classifies why a sandbox could not be started. Reason is a
// types.BuildFailure* value; Error is the wrapped error's text unchanged.
type StartupError struct {
	Reason string
	Err    error
}

func (e *StartupError) Error() string { return e.Err.Error() }

func (e *StartupError) Unwrap() error { return e.Err }

// startupErr wraps err with reason; a nil err stays nil, and an error
// already classified keeps its first reason.
func startupErr(reason string, err error) error {
	if err == nil {
		return nil
	}
	var classified *StartupError
	if errors.As(err, &classified) {
		return err
	}
	return &StartupError{Reason: reason, Err: err}
}

// imageFailureReasons are agent container waiting reasons that mean the
// image could not be obtained or its container keeps exiting.
var imageFailureReasons = map[string]bool{
	"ErrImagePull":      true,
	"ImagePullBackOff":  true,
	"InvalidImageName":  true,
	"ErrImageNeverPull": true,
	"ImageInspectError": true,
	"CrashLoopBackOff":  true,
}

// classifyK8sStartup classifies a failure of the Kubernetes executors'
// construction after the clients are built: admission and diagnostic
// failures by their type, an RBAC denial or other API rejection as the
// platform's, and anything else as otherwise. A diagnostic that could not
// run its exec is a sandbox that is not ready, not a failed isolation
// check.
func classifyK8sStartup(err error, otherwise string) error {
	if err == nil {
		return nil
	}
	var mismatch *admissionMismatchError
	var diagnostic *k8sRuntimeDiagnosticError
	var status apierrors.APIStatus
	switch {
	case errors.As(err, &mismatch):
		return startupErr(types.BuildFailureAdmissionMismatch, err)
	case errors.As(err, &diagnostic) && diagnostic.check == diagnosticCheckExec:
		return startupErr(types.BuildFailureSandboxNotReady, err)
	case errors.As(err, &diagnostic):
		return startupErr(types.BuildFailureRuntimeDiagnosticFailed, err)
	case errors.As(err, &status):
		return startupErr(types.BuildFailureSandboxPlatform, err)
	}
	return startupErr(otherwise, err)
}

// podImageFailure reports whether pod's agent container shows its image
// could not be obtained or its process exited on its own. A container the
// kernel killed for memory is not the image's failure.
func podImageFailure(pod *corev1.Pod) bool {
	cs := agentContainerStatus(pod)
	if cs == nil {
		return false
	}
	if term := cs.State.Terminated; term != nil {
		return term.Reason != "OOMKilled"
	}
	return cs.State.Waiting != nil && imageFailureReasons[cs.State.Waiting.Reason]
}
