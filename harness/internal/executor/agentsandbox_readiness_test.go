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
	metav1unstructured "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/rxbynerd/stirrup/types"
)

// sandboxCluster is a fake cluster holding one Sandbox named testK8sPodName
// and, optionally, its backing Pod.
type sandboxCluster struct {
	cs  *fake.Clientset
	dyn *dynamicfake.FakeDynamicClient
}

func newSandboxCluster(t *testing.T, sandbox *metav1unstructured.Unstructured, pod *corev1.Pod) sandboxCluster {
	t.Helper()
	must := require.New(t)
	c := sandboxCluster{
		cs:  fake.NewClientset(),
		dyn: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{sandboxGVR: "SandboxList"}),
	}
	if sandbox != nil {
		must.NoError(c.dyn.Tracker().Create(sandboxGVR, sandbox, testK8sNamespace))
	}
	if pod != nil {
		must.NoError(c.cs.Tracker().Add(pod))
	}
	return c
}

// readinessTemplate is the Pod template a k8s-sandbox run submits for the
// Sandbox named testK8sPodName.
func readinessTemplate() *corev1.Pod {
	cfg := K8sExecutorConfig{Image: "busybox", Namespace: testK8sNamespace, Network: &types.NetworkConfig{Mode: "none"}}
	spec := buildSandboxPodSpec(cfg, nil, nil)
	applyAgentSandboxAdmissionDeltas(&spec)
	return sandboxPodTemplate(testK8sNamespace, testK8sPodName, testK8sRunID, spec)
}

// sandboxIn returns the Sandbox named testK8sPodName whose Ready condition has
// the given status, reason and message.
func sandboxIn(status, reason, message string) *metav1unstructured.Unstructured {
	cond := readyCondition(sandboxReadyConditionType, status)
	cond["reason"], cond["message"] = reason, message
	sandbox := sandboxWithConditions([]any{cond})
	sandbox.SetName(testK8sPodName)
	sandbox.SetNamespace(testK8sNamespace)
	sandbox.SetUID(testSandboxUID)
	return sandbox
}

func readySandbox() *metav1unstructured.Unstructured {
	return sandboxIn("True", "PodReady", "backing pod is ready")
}

// backingPod is the controller-created Pod for readinessTemplate: controlled
// by the Sandbox and Running with the agent container Ready.
func backingPod(uid k8stypes.UID) *corev1.Pod {
	pod := readyPod(uid)
	pod.Spec = readinessTemplate().Spec
	pod.OwnerReferences = []metav1.OwnerReference{sandboxControllerRef(testK8sPodName, testSandboxUID)}
	return pod
}

func pendingBackingPod(uid k8stypes.UID, statuses []corev1.ContainerStatus) *corev1.Pod {
	pod := backingPod(uid)
	pod.Status.Phase = corev1.PodPending
	pod.Status.ContainerStatuses = statuses
	return pod
}

func (c sandboxCluster) wait(t *testing.T, timing k8sTiming) (*corev1.Pod, time.Duration, *syncBuffer, error) {
	t.Helper()
	logger, logs := testLogger()
	start := time.Now()
	pod, err := waitForSandboxReady(context.Background(), c.dyn, c.cs, readinessTemplate(), timing, logger)
	return pod, time.Since(start), logs, err
}

// failReadsUntil makes the first n reads of resource on tracker fail with err.
func failReadsUntil(tracker interface {
	PrependReactor(verb, resource string, fn clienttesting.ReactionFunc)
}, resource string, n int, err error) *int {
	reads := 0
	tracker.PrependReactor("get", resource, func(clienttesting.Action) (bool, runtime.Object, error) {
		reads++
		if reads <= n {
			return true, nil, err
		}
		return false, nil, nil
	})
	return &reads
}

func TestWaitForSandboxReady_ReturnsWhenSandboxAndPodAreReady(t *testing.T) {
	must := require.New(t)
	c := newSandboxCluster(t, readySandbox(), backingPod("uid-1"))

	pod, _, _, err := c.wait(t, fastK8sTiming)

	must.NoError(err)
	must.EqualValues("uid-1", pod.UID)
}

func TestWaitForSandboxReady_WaitsForTheControllerToCreateThePod(t *testing.T) {
	must := require.New(t)
	c := newSandboxCluster(t, readySandbox(), nil)
	reads := 0
	c.cs.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		reads++
		if reads == 3 {
			must.NoError(c.cs.Tracker().Add(backingPod("uid-1")))
		}
		return false, nil, nil
	})

	pod, _, _, err := c.wait(t, slowReadyTiming)

	must.NoError(err)
	must.EqualValues("uid-1", pod.UID)
	must.Equal(3, reads)
}

func TestWaitForSandboxReady_RetriesTransientErrors(t *testing.T) {
	must := require.New(t)
	c := newSandboxCluster(t, readySandbox(), backingPod("uid-1"))
	sandboxReads := failReadsUntil(c.dyn, "sandboxes", 2, errors.New("connection refused"))
	podReads := failReadsUntil(c.cs, "pods", 4, apierrors.NewServerTimeout(podsResource.GroupResource(), "get", 1))

	_, _, _, err := c.wait(t, slowReadyTiming)

	must.NoError(err)
	must.Equal(7, *sandboxReads, "two failed sandbox reads, then one per pod read")
	must.Equal(5, *podReads, "the pod is read only once the sandbox has been")
}

func TestWaitForSandboxReady_RequiresTheSandboxReadyCondition(t *testing.T) {
	must := require.New(t)
	c := newSandboxCluster(t, sandboxIn("False", "DependenciesNotReady", "Pod is not ready"), backingPod("uid-1"))

	_, _, _, err := c.wait(t, fastK8sTiming)

	must.ErrorIs(err, errPodNotReady)
	must.ErrorIs(err, context.DeadlineExceeded)
	must.ErrorContains(err, "not ready within 200ms: sandbox Ready False (DependenciesNotReady: Pod is not ready); backing pod: phase Running")
}

func TestWaitForSandboxReady_FailsFastOnTerminalPodState(t *testing.T) {
	cases := []struct {
		name     string
		statuses []corev1.ContainerStatus
		wantErr  string
	}{
		{
			name:     "invalid image name",
			statuses: agentWaiting("InvalidImageName", `couldn't parse image name "UPPER:case"`),
			wantErr:  "agent container cannot start: InvalidImageName",
		},
		{
			name:     "image never pulled",
			statuses: agentWaiting("ErrImageNeverPull", "image is not present with pull policy of Never"),
			wantErr:  "agent container cannot start: ErrImageNeverPull",
		},
		{
			name: "agent container terminated",
			statuses: []corev1.ContainerStatus{{
				Name:  k8sAgentContainer,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 127, Reason: "Error", Message: "exec: /bin/sh: not found"}},
			}},
			wantErr: "agent container terminated (exit code 127, Error: exec: /bin/sh: not found)",
		},
		{
			name: "agent container in a crash loop",
			statuses: []corev1.ContainerStatus{{
				Name:         k8sAgentContainer,
				RestartCount: 3,
				State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}},
			wantErr: "agent container restarted (restart count 3)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			c := newSandboxCluster(t, sandboxIn("False", "DependenciesNotReady", "Pod is not ready"), pendingBackingPod("uid-1", tc.statuses))

			_, elapsed, _, err := c.wait(t, failFastTiming)

			must.ErrorIs(err, errPodNotReady)
			must.ErrorContains(err, tc.wantErr)
			check.Less(elapsed, 4*time.Second, "a terminal state waited for the ceiling")
			check.Equal(1, countVerb(c.cs, "get", "pods"), "a terminal state was polled again")
		})
	}
}

func TestWaitForSandboxReady_TimeoutReportsTheBackingPod(t *testing.T) {
	t.Run("pod observed", func(t *testing.T) {
		must := require.New(t)
		pod := pendingBackingPod("uid-1", agentWaiting("ImagePullBackOff", `Back-off pulling image "registry.invalid/sandbox:1"`))
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}}
		c := newSandboxCluster(t, sandboxIn("False", "DependenciesNotReady", "Pod is not ready"), pod)

		_, _, _, err := c.wait(t, fastK8sTiming)

		must.ErrorIs(err, context.DeadlineExceeded)
		must.ErrorContains(err, "backing pod: phase Pending; PodScheduled True; "+`last waiting reason ImagePullBackOff (Back-off pulling image "registry.invalid/sandbox:1")`)
	})
	t.Run("nothing observed", func(t *testing.T) {
		must := require.New(t)
		c := newSandboxCluster(t, nil, nil)

		_, _, _, err := c.wait(t, fastK8sTiming)

		must.ErrorIs(err, context.DeadlineExceeded)
		must.ErrorContains(err, "the sandbox was never observed; last sandbox API error: ")
		must.ErrorContains(err, "backing pod: the pod was never observed")
	})
	t.Run("sandbox without a Ready condition", func(t *testing.T) {
		must := require.New(t)
		sandbox := sandboxWithConditions(nil)
		sandbox.SetName(testK8sPodName)
		sandbox.SetNamespace(testK8sNamespace)
		c := newSandboxCluster(t, sandbox, nil)

		_, _, _, err := c.wait(t, fastK8sTiming)

		must.ErrorContains(err, "sandbox has no Ready condition; backing pod: the pod was never observed")
	})
}

func TestWaitForSandboxReady_BoundsClusterText(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	long := strings.Repeat("x", 600)
	c := newSandboxCluster(t, sandboxIn("False", "Bad\u202eReason", long), nil)

	_, _, _, err := c.wait(t, fastK8sTiming)

	must.Error(err)
	check.NotContains(err.Error(), "\u202e")
	check.NotContains(err.Error(), long)
	check.Contains(err.Error(), "BadReason")
}

func TestWaitForSandboxReady_LogsStartAndProgress(t *testing.T) {
	must := require.New(t)
	c := newSandboxCluster(t, sandboxIn("False", "DependenciesNotReady", "Pod is not ready"), pendingBackingPod("uid-1", agentWaiting("ContainerCreating", "")))
	timing := k8sTiming{readyTimeout: 300 * time.Millisecond, pollPeriod: 5 * time.Millisecond, progressPeriod: 50 * time.Millisecond}

	_, _, logs, err := c.wait(t, timing)
	must.ErrorIs(err, context.DeadlineExceeded)

	out := logs.String()
	must.Equal(1, strings.Count(out, `msg="k8s executor waiting for sandbox pod readiness"`), "the start line is logged once")
	progress := strings.Count(out, "still waiting for sandbox pod readiness")
	must.GreaterOrEqual(progress, 2, "progress lines at the cadence")
	must.LessOrEqual(progress, 7, "progress is logged at the cadence, not on every poll")
	must.Contains(out, "sandbox="+testK8sPodName)
	must.Contains(out, "sandbox Ready False (DependenciesNotReady: Pod is not ready)")
	must.Contains(out, "last waiting reason ContainerCreating")
}

func TestWaitForSandboxReady_ChecksTheFirstPodAgainstTheTemplate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(pod *corev1.Pod)
		wantErr string
	}{
		{
			name:    "runtime class rewritten",
			mutate:  func(pod *corev1.Pod) { pod.Spec.RuntimeClassName = ptr.To("runc") },
			wantErr: `spec.runtimeClassName: expected "gvisor", observed "runc"`,
		},
		{
			name:    "runtime class removed",
			mutate:  func(pod *corev1.Pod) { pod.Spec.RuntimeClassName = nil },
			wantErr: `spec.runtimeClassName: expected "gvisor", observed unset`,
		},
		{
			name:    "policy-selected label rewritten",
			mutate:  func(pod *corev1.Pod) { pod.Labels[k8sPodNameLabel] = "other" },
			wantErr: `metadata.labels[` + k8sPodNameLabel + `]: expected "` + testK8sPodName + `", observed "other"`,
		},
		{
			name:    "sandbox marker removed",
			mutate:  func(pod *corev1.Pod) { delete(pod.Labels, k8sSandboxLabel) },
			wantErr: `metadata.labels[` + k8sSandboxLabel + `]: expected "true", observed absent`,
		},
		{
			name:    "no UID",
			mutate:  func(pod *corev1.Pod) { pod.UID = "" },
			wantErr: "metadata.uid: expected a server-assigned UID, observed none",
		},
		{
			name:    "no controller reference",
			mutate:  func(pod *corev1.Pod) { pod.OwnerReferences = nil },
			wantErr: `metadata.ownerReferences[controller]: expected Sandbox "` + testK8sPodName + `" (UID uid-sandbox), observed none`,
		},
		{
			name: "controlled by another Sandbox",
			mutate: func(pod *corev1.Pod) {
				pod.OwnerReferences = []metav1.OwnerReference{sandboxControllerRef(testK8sPodName, "uid-other")}
			},
			wantErr: `observed agents.x-k8s.io/v1alpha1 Sandbox "` + testK8sPodName + `" (UID uid-other)`,
		},
		{
			name: "controlled by another kind",
			mutate: func(pod *corev1.Pod) {
				ref := sandboxControllerRef(testK8sPodName, testSandboxUID)
				ref.APIVersion, ref.Kind = "apps/v1", "ReplicaSet"
				pod.OwnerReferences = []metav1.OwnerReference{ref}
			},
			wantErr: `observed apps/v1 ReplicaSet "` + testK8sPodName + `" (UID uid-sandbox)`,
		},
		{
			name: "foreign image and security context",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].Image = "attacker.example/collector:latest"
				pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: ptr.To(int64(0)), AllowPrivilegeEscalation: ptr.To(true)}
			},
			wantErr: "spec.containers[agent].image",
		},
		{
			name:    "service account token mounted",
			mutate:  func(pod *corev1.Pod) { pod.Spec.AutomountServiceAccountToken = ptr.To(true) },
			wantErr: "spec.automountServiceAccountToken",
		},
		{
			name: "volume injected",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "injected", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
			},
			wantErr: "spec.volumes",
		},
		{
			name:    "service links enabled",
			mutate:  func(pod *corev1.Pod) { pod.Spec.EnableServiceLinks = ptr.To(true) },
			wantErr: "spec.enableServiceLinks: expected false, observed true",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			pod := backingPod("uid-1")
			tc.mutate(pod)
			c := newSandboxCluster(t, readySandbox(), pod)

			_, elapsed, _, err := c.wait(t, failFastTiming)

			var mismatch *admissionMismatchError
			must.ErrorAs(err, &mismatch)
			must.ErrorContains(err, "admitted pod "+testK8sNamespace+"/"+testK8sPodName+" does not match the submitted pod")
			must.ErrorContains(err, tc.wantErr)
			must.Less(elapsed, 4*time.Second)
		})
	}
	t.Run("controller labels, annotations and API defaulting are accepted", func(t *testing.T) {
		must := require.New(t)
		pod := backingPod("uid-1")
		pod.Labels[sandboxNameHashLabel] = "1234abcd"
		pod.Annotations = map[string]string{"controller.example/managed": "true"}
		pod.Spec.DNSPolicy = corev1.DNSClusterFirst
		pod.Spec.SchedulerName = corev1.DefaultSchedulerName
		pod.Spec.NodeName = "gvisor-node-1"
		pod.Spec.Containers[0].ImagePullPolicy = corev1.PullAlways
		pod.Spec.Containers[0].TerminationMessagePath = corev1.TerminationMessagePathDefault
		pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr.To(int64(300))})
		c := newSandboxCluster(t, readySandbox(), pod)

		_, _, _, err := c.wait(t, fastK8sTiming)

		must.NoError(err)
	})
}

func TestWaitForSandboxReady_ChecksLaterPodsAgainstTheFirst(t *testing.T) {
	cases := []struct {
		name    string
		replace func(pod *corev1.Pod)
		wantErr string
	}{
		{
			name:    "pod replaced under the same name",
			replace: func(pod *corev1.Pod) { pod.UID = "uid-2" },
			wantErr: "metadata.uid: expected uid-1, observed uid-2",
		},
		{
			name:    "runtime class changed after the first observation",
			replace: func(pod *corev1.Pod) { pod.Spec.RuntimeClassName = ptr.To("runc") },
			wantErr: `spec.runtimeClassName: expected "gvisor", observed "runc"`,
		},
		{
			name:    "image changed after the first observation",
			replace: func(pod *corev1.Pod) { pod.Spec.Containers[0].Image = "attacker.example/collector:latest" },
			wantErr: "spec.containers[agent].image",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := newSandboxCluster(t, readySandbox(), pendingBackingPod("uid-1", agentWaiting("ContainerCreating", "")))
			reads := 0
			c.cs.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
				reads++
				if reads == 2 {
					later := backingPod("uid-1")
					tc.replace(later)
					must.NoError(c.cs.Tracker().Update(podsResource, later, testK8sNamespace))
				}
				return false, nil, nil
			})

			_, _, _, err := c.wait(t, failFastTiming)

			must.ErrorContains(err, "observed pod "+testK8sNamespace+"/"+testK8sPodName+" no longer matches the admitted pod")
			must.ErrorContains(err, tc.wantErr)
		})
	}
}

func TestWaitForSandboxReady_FailsWhenAnObservedObjectDisappears(t *testing.T) {
	t.Run("backing pod", func(t *testing.T) {
		must := require.New(t)
		c := newSandboxCluster(t, readySandbox(), pendingBackingPod("uid-1", agentWaiting("ContainerCreating", "")))
		reads := 0
		c.cs.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
			reads++
			if reads == 2 {
				must.NoError(c.cs.Tracker().Delete(podsResource, testK8sNamespace, testK8sPodName))
			}
			return false, nil, nil
		})

		_, elapsed, _, err := c.wait(t, failFastTiming)

		must.ErrorContains(err, "the pod was deleted during the readiness wait")
		must.Less(elapsed, 4*time.Second)
	})
	t.Run("sandbox", func(t *testing.T) {
		must := require.New(t)
		c := newSandboxCluster(t, sandboxIn("False", "DependenciesNotReady", "Pod is not ready"), nil)
		reads := 0
		c.dyn.PrependReactor("get", "sandboxes", func(clienttesting.Action) (bool, runtime.Object, error) {
			reads++
			if reads == 2 {
				must.NoError(c.dyn.Tracker().Delete(sandboxGVR, testK8sNamespace, testK8sPodName))
			}
			return false, nil, nil
		})

		_, elapsed, _, err := c.wait(t, failFastTiming)

		must.ErrorContains(err, "the sandbox was deleted during the readiness wait")
		must.Less(elapsed, 4*time.Second)
	})
}

func TestWaitForSandboxReady_FailsFastOnRBACDenial(t *testing.T) {
	cases := []struct {
		name     string
		resource string
		err      error
		wantErr  string
	}{
		{name: "sandbox read forbidden", resource: "sandboxes", err: apierrors.NewForbidden(sandboxGVR.GroupResource(), testK8sPodName, errors.New("denied by RBAC")), wantErr: "get sandbox: "},
		{name: "sandbox read unauthorized", resource: "sandboxes", err: apierrors.NewUnauthorized("token expired"), wantErr: "get sandbox: token expired"},
		{name: "pod read forbidden", resource: "pods", err: errForbiddenPods, wantErr: "get pod: "},
		{name: "pod read unauthorized", resource: "pods", err: apierrors.NewUnauthorized("token expired"), wantErr: "get pod: token expired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := newSandboxCluster(t, readySandbox(), backingPod("uid-1"))
			if tc.resource == "pods" {
				failVerb(c.cs, "get", "pods", tc.err)
			} else {
				failReadsUntil(c.dyn, "sandboxes", 1<<30, tc.err)
			}

			_, elapsed, _, err := c.wait(t, failFastTiming)

			must.ErrorIs(err, errPodNotReady)
			must.ErrorIs(err, tc.err)
			must.ErrorContains(err, tc.wantErr)
			must.Less(elapsed, 4*time.Second)
		})
	}
}

func TestWaitForSandboxReady_RejectsAWarmPoolPod(t *testing.T) {
	must := require.New(t)
	sandbox := sandboxIn("False", "DependenciesNotReady", "Pod is not ready")
	sandbox.SetAnnotations(map[string]string{sandboxPodNameAnnotation: "warm-pool-pod"})
	c := newSandboxCluster(t, sandbox, nil)

	_, elapsed, _, err := c.wait(t, failFastTiming)

	must.ErrorContains(err, `resolved to backing pod "warm-pool-pod": warm-pool adoption is not supported`)
	must.Less(elapsed, 4*time.Second)
	must.Zero(countVerb(c.cs, "get", "pods"), "a pod that cannot be the backing pod was read")
}

func TestNewAgentSandboxExecutor_FailsFastAndRollsBackOnAnUnusableBackingPod(t *testing.T) {
	cases := []struct {
		name      string
		mutatePod func(pod *corev1.Pod)
		wantErr   string
	}{
		{
			name: "invalid image name",
			mutatePod: func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodPending
				pod.Status.ContainerStatuses = agentWaiting("InvalidImageName", "")
			},
			wantErr: "not ready: agent container cannot start: InvalidImageName",
		},
		{
			name:      "runtime class rewritten",
			mutatePod: func(pod *corev1.Pod) { pod.Spec.RuntimeClassName = ptr.To("runc") },
			wantErr:   `spec.runtimeClassName: expected "gvisor", observed "runc"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			f := newFakeAgentSandbox(t)
			f.mutatePod = tc.mutatePod
			f.admitSandboxes(nil)
			f.lingerOnDelete(t, 3)

			exec, _, err := f.build(t, failFastTiming)

			must.Nil(exec)
			name := f.sandboxName(t)
			must.ErrorContains(err, "sandbox "+testK8sNamespace+"/"+name+" not ready")
			must.ErrorContains(err, tc.wantErr)
			f.requireSandboxRemovedThenPolicy(t, name)
		})
	}
}

func TestNewAgentSandboxExecutor_WaitsForExecBeforeReturning(t *testing.T) {
	must := require.New(t)
	f := newFakeAgentSandbox(t)
	f.admitSandboxes(nil)
	f.lingerOnDelete(t, 3)
	calls := 0
	f.execFn = scriptedExec(&calls, errKubeletUnreachable, nil)

	exec, _, err := f.build(t, fastK8sTiming)

	must.NoError(err)
	must.Equal(3, calls, "two reachability probes, then the scratch setup")
	must.Empty(f.sandboxDeletes())
	must.NoError(exec.Close())
}
