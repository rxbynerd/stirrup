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

const (
	// k8sSandboxStateReadTimeout bounds the Pod reads that decide whether
	// a failed exec found the sandbox gone.
	k8sSandboxStateReadTimeout = 10 * time.Second
	// k8sSandboxLossSettle is how long a Pod that still looks usable is
	// re-read after a failed exec. The kubelet reports a killed container
	// about a second after the runtime has removed it, and the Pod's
	// terminal phase about a second after that.
	k8sSandboxLossSettle     = 5 * time.Second
	k8sSandboxLossPollPeriod = time.Second
)

// SandboxLost reports the Pod's terminal state once an exec has found it
// ended; see SandboxLossReporter.
func (e *podExecCore) SandboxLost() error {
	if lost := e.lost.Load(); lost != nil {
		return lost
	}
	return nil
}

var (
	_ SandboxLossReporter = (*K8sExecutor)(nil)
	_ SandboxLossReporter = (*AgentSandboxExecutor)(nil)
)

// sandboxLostAfter reads the Pod after an exec failed without an exit
// status, re-reading a Pod that still looks usable until the settle window
// ends. A Pod that has ended yields a *SandboxLostError, recorded so later
// operations fail without contacting the cluster. Anything else, including
// a failed read, returns execErr unchanged: the failure may be transient.
func (e *podExecCore) sandboxLostAfter(ctx context.Context, execErr error) error {
	if e.clientset == nil {
		return execErr
	}
	readCtx, cancel := context.WithTimeout(ctx, k8sSandboxStateReadTimeout)
	defer cancel()

	settle := e.lossSettle
	if settle <= 0 {
		settle = k8sSandboxLossSettle
	}
	// The window ends a poll period before the operation's own deadline,
	// so the wait never turns the exec error into a timeout.
	settled := time.Now().Add(settle)
	if deadline, ok := ctx.Deadline(); ok && deadline.Add(-k8sSandboxLossPollPeriod).Before(settled) {
		settled = deadline.Add(-k8sSandboxLossPollPeriod)
	}
	for {
		state, ended, err := e.readEndedState(readCtx)
		if err != nil {
			e.log().Debug("k8s executor could not read the sandbox pod after a failed exec", "namespace", e.namespace, "pod", e.podName, "err", clusterText(err.Error()))
			return execErr
		}
		if ended {
			if e.lost.CompareAndSwap(nil, &SandboxLostError{State: state, Err: boundClusterErr(execErr)}) {
				e.log().Warn("k8s executor sandbox pod has ended; later operations fail without contacting it", "namespace", e.namespace, "pod", e.podName, "state", state)
			}
			return e.lost.Load()
		}
		if !time.Now().Before(settled) || !sleepCtx(readCtx, min(k8sSandboxLossPollPeriod, time.Until(settled))) {
			return execErr
		}
	}
}

// readEndedState reads the Pod and reports whether it has ended, and how; a
// Pod that no longer exists has ended.
func (e *podExecCore) readEndedState(ctx context.Context) (string, bool, error) {
	pod, err := e.clientset.CoreV1().Pods(e.namespace).Get(ctx, e.podName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Sprintf("sandbox pod %s/%s no longer exists", e.namespace, e.podName), true, nil
	case err != nil:
		return "", false, err
	case e.podUID != "" && pod.UID != e.podUID:
		return fmt.Sprintf("sandbox pod %s/%s was replaced by another pod with the same name", e.namespace, e.podName), true, nil
	}
	state, ended := podEndedState(e.namespace, e.podName, pod)
	return state, ended, nil
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
