package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1unstructured "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	utilexec "k8s.io/client-go/util/exec"
	"k8s.io/utils/ptr"

	"github.com/rxbynerd/stirrup/types"
)

// testSandboxUID is the UID the fake controller gives every Sandbox.
const testSandboxUID k8stypes.UID = "uid-sandbox"

// fakeAgentSandbox is a fake cluster for the k8s-sandbox executor: a typed
// clientset for the NetworkPolicy and the backing Pod, and a dynamic client
// for the Sandbox.
type fakeAgentSandbox struct {
	cs  *fake.Clientset
	dyn *dynamicfake.FakeDynamicClient
	// execFn serves the constructor's pods/exec calls; nil means reachableExec.
	execFn podExecFunc
	// mutatePod, when set, rewrites the backing Pod admitSandboxes creates.
	mutatePod func(*corev1.Pod)
	// orphanPod keeps the backing Pod when its Sandbox is removed.
	orphanPod bool
	// lingering records how teardown treated a Sandbox that stays, marked
	// for deletion, for a number of reads after its delete.
	terminating        bool
	released           bool
	policyDeletedEarly bool
	readsLeft          int
	// gvr is the Sandbox version the cluster serves and the executor resolves.
	gvr schema.GroupVersionResource
}

// newFakeAgentSandbox returns a cluster serving the Sandbox API at v1beta1.
func newFakeAgentSandbox(t *testing.T) *fakeAgentSandbox {
	t.Helper()
	return newFakeAgentSandboxServing(t, "v1beta1")
}

// newFakeAgentSandboxServing returns a cluster serving the Sandbox API at
// exactly the given versions.
func newFakeAgentSandboxServing(t *testing.T, versions ...string) *fakeAgentSandbox {
	t.Helper()
	f := &fakeAgentSandbox{
		cs: fake.NewClientset(),
		dyn: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
			testSandboxGVR:      "SandboxList",
			testSandboxGVRAlpha: "SandboxList",
		}),
	}
	serveSandboxVersions(f.cs, versions...)
	f.gvr = testSandboxGVRAlpha
	if slices.Contains(versions, "v1beta1") {
		f.gvr = testSandboxGVR
	}
	return f
}

// admitSandboxes stands in for the Agent Sandbox controller: every created
// Sandbox is reported Ready after mutate, if any, rewrites it, and gets a
// Ready backing Pod built from its podTemplate. Removing the Sandbox removes
// the Pod unless orphanPod is set.
func (f *fakeAgentSandbox) admitSandboxes(mutate func(obj *metav1unstructured.Unstructured)) {
	f.dyn.PrependReactor("create", "sandboxes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		obj := action.(clienttesting.CreateAction).GetObject().(*metav1unstructured.Unstructured)
		obj.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
		obj.SetUID(testSandboxUID)
		if mutate != nil {
			mutate(obj)
		}
		if err := f.createBackingPod(obj); err != nil {
			return true, nil, err
		}
		return false, nil, nil
	})
	f.dyn.PrependReactor("delete", "sandboxes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		f.removeBackingPod(action.GetNamespace(), action.(clienttesting.DeleteAction).GetName())
		return false, nil, nil
	})
}

// createBackingPod adds the Pod the Sandbox's podTemplate describes,
// controlled by the Sandbox and Running with the agent container Ready, then
// applies mutatePod.
func (f *fakeAgentSandbox) createBackingPod(sandbox *metav1unstructured.Unstructured) error {
	raw, found, err := metav1unstructured.NestedFieldNoCopy(sandbox.Object, "spec", "podTemplate")
	if err != nil || !found {
		return fmt.Errorf("sandbox has no podTemplate (found %v): %w", found, err)
	}
	rawTemplate, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("sandbox podTemplate is %T", raw)
	}
	var template corev1.PodTemplateSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(rawTemplate, &template); err != nil {
		return err
	}
	pod := &corev1.Pod{ObjectMeta: template.ObjectMeta, Spec: template.Spec}
	pod.Name = sandbox.GetName()
	pod.Namespace = sandbox.GetNamespace()
	pod.UID = "uid-backing"
	pod.OwnerReferences = []metav1.OwnerReference{sandboxControllerRef(f.gvr, sandbox.GetName(), sandbox.GetUID())}
	pod.Status = corev1.PodStatus{
		Phase:             corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{Name: k8sAgentContainer, Ready: true}},
	}
	if f.mutatePod != nil {
		f.mutatePod(pod)
	}
	return f.cs.Tracker().Add(pod)
}

// sandboxControllerRef is the controller reference the Agent Sandbox
// controller sets on a backing Pod.
func sandboxControllerRef(gvr schema.GroupVersionResource, name string, uid k8stypes.UID) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: gvr.GroupVersion().String(),
		Kind:       "Sandbox",
		Name:       name,
		UID:        uid,
		Controller: ptr.To(true),
	}
}

func (f *fakeAgentSandbox) removeBackingPod(namespace, name string) {
	if !f.orphanPod {
		_ = f.cs.Tracker().Delete(podsResource, namespace, name)
	}
}

// lingerOnDelete makes a Sandbox delete set its deletion timestamp; the
// Sandbox disappears on the reads-th read afterwards.
func (f *fakeAgentSandbox) lingerOnDelete(t *testing.T, reads int) {
	t.Helper()
	f.readsLeft = reads
	f.dyn.PrependReactor("delete", "sandboxes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		del := action.(clienttesting.DeleteAction)
		obj, err := f.dyn.Tracker().Get(f.gvr, del.GetNamespace(), del.GetName())
		if err != nil {
			return false, nil, nil
		}
		sandbox := obj.(*metav1unstructured.Unstructured).DeepCopy()
		sandbox.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})
		if err := f.dyn.Tracker().Update(f.gvr, sandbox, del.GetNamespace()); err != nil {
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
		if err := f.dyn.Tracker().Delete(f.gvr, get.GetNamespace(), get.GetName()); err != nil && !apierrors.IsNotFound(err) {
			return true, nil, err
		}
		f.removeBackingPod(get.GetNamespace(), get.GetName())
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
	execFn := f.execFn
	if execFn == nil {
		execFn = reachableExec
	}
	exec, err := newAgentSandboxExecutor(context.Background(), cfg, nil, f.cs, f.dyn, nil, execFn, timing, logger)
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

func TestNewAgentSandboxExecutor_UsesTheServedSandboxVersion(t *testing.T) {
	tests := []struct {
		name    string
		served  []string
		want    schema.GroupVersionResource
		wantAPI string
	}{
		{name: "both served", served: []string{"v1alpha1", "v1beta1"}, want: testSandboxGVR, wantAPI: "agents.x-k8s.io/v1beta1"},
		{name: "only v1alpha1 served", served: []string{"v1alpha1"}, want: testSandboxGVRAlpha, wantAPI: "agents.x-k8s.io/v1alpha1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			f := newFakeAgentSandboxServing(t, tt.served...)
			f.admitSandboxes(nil)

			exec, _, err := f.build(t, fastK8sTiming)
			must.NoError(err)
			must.NoError(exec.Close())

			var created *metav1unstructured.Unstructured
			for _, a := range f.dyn.Actions() {
				check.Equal(tt.want, a.GetResource(), "a %s used another Sandbox version", a.GetVerb())
				if c, ok := a.(clienttesting.CreateAction); ok {
					created = c.GetObject().(*metav1unstructured.Unstructured)
				}
			}
			must.NotNil(created, "no sandbox create recorded")
			check.Equal(tt.wantAPI, created.GetAPIVersion())
		})
	}
}

func TestNewAgentSandboxExecutor_CreatesTheSandboxWithStrictFieldValidation(t *testing.T) {
	must := require.New(t)
	f := newFakeAgentSandbox(t)
	f.admitSandboxes(nil)

	exec, _, err := f.build(t, fastK8sTiming)
	must.NoError(err)
	must.NoError(exec.Close())

	var creates []clienttesting.CreateActionImpl
	for _, a := range f.dyn.Actions() {
		if c, ok := a.(clienttesting.CreateActionImpl); ok {
			creates = append(creates, c)
		}
	}
	must.Len(creates, 1)
	must.Equal("Strict", creates[0].CreateOptions.FieldValidation)
}

func TestNewAgentSandboxExecutor_FailsBeforeAnyWriteWhenNoSandboxVersionIsServed(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	f := newFakeAgentSandboxServing(t)

	_, _, err := f.build(t, fastK8sTiming)

	must.ErrorIs(err, errSandboxAPINotServed)
	must.ErrorContains(err, "install the Agent Sandbox controller")
	var startup *StartupError
	must.ErrorAs(err, &startup)
	check.Equal(types.BuildFailureSandboxPlatform, startup.Reason)
	check.Empty(f.dyn.Actions(), "the Sandbox API was used without a served version")
	for _, a := range f.cs.Actions() {
		check.Equal("get", a.GetVerb(), "a cluster write preceded version resolution")
		check.Equal(fakeDiscoveryResource, a.GetResource().Resource)
	}
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

	t.Run("exec unreachable", func(t *testing.T) {
		must := require.New(t)
		f := newFakeAgentSandbox(t)
		f.admitSandboxes(nil)
		f.lingerOnDelete(t, 3)
		calls := 0
		f.execFn = scriptedExec(&calls, errKubeletUnreachable)
		timing := fastK8sTiming
		timing.execReachableTimeout = 50 * time.Millisecond

		_, _, err := f.build(t, timing)

		must.ErrorIs(err, errExecUnreachable)
		must.Equal(types.BuildFailureSandboxNotReady, startupReason(err))
		f.requireSandboxRemovedThenPolicy(t, f.sandboxName(t))
	})

	t.Run("scratch setup failure", func(t *testing.T) {
		must := require.New(t)
		f := newFakeAgentSandbox(t)
		f.admitSandboxes(nil)
		f.lingerOnDelete(t, 3)
		var commands [][]string
		f.execFn = func(_ context.Context, command []string, _ io.Reader, _, stderr io.Writer) error {
			commands = append(commands, command)
			if isExecProbe(command) {
				return nil
			}
			_, _ = io.WriteString(stderr, "mkdir: can't create directory '/scratch/home': File exists\n")
			return utilexec.CodeExitError{Err: errors.New("command terminated with exit code 1"), Code: 1}
		}

		_, _, err := f.build(t, fastK8sTiming)

		must.ErrorContains(err, "prepare sandbox scratch: exit 1: mkdir: can't create directory '/scratch/home': File exists")
		must.Equal([][]string{execProbeCommand, scratchSetupCommand()}, commands, "the reachability probe precedes the scratch setup")
		f.requireSandboxRemovedThenPolicy(t, f.sandboxName(t))
	})
}

func TestAgentSandboxExecutor_CloseRetainsPolicy(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(t *testing.T, f *fakeAgentSandbox, name string)
		wantErr string
	}{
		{
			name: "sandbox delete keeps failing",
			arrange: func(_ *testing.T, f *fakeAgentSandbox, _ string) {
				f.dyn.PrependReactor("delete", "sandboxes", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("connection refused")
				})
			},
			wantErr: "delete sandbox: connection refused (gave up within the 100ms teardown bound)",
		},
		{
			name: "sandbox delete forbidden",
			arrange: func(_ *testing.T, f *fakeAgentSandbox, _ string) {
				f.dyn.PrependReactor("delete", "sandboxes", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(testSandboxGVR.GroupResource(), "x", errors.New("denied by RBAC"))
				})
			},
			wantErr: "denied by RBAC",
		},
		{
			name: "sandbox not confirmed gone",
			arrange: func(_ *testing.T, f *fakeAgentSandbox, _ string) {
				f.dyn.PrependReactor("delete", "sandboxes", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, nil
				})
			},
			wantErr: "sandbox deletion not confirmed within the 100ms teardown bound",
		},
		{
			name: "sandbox delete answers not found while the sandbox remains",
			arrange: func(_ *testing.T, f *fakeAgentSandbox, name string) {
				f.dyn.PrependReactor("delete", "sandboxes", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewNotFound(testSandboxGVR.GroupResource(), name)
				})
			},
			wantErr: "sandbox deletion not confirmed within the 100ms teardown bound",
		},
		{
			name: "sandbox delete answers conflict while the sandbox remains",
			arrange: func(_ *testing.T, f *fakeAgentSandbox, name string) {
				f.dyn.PrependReactor("delete", "sandboxes", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewConflict(testSandboxGVR.GroupResource(), name, errors.New("precondition failed"))
				})
			},
			wantErr: "gave up within the 100ms teardown bound",
		},
		{
			name: "sandbox read unauthorized",
			arrange: func(_ *testing.T, f *fakeAgentSandbox, _ string) {
				f.dyn.PrependReactor("get", "sandboxes", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewUnauthorized("token expired")
				})
			},
			wantErr: "confirm sandbox deletion: token expired",
		},
		{
			name: "backing pod remains after the sandbox is gone",
			arrange: func(_ *testing.T, f *fakeAgentSandbox, _ string) {
				f.orphanPod = true
			},
			wantErr: "pod termination not confirmed within the 100ms teardown bound",
		},
		{
			name: "backing pod read forbidden",
			arrange: func(_ *testing.T, f *fakeAgentSandbox, _ string) {
				failVerb(f.cs, "get", "pods", errForbiddenPods)
			},
			wantErr: "confirm pod termination: ",
		},
		{
			name: "backing pod read unauthorized",
			arrange: func(_ *testing.T, f *fakeAgentSandbox, _ string) {
				failVerb(f.cs, "get", "pods", apierrors.NewUnauthorized("token expired"))
			},
			wantErr: "confirm pod termination: token expired",
		},
		{
			name: "policy delete fails",
			arrange: func(_ *testing.T, f *fakeAgentSandbox, _ string) {
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
			tc.arrange(t, f, f.sandboxName(t))

			err = exec.Close()

			must.ErrorContains(err, tc.wantErr)
			f.requireRetained(t, err, logs, f.sandboxName(t))
			for _, d := range f.sandboxDeletes() {
				must.Nil(d.GracePeriodSeconds, "teardown escalated to a forced delete")
			}
		})
	}
}

func TestAgentSandboxExecutor_CloseWaitsForTheBackingPod(t *testing.T) {
	must := require.New(t)
	f := newFakeAgentSandbox(t)
	f.admitSandboxes(nil)
	exec, _, err := f.build(t, fastK8sTiming)
	must.NoError(err)
	name := f.sandboxName(t)
	f.orphanPod = true

	readsLeft, released, policyDeletedEarly := 3, false, false
	f.cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if released {
			return false, nil, nil
		}
		if readsLeft > 0 {
			readsLeft--
			return false, nil, nil
		}
		get := action.(clienttesting.GetAction)
		if err := f.cs.Tracker().Delete(podsResource, get.GetNamespace(), get.GetName()); err != nil {
			return true, nil, err
		}
		released = true
		return false, nil, nil
	})
	f.cs.PrependReactor("delete", "networkpolicies", func(clienttesting.Action) (bool, runtime.Object, error) {
		policyDeletedEarly = !released
		return false, nil, nil
	})

	must.NoError(exec.Close())
	must.True(released, "teardown stopped reading before the backing pod disappeared")
	must.False(policyDeletedEarly, "the policy was deleted while the backing pod remained")
	must.False(f.hasPolicy(t, name))
	must.Zero(countVerb(f.cs, "delete", "pods"), "the controller-owned pod was deleted directly")
}

func TestNewAgentSandboxExecutor_ReconcilesFailedCreate(t *testing.T) {
	for _, version := range []string{"v1beta1", "v1alpha1"} {
		t.Run(version, func(t *testing.T) { reconcileFailedCreateScenarios(t, version) })
	}
}

// reconcileFailedCreateScenarios runs the failed-create cases against a
// cluster serving only the given Sandbox version.
func reconcileFailedCreateScenarios(t *testing.T, version string) {
	definitive := apierrors.NewForbidden(testSandboxGVR.GroupResource(), "x", errors.New("exceeded quota"))
	ambiguous := apierrors.NewServerTimeout(testSandboxGVR.GroupResource(), "create", 1)
	persistThenFail := func(f *fakeAgentSandbox, createErr error, labels map[string]string) {
		f.dyn.PrependReactor("create", "sandboxes", func(action clienttesting.Action) (bool, runtime.Object, error) {
			obj := action.(clienttesting.CreateAction).GetObject().(*metav1unstructured.Unstructured).DeepCopy()
			if labels != nil {
				obj.SetLabels(labels)
			}
			if err := f.dyn.Tracker().Create(f.gvr, obj, testK8sNamespace); err != nil {
				return true, nil, err
			}
			return true, nil, createErr
		})
	}

	t.Run("definitive rejection with no sandbox removes the policy", func(t *testing.T) {
		must := require.New(t)
		f := newFakeAgentSandboxServing(t, version)
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
		f := newFakeAgentSandboxServing(t, version)
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
			f := newFakeAgentSandboxServing(t, version)
			persistThenFail(f, createErr, nil)
			f.lingerOnDelete(t, 3)

			_, _, err := f.build(t, fastK8sTiming)

			must.ErrorIs(err, createErr)
			f.requireSandboxRemovedThenPolicy(t, f.sandboxName(t))
		})
	}

	t.Run("sandbox without this run's labels is left alone", func(t *testing.T) {
		must := require.New(t)
		f := newFakeAgentSandboxServing(t, version)
		persistThenFail(f, ambiguous, map[string]string{"app": "other"})

		_, logs, err := f.build(t, fastK8sTiming)

		must.ErrorContains(err, "does not carry this run's label")
		must.Empty(f.sandboxDeletes(), "a sandbox this run cannot prove it owns was deleted")
		f.requireRetained(t, err, logs, f.sandboxName(t))
	})
}

func TestNewAgentSandboxExecutor_RejectsNonGVisorRuntimeBeforeAnyClusterCall(t *testing.T) {
	for _, runtime := range []string{"nitro-isolated", "runc", "kata-qemu"} {
		t.Run(runtime, func(t *testing.T) {
			must := require.New(t)
			f := newFakeAgentSandbox(t)
			cfg := K8sExecutorConfig{
				Image:     "busybox",
				Namespace: testK8sNamespace,
				Runtime:   runtime,
				Network:   &types.NetworkConfig{Mode: "none"},
				Lifecycle: Lifecycle{RunID: testK8sRunID},
			}
			logger, _ := testLogger()

			_, err := newAgentSandboxExecutor(context.Background(), cfg, nil, f.cs, f.dyn, nil, reachableExec, fastK8sTiming, logger)

			must.ErrorContains(err, `executor.runtime must be "gvisor" or empty (got "`+runtime+`")`)
			must.Empty(f.cs.Actions(), "a rejected runtime reached the cluster")
			must.Empty(f.dyn.Actions(), "a rejected runtime reached the cluster")
		})
	}
	t.Run("public constructor rejects before building a client", func(t *testing.T) {
		must := require.New(t)
		_, err := NewAgentSandboxExecutor(context.Background(), K8sExecutorConfig{Runtime: "nitro-isolated", Kubeconfig: "/nonexistent/kubeconfig"})
		must.ErrorContains(err, `executor.runtime must be "gvisor" or empty`)
	})
}
