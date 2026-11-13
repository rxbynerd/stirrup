package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestPodTeardown_RemovesPolicyOnlyAfterPodTerminated(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cs := fake.NewClientset(readyPod("uid-1"), testPolicy())
	ignorePodDeletes(cs)

	// The kubelet "confirms" termination on the third poll after the delete.
	polls := 0
	cs.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		polls++
		if polls == 3 {
			must.NoError(cs.Tracker().Delete(podsResource, testK8sNamespace, testK8sPodName))
		}
		return false, nil, nil
	})
	podPresentAtPolicyDelete := true
	cs.PrependReactor("delete", "networkpolicies", func(clienttesting.Action) (bool, runtime.Object, error) {
		podPresentAtPolicyDelete = trackerHas(t, cs, podsResource, testK8sPodName)
		return false, nil, nil
	})

	logger, _ := testLogger()
	must.NoError(testTeardown(cs, "uid-1", logger).run())

	check.False(podPresentAtPolicyDelete, "policy deleted while the pod still existed")
	check.GreaterOrEqual(polls, 3)
	check.False(trackerHas(t, cs, networkPoliciesResource, networkPolicyName(testK8sPodName)), "policy left behind")
}

func TestPodTeardown_DeletesGracefullyWithUIDPrecondition(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset(readyPod("uid-1"), testPolicy())

	logger, _ := testLogger()
	must.NoError(testTeardown(cs, "uid-1", logger).run())

	deletes := podDeletes(cs)
	must.Len(deletes, 1)
	must.NotNil(deletes[0].GracePeriodSeconds)
	must.Equal(k8sTerminationGracePeriod, *deletes[0].GracePeriodSeconds)
	must.Positive(*deletes[0].GracePeriodSeconds, "a zero grace period removes the object before the kubelet confirms termination")
	must.NotNil(deletes[0].Preconditions)
	must.NotNil(deletes[0].Preconditions.UID)
	must.EqualValues("uid-1", *deletes[0].Preconditions.UID)
}

func TestPodTeardown_RetainsPolicy(t *testing.T) {
	policyName := networkPolicyName(testK8sPodName)

	cases := []struct {
		name    string
		arrange func(cs *fake.Clientset)
		wantErr string
	}{
		{
			name:    "delete forbidden",
			arrange: func(cs *fake.Clientset) { failVerb(cs, "delete", "pods", errForbiddenPods) },
			wantErr: "denied by RBAC",
		},
		{
			name: "delete keeps failing",
			arrange: func(cs *fake.Clientset) {
				failVerb(cs, "delete", "pods", errors.New("connection refused"))
			},
			wantErr: "connection refused",
		},
		{
			name:    "termination not confirmed",
			arrange: ignorePodDeletes,
			wantErr: "pod termination not confirmed",
		},
		{
			name: "termination polls fail",
			arrange: func(cs *fake.Clientset) {
				ignorePodDeletes(cs)
				failVerb(cs, "get", "pods", apierrors.NewServiceUnavailable("apiserver overloaded"))
			},
			wantErr: "apiserver overloaded",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			cs := fake.NewClientset(readyPod("uid-1"), testPolicy())
			tc.arrange(cs)
			logger, logs := testLogger()

			err := testTeardown(cs, "uid-1", logger).run()

			must.Error(err)
			must.ErrorContains(err, tc.wantErr)
			must.ErrorContains(err, "retained egress NetworkPolicy "+testK8sNamespace+"/"+policyName)
			must.ErrorContains(err, testK8sRunID)
			check.True(trackerHas(t, cs, networkPoliciesResource, policyName), "policy removed despite unconfirmed termination")
			check.Zero(countVerb(cs, "delete", "networkpolicies"), "policy delete attempted")
			for _, want := range []string{"level=WARN", "namespace=" + testK8sNamespace, "pod=" + testK8sPodName, "networkpolicy=" + policyName, "runID=" + testK8sRunID} {
				check.Contains(logs.String(), want)
			}
		})
	}
}

func TestPodTeardown_RetriesTransientDeleteFailure(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset(readyPod("uid-1"), testPolicy())
	failures := 0
	cs.PrependReactor("delete", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		if failures < 2 {
			failures++
			return true, nil, apierrors.NewTooManyRequests("slow down", 0)
		}
		return false, nil, nil
	})

	logger, _ := testLogger()
	must.NoError(testTeardown(cs, "uid-1", logger).run())
	must.Equal(3, countVerb(cs, "delete", "pods"))
	must.False(trackerHas(t, cs, networkPoliciesResource, networkPolicyName(testK8sPodName)))
}

func TestPodTeardown_TreatsReplacedPodAsTerminated(t *testing.T) {
	t.Run("uid precondition conflict", func(t *testing.T) {
		must := require.New(t)
		cs := fake.NewClientset(readyPod("uid-2"), testPolicy())
		failVerb(cs, "delete", "pods", apierrors.NewConflict(podsResource.GroupResource(), testK8sPodName, errors.New("precondition failed: UID mismatch")))

		logger, _ := testLogger()
		must.NoError(testTeardown(cs, "uid-1", logger).run())
		must.True(trackerHas(t, cs, podsResource, testK8sPodName), "another run's pod was deleted")
		must.False(trackerHas(t, cs, networkPoliciesResource, networkPolicyName(testK8sPodName)))
	})

	t.Run("name reused while waiting", func(t *testing.T) {
		must := require.New(t)
		cs := fake.NewClientset(readyPod("uid-1"), testPolicy())
		cs.PrependReactor("delete", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
			must.NoError(cs.Tracker().Update(podsResource, readyPod("uid-2"), testK8sNamespace))
			return true, nil, nil
		})

		logger, _ := testLogger()
		must.NoError(testTeardown(cs, "uid-1", logger).run())
		must.False(trackerHas(t, cs, networkPoliciesResource, networkPolicyName(testK8sPodName)))
	})
}

func TestPodTeardown_PodAlreadyGoneIsIdempotent(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset()
	logger, _ := testLogger()
	td := testTeardown(cs, "uid-1", logger)
	must.NoError(td.run())
	must.NoError(td.run())
}

func TestPodTeardown_ReturnsPolicyDeleteFailure(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset(readyPod("uid-1"), testPolicy())
	failVerb(cs, "delete", "networkpolicies", errors.New("etcd unavailable"))

	logger, _ := testLogger()
	err := testTeardown(cs, "uid-1", logger).run()
	must.ErrorContains(err, "delete egress NetworkPolicy "+testK8sNamespace+"/"+networkPolicyName(testK8sPodName))
	must.ErrorContains(err, "etcd unavailable")
}

func TestPodTeardown_ReconcileAmbiguousCreate(t *testing.T) {
	policyName := networkPolicyName(testK8sPodName)
	ours := podLabels(testK8sPodName, testK8sRunID)

	t.Run("pod found and ours", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		cs := fake.NewClientset(readyPod("uid-7"), testPolicy())

		logger, _ := testLogger()
		must.NoError(testTeardown(cs, "", logger).reconcileAmbiguousCreate(ours))

		deletes := podDeletes(cs)
		must.Len(deletes, 1)
		must.NotNil(deletes[0].Preconditions)
		check.EqualValues("uid-7", *deletes[0].Preconditions.UID, "delete must be pinned to the UID found")
		check.False(trackerHas(t, cs, podsResource, testK8sPodName))
		check.False(trackerHas(t, cs, networkPoliciesResource, policyName))
	})

	t.Run("pod found and not ours", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		foreign := readyPod("uid-9")
		foreign.Labels = map[string]string{"app": "something-else"}
		cs := fake.NewClientset(foreign, testPolicy())

		logger, _ := testLogger()
		err := testTeardown(cs, "", logger).reconcileAmbiguousCreate(ours)

		must.Error(err)
		must.ErrorContains(err, "does not carry this run's label")
		must.ErrorContains(err, "retained egress NetworkPolicy")
		check.Empty(podDeletes(cs), "a pod this run cannot prove it owns was deleted")
		check.True(trackerHas(t, cs, networkPoliciesResource, policyName))
	})

	t.Run("pod absent", func(t *testing.T) {
		must := require.New(t)
		cs := fake.NewClientset(testPolicy())

		logger, _ := testLogger()
		must.NoError(testTeardown(cs, "", logger).reconcileAmbiguousCreate(ours))
		must.Empty(podDeletes(cs))
		must.False(trackerHas(t, cs, networkPoliciesResource, policyName))
	})

	t.Run("lookup retried after a transient error", func(t *testing.T) {
		must := require.New(t)
		cs := fake.NewClientset(readyPod("uid-7"), testPolicy())
		failures := 0
		cs.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
			if failures == 0 {
				failures++
				return true, nil, errors.New("connection reset")
			}
			return false, nil, nil
		})

		logger, _ := testLogger()
		must.NoError(testTeardown(cs, "", logger).reconcileAmbiguousCreate(ours))
		must.Len(podDeletes(cs), 1)
		must.False(trackerHas(t, cs, networkPoliciesResource, policyName))
	})

	t.Run("lookup keeps failing", func(t *testing.T) {
		must := require.New(t)
		cs := fake.NewClientset(testPolicy())
		failVerb(cs, "get", "pods", errors.New("connection reset"))

		logger, _ := testLogger()
		err := testTeardown(cs, "", logger).reconcileAmbiguousCreate(ours)
		must.ErrorContains(err, "connection reset")
		must.True(trackerHas(t, cs, networkPoliciesResource, policyName))
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

func TestPodCreateOutcomeAmbiguous_ClassifiesErrors(t *testing.T) {
	gr := schema.GroupResource{Resource: "pods"}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, testK8sPodName, nil), false},
		{"forbidden", apierrors.NewForbidden(gr, testK8sPodName, errors.New("exceeded quota")), false},
		{"bad request", apierrors.NewBadRequest("malformed"), false},
		{"namespace not found", apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, testK8sNamespace), false},
		{"already exists", apierrors.NewAlreadyExists(gr, testK8sPodName), true},
		{"too many requests", apierrors.NewTooManyRequests("slow down", 1), true},
		{"request timeout", apierrors.NewTimeoutError("timed out", 1), true},
		{"server timeout", apierrors.NewServerTimeout(gr, "create", 1), true},
		{"internal error", apierrors.NewInternalError(errors.New("webhook failed")), true},
		{"service unavailable", apierrors.NewServiceUnavailable("unavailable"), true},
		{"transport error", errors.New("dial tcp: connection refused"), true},
		{"context deadline", context.DeadlineExceeded, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, podCreateOutcomeAmbiguous(tc.err))
		})
	}
}
