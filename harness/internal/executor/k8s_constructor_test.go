package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/rxbynerd/stirrup/types"
)

// constructorCfg returns a k8s executor config for the fake-cluster
// constructor tests.
func constructorCfg(runtimeClass string) K8sExecutorConfig {
	return K8sExecutorConfig{
		Image:            "busybox",
		Namespace:        testK8sNamespace,
		RuntimeClassName: runtimeClass,
		Network:          &types.NetworkConfig{Mode: "none"},
		Lifecycle:        Lifecycle{RunID: testK8sRunID},
	}
}

// admitPods makes the fake API server assign a UID to every created Pod, let
// mutate rewrite it as admission might, and report it Running and Ready.
func admitPods(cs *fake.Clientset, mutate func(pod *corev1.Pod)) {
	cs.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pod := action.(clienttesting.CreateAction).GetObject().(*corev1.Pod)
		pod.UID = "uid-1"
		pod.Status = readyPod("uid-1").Status
		if mutate != nil {
			mutate(pod)
		}
		return false, nil, nil
	})
}

// createdPodName returns the name of the Pod the constructor submitted.
func createdPodName(t *testing.T, cs *fake.Clientset) string {
	t.Helper()
	for _, a := range actionsOf(cs, "pods") {
		if c, ok := a.(clienttesting.CreateAction); ok {
			return c.GetObject().(*corev1.Pod).Name
		}
	}
	require.New(t).FailNow("no pod create recorded")
	return ""
}

func newFakeK8sExecutor(ctx context.Context, cs *fake.Clientset, cfg K8sExecutorConfig) (*K8sExecutor, error) {
	return newK8sExecutor(ctx, cfg, nil, cs, nil, fastK8sTiming)
}

// requireTornDown asserts the constructor deleted the Pod gracefully under
// its UID and only then removed the policy.
func requireTornDown(t *testing.T, cs *fake.Clientset) {
	t.Helper()
	must := require.New(t)
	podName := createdPodName(t, cs)
	deletes := podDeletes(cs)
	must.NotEmpty(deletes, "pod was not deleted")
	must.Equal(ptr.To(k8sDeleteGracePeriod), deletes[0].GracePeriodSeconds)
	must.NotNil(deletes[0].Preconditions)
	must.Equal(ptr.To(k8stypes.UID("uid-1")), deletes[0].Preconditions.UID)
	must.False(trackerHas(t, cs, podsResource, podName), "pod left behind")
	must.False(trackerHas(t, cs, networkPoliciesResource, networkPolicyName(podName)), "policy left behind")

	var podDeleteAt, policyDeleteAt int
	for i, a := range cs.Actions() {
		if a.GetVerb() != "delete" {
			continue
		}
		switch a.GetResource().Resource {
		case "pods":
			podDeleteAt = i
		case "networkpolicies":
			policyDeleteAt = i
		}
	}
	must.Greater(policyDeleteAt, podDeleteAt, "policy deleted before the pod")
}

func TestNewK8sExecutor_BindsAdmittedReadyPod(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset()
	admitPods(cs, nil)

	exec, err := newFakeK8sExecutor(context.Background(), cs, constructorCfg("gvisor"))
	must.NoError(err)
	must.EqualValues("uid-1", exec.podUID)
	must.Equal(createdPodName(t, cs), exec.podName)
	must.Equal(networkPolicyName(exec.podName), exec.networkPolicyName)

	must.NoError(exec.Close())
	requireTornDown(t, cs)
}

func TestNewK8sExecutor_CleansUpAfterCallerCancellation(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset()
	admitPods(cs, func(pod *corev1.Pod) { pod.Status.Phase = corev1.PodPending })
	ctx, cancel := context.WithCancel(context.Background())
	cs.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		cancel()
		return false, nil, nil
	})

	_, err := newFakeK8sExecutor(ctx, cs, constructorCfg("gvisor"))

	must.ErrorIs(err, context.Canceled)
	requireTornDown(t, cs)
}

func TestNewK8sExecutor_ReportsRetainedPolicyOnRollbackFailure(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cs := fake.NewClientset()
	admitPods(cs, func(pod *corev1.Pod) { pod.Status.Phase = corev1.PodPending })
	ignorePodDeletes(cs)
	ctx, cancel := context.WithCancel(context.Background())
	cs.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		cancel()
		return false, nil, nil
	})

	_, err := newFakeK8sExecutor(ctx, cs, constructorCfg("gvisor"))

	must.ErrorIs(err, context.Canceled, "the readiness failure stays the primary error")
	podName := createdPodName(t, cs)
	must.ErrorContains(err, "retained egress NetworkPolicy "+testK8sNamespace+"/"+networkPolicyName(podName))
	check.True(trackerHas(t, cs, networkPoliciesResource, networkPolicyName(podName)))
	check.Zero(countVerb(cs, "delete", "networkpolicies"))
}

func TestNewK8sExecutor_DefinitiveCreateRejectionRemovesPolicy(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cs := fake.NewClientset()
	quota := apierrors.NewForbidden(podsResource.GroupResource(), "stirrup-x", errors.New(`exceeded quota: compute, requested: limits.cpu=2, used: limits.cpu=8, limited: limits.cpu=8`))
	failVerb(cs, "create", "pods", quota)

	_, err := newFakeK8sExecutor(context.Background(), cs, constructorCfg("gvisor"))

	must.ErrorIs(err, quota)
	check.NotContains(err.Error(), "kubectl get runtimeclass", "a quota rejection was blamed on the RuntimeClass")
	check.Empty(podDeletes(cs), "no pod exists after a definitive rejection")
	check.Equal(1, countVerb(cs, "delete", "networkpolicies"))
	check.False(trackerHas(t, cs, networkPoliciesResource, networkPolicyName(createdPodName(t, cs))))
}

func TestNewK8sExecutor_ReconcilesAmbiguousCreate(t *testing.T) {
	t.Run("pod created despite the error", func(t *testing.T) {
		must := require.New(t)
		cs := fake.NewClientset()
		cs.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
			pod := action.(clienttesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
			pod.UID = "uid-1"
			must.NoError(cs.Tracker().Add(pod))
			return true, nil, apierrors.NewServerTimeout(podsResource.GroupResource(), "create", 1)
		})

		_, err := newFakeK8sExecutor(context.Background(), cs, constructorCfg("gvisor"))

		must.True(apierrors.IsServerTimeout(err))
		requireTornDown(t, cs)
	})

	t.Run("pod absent", func(t *testing.T) {
		must := require.New(t)
		cs := fake.NewClientset()
		failVerb(cs, "create", "pods", errors.New("net/http: request canceled (Client.Timeout exceeded while awaiting headers)"))

		_, err := newFakeK8sExecutor(context.Background(), cs, constructorCfg("gvisor"))

		must.ErrorContains(err, "Client.Timeout exceeded")
		must.Empty(podDeletes(cs))
		must.Equal(1, countVerb(cs, "get", "pods"), "the ambiguous create was not reconciled")
		must.False(trackerHas(t, cs, networkPoliciesResource, networkPolicyName(createdPodName(t, cs))))
	})
}

func TestK8sExecutor_CloseRetainsPolicyAndNamesIt(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cs := fake.NewClientset(readyPod("uid-1"), testPolicy())
	ignorePodDeletes(cs)
	logger, logs := testLogger()
	exec := &K8sExecutor{
		podExecCore:       podExecCore{clientset: cs, namespace: testK8sNamespace, podName: testK8sPodName, logger: logger},
		podUID:            "uid-1",
		networkPolicyName: networkPolicyName(testK8sPodName),
		runID:             testK8sRunID,
		timing:            fastK8sTiming,
	}

	err := exec.Close()

	must.Error(err)
	must.ErrorContains(err, "tear down pod "+testK8sNamespace+"/"+testK8sPodName)
	must.ErrorContains(err, "retained egress NetworkPolicy "+testK8sNamespace+"/"+networkPolicyName(testK8sPodName))
	check.Contains(logs.String(), "retaining its egress NetworkPolicy")
	check.True(trackerHas(t, cs, networkPoliciesResource, networkPolicyName(testK8sPodName)))
}
