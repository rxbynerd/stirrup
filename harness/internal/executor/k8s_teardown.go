package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
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
	// progressPeriod spaces the readiness wait's progress log lines; zero
	// disables them.
	progressPeriod time.Duration
}

var defaultK8sTiming = k8sTiming{
	readyTimeout:    k8sReadyTimeout,
	pollPeriod:      k8sReadyPollPeriod,
	teardownTimeout: k8sCloseTimeout,
	progressPeriod:  k8sReadyProgressPeriod,
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

// reconcileFailedCreate cleans up after a Pod create that returned an error,
// whatever the error: a client may have retried a request whose first
// attempt was persisted. The Pod is looked up under its preselected name and
// torn down only when it carries every label this run submitted; one that
// does not may be this run's Pod rewritten by admission, so the policy is
// retained. With no Pod found the policy is removed only after a definitive
// rejection; after any other error a late-persisted create would start the
// Pod, so the policy is retained.
func (t podTeardown) reconcileFailedCreate(submitted map[string]string, createErr error) error {
	ctx, cancel := context.WithTimeout(context.Background(), t.timing.teardownTimeout)
	defer cancel()

	pod, err := t.lookUpPod(ctx)
	if err != nil {
		return t.retain(fmt.Errorf("look up pod after a failed create: %w", err))
	}
	if pod == nil {
		if podCreateRejectedDefinitively(createErr) {
			return t.removePolicy()
		}
		return t.retain(fmt.Errorf("pod %s/%s was not found after a create whose outcome is unknown; a create persisted later starts under the retained policy", t.namespace, t.podName))
	}
	keys := make([]string, 0, len(submitted))
	for k := range submitted {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if got, ok := pod.Labels[k]; !ok || got != submitted[k] {
			return t.retain(fmt.Errorf("pod %s/%s exists but does not carry this run's label %s=%s, so it is not deleted", t.namespace, t.podName, k, submitted[k]))
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
func (t podTeardown) removePolicy() error { return t.egress().removePolicy() }

// retain logs and reports a teardown that left the policy in place.
func (t podTeardown) retain(cause error) error { return t.egress().retain(cause) }

func (t podTeardown) egress() sandboxEgress {
	return sandboxEgress{
		clientset:  t.clientset,
		kind:       "pod",
		namespace:  t.namespace,
		name:       t.podName,
		policyName: t.policyName,
		runID:      t.runID,
		logger:     t.logger,
	}
}

// sandboxEgress removes a sandbox's egress NetworkPolicy and reports its
// retention in the one warning and error form both Kubernetes executors use.
type sandboxEgress struct {
	clientset kubernetes.Interface
	// kind names the object torn down ahead of the policy: "pod" or
	// "sandbox". name is that object's name, which is also the Pod name on
	// both executors' cold-create path.
	kind       string
	namespace  string
	name       string
	policyName string
	runID      string
	logger     *slog.Logger
}

// removePolicy makes one delete attempt under k8sAPITimeout; NotFound counts
// as success and a failure is reported through retain.
func (e sandboxEgress) removePolicy() error {
	if e.policyName == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), k8sAPITimeout)
	defer cancel()
	err := e.clientset.NetworkingV1().NetworkPolicies(e.namespace).Delete(ctx, e.policyName, metav1.DeleteOptions{})
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}
	return e.retain(fmt.Errorf("delete egress NetworkPolicy: %w", boundClusterErr(err)))
}

// retain logs a warning and returns an error naming the retained policy.
// cause carries only bounded cluster text.
func (e sandboxEgress) retain(cause error) error {
	e.logger.Warn("sandbox teardown incomplete; retaining the egress NetworkPolicy (fail closed)",
		"namespace", e.namespace, "pod", e.name, "networkpolicy", e.policyName, "runId", e.runID, "err", cause)
	if e.policyName == "" {
		return fmt.Errorf("tear down %s %s/%s (run ID %q): %w", e.kind, e.namespace, e.name, e.runID, cause)
	}
	return fmt.Errorf("tear down %s %s/%s (run ID %q): %w; retained egress NetworkPolicy %s/%s",
		e.kind, e.namespace, e.name, e.runID, cause, e.namespace, e.policyName)
}

// podCreateRejectedDefinitively reports whether a Pod create error is a
// client-error status the API server returns before persisting anything.
// A timeout, a transport failure, a server error, a throttled request (which
// the client may have retried) and a 409 (AlreadyExists under the run's own
// random name, from such a retry) are not.
func podCreateRejectedDefinitively(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	code := int(status.Status().Code)
	switch code {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		return false
	}
	return code >= 400 && code < 500
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
