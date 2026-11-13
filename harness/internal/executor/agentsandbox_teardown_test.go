package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1unstructured "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/rxbynerd/stirrup/types"
)

// fakeAgentSandbox is a fake cluster for the k8s-sandbox executor: a typed
// clientset for the NetworkPolicy and a dynamic client for the Sandbox.
type fakeAgentSandbox struct {
	cs  *fake.Clientset
	dyn *dynamicfake.FakeDynamicClient
	// lingering records how teardown treated a Sandbox that stays, marked
	// for deletion, for a number of reads after its delete.
	terminating        bool
	released           bool
	policyDeletedEarly bool
	readsLeft          int
}

func newFakeAgentSandbox(t *testing.T) *fakeAgentSandbox {
	t.Helper()
	return &fakeAgentSandbox{
		cs:  fake.NewClientset(),
		dyn: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{sandboxGVR: "SandboxList"}),
	}
}

// admitSandboxes reports every created Sandbox Ready after mutate, if any,
// rewrites it.
func (f *fakeAgentSandbox) admitSandboxes(mutate func(obj *metav1unstructured.Unstructured)) {
	f.dyn.PrependReactor("create", "sandboxes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		obj := action.(clienttesting.CreateAction).GetObject().(*metav1unstructured.Unstructured)
		obj.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
		if mutate != nil {
			mutate(obj)
		}
		return false, nil, nil
	})
}

// lingerOnDelete makes a Sandbox delete set its deletion timestamp; the
// Sandbox disappears on the reads-th read afterwards.
func (f *fakeAgentSandbox) lingerOnDelete(t *testing.T, reads int) {
	t.Helper()
	f.readsLeft = reads
	f.dyn.PrependReactor("delete", "sandboxes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		del := action.(clienttesting.DeleteAction)
		obj, err := f.dyn.Tracker().Get(sandboxGVR, del.GetNamespace(), del.GetName())
		if err != nil {
			return false, nil, nil
		}
		sandbox := obj.(*metav1unstructured.Unstructured).DeepCopy()
		sandbox.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})
		if err := f.dyn.Tracker().Update(sandboxGVR, sandbox, del.GetNamespace()); err != nil {
			return true, nil, err
		}
		f.terminating = true
		return true, nil, nil
	})
	f.dyn.PrependReactor("get", "sandboxes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if !f.terminating || f.released {
			return false, nil, nil
		}
		if f.readsLeft > 0 {
			f.readsLeft--
			return false, nil, nil
		}
		get := action.(clienttesting.GetAction)
		if err := f.dyn.Tracker().Delete(sandboxGVR, get.GetNamespace(), get.GetName()); err != nil && !apierrors.IsNotFound(err) {
			return true, nil, err
		}
		f.released = true
		return false, nil, nil
	})
	f.cs.PrependReactor("delete", "networkpolicies", func(clienttesting.Action) (bool, runtime.Object, error) {
		if f.terminating && !f.released {
			f.policyDeletedEarly = true
		}
		return false, nil, nil
	})
}

func (f *fakeAgentSandbox) build(t *testing.T, timing k8sTiming) (*AgentSandboxExecutor, *syncBuffer, error) {
	t.Helper()
	logger, logs := testLogger()
	cfg := K8sExecutorConfig{
		Image:     "busybox",
		Namespace: testK8sNamespace,
		Network:   &types.NetworkConfig{Mode: "none"},
		Lifecycle: Lifecycle{RunID: testK8sRunID},
	}
	exec, err := newAgentSandboxExecutor(context.Background(), cfg, nil, f.cs, f.dyn, nil, timing, logger)
	return exec, logs, err
}

// sandboxName returns the name of the Sandbox the constructor submitted.
func (f *fakeAgentSandbox) sandboxName(t *testing.T) string {
	t.Helper()
	for _, a := range f.dyn.Actions() {
		if c, ok := a.(clienttesting.CreateAction); ok && a.GetResource().Resource == "sandboxes" {
			return c.GetObject().(*metav1unstructured.Unstructured).GetName()
		}
	}
	require.New(t).FailNow("no sandbox create recorded")
	return ""
}

func (f *fakeAgentSandbox) hasPolicy(t *testing.T, sandboxName string) bool {
	t.Helper()
	return trackerHas(t, f.cs, networkPoliciesResource, networkPolicyName(sandboxName))
}

func (f *fakeAgentSandbox) sandboxDeletes() []metav1.DeleteOptions {
	var out []metav1.DeleteOptions
	for _, a := range f.dyn.Actions() {
		if d, ok := a.(clienttesting.DeleteActionImpl); ok && a.GetResource().Resource == "sandboxes" {
			out = append(out, d.DeleteOptions)
		}
	}
	return out
}

// requireSandboxRemovedThenPolicy asserts the Sandbox lingered, was released,
// and the policy was deleted only afterwards.
func (f *fakeAgentSandbox) requireSandboxRemovedThenPolicy(t *testing.T, sandboxName string) {
	t.Helper()
	must := require.New(t)
	must.True(f.terminating, "the sandbox was never deleted")
	must.True(f.released, "teardown stopped reading before the sandbox disappeared")
	must.False(f.policyDeletedEarly, "the policy was deleted while the sandbox was still terminating")
	must.False(f.hasPolicy(t, sandboxName), "policy left behind")
}

// requireRetained asserts err and the log report a retained policy for the
// Sandbox, and that the policy is still present.
func (f *fakeAgentSandbox) requireRetained(t *testing.T, err error, logs *syncBuffer, sandboxName string) {
	t.Helper()
	must := require.New(t)
	check := assert.New(t)
	must.Error(err)
	must.ErrorContains(err, "tear down sandbox "+testK8sNamespace+"/"+sandboxName)
	must.ErrorContains(err, testK8sRunID)
	must.ErrorContains(err, "retained egress NetworkPolicy "+testK8sNamespace+"/"+networkPolicyName(sandboxName))
	check.True(f.hasPolicy(t, sandboxName), "policy removed despite unconfirmed teardown")
	for _, want := range []string{"retaining the egress NetworkPolicy (fail closed)", "pod=" + sandboxName, "runId=" + testK8sRunID} {
		check.Contains(logs.String(), want)
	}
}

func TestNewAgentSandboxExecutor_BindsReadySandboxAndClosesInOrder(t *testing.T) {
	must := require.New(t)
	f := newFakeAgentSandbox(t)
	f.admitSandboxes(nil)
	f.lingerOnDelete(t, 3)

	exec, _, err := f.build(t, fastK8sTiming)
	must.NoError(err)
	name := f.sandboxName(t)
	must.Equal(name, exec.podName)

	must.NoError(exec.Close())
	f.requireSandboxRemovedThenPolicy(t, name)
	deletes := f.sandboxDeletes()
	must.Len(deletes, 1)
	must.NotNil(deletes[0].PropagationPolicy)
	must.Equal(metav1.DeletePropagationForeground, *deletes[0].PropagationPolicy)

	must.NoError(exec.Close(), "second Close")
	must.Len(f.sandboxDeletes(), 1, "a second Close issued another delete")
}

func TestNewAgentSandboxExecutor_RollsBackThroughClose(t *testing.T) {
	t.Run("readiness failure", func(t *testing.T) {
		must := require.New(t)
		f := newFakeAgentSandbox(t)
		f.lingerOnDelete(t, 3)
		timing := fastK8sTiming
		timing.readyTimeout = 50 * time.Millisecond

		start := time.Now()
		_, _, err := f.build(t, timing)

		must.ErrorContains(err, "not ready")
		must.Less(time.Since(start), 5*time.Second, "the readiness wait ignored the timing it was given")
		f.requireSandboxRemovedThenPolicy(t, f.sandboxName(t))
	})

	t.Run("backing pod name mismatch", func(t *testing.T) {
		must := require.New(t)
		f := newFakeAgentSandbox(t)
		f.admitSandboxes(func(obj *metav1unstructured.Unstructured) {
			obj.SetAnnotations(map[string]string{sandboxPodNameAnnotation: "warm-pool-pod"})
		})
		f.lingerOnDelete(t, 3)

		_, _, err := f.build(t, fastK8sTiming)

		must.ErrorContains(err, "warm-pool adoption is not supported")
		f.requireSandboxRemovedThenPolicy(t, f.sandboxName(t))
	})
}

func TestAgentSandboxExecutor_CloseRetainsPolicy(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(f *fakeAgentSandbox)
		wantErr string
	}{
		{
			name: "sandbox delete keeps failing",
			arrange: func(f *fakeAgentSandbox) {
				f.dyn.PrependReactor("delete", "sandboxes", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("connection refused")
				})
			},
			wantErr: "delete sandbox: connection refused (gave up within the 100ms teardown bound)",
		},
		{
			name: "sandbox delete forbidden",
			arrange: func(f *fakeAgentSandbox) {
				f.dyn.PrependReactor("delete", "sandboxes", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(sandboxGVR.GroupResource(), "x", errors.New("denied by RBAC"))
				})
			},
			wantErr: "denied by RBAC",
		},
		{
			name: "sandbox not confirmed gone",
			arrange: func(f *fakeAgentSandbox) {
				f.dyn.PrependReactor("delete", "sandboxes", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, nil
				})
			},
			wantErr: "sandbox deletion not confirmed within the 100ms teardown bound",
		},
		{
			name: "policy delete fails",
			arrange: func(f *fakeAgentSandbox) {
				failVerb(f.cs, "delete", "networkpolicies", errors.New("etcd unavailable"))
			},
			wantErr: "delete egress NetworkPolicy: etcd unavailable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			f := newFakeAgentSandbox(t)
			f.admitSandboxes(nil)
			exec, logs, err := f.build(t, fastK8sTiming)
			must.NoError(err)
			tc.arrange(f)

			err = exec.Close()

			must.ErrorContains(err, tc.wantErr)
			f.requireRetained(t, err, logs, f.sandboxName(t))
			for _, d := range f.sandboxDeletes() {
				must.Nil(d.GracePeriodSeconds, "teardown escalated to a forced delete")
			}
		})
	}
}

func TestNewAgentSandboxExecutor_ReconcilesFailedCreate(t *testing.T) {
	definitive := apierrors.NewForbidden(sandboxGVR.GroupResource(), "x", errors.New("exceeded quota"))
	ambiguous := apierrors.NewServerTimeout(sandboxGVR.GroupResource(), "create", 1)
	persistThenFail := func(f *fakeAgentSandbox, createErr error, labels map[string]string) {
		f.dyn.PrependReactor("create", "sandboxes", func(action clienttesting.Action) (bool, runtime.Object, error) {
			obj := action.(clienttesting.CreateAction).GetObject().(*metav1unstructured.Unstructured).DeepCopy()
			if labels != nil {
				obj.SetLabels(labels)
			}
			if err := f.dyn.Tracker().Create(sandboxGVR, obj, testK8sNamespace); err != nil {
				return true, nil, err
			}
			return true, nil, createErr
		})
	}

	t.Run("definitive rejection with no sandbox removes the policy", func(t *testing.T) {
		must := require.New(t)
		f := newFakeAgentSandbox(t)
		f.dyn.PrependReactor("create", "sandboxes", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, definitive
		})

		_, _, err := f.build(t, fastK8sTiming)

		must.ErrorIs(err, definitive)
		must.Empty(f.sandboxDeletes())
		must.False(f.hasPolicy(t, f.sandboxName(t)))
	})

	t.Run("ambiguous error with no sandbox retains the policy", func(t *testing.T) {
		must := require.New(t)
		f := newFakeAgentSandbox(t)
		f.dyn.PrependReactor("create", "sandboxes", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, ambiguous
		})

		_, logs, err := f.build(t, fastK8sTiming)

		must.ErrorIs(err, ambiguous)
		must.ErrorContains(err, "was not found after a create whose outcome is unknown")
		f.requireRetained(t, err, logs, f.sandboxName(t))
	})

	for name, createErr := range map[string]error{"ambiguous": ambiguous, "definitive": definitive} {
		t.Run(name+" error with the sandbox present tears it down", func(t *testing.T) {
			must := require.New(t)
			f := newFakeAgentSandbox(t)
			persistThenFail(f, createErr, nil)
			f.lingerOnDelete(t, 3)

			_, _, err := f.build(t, fastK8sTiming)

			must.ErrorIs(err, createErr)
			f.requireSandboxRemovedThenPolicy(t, f.sandboxName(t))
		})
	}

	t.Run("sandbox without this run's labels is left alone", func(t *testing.T) {
		must := require.New(t)
		f := newFakeAgentSandbox(t)
		persistThenFail(f, ambiguous, map[string]string{"app": "other"})

		_, logs, err := f.build(t, fastK8sTiming)

		must.ErrorContains(err, "does not carry this run's label")
		must.Empty(f.sandboxDeletes(), "a sandbox this run cannot prove it owns was deleted")
		f.requireRetained(t, err, logs, f.sandboxName(t))
	})
}
