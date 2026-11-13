package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	statusError := func(code int32) error {
		return &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: code}}
	}

	cases := []struct {
		name    string
		arrange func(t *testing.T, cs *fake.Clientset)
		wantErr []string
		// wantDeletes is the exact number of Pod deletes; zero skips the check.
		wantDeletes int
		// wantReads is the exact number of Pod reads; zero skips the check.
		wantReads int
		// failsFast cases run under a long bound they must not wait out.
		failsFast bool
	}{
		{
			name:        "delete forbidden",
			arrange:     func(_ *testing.T, cs *fake.Clientset) { failVerb(cs, "delete", "pods", errForbiddenPods) },
			wantErr:     []string{"delete pod", "denied by RBAC"},
			wantDeletes: 1,
			failsFast:   true,
		},
		{
			name: "delete unauthorized",
			arrange: func(_ *testing.T, cs *fake.Clientset) {
				failVerb(cs, "delete", "pods", apierrors.NewUnauthorized("token expired"))
			},
			wantErr:     []string{"delete pod", "token expired"},
			wantDeletes: 1,
			failsFast:   true,
		},
		{
			name: "delete keeps failing",
			arrange: func(_ *testing.T, cs *fake.Clientset) {
				failVerb(cs, "delete", "pods", errors.New("connection refused"))
			},
			wantErr: []string{"connection refused", "within the 100ms teardown bound"},
		},
		{
			name:        "termination not confirmed",
			arrange:     func(_ *testing.T, cs *fake.Clientset) { ignorePodDeletes(cs) },
			wantErr:     []string{"pod termination not confirmed within the 100ms teardown bound"},
			wantDeletes: 1,
		},
		{
			name: "termination polls fail",
			arrange: func(_ *testing.T, cs *fake.Clientset) {
				ignorePodDeletes(cs)
				failVerb(cs, "get", "pods", apierrors.NewServiceUnavailable("apiserver overloaded"))
			},
			wantErr:     []string{"pod termination not confirmed within the 100ms teardown bound", "apiserver overloaded"},
			wantDeletes: 1,
		},
		{
			name: "termination read forbidden",
			arrange: func(_ *testing.T, cs *fake.Clientset) {
				ignorePodDeletes(cs)
				failVerb(cs, "get", "pods", errForbiddenPods)
			},
			wantErr:     []string{"confirm pod termination", "denied by RBAC"},
			wantDeletes: 1,
			wantReads:   1,
			failsFast:   true,
		},
		{
			// apimachinery reads any 409 with an unknown reason as Conflict.
			name: "delete answers 409 while the pod remains",
			arrange: func(_ *testing.T, cs *fake.Clientset) {
				failVerb(cs, "delete", "pods", statusError(409))
			},
			wantErr:     []string{"pod termination not confirmed"},
			wantDeletes: 1,
		},
		{
			name: "delete answers 404 while the pod remains",
			arrange: func(_ *testing.T, cs *fake.Clientset) {
				failVerb(cs, "delete", "pods", statusError(404))
			},
			wantErr:     []string{"pod termination not confirmed"},
			wantDeletes: 1,
		},
		{
			name: "delete answers NotFound while the same pod remains",
			arrange: func(_ *testing.T, cs *fake.Clientset) {
				failVerb(cs, "delete", "pods", apierrors.NewNotFound(podsResource.GroupResource(), testK8sPodName))
			},
			wantErr:     []string{"pod termination not confirmed"},
			wantDeletes: 1,
		},
		{
			name: "delete answers a UID conflict while the same pod remains",
			arrange: func(_ *testing.T, cs *fake.Clientset) {
				failVerb(cs, "delete", "pods", apierrors.NewConflict(podsResource.GroupResource(), testK8sPodName, errors.New("the UID in the precondition does not match")))
			},
			wantErr:     []string{"pod termination not confirmed"},
			wantDeletes: 1,
		},
		{
			name: "termination read unauthorized",
			arrange: func(_ *testing.T, cs *fake.Clientset) {
				ignorePodDeletes(cs)
				failVerb(cs, "get", "pods", apierrors.NewUnauthorized("token expired"))
			},
			wantErr:     []string{"confirm pod termination", "token expired"},
			wantDeletes: 1,
			wantReads:   1,
			failsFast:   true,
		},
		{
			name: "pod held terminating by a finalizer",
			arrange: func(t *testing.T, cs *fake.Clientset) {
				pod := readyPod("uid-1")
				pod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
				pod.Finalizers = []string{"test.stirrup.dev/hold"}
				require.New(t).NoError(cs.Tracker().Update(podsResource, pod, testK8sNamespace))
				ignorePodDeletes(cs)
			},
			wantErr:     []string{"pod termination not confirmed"},
			wantDeletes: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			cs := fake.NewClientset(readyPod("uid-1"), testPolicy())
			tc.arrange(t, cs)
			logger, logs := testLogger()
			td := testTeardown(cs, "uid-1", logger)
			if tc.failsFast {
				// A long bound shows that the call does not wait it out.
				td.timing.teardownTimeout = 10 * time.Second
			}

			start := time.Now()
			err := td.run()
			elapsed := time.Since(start)

			must.Error(err)
			for _, want := range tc.wantErr {
				must.ErrorContains(err, want)
			}
			must.ErrorContains(err, "retained egress NetworkPolicy "+testK8sNamespace+"/"+policyName)
			must.ErrorContains(err, testK8sRunID)
			check.Less(elapsed, 5*time.Second)
			check.True(trackerHas(t, cs, networkPoliciesResource, policyName), "policy removed despite unconfirmed termination")
			check.Zero(countVerb(cs, "delete", "networkpolicies"), "policy delete attempted")
			if tc.wantDeletes > 0 {
				check.Len(podDeletes(cs), tc.wantDeletes)
			}
			if tc.wantReads > 0 {
				check.Equal(tc.wantReads, countVerb(cs, "get", "pods"))
			}
			for _, d := range podDeletes(cs) {
				check.NotNil(d.GracePeriodSeconds)
				if d.GracePeriodSeconds != nil {
					check.Positive(*d.GracePeriodSeconds, "an unconfirmed termination escalated to a zero-grace delete")
				}
			}
			for _, want := range []string{"level=WARN", "namespace=" + testK8sNamespace, "pod=" + testK8sPodName, "networkpolicy=" + policyName, "runId=" + testK8sRunID} {
				check.Contains(logs.String(), want)
			}
		})
	}
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

	must.ErrorContains(err, fmt.Sprintf("tear down pod %s/%s (run ID %q)", testK8sNamespace, testK8sPodName, testK8sRunID))
	must.ErrorContains(err, "retained egress NetworkPolicy "+testK8sNamespace+"/"+networkPolicyName(testK8sPodName))
	check.Contains(logs.String(), "sandbox teardown incomplete; retaining the egress NetworkPolicy (fail closed)")
	check.True(trackerHas(t, cs, networkPoliciesResource, networkPolicyName(testK8sPodName)))
}

func TestK8sExecutor_CloseRetriesAfterRetaining(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset(readyPod("uid-1"), testPolicy())
	holding := true
	cs.PrependReactor("delete", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return holding, nil, nil
	})
	logger, _ := testLogger()
	exec := &K8sExecutor{
		podExecCore:       podExecCore{clientset: cs, namespace: testK8sNamespace, podName: testK8sPodName, logger: logger},
		podUID:            "uid-1",
		networkPolicyName: networkPolicyName(testK8sPodName),
		runID:             testK8sRunID,
		timing:            fastK8sTiming,
	}

	must.ErrorContains(exec.Close(), "retained egress NetworkPolicy")
	must.True(trackerHas(t, cs, networkPoliciesResource, networkPolicyName(testK8sPodName)))

	holding = false
	must.NoError(exec.Close(), "a later Close after the pod went away")
	must.False(trackerHas(t, cs, networkPoliciesResource, networkPolicyName(testK8sPodName)))

	deletes := countVerb(cs, "delete", "pods")
	must.NoError(exec.Close())
	must.Equal(deletes, countVerb(cs, "delete", "pods"), "a Close after a completed teardown touched the cluster")
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
		cs := fake.NewClientset(unselectedPod("uid-2"), testPolicy())
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
			must.NoError(cs.Tracker().Update(podsResource, unselectedPod("uid-2"), testK8sNamespace))
			return true, nil, nil
		})

		logger, _ := testLogger()
		must.NoError(testTeardown(cs, "uid-1", logger).run())
		must.False(trackerHas(t, cs, networkPoliciesResource, networkPolicyName(testK8sPodName)))
	})
}

func TestPodTeardown_RetainsPolicyForReplacementItSelects(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset(readyPod("uid-2"), testPolicy())
	failVerb(cs, "delete", "pods", apierrors.NewConflict(podsResource.GroupResource(), testK8sPodName, errors.New("precondition failed: UID mismatch")))

	logger, _ := testLogger()
	err := testTeardown(cs, "uid-1", logger).run()

	must.ErrorContains(err, "pod termination not confirmed")
	must.ErrorContains(err, "retained egress NetworkPolicy")
	must.True(trackerHas(t, cs, podsResource, testK8sPodName), "another run's pod was deleted")
	must.True(trackerHas(t, cs, networkPoliciesResource, networkPolicyName(testK8sPodName)))
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

	logger, logs := testLogger()
	err := testTeardown(cs, "uid-1", logger).run()
	must.ErrorContains(err, "tear down pod "+testK8sNamespace+"/"+testK8sPodName+` (run ID "`+testK8sRunID+`"): delete egress NetworkPolicy: etcd unavailable`)
	must.ErrorContains(err, "retained egress NetworkPolicy "+testK8sNamespace+"/"+networkPolicyName(testK8sPodName))
	must.Contains(logs.String(), "retaining the egress NetworkPolicy (fail closed)")
	must.Contains(logs.String(), "runId="+testK8sRunID)
}

func TestPodTeardown_RetriesTransientPolicyDeleteFailure(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset(readyPod("uid-1"), testPolicy())
	failures := 0
	cs.PrependReactor("delete", "networkpolicies", func(clienttesting.Action) (bool, runtime.Object, error) {
		if failures == 0 {
			failures++
			return true, nil, apierrors.NewServiceUnavailable("etcd leader election")
		}
		return false, nil, nil
	})

	logger, _ := testLogger()
	must.NoError(testTeardown(cs, "uid-1", logger).run())
	must.Equal(2, countVerb(cs, "delete", "networkpolicies"))
	must.False(trackerHas(t, cs, networkPoliciesResource, networkPolicyName(testK8sPodName)))
}

func TestPodTeardown_PolicyDeleteDenialFailsAtOnce(t *testing.T) {
	for name, denial := range map[string]error{
		"forbidden":    apierrors.NewForbidden(networkPoliciesResource.GroupResource(), "x", errors.New("denied by RBAC")),
		"unauthorized": apierrors.NewUnauthorized("token expired"),
	} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			cs := fake.NewClientset(readyPod("uid-1"), testPolicy())
			failVerb(cs, "delete", "networkpolicies", denial)

			logger, _ := testLogger()
			td := testTeardown(cs, "uid-1", logger)
			td.timing.policyTimeout = 10 * time.Second
			start := time.Now()
			err := td.run()

			must.ErrorIs(err, denial)
			must.ErrorContains(err, "retained egress NetworkPolicy")
			must.Equal(1, countVerb(cs, "delete", "networkpolicies"))
			must.Less(time.Since(start), 5*time.Second)
		})
	}
}

func TestPodTeardown_ReconcileFailedCreate(t *testing.T) {
	policyName := networkPolicyName(testK8sPodName)
	ours := podLabels(testK8sPodName, testK8sRunID)
	ambiguous := apierrors.NewServerTimeout(podsResource.GroupResource(), "create", 1)
	definitive := apierrors.NewForbidden(podsResource.GroupResource(), testK8sPodName, errors.New("exceeded quota: compute"))

	for name, createErr := range map[string]error{"ambiguous": ambiguous, "definitive": definitive} {
		t.Run("pod found and ours after a "+name+" error", func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			cs := fake.NewClientset(readyPod("uid-7"), testPolicy())
			lingering := lingerOnDelete(t, cs, 3)

			logger, _ := testLogger()
			must.NoError(testTeardown(cs, "", logger).reconcileFailedCreate(ours, createErr))

			deletes := podDeletes(cs)
			must.Len(deletes, 1)
			must.NotNil(deletes[0].Preconditions)
			check.EqualValues("uid-7", *deletes[0].Preconditions.UID, "delete must be pinned to the UID found")
			requireRemovedAfterRelease(t, lingering)
			check.False(trackerHas(t, cs, networkPoliciesResource, policyName))
		})
	}

	t.Run("pod found and ours but never terminates", func(t *testing.T) {
		must := require.New(t)
		cs := fake.NewClientset(readyPod("uid-7"), testPolicy())
		ignorePodDeletes(cs)

		logger, _ := testLogger()
		err := testTeardown(cs, "", logger).reconcileFailedCreate(ours, ambiguous)

		must.ErrorContains(err, "pod termination not confirmed")
		must.ErrorContains(err, "retained egress NetworkPolicy")
		must.True(trackerHas(t, cs, networkPoliciesResource, policyName))
	})

	t.Run("pod found and not ours", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		foreign := readyPod("uid-9")
		foreign.Labels = map[string]string{"app": "something-else"}
		cs := fake.NewClientset(foreign, testPolicy())

		logger, _ := testLogger()
		err := testTeardown(cs, "", logger).reconcileFailedCreate(ours, ambiguous)

		must.Error(err)
		must.ErrorContains(err, "does not carry this run's label")
		must.ErrorContains(err, "retained egress NetworkPolicy")
		check.Empty(podDeletes(cs), "a pod this run cannot prove it owns was deleted")
		check.True(trackerHas(t, cs, networkPoliciesResource, policyName))
	})

	t.Run("label mismatch is reported deterministically", func(t *testing.T) {
		must := require.New(t)
		foreign := readyPod("uid-9")
		foreign.Labels = map[string]string{}
		var first string
		for range 20 {
			cs := fake.NewClientset(foreign.DeepCopy(), testPolicy())
			logger, _ := testLogger()
			err := testTeardown(cs, "", logger).reconcileFailedCreate(ours, ambiguous)
			must.Error(err)
			if first == "" {
				first = err.Error()
			}
			must.Equal(first, err.Error())
		}
	})

	t.Run("pod absent after a definitive rejection", func(t *testing.T) {
		must := require.New(t)
		cs := fake.NewClientset(testPolicy())

		logger, _ := testLogger()
		must.NoError(testTeardown(cs, "", logger).reconcileFailedCreate(ours, definitive))
		must.Empty(podDeletes(cs))
		must.Equal(1, countVerb(cs, "get", "pods"))
		must.False(trackerHas(t, cs, networkPoliciesResource, policyName))
	})

	t.Run("pod absent after an ambiguous error", func(t *testing.T) {
		must := require.New(t)
		cs := fake.NewClientset(testPolicy())

		logger, logs := testLogger()
		err := testTeardown(cs, "", logger).reconcileFailedCreate(ours, ambiguous)

		must.ErrorContains(err, "was not found after a create whose outcome is unknown")
		must.ErrorContains(err, "retained egress NetworkPolicy "+testK8sNamespace+"/"+policyName)
		must.ErrorContains(err, testK8sRunID)
		must.Contains(logs.String(), "level=WARN")
		must.True(trackerHas(t, cs, networkPoliciesResource, policyName))
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
		must.NoError(testTeardown(cs, "", logger).reconcileFailedCreate(ours, ambiguous))
		must.Len(podDeletes(cs), 1)
		must.False(trackerHas(t, cs, networkPoliciesResource, policyName))
	})

	t.Run("lookup keeps failing", func(t *testing.T) {
		must := require.New(t)
		cs := fake.NewClientset(testPolicy())
		failVerb(cs, "get", "pods", errors.New("connection reset"))

		logger, _ := testLogger()
		err := testTeardown(cs, "", logger).reconcileFailedCreate(ours, definitive)
		must.ErrorContains(err, "connection reset")
		must.True(trackerHas(t, cs, networkPoliciesResource, policyName))
	})

	for name, denial := range map[string]error{"forbidden": errForbiddenPods, "unauthorized": apierrors.NewUnauthorized("token expired")} {
		t.Run("lookup "+name, func(t *testing.T) {
			must := require.New(t)
			cs := fake.NewClientset(testPolicy())
			failVerb(cs, "get", "pods", denial)

			logger, _ := testLogger()
			td := testTeardown(cs, "", logger)
			td.timing.teardownTimeout = 10 * time.Second
			start := time.Now()
			err := td.reconcileFailedCreate(ours, definitive)

			must.ErrorIs(err, denial)
			must.ErrorContains(err, "retained egress NetworkPolicy")
			must.Equal(1, countVerb(cs, "get", "pods"))
			must.Less(time.Since(start), 5*time.Second)
			must.True(trackerHas(t, cs, networkPoliciesResource, policyName))
		})
	}
}

func TestPodCreateRejectedDefinitively_ClassifiesErrors(t *testing.T) {
	gr := schema.GroupResource{Resource: "pods"}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, testK8sPodName, nil), true},
		{"forbidden", apierrors.NewForbidden(gr, testK8sPodName, errors.New("exceeded quota")), true},
		{"bad request", apierrors.NewBadRequest("malformed"), true},
		{"namespace not found", apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, testK8sNamespace), true},
		{"already exists", apierrors.NewAlreadyExists(gr, testK8sPodName), false},
		{"too many requests", apierrors.NewTooManyRequests("slow down", 1), false},
		{"request timeout", apierrors.NewTimeoutError("timed out", 1), false},
		{"server timeout", apierrors.NewServerTimeout(gr, "create", 1), false},
		{"internal error", apierrors.NewInternalError(errors.New("webhook failed")), false},
		{"service unavailable", apierrors.NewServiceUnavailable("unavailable"), false},
		{"transport error", errors.New("dial tcp: connection refused"), false},
		{"context deadline", context.DeadlineExceeded, false},
		{"context canceled", context.Canceled, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, podCreateRejectedDefinitively(tc.err))
		})
	}
}

func TestK8sExecutor_CloseUsesDefaultTimingWhenUnset(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset(readyPod("uid-1"), testPolicy())
	logger, _ := testLogger()
	exec := &K8sExecutor{
		podExecCore:       podExecCore{clientset: cs, namespace: testK8sNamespace, podName: testK8sPodName, logger: logger},
		podUID:            "uid-1",
		networkPolicyName: networkPolicyName(testK8sPodName),
	}
	must.NoError(exec.Close())
	must.False(trackerHas(t, cs, networkPoliciesResource, networkPolicyName(testK8sPodName)))
}
