package executor

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// k8sSandboxStateReadTimeout bounds the Pod read that decides whether a
// failed exec found the sandbox gone.
const k8sSandboxStateReadTimeout = 10 * time.Second

// SandboxLost reports the Pod's terminal state once an exec has found it
// ended; see SandboxLossReporter.
func (e *podExecCore) SandboxLost() error {
	if lost := e.lost.Load(); lost != nil {
		return lost
	}
	return nil
}

var _ SandboxLossReporter = (*podExecCore)(nil)

// sandboxLostAfter reads the Pod once after an exec failed without an exit
// status. A Pod that has ended yields a *SandboxLostError, recorded so later
// operations fail without contacting the cluster. Anything else, including
// a failed read, returns execErr unchanged: the failure may be transient.
func (e *podExecCore) sandboxLostAfter(ctx context.Context, execErr error) error {
	if e.clientset == nil {
		return execErr
	}
	readCtx, cancel := context.WithTimeout(ctx, k8sSandboxStateReadTimeout)
	defer cancel()

	var state string
	pod, err := e.clientset.CoreV1().Pods(e.namespace).Get(readCtx, e.podName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		state = fmt.Sprintf("sandbox pod %s/%s no longer exists", e.namespace, e.podName)
	case err != nil:
		e.log().Debug("k8s executor could not read the sandbox pod after a failed exec", "namespace", e.namespace, "pod", e.podName, "err", clusterText(err.Error()))
		return execErr
	default:
		var ended bool
		if state, ended = podEndedState(e.namespace, e.podName, pod); !ended {
			return execErr
		}
	}

	if e.lost.CompareAndSwap(nil, &SandboxLostError{State: state, Err: boundClusterErr(execErr)}) {
		e.log().Warn("k8s executor sandbox pod has ended; later operations fail without contacting it", "namespace", e.namespace, "pod", e.podName, "state", state)
	}
	return e.lost.Load()
}

// podEndedState reports whether the Pod can never run another exec, and
// describes its terminal state: a terminal phase, a deletion in progress,
// or an agent container that terminated and will not be restarted. The
// container's termination message is left out because the sandboxed
// workload writes it.
func podEndedState(namespace, name string, pod *corev1.Pod) (string, bool) {
	term := agentTermination(pod)
	var verb string
	switch {
	case pod.Status.Phase == corev1.PodFailed:
		verb = "has failed"
	case pod.Status.Phase == corev1.PodSucceeded:
		verb = "has exited"
	case pod.DeletionTimestamp != nil:
		verb = "is being deleted"
	case term != nil && !containerRestarts(pod.Spec.RestartPolicy, term.ExitCode):
		verb = "has stopped"
	default:
		return "", false
	}

	var details []string
	if reason := clusterText(pod.Status.Reason); reason != "" {
		if msg := clusterText(pod.Status.Message); msg != "" {
			reason += " (" + msg + ")"
		}
		details = append(details, reason)
	}
	if term != nil {
		details = append(details, describeAgentTermination(term))
	}
	state := fmt.Sprintf("sandbox pod %s/%s %s", namespace, name, verb)
	if len(details) > 0 {
		state += ": " + strings.Join(details, "; ")
	}
	return state, true
}

// agentTermination returns the agent container's terminated state, or nil
// while the container has not terminated.
func agentTermination(pod *corev1.Pod) *corev1.ContainerStateTerminated {
	cs := agentContainerStatus(pod)
	if cs == nil {
		return nil
	}
	return cs.State.Terminated
}

// containerRestarts reports whether the kubelet restarts a container that
// exited with exitCode under policy; an empty policy is the API default,
// Always.
func containerRestarts(policy corev1.RestartPolicy, exitCode int32) bool {
	switch policy {
	case corev1.RestartPolicyNever:
		return false
	case corev1.RestartPolicyOnFailure:
		return exitCode != 0
	default:
		return true
	}
}

func describeAgentTermination(term *corev1.ContainerStateTerminated) string {
	switch reason := clusterText(term.Reason); reason {
	case "":
		return fmt.Sprintf("container %s terminated (exit code %d)", k8sAgentContainer, term.ExitCode)
	case "OOMKilled":
		return fmt.Sprintf("container %s was OOMKilled (exit code %d)", k8sAgentContainer, term.ExitCode)
	default:
		return fmt.Sprintf("container %s terminated: %s (exit code %d)", k8sAgentContainer, reason, term.ExitCode)
	}
}

func (e *podExecCore) log() *slog.Logger {
	if e.logger == nil {
		return slog.Default()
	}
	return e.logger
}
