package executor

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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
		Image:     "busybox",
		Namespace: testK8sNamespace,
		Runtime:   runtimeClass,
		Network:   &types.NetworkConfig{Mode: "none"},
		Lifecycle: Lifecycle{RunID: testK8sRunID},
	}
}

// admitPods makes the fake API server assign a UID to every created Pod, add
// the RuntimeClass marks a profile requires (as the RuntimeClass admission
// plugin does for the platform's class), let mutate rewrite it as admission
// might, and report it Running and Ready.
func admitPods(cs *fake.Clientset, mutate func(pod *corev1.Pod)) {
	cs.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pod := action.(clienttesting.CreateAction).GetObject().(*corev1.Pod)
		pod.UID = "uid-1"
		pod.Status = readyPod("uid-1").Status
		applyRuntimeClassMarks(pod)
		if mutate != nil {
			mutate(pod)
		}
		return false, nil, nil
	})
}

// applyRuntimeClassMarks adds the overhead and node-selector marks of the
// profile whose RuntimeClass the Pod names.
func applyRuntimeClassMarks(pod *corev1.Pod) {
	if pod.Spec.RuntimeClassName == nil {
		return
	}
	for _, profile := range k8sRuntimeProfiles {
		if profile.runtimeClassName != *pod.Spec.RuntimeClassName {
			continue
		}
		for _, name := range profile.requiredOverheadResources {
			if pod.Spec.Overhead == nil {
				pod.Spec.Overhead = corev1.ResourceList{}
			}
			pod.Spec.Overhead[name] = resource.MustParse("1")
		}
		for k, v := range profile.requiredNodeSelector {
			if pod.Spec.NodeSelector == nil {
				pod.Spec.NodeSelector = map[string]string{}
			}
			pod.Spec.NodeSelector[k] = v
		}
		return
	}
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
	exec, _, err := newLoggedFakeK8sExecutor(ctx, cs, cfg)
	return exec, err
}

// newLoggedFakeK8sExecutor is newFakeK8sExecutor that also returns the log.
func newLoggedFakeK8sExecutor(ctx context.Context, cs *fake.Clientset, cfg K8sExecutorConfig) (*K8sExecutor, *syncBuffer, error) {
	logger, logs := testLogger()
	profile, proxyEnv, err := prepareK8sExecutor(cfg)
	if err != nil {
		return nil, logs, err
	}
	exec, err := newK8sExecutor(ctx, cfg, profile, proxyEnv, cs, nil, nil, fastK8sTiming, logger)
	return exec, logs, err
}

// newFakeK8sExecutorWithExec is newFakeK8sExecutor with pods/exec served by
// execFn.
func newFakeK8sExecutorWithExec(ctx context.Context, cs *fake.Clientset, cfg K8sExecutorConfig, execFn podExecFunc) (*K8sExecutor, error) {
	logger, _ := testLogger()
	profile, proxyEnv, err := prepareK8sExecutor(cfg)
	if err != nil {
		return nil, err
	}
	return newK8sExecutor(ctx, cfg, profile, proxyEnv, cs, nil, execFn, fastK8sTiming, logger)
}

// nitroCfg returns a k8s executor config that meets the nitro-isolated
// runtime's requirements.
func nitroCfg() K8sExecutorConfig {
	cfg := constructorCfg("nitro-isolated")
	cfg.Resources = &types.ResourceLimits{CPUs: 1, MemoryMB: 1024}
	return cfg
}

func TestNewK8sExecutor_RejectsUnsupportedConfigBeforeAnyClusterCall(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(cfg *K8sExecutorConfig)
		wantErr string
	}{
		{
			name:    "runtime without a profile",
			mutate:  func(cfg *K8sExecutorConfig) { cfg.Runtime = "isolated-sandbox" },
			wantErr: `unsupported executor.runtime "isolated-sandbox"`,
		},
		{
			name:    "confidential runtime",
			mutate:  func(cfg *K8sExecutorConfig) { cfg.Runtime = "confidential-sandbox" },
			wantErr: `unsupported executor.runtime "confidential-sandbox"`,
		},
		{
			name: "nitro-isolated with allowlist egress",
			mutate: func(cfg *K8sExecutorConfig) {
				cfg.Network = &types.NetworkConfig{Mode: "allowlist", Allowlist: []string{"example.com"}}
				cfg.EgressProxyURL = "http://stirrup-egress-proxy:8080"
			},
			wantErr: `executor.network.mode must be "none" for executor.runtime="nitro-isolated" (got "allowlist")`,
		},
		{
			name:    "nitro-isolated without resources",
			mutate:  func(cfg *K8sExecutorConfig) { cfg.Resources = nil },
			wantErr: `executor.resources.cpus must be positive`,
		},
		{
			name:    "nitro-isolated without memory",
			mutate:  func(cfg *K8sExecutorConfig) { cfg.Resources.MemoryMB = 0 },
			wantErr: `executor.resources.memoryMb must be positive`,
		},
		{
			name:    "nitro-isolated with a disk limit",
			mutate:  func(cfg *K8sExecutorConfig) { cfg.Resources.DiskMB = 512 },
			wantErr: `executor.resources.diskMb must be unset`,
		},
		{
			name:    "nitro-isolated with an empty network mode",
			mutate:  func(cfg *K8sExecutorConfig) { cfg.Network = &types.NetworkConfig{} },
			wantErr: `unsupported network mode ""`,
		},
		{
			name:    "nitro-isolated with an unknown network mode",
			mutate:  func(cfg *K8sExecutorConfig) { cfg.Network = &types.NetworkConfig{Mode: "bogus"} },
			wantErr: `unsupported network mode "bogus"`,
		},
		{
			name:    "nitro-isolated with infinite cpus",
			mutate:  func(cfg *K8sExecutorConfig) { cfg.Resources.CPUs = math.Inf(1) },
			wantErr: `executor.resources.cpus must be a finite number (got +Inf)`,
		},
		{
			name: "gvisor with not-a-number cpus",
			mutate: func(cfg *K8sExecutorConfig) {
				cfg.Runtime = "gvisor"
				cfg.Resources.CPUs = math.NaN()
			},
			wantErr: `executor.resources.cpus must be a finite number (got NaN)`,
		},
		{
			name:    "nitro-isolated with a sandbox identity token",
			mutate:  func(cfg *K8sExecutorConfig) { cfg.SandboxIdentityToken = true },
			wantErr: `executor.sandboxIdentity and executor.gitProxy must be unset`,
		},
		{
			name:    "nitro-isolated with identity environment",
			mutate:  func(cfg *K8sExecutorConfig) { cfg.ExtraEnv = []EnvPair{{Name: "HAYBALE_TOKEN", Value: "x"}} },
			wantErr: `executor.sandboxIdentity and executor.gitProxy must be unset`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			cfg := nitroCfg()
			tc.mutate(&cfg)
			cs := fake.NewClientset()

			_, err := newFakeK8sExecutor(context.Background(), cs, cfg)

			must.ErrorContains(err, tc.wantErr)
			must.Empty(cs.Actions(), "a rejected config reached the cluster")
		})
	}
}

func TestNewK8sExecutor_MissingNitroRuntimeClassNamesTheClassAndEKS(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset()
	failVerb(cs, "create", "pods", apierrors.NewForbidden(podsResource.GroupResource(), testK8sPodName,
		errors.New(`pod rejected: RuntimeClass "isolated-sandbox" not found`)))

	_, err := newFakeK8sExecutor(context.Background(), cs, nitroCfg())

	must.ErrorContains(err, `RuntimeClass "isolated-sandbox" was rejected by the cluster`)
	must.ErrorContains(err, K8sRuntimeClassMissingHint("nitro-isolated"))
	must.False(trackerHas(t, cs, networkPoliciesResource, networkPolicyName(createdPodName(t, cs))), "policy left behind")
}

// The public constructor applies the same checks before it builds a client,
// so a rejected config never needs a reachable cluster or kubeconfig.
func TestNewK8sExecutor_RejectsUnsupportedRuntimeBeforeBuildingAClient(t *testing.T) {
	must := require.New(t)
	cfg := nitroCfg()
	cfg.Resources = nil
	cfg.Kubeconfig = "/nonexistent/kubeconfig"

	_, err := NewK8sExecutor(context.Background(), cfg)

	must.ErrorContains(err, "executor.resources.cpus must be positive")
	must.NotContains(err.Error(), "kube rest config")

	cfg.Resources = &types.ResourceLimits{CPUs: math.Inf(1), MemoryMB: 1024}
	_, err = NewK8sExecutor(context.Background(), cfg)
	must.ErrorContains(err, "executor.resources.cpus must be a finite number (got +Inf)")
	must.NotContains(err.Error(), "kube rest config")
}

// requireTornDown asserts the constructor deleted the Pod gracefully under
// its UID and issued the policy delete after the Pod delete. It checks call
// order, not termination; lingerOnDelete covers the wait.
func requireTornDown(t *testing.T, cs *fake.Clientset) {
	t.Helper()
	must := require.New(t)
	podName := createdPodName(t, cs)
	deletes := podDeletes(cs)
	must.NotEmpty(deletes, "pod was not deleted")
	must.Equal(ptr.To(k8sTerminationGracePeriod), deletes[0].GracePeriodSeconds)
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

	lingering := lingerOnDelete(t, cs, 3)

	exec, err := newFakeK8sExecutor(context.Background(), cs, constructorCfg("gvisor"))
	must.NoError(err)
	must.EqualValues("uid-1", exec.podUID)
	must.Equal(createdPodName(t, cs), exec.podName)
	must.Equal(networkPolicyName(exec.podName), exec.networkPolicyName)

	must.NoError(exec.Close())
	requireTornDown(t, cs)
	requireRemovedAfterRelease(t, lingering)

	deletes := len(podDeletes(cs))
	must.NoError(exec.Close(), "second Close")
	must.Len(podDeletes(cs), deletes, "a second Close issued another delete")
}

func TestDefaultK8sTiming_WaitsFiveMinutesForReadiness(t *testing.T) {
	must := require.New(t)
	must.Equal(5*time.Minute, defaultK8sTiming.readyTimeout)
}

func TestNewK8sExecutor_TearsDownOnAdmissionMismatch(t *testing.T) {
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
			name:    "selector label rewritten",
			mutate:  func(pod *corev1.Pod) { pod.Labels[k8sPodNameLabel] = "unselected" },
			wantErr: `metadata.labels[stirrup.dev/pod]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			cs := fake.NewClientset()
			admitPods(cs, tc.mutate)
			lingering := lingerOnDelete(t, cs, 3)

			_, err := newFakeK8sExecutor(context.Background(), cs, constructorCfg("gvisor"))

			must.ErrorContains(err, tc.wantErr)
			must.Zero(lingering.readsBeforeDelete, "readiness wait ran after an admission mismatch")
			requireTornDown(t, cs)
			requireRemovedAfterRelease(t, lingering)
		})
	}
}

func TestNewK8sExecutor_LogsDefaultedRuntimeClass(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset()
	admitPods(cs, func(pod *corev1.Pod) { pod.Spec.RuntimeClassName = ptr.To("cluster-default") })

	exec, logs, err := newLoggedFakeK8sExecutor(context.Background(), cs, constructorCfg(""))
	must.NoError(err, "a RuntimeClass the cluster assigns when none was requested is not a mismatch")
	for _, want := range []string{
		"level=INFO",
		"k8s executor pod admitted with a cluster-assigned RuntimeClass",
		"namespace=" + testK8sNamespace,
		"pod=" + exec.podName,
		"runtimeClassName=cluster-default",
	} {
		must.Contains(logs.String(), want)
	}
	must.NoError(exec.Close())
}

func TestNewK8sExecutor_TearsDownWhenUIDChangesBeforeReady(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset()
	admitPods(cs, nil)
	lingering := lingerOnDelete(t, cs, 3)
	cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if lingering.terminating {
			return false, nil, nil
		}
		replaced := readyPod("uid-2")
		replaced.Name = action.(clienttesting.GetAction).GetName()
		replaced.Labels = podLabels(replaced.Name, testK8sRunID)
		return true, replaced, nil
	})

	_, err := newFakeK8sExecutor(context.Background(), cs, constructorCfg("gvisor"))

	must.ErrorIs(err, errPodNotReady)
	must.ErrorContains(err, "metadata.uid: expected uid-1, observed uid-2")
	requireTornDown(t, cs)
	requireRemovedAfterRelease(t, lingering)
}

func TestNewK8sExecutor_TearsDownOnReadinessFailure(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset()
	admitPods(cs, func(pod *corev1.Pod) {
		pod.Status.Phase = corev1.PodFailed
		pod.Status.Reason = "Evicted"
	})
	lingering := lingerOnDelete(t, cs, 3)

	_, err := newFakeK8sExecutor(context.Background(), cs, constructorCfg("gvisor"))

	must.ErrorIs(err, errPodNotReady)
	must.ErrorContains(err, "pod "+testK8sNamespace+"/"+createdPodName(t, cs)+" not ready: pod phase Failed (Evicted")
	requireTornDown(t, cs)
	requireRemovedAfterRelease(t, lingering)
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

// createPersistedThenFail makes the fake API server persist the submitted Pod,
// optionally relabelled, and then answer the create with err, as a client
// sees when a retried request's first attempt was persisted.
func createPersistedThenFail(t *testing.T, cs *fake.Clientset, err error, labels map[string]string) {
	t.Helper()
	must := require.New(t)
	cs.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pod := action.(clienttesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
		pod.UID = "uid-1"
		if labels != nil {
			pod.Labels = labels
		}
		must.NoError(cs.Tracker().Add(pod))
		return true, nil, err
	})
}

func TestNewK8sExecutor_ReconcilesEveryCreateError(t *testing.T) {
	quota := apierrors.NewForbidden(podsResource.GroupResource(), "stirrup-x", errors.New(`exceeded quota: compute, requested: limits.cpu=2, used: limits.cpu=8, limited: limits.cpu=8`))

	t.Run("definitive rejection with no pod removes the policy", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		cs := fake.NewClientset()
		failVerb(cs, "create", "pods", quota)

		_, err := newFakeK8sExecutor(context.Background(), cs, constructorCfg("gvisor"))

		must.ErrorIs(err, quota)
		check.NotContains(err.Error(), "kubectl get runtimeclass", "a quota rejection was blamed on the RuntimeClass")
		check.Equal(1, countVerb(cs, "get", "pods"), "the create error was not reconciled")
		check.Empty(podDeletes(cs), "no pod exists after a definitive rejection")
		check.Equal(1, countVerb(cs, "delete", "networkpolicies"))
		check.False(trackerHas(t, cs, networkPoliciesResource, networkPolicyName(createdPodName(t, cs))))
	})

	t.Run("definitive rejection with the pod present tears it down", func(t *testing.T) {
		must := require.New(t)
		cs := fake.NewClientset()
		createPersistedThenFail(t, cs, quota, nil)
		lingering := lingerOnDelete(t, cs, 3)

		_, err := newFakeK8sExecutor(context.Background(), cs, constructorCfg("gvisor"))

		must.ErrorIs(err, quota)
		requireTornDown(t, cs)
		requireRemovedAfterRelease(t, lingering)
	})

	ambiguous := map[string]error{
		"server timeout":    apierrors.NewServerTimeout(podsResource.GroupResource(), "create", 1),
		"request timeout":   apierrors.NewTimeoutError("timed out", 1),
		"already exists":    apierrors.NewAlreadyExists(podsResource.GroupResource(), "stirrup-x"),
		"too many requests": apierrors.NewTooManyRequests("slow down", 1),
		"internal error":    apierrors.NewInternalError(errors.New("webhook failed")),
		"transport error":   errors.New("net/http: request canceled (Client.Timeout exceeded while awaiting headers)"),
		// The fake ignores contexts, so a cancelled caller is modelled by the
		// error a real client returns for one.
		"cancelled context": context.Canceled,
	}
	for name, createErr := range ambiguous {
		t.Run(name+" with no pod retains the policy", func(t *testing.T) {
			must := require.New(t)
			cs := fake.NewClientset()
			failVerb(cs, "create", "pods", createErr)

			_, logs, err := newLoggedFakeK8sExecutor(context.Background(), cs, constructorCfg("gvisor"))

			podName := createdPodName(t, cs)
			must.ErrorIs(err, createErr)
			must.ErrorContains(err, "retained egress NetworkPolicy "+testK8sNamespace+"/"+networkPolicyName(podName))
			must.ErrorContains(err, testK8sRunID)
			must.Equal(1, countVerb(cs, "get", "pods"), "the create error was not reconciled")
			must.Empty(podDeletes(cs))
			must.True(trackerHas(t, cs, networkPoliciesResource, networkPolicyName(podName)))
			must.Contains(logs.String(), "level=WARN")
		})

		t.Run(name+" with the pod present tears it down", func(t *testing.T) {
			must := require.New(t)
			cs := fake.NewClientset()
			createPersistedThenFail(t, cs, createErr, nil)
			lingering := lingerOnDelete(t, cs, 3)

			_, err := newFakeK8sExecutor(context.Background(), cs, constructorCfg("gvisor"))

			must.ErrorIs(err, createErr)
			requireTornDown(t, cs)
			requireRemovedAfterRelease(t, lingering)
		})
	}

	t.Run("already exists under a pod without this run's labels", func(t *testing.T) {
		must := require.New(t)
		cs := fake.NewClientset()
		createPersistedThenFail(t, cs, apierrors.NewAlreadyExists(podsResource.GroupResource(), "stirrup-x"), map[string]string{"app": "other"})

		_, err := newFakeK8sExecutor(context.Background(), cs, constructorCfg("gvisor"))

		podName := createdPodName(t, cs)
		must.ErrorContains(err, "does not carry this run's label")
		must.ErrorContains(err, "retained egress NetworkPolicy")
		must.Empty(podDeletes(cs), "a pod this run cannot prove it owns was deleted")
		must.True(trackerHas(t, cs, podsResource, podName))
		must.True(trackerHas(t, cs, networkPoliciesResource, networkPolicyName(podName)))
	})

	t.Run("owned pod whose delete is forbidden", func(t *testing.T) {
		must := require.New(t)
		cs := fake.NewClientset()
		createPersistedThenFail(t, cs, apierrors.NewServerTimeout(podsResource.GroupResource(), "create", 1), nil)
		failVerb(cs, "delete", "pods", errForbiddenPods)

		_, err := newFakeK8sExecutor(context.Background(), cs, constructorCfg("gvisor"))

		must.ErrorContains(err, "denied by RBAC")
		must.ErrorContains(err, "retained egress NetworkPolicy")
		must.True(trackerHas(t, cs, networkPoliciesResource, networkPolicyName(createdPodName(t, cs))))
	})

	t.Run("lookup retried after a transient error", func(t *testing.T) {
		must := require.New(t)
		cs := fake.NewClientset()
		createPersistedThenFail(t, cs, apierrors.NewTimeoutError("timed out", 1), nil)
		failures := 0
		cs.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
			if failures == 0 {
				failures++
				return true, nil, errors.New("connection reset")
			}
			return false, nil, nil
		})

		_, err := newFakeK8sExecutor(context.Background(), cs, constructorCfg("gvisor"))

		must.Error(err)
		must.NotContains(err.Error(), "retained")
		requireTornDown(t, cs)
	})
}
