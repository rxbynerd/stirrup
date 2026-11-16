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
// image itself could not be obtained or started.
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
// platform's, and anything else as sandbox_not_ready.
func classifyK8sStartup(err error) error {
	if err == nil {
		return nil
	}
	var mismatch *admissionMismatchError
	var diagnostic *k8sRuntimeDiagnosticError
	var status apierrors.APIStatus
	switch {
	case errors.As(err, &mismatch):
		return startupErr(types.BuildFailureAdmissionMismatch, err)
	case errors.As(err, &diagnostic):
		return startupErr(types.BuildFailureRuntimeDiagnosticFailed, err)
	case errors.As(err, &status):
		return startupErr(types.BuildFailureSandboxPlatform, err)
	}
	return startupErr(types.BuildFailureSandboxNotReady, err)
}

// podImageFailure reports whether pod's agent container shows its image
// could not be obtained or started.
func podImageFailure(pod *corev1.Pod) bool {
	cs := agentContainerStatus(pod)
	if cs == nil {
		return false
	}
	if cs.State.Terminated != nil {
		return true
	}
	return cs.State.Waiting != nil && imageFailureReasons[cs.State.Waiting.Reason]
}
