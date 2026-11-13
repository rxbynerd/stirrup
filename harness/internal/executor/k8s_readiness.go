package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// permanentWaitingReasons are agent container waiting reasons no retry can
// clear: an unparseable image reference, or imagePullPolicy Never with the
// image absent from the Pod's node. Image pull errors stay retryable.
var permanentWaitingReasons = map[string]bool{
	"InvalidImageName":  true,
	"ErrImageNeverPull": true,
}

// errPodNotReady wraps every readiness failure so callers can tell it from a
// teardown error joined onto it.
var errPodNotReady = errors.New("not ready")

// podReadiness tracks what the readiness wait last observed, for the error
// reported when the wait ends without a Ready Pod.
type podReadiness struct {
	phase      corev1.PodPhase
	scheduled  string
	waiting    string
	lastAPIErr error
	sawPod     bool
}

// waitForPodReady polls until the Pod is Running with the agent container
// Ready, re-checking the admission contract on every observation. A terminal
// state, a mismatch, a missing Pod or an RBAC denial fails at once; other API
// errors are retried until timing.readyTimeout or ctx ends. It logs when it
// starts and every timing.progressPeriod while it waits.
func waitForPodReady(ctx context.Context, clientset kubernetes.Interface, namespace, name string, admission podAdmission, timing k8sTiming, logger *slog.Logger) (*corev1.Pod, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timing.readyTimeout)
	defer cancel()

	logger.Info("k8s executor waiting for sandbox pod readiness", "namespace", namespace, "pod", name, "timeout", timing.readyTimeout)
	var seen podReadiness
	nextProgress := time.Now().Add(timing.progressPeriod)
	for {
		pod, err := clientset.CoreV1().Pods(namespace).Get(waitCtx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			seen.observe(pod)
			if err := admission.checkPoll(pod); err != nil {
				return nil, fmt.Errorf("%w: %w", errPodNotReady, err)
			}
			if reason, terminal := podTerminalReason(pod); terminal {
				return nil, fmt.Errorf("%w: %s", errPodNotReady, reason)
			}
			if podReady(pod) {
				return pod, nil
			}
		case apierrors.IsNotFound(err):
			if seen.sawPod {
				return nil, fmt.Errorf("%w: the pod was deleted during the readiness wait", errPodNotReady)
			}
			return nil, fmt.Errorf("%w: the pod was not found", errPodNotReady)
		case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
			return nil, fmt.Errorf("%w: get pod: %w", errPodNotReady, boundClusterErr(err))
		case waitCtx.Err() == nil:
			seen.lastAPIErr = err
			logger.Debug("transient error polling pod readiness, retrying", "namespace", namespace, "pod", name, "err", err)
		}

		if timing.progressPeriod > 0 && !time.Now().Before(nextProgress) {
			logger.Info("k8s executor still waiting for sandbox pod readiness", "namespace", namespace, "pod", name, "status", seen.summary())
			nextProgress = time.Now().Add(timing.progressPeriod)
		}

		if !sleepCtx(waitCtx, timing.pollPeriod) {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("%w before the run context ended: %s: %w", errPodNotReady, seen.summary(), err)
			}
			return nil, fmt.Errorf("%w within %s: %s: %w", errPodNotReady, timing.readyTimeout, seen.summary(), waitCtx.Err())
		}
	}
}

// observe records the diagnostics a timeout error reports.
func (r *podReadiness) observe(pod *corev1.Pod) {
	r.sawPod = true
	r.lastAPIErr = nil
	r.phase = pod.Status.Phase
	r.scheduled = ""
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled {
			r.scheduled = string(c.Status)
			if c.Reason != "" || c.Message != "" {
				r.scheduled += fmt.Sprintf(" (%s: %s)", clusterText(c.Reason), clusterText(c.Message))
			}
		}
	}
	if cs := agentContainerStatus(pod); cs != nil && cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
		r.waiting = clusterText(cs.State.Waiting.Reason)
		if cs.State.Waiting.Message != "" {
			r.waiting += " (" + clusterText(cs.State.Waiting.Message) + ")"
		}
	}
}

// summary renders the last observation for a timeout error.
func (r *podReadiness) summary() string {
	if !r.sawPod {
		if r.lastAPIErr != nil {
			return "the pod was never observed; last API error: " + clusterText(r.lastAPIErr.Error())
		}
		return "the pod was never observed"
	}
	parts := []string{"phase " + string(r.phase)}
	if r.phase == "" {
		parts[0] = "phase unknown"
	}
	if r.scheduled != "" {
		parts = append(parts, "PodScheduled "+r.scheduled)
	} else {
		parts = append(parts, "no PodScheduled condition")
	}
	if r.waiting != "" {
		parts = append(parts, "last waiting reason "+r.waiting)
	}
	if r.lastAPIErr != nil {
		parts = append(parts, "last API error: "+clusterText(r.lastAPIErr.Error()))
	}
	return strings.Join(parts, "; ")
}

// podReady reports whether the Pod is Running with the agent container Ready.
func podReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	cs := agentContainerStatus(pod)
	return cs != nil && cs.Ready
}

// podTerminalReason reports whether the Pod can no longer become Ready, and
// why.
func podTerminalReason(pod *corev1.Pod) (string, bool) {
	switch pod.Status.Phase {
	case corev1.PodFailed:
		return fmt.Sprintf("pod phase Failed (%s: %s)", clusterText(pod.Status.Reason), clusterText(pod.Status.Message)), true
	case corev1.PodSucceeded:
		return "pod phase Succeeded before it became ready", true
	}
	if pod.DeletionTimestamp != nil {
		return "the pod is being deleted", true
	}
	cs := agentContainerStatus(pod)
	if cs == nil {
		return "", false
	}
	if term := cs.State.Terminated; term != nil {
		return fmt.Sprintf("agent container terminated (exit code %d, %s: %s)", term.ExitCode, clusterText(term.Reason), clusterText(term.Message)), true
	}
	if w := cs.State.Waiting; w != nil && permanentWaitingReasons[w.Reason] {
		return fmt.Sprintf("agent container cannot start: %s (%s)", w.Reason, clusterText(w.Message)), true
	}
	return "", false
}

func agentContainerStatus(pod *corev1.Pod) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == k8sAgentContainer {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}

// execProbeCommand is a no-op every image under the image contract can run.
var execProbeCommand = []string{"/bin/sh", "-c", ":"}

// execBackendUnreachableText marks a pods/exec error in which the API server
// could not open a connection to the Pod's kubelet. Neither exec transport
// gives that failure a type, so the text is the only signal.
const execBackendUnreachableText = "error dialing backend"

// waitForExecReachable runs a no-op exec until the API server can reach the
// Pod's kubelet, for at most timing.execReachableTimeout. A Pod can be Ready
// on a new node before the kubelet's serving certificate is issued, and until
// then every exec fails in the API server's dial. Only that failure and an
// attempt that times out are retried; any other outcome ends the wait
// without an error, leaving the first real exec to report it.
func waitForExecReachable(ctx context.Context, core *podExecCore, timing k8sTiming, logger *slog.Logger) error {
	bound := timing.execReachableTimeout
	if bound <= 0 {
		bound = k8sExecReachableTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()

	for attempt := 1; ; attempt++ {
		attemptCtx, cancelAttempt := context.WithTimeout(waitCtx, k8sExecProbeTimeout)
		err := core.streamExec(attemptCtx, execProbeCommand, nil, io.Discard, io.Discard)
		attemptTimedOut := attemptCtx.Err() != nil
		cancelAttempt()
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("exec reachability wait interrupted: %w", ctxErr)
		}
		if !attemptTimedOut && !strings.Contains(err.Error(), execBackendUnreachableText) {
			logger.Debug("k8s executor exec probe failed; leaving the failure to the first exec", "namespace", core.namespace, "pod", core.podName, "err", clusterText(err.Error()))
			return nil
		}
		if attempt == 1 {
			logger.Info("k8s executor waiting for exec to reach the sandbox pod", "namespace", core.namespace, "pod", core.podName, "timeout", bound, "err", clusterText(err.Error()))
		}
		select {
		case <-waitCtx.Done():
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("exec reachability wait interrupted: %w", ctxErr)
			}
			return fmt.Errorf("%w within %s: %s", errExecUnreachable, bound, clusterText(err.Error()))
		case <-time.After(timing.pollPeriod):
		}
	}
}

// errExecUnreachable marks a Ready Pod whose kubelet the API server could
// not reach for exec within the bound.
var errExecUnreachable = errors.New("is not reachable for exec")
