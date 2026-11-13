package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// testAdmission returns the admission contract for a Pod created as
// readyPod(uid).
func testAdmission(uid k8stypes.UID) podAdmission {
	return podAdmission{submitted: readyPod(uid), uid: uid, checks: baseAdmissionChecks}
}

// slowReadyTiming has a ceiling long enough that a test returning quickly
// proves the wait failed fast rather than timing out.
var slowReadyTiming = k8sTiming{readyTimeout: 30 * time.Second, pollPeriod: time.Millisecond, teardownTimeout: 100 * time.Millisecond}

func waitReady(t *testing.T, cs *fake.Clientset, timing k8sTiming) (*corev1.Pod, time.Duration, error) {
	t.Helper()
	logger, _ := testLogger()
	start := time.Now()
	pod, err := waitForPodReady(context.Background(), cs, testK8sNamespace, testK8sPodName, testAdmission("uid-1"), timing, logger)
	return pod, time.Since(start), err
}

func agentWaiting(reason, message string) []corev1.ContainerStatus {
	return []corev1.ContainerStatus{{
		Name:  k8sAgentContainer,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}},
	}}
}

func TestWaitForPodReady_ReturnsReadyPod(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset(readyPod("uid-1"))
	pod, _, err := waitReady(t, cs, fastK8sTiming)
	must.NoError(err)
	must.EqualValues("uid-1", pod.UID)
}

func TestWaitForPodReady_FailsFastOnTerminalState(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(pod *corev1.Pod)
		wantErr string
	}{
		{
			name: "phase failed",
			mutate: func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodFailed
				pod.Status.Reason = "DeadlineExceeded"
				pod.Status.ContainerStatuses = nil
			},
			wantErr: "pod phase Failed (DeadlineExceeded",
		},
		{
			name: "phase succeeded before ready",
			mutate: func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodSucceeded
				pod.Status.ContainerStatuses = nil
			},
			wantErr: "pod phase Succeeded before it became ready",
		},
		{
			name: "agent container terminated",
			mutate: func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodPending
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:  k8sAgentContainer,
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 127, Reason: "Error", Message: "exec: /bin/sh: not found"}},
				}}
			},
			wantErr: "agent container terminated (exit code 127, Error: exec: /bin/sh: not found)",
		},
		{
			name: "invalid image name",
			mutate: func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodPending
				pod.Status.ContainerStatuses = agentWaiting("InvalidImageName", `couldn't parse image name "UPPER:case"`)
			},
			wantErr: "agent container cannot start: InvalidImageName",
		},
		{
			name: "image never pulled",
			mutate: func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodPending
				pod.Status.ContainerStatuses = agentWaiting("ErrImageNeverPull", "image is not present with pull policy of Never")
			},
			wantErr: "agent container cannot start: ErrImageNeverPull",
		},
		{
			name: "pod being deleted",
			mutate: func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodPending
				pod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
				pod.Finalizers = []string{"test.stirrup.dev/hold"}
			},
			wantErr: "the pod is being deleted",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			pod := readyPod("uid-1")
			tc.mutate(pod)
			cs := fake.NewClientset(pod)

			_, elapsed, err := waitReady(t, cs, slowReadyTiming)

			must.ErrorIs(err, errPodNotReady)
			must.ErrorContains(err, tc.wantErr)
			check.Less(elapsed, 5*time.Second, "a terminal state waited for the ceiling")
			check.Equal(1, countVerb(cs, "get", "pods"), "a terminal state was polled again")
		})
	}
}

func TestWaitForPodReady_RetriesImagePullErrors(t *testing.T) {
	must := require.New(t)
	pulling := readyPod("uid-1")
	pulling.Status.Phase = corev1.PodPending
	pulling.Status.ContainerStatuses = agentWaiting("ErrImagePull", "registry unavailable")
	backoff := pulling.DeepCopy()
	backoff.Status.ContainerStatuses = agentWaiting("ImagePullBackOff", "Back-off pulling image")
	sequence := []*corev1.Pod{pulling, backoff, readyPod("uid-1")}

	cs := fake.NewClientset()
	polls := 0
	cs.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		pod := sequence[min(polls, len(sequence)-1)]
		polls++
		return true, pod.DeepCopy(), nil
	})

	_, _, err := waitReady(t, cs, fastK8sTiming)
	must.NoError(err)
	must.Equal(3, polls)
}

func TestWaitForPodReady_RetriesTransientAPIErrors(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset(readyPod("uid-1"))
	transient := []error{
		errors.New("read: connection reset by peer"),
		apierrors.NewServerTimeout(podsResource.GroupResource(), "get", 1),
		apierrors.NewTooManyRequests("slow down", 0),
		apierrors.NewInternalError(errors.New("etcd leader changed")),
	}
	failures := 0
	cs.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		if failures < len(transient) {
			failures++
			return true, nil, transient[failures-1]
		}
		return false, nil, nil
	})

	pod, _, err := waitReady(t, cs, fastK8sTiming)
	must.NoError(err)
	must.NotNil(pod)
	must.Equal(len(transient)+1, countVerb(cs, "get", "pods"))
}

func TestWaitForPodReady_FailsFastOnRBACDenial(t *testing.T) {
	cases := map[string]error{
		"forbidden":    errForbiddenPods,
		"unauthorized": apierrors.NewUnauthorized("token expired"),
	}
	for name, denial := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			cs := fake.NewClientset(readyPod("uid-1"))
			failVerb(cs, "get", "pods", denial)

			_, elapsed, err := waitReady(t, cs, slowReadyTiming)

			must.ErrorIs(err, errPodNotReady)
			must.ErrorIs(err, denial)
			check.Less(elapsed, 5*time.Second)
			check.Equal(1, countVerb(cs, "get", "pods"))
		})
	}
}

func TestWaitForPodReady_FailsWhenPodDeleted(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset()
	_, elapsed, err := waitReady(t, cs, slowReadyTiming)
	must.ErrorContains(err, "the pod was deleted during the readiness wait")
	must.Less(elapsed, 5*time.Second)
}

func TestWaitForPodReady_TimeoutReportsLastObservation(t *testing.T) {
	must := require.New(t)
	pod := readyPod("uid-1")
	pod.Status.Phase = corev1.PodPending
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:    corev1.PodScheduled,
		Status:  corev1.ConditionFalse,
		Reason:  "Unschedulable",
		Message: "0/3 nodes are available: 3 Insufficient cpu.",
	}}
	pod.Status.ContainerStatuses = agentWaiting("ImagePullBackOff", "Back-off pulling image \"registry.invalid/sandbox:1\"")
	cs := fake.NewClientset(pod)

	_, _, err := waitReady(t, cs, fastK8sTiming)

	must.ErrorIs(err, errPodNotReady)
	must.ErrorIs(err, context.DeadlineExceeded)
	must.ErrorContains(err, "within 200ms")
	must.ErrorContains(err, "phase Pending")
	must.ErrorContains(err, "PodScheduled False (Unschedulable: 0/3 nodes are available: 3 Insufficient cpu.)")
	must.ErrorContains(err, `last waiting reason ImagePullBackOff (Back-off pulling image "registry.invalid/sandbox:1")`)
}

func TestWaitForPodReady_TimeoutBoundsClusterText(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	pod := readyPod("uid-1")
	pod.Status.Phase = corev1.PodPending
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:    corev1.PodScheduled,
		Status:  corev1.ConditionFalse,
		Reason:  "Unschedulable",
		Message: "line one\nline two " + strings.Repeat("x", 4*k8sClusterTextMax),
	}}
	cs := fake.NewClientset(pod)

	_, _, err := waitReady(t, cs, fastK8sTiming)

	must.Error(err)
	check.NotContains(err.Error(), "\n", "a cluster-supplied newline reached the error")
	check.Contains(err.Error(), "line oneline two")
	check.Contains(err.Error(), "…")
	check.Less(len(err.Error()), 2*k8sClusterTextMax+300)
}

func TestWaitForPodReady_TimeoutReportsLastAPIError(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset()
	failVerb(cs, "get", "pods", errors.New("dial tcp 10.0.0.1:443: i/o timeout"))

	_, _, err := waitReady(t, cs, fastK8sTiming)
	must.ErrorContains(err, "the pod was never observed; last API error: dial tcp 10.0.0.1:443: i/o timeout")
}

func TestWaitForPodReady_CallerCancellationEndsWait(t *testing.T) {
	must := require.New(t)
	pending := readyPod("uid-1")
	pending.Status.Phase = corev1.PodPending
	cs := fake.NewClientset(pending)
	ctx, cancel := context.WithCancel(context.Background())
	cs.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		cancel()
		return false, nil, nil
	})

	logger, _ := testLogger()
	_, err := waitForPodReady(ctx, cs, testK8sNamespace, testK8sPodName, testAdmission("uid-1"), slowReadyTiming, logger)
	must.ErrorIs(err, context.Canceled)
	must.ErrorContains(err, "before the run context ended")
}

func TestWaitForPodReady_RechecksAdmission(t *testing.T) {
	t.Run("uid changed since create", func(t *testing.T) {
		must := require.New(t)
		cs := fake.NewClientset(readyPod("uid-2"))
		_, _, err := waitReady(t, cs, slowReadyTiming)
		must.ErrorIs(err, errPodNotReady)
		must.ErrorContains(err, `metadata.uid: expected uid-1, observed uid-2`)
	})

	t.Run("selector label rewritten after create", func(t *testing.T) {
		must := require.New(t)
		pod := readyPod("uid-1")
		pod.Labels[k8sPodNameLabel] = "someone-else"
		cs := fake.NewClientset(pod)
		_, _, err := waitReady(t, cs, slowReadyTiming)
		must.ErrorContains(err, `metadata.labels[stirrup.dev/pod]: expected "`+testK8sPodName+`", observed "someone-else"`)
	})
}
