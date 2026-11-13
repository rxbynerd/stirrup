package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// k8sTiming bounds the k8s executor's cluster waits.
type k8sTiming struct {
	readyTimeout time.Duration
	pollPeriod   time.Duration
	// teardownTimeout bounds the Pod delete plus the wait for its
	// termination; the NetworkPolicy delete that follows gets its own
	// k8sAPITimeout.
	teardownTimeout time.Duration
}

var defaultK8sTiming = k8sTiming{
	readyTimeout:    k8sReadyTimeout,
	pollPeriod:      k8sReadyPollPeriod,
	teardownTimeout: k8sCloseTimeout,
}

// podTeardown removes a sandbox Pod and then its egress NetworkPolicy. The
// policy is deleted only once the Pod's termination is established: the
// object is gone, or the name belongs to another UID. Otherwise the policy is
// retained, because removing it could reopen egress for a running workload.
type podTeardown struct {
	clientset kubernetes.Interface
	namespace string
	podName   string
	// podUID guards the delete with a UID precondition and identifies the
	// Pod while waiting. Empty when no Pod is known to exist.
	podUID     k8stypes.UID
	policyName string
	runID      string
	logger     *slog.Logger
	timing     k8sTiming
}

// run deletes the Pod gracefully, waits for termination, and then deletes the
// policy. Every wait uses its own bounded context, independent of the run's,
// so a cancelled run still cleans up. An error names the objects retained.
func (t podTeardown) run() error {
	ctx, cancel := context.WithTimeout(context.Background(), t.timing.teardownTimeout)
	defer cancel()

	if err := t.terminatePod(ctx); err != nil {
		return t.retain(err)
	}
	return t.removePolicy()
}

// reconcileAmbiguousCreate cleans up after a Pod create whose outcome is
// unknown. A Pod found under the preselected name is torn down only when it
// carries every label this run submitted; one that does not may still be
// this run's Pod rewritten by admission, so the policy is retained.
func (t podTeardown) reconcileAmbiguousCreate(submitted map[string]string) error {
	ctx, cancel := context.WithTimeout(context.Background(), t.timing.teardownTimeout)
	defer cancel()

	pod, err := t.lookUpPod(ctx)
	if err != nil {
		return t.retain(fmt.Errorf("look up pod after an ambiguous create: %w", err))
	}
	if pod == nil {
		return t.removePolicy()
	}
	for k, v := range submitted {
		if got, ok := pod.Labels[k]; !ok || got != v {
			return t.retain(fmt.Errorf("pod %s/%s exists but does not carry this run's label %s=%s, so it is not deleted", t.namespace, t.podName, k, v))
		}
	}
	t.podUID = pod.UID
	if err := t.terminatePod(ctx); err != nil {
		return t.retain(err)
	}
	return t.removePolicy()
}

// terminatePod issues the graceful delete, retrying transient failures, and
// waits until the Pod's termination is established or ctx ends.
func (t podTeardown) terminatePod(ctx context.Context) error {
	opts := metav1.DeleteOptions{GracePeriodSeconds: ptr.To(k8sTerminationGracePeriod)}
	if t.podUID != "" {
		opts.Preconditions = &metav1.Preconditions{UID: ptr.To(t.podUID)}
	}

	var lastErr error
	for {
		err := t.clientset.CoreV1().Pods(t.namespace).Delete(ctx, t.podName, opts)
		switch {
		case err == nil, apierrors.IsNotFound(err), apierrors.IsConflict(err):
			// NotFound and Conflict (the UID precondition failing) suggest the
			// Pod is gone, but a webhook or proxy can answer either while it
			// runs, so only a read establishes termination.
			return t.waitForTermination(ctx)
		case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
			return fmt.Errorf("delete pod: %w", boundClusterErr(err))
		}
		lastErr = err
		if !sleepCtx(ctx, t.timing.pollPeriod) {
			return fmt.Errorf("delete pod: %w (gave up within the %s teardown bound)", boundClusterErr(lastErr), t.timing.teardownTimeout)
		}
	}
}

// waitForTermination polls until the Pod is NotFound or its name carries a
// different UID. Forbidden and Unauthorized stop at once; other poll errors
// are retried until ctx ends.
func (t podTeardown) waitForTermination(ctx context.Context) error {
	var lastErr error
	for {
		pod, err := t.getPod(ctx)
		switch {
		case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
			return fmt.Errorf("confirm pod termination: %w", boundClusterErr(err))
		case err != nil:
			lastErr = err
		case pod == nil:
			return nil
		case t.podUID != "" && pod.UID != t.podUID:
			return nil
		default:
			lastErr = nil
		}
		if !sleepCtx(ctx, t.timing.pollPeriod) {
			if lastErr != nil {
				return fmt.Errorf("pod termination not confirmed within the %s teardown bound: %w", t.timing.teardownTimeout, boundClusterErr(lastErr))
			}
			return fmt.Errorf("pod termination not confirmed within the %s teardown bound", t.timing.teardownTimeout)
		}
	}
}

// lookUpPod is getPod retrying transient errors until ctx ends; Forbidden and
// Unauthorized stop at once.
func (t podTeardown) lookUpPod(ctx context.Context) (*corev1.Pod, error) {
	for {
		pod, err := t.getPod(ctx)
		if err == nil || apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
			return pod, boundClusterErr(err)
		}
		if !sleepCtx(ctx, t.timing.pollPeriod) {
			return nil, boundClusterErr(err)
		}
	}
}

// getPod returns the Pod under podName, or nil when it does not exist.
func (t podTeardown) getPod(ctx context.Context) (*corev1.Pod, error) {
	pod, err := t.clientset.CoreV1().Pods(t.namespace).Get(ctx, t.podName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return pod, nil
}

// removePolicy deletes the egress NetworkPolicy; NotFound counts as success.
func (t podTeardown) removePolicy() error {
	if t.policyName == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), k8sAPITimeout)
	defer cancel()
	err := t.clientset.NetworkingV1().NetworkPolicies(t.namespace).Delete(ctx, t.policyName, metav1.DeleteOptions{})
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}
	t.logger.Warn("k8s executor networkpolicy delete failed; the policy is retained",
		"namespace", t.namespace, "pod", t.podName, "networkpolicy", t.policyName, "runID", t.runID, "err", err)
	return fmt.Errorf("delete egress NetworkPolicy %s/%s (run ID %q): %w", t.namespace, t.policyName, t.runID, boundClusterErr(err))
}

// retain logs and reports a teardown that left the policy in place.
func (t podTeardown) retain(cause error) error {
	t.logger.Warn("k8s executor could not establish sandbox pod termination; retaining its egress NetworkPolicy (fail closed)",
		"namespace", t.namespace, "pod", t.podName, "networkpolicy", t.policyName, "runID", t.runID, "err", cause)
	if t.policyName == "" {
		return fmt.Errorf("tear down pod %s/%s (run ID %q): %w", t.namespace, t.podName, t.runID, cause)
	}
	return fmt.Errorf("tear down pod %s/%s (run ID %q): %w; retained egress NetworkPolicy %s/%s",
		t.namespace, t.podName, t.runID, cause, t.namespace, t.policyName)
}

// podCreateOutcomeAmbiguous reports whether a Pod create error leaves it
// unknown whether the Pod exists. Only a client-error status the API server
// returns before persisting anything is definitive; a timeout, a transport
// failure, a server error, a throttled request (which the client may have
// retried) or an AlreadyExists under the run's own random name is not.
func podCreateOutcomeAmbiguous(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return true
	}
	code := int(status.Status().Code)
	switch code {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		return true
	}
	return code < 400 || code >= 500
}

// sleepCtx waits for d or until ctx ends, reporting whether the full wait
// elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}
