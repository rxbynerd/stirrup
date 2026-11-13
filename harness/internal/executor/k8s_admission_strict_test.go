package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/rxbynerd/stirrup/types"
)

// TestStrictAdmissionFields_ClassifyEveryField pins the classification
// tables to the client-go structs in both directions, so a new field forces
// a decision and a removed one cannot linger.
func TestStrictAdmissionFields_ClassifyEveryField(t *testing.T) {
	cases := []struct {
		name  string
		typ   reflect.Type
		table map[string]admissionField
	}{
		{name: "pod spec", typ: reflect.TypeOf(corev1.PodSpec{}), table: strictPodSpecFields},
		{name: "container", typ: reflect.TypeOf(corev1.Container{}), table: strictContainerFields},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			fields := map[string]bool{}
			for _, f := range reflect.VisibleFields(tc.typ) {
				name := f.Name
				fields[name] = true
				must.Contains(tc.table, name, "%s.%s is not classified", tc.typ.Name(), name)
			}
			for name, field := range tc.table {
				must.True(fields[name], "the table classifies %s.%s, which the struct does not have", tc.typ.Name(), name)
				must.NotZero(field.rule, "%s has no rule", name)
				if field.rule != fieldCompared {
					must.NotEmpty(field.why, "%s is not compared and gives no reason", name)
				}
			}
		})
	}
}

// nitroFixtureCfg is the config whose submitted Pod the admitted-Pod
// fixture models.
func nitroFixtureCfg() K8sExecutorConfig {
	return K8sExecutorConfig{
		Image:        "public.ecr.aws/docker/library/busybox@sha256:66a6306db78bf2dbf3487f293aa8d6990d8e506fdffab9cc43fe422becf886e4",
		Namespace:    testK8sNamespace,
		NodeSelector: map[string]string{"kubernetes.io/arch": "amd64"},
		Runtime:      "nitro-isolated",
		Resources:    &types.ResourceLimits{CPUs: 1, MemoryMB: 1024},
		Network:      &types.NetworkConfig{Mode: "none"},
		Lifecycle:    Lifecycle{RunID: testK8sRunID, MaxLifetime: 10 * time.Minute},
	}
}

// decodeAdmittedNitroPod decodes the admitted-Pod fixture as a client
// decodes an API response; testdata/README.md gives its provenance.
func decodeAdmittedNitroPod(t *testing.T) *corev1.Pod {
	t.Helper()
	must := require.New(t)
	data, err := os.ReadFile(filepath.Join("testdata", "nitro-admitted-pod.json"))
	must.NoError(err)
	obj, err := runtime.Decode(scheme.Codecs.UniversalDeserializer(), data)
	must.NoError(err)
	pod, ok := obj.(*corev1.Pod)
	must.True(ok, "fixture decodes to %T", obj)
	return pod
}

func TestPodAdmission_StrictAcceptsAdmittedNitroPodFixture(t *testing.T) {
	must := require.New(t)
	profile := k8sRuntimeProfiles["nitro-isolated"]
	submitted := buildSandboxPod(nitroFixtureCfg(), profile, testK8sPodName, nil)
	admitted := decodeAdmittedNitroPod(t)

	must.NotEqual(submitted.Spec.Containers[0].Resources, admitted.Spec.Containers[0].Resources,
		"the fixture's quantities must differ structurally so the comparison is shown to be semantic")
	must.Empty(admitted.Spec.Volumes[0].Projected, "no service-account token volume is injected")
	must.Len(admitted.Spec.Volumes, 1)

	admission := newPodAdmission(submitted, admitted, profile)
	must.NoError(admission.checkCreate())
	must.NoError(admission.checkPoll(admitted.DeepCopy()))
}

// strictMutation is an admission change the whole-Pod admission check must
// reject, and the mismatch text it must report.
type strictMutation struct {
	name      string
	mutate    func(pod *corev1.Pod)
	wantField string
}

// strictAdmissionMutations are named rewrites of the nitro-isolated Pod
// built from nitroFixtureCfg: the vectors the check exists for.
func strictAdmissionMutations() []strictMutation {
	return []strictMutation{
		{name: "no-network annotation stripped", mutate: func(p *corev1.Pod) { delete(p.Annotations, "sandbox.amazonaws.com/network-isolated") }, wantField: "metadata.annotations[sandbox.amazonaws.com/network-isolated]: expected \"true\", observed absent"},
		{name: "no-network annotation changed", mutate: func(p *corev1.Pod) { p.Annotations["sandbox.amazonaws.com/network-isolated"] = "false" }, wantField: "metadata.annotations[sandbox.amazonaws.com/network-isolated]"},
		{name: "run label rewritten", mutate: func(p *corev1.Pod) { p.Labels[k8sRunIDLabel] = "other" }, wantField: "metadata.labels[" + k8sRunIDLabel + "]"},
		{name: "owner reference added", mutate: func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: "x", UID: "u"}}
		}, wantField: "metadata.ownerReferences"},
		{name: "sidecar", mutate: func(p *corev1.Pod) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "proxy", Image: "proxy"})
		}, wantField: "spec.containers: expected containers [agent], observed containers [agent, proxy]"},
		{name: "init container", mutate: func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "init", Image: "init", Env: []corev1.EnvVar{{Name: "K", Value: "init-secret-value"}}}}
		}, wantField: "spec.initContainers: expected containers [], observed containers [init]"},
		{name: "ephemeral container", mutate: func(p *corev1.Pod) {
			p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug"}}}
		}, wantField: "spec.ephemeralContainers"},
		{name: "injected env", mutate: func(p *corev1.Pod) {
			p.Spec.Containers[0].Env = append(p.Spec.Containers[0].Env, corev1.EnvVar{Name: "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", Value: "/var/run/secrets/pods.eks.amazonaws.com/serviceaccount/eks-pod-identity-token"})
		}, wantField: `spec.containers[agent].env: expected no further variables, observed added variable "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"`},
		{name: "envFrom", mutate: func(p *corev1.Pod) {
			p.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "creds"}}}}
		}, wantField: "spec.containers[agent].envFrom"},
		{name: "injected volume", mutate: func(p *corev1.Pod) {
			p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "eks-pod-identity-token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{}}})
		}, wantField: "spec.volumes"},
		{name: "injected volume mount", mutate: func(p *corev1.Pod) {
			p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "token", MountPath: "/var/run/secrets"})
		}, wantField: "spec.containers[agent].volumeMounts"},
		{name: "changed resources", mutate: func(p *corev1.Pod) {
			p.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("512Mi")
		}, wantField: "spec.containers[agent].resources"},
		{name: "limit range ephemeral storage", mutate: func(p *corev1.Pod) {
			p.Spec.Containers[0].Resources.Limits[corev1.ResourceEphemeralStorage] = resource.MustParse("1Gi")
		}, wantField: "spec.containers[agent].resources"},
		{name: "changed image", mutate: func(p *corev1.Pod) { p.Spec.Containers[0].Image = "mirror.example/busybox:latest" }, wantField: `spec.containers[agent].image: expected "public.ecr.aws`},
		{name: "changed command", mutate: func(p *corev1.Pod) { p.Spec.Containers[0].Command = []string{"/bin/sh", "-c", "evil"} }, wantField: "spec.containers[agent].command"},
		{name: "changed runtime class", mutate: func(p *corev1.Pod) { p.Spec.RuntimeClassName = ptr.To("runc") }, wantField: "spec.runtimeClassName"},
		{name: "changed service account", mutate: func(p *corev1.Pod) { p.Spec.ServiceAccountName = "privileged" }, wantField: `spec.serviceAccountName: expected "default", observed "privileged"`},
		{name: "token automount enabled", mutate: func(p *corev1.Pod) { p.Spec.AutomountServiceAccountToken = ptr.To(true) }, wantField: "spec.automountServiceAccountToken"},
		{name: "changed active deadline", mutate: func(p *corev1.Pod) { p.Spec.ActiveDeadlineSeconds = ptr.To(int64(86400)) }, wantField: "spec.activeDeadlineSeconds: expected 600, observed 86400"},
		{name: "changed termination grace", mutate: func(p *corev1.Pod) { p.Spec.TerminationGracePeriodSeconds = ptr.To(int64(30)) }, wantField: "spec.terminationGracePeriodSeconds"},
		{name: "weakened container security context", mutate: func(p *corev1.Pod) {
			p.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation = ptr.To(true)
		}, wantField: "spec.containers[agent].securityContext"},
		{name: "privileged container", mutate: func(p *corev1.Pod) { p.Spec.Containers[0].SecurityContext.Privileged = ptr.To(true) }, wantField: "spec.containers[agent].securityContext"},
		{name: "weakened pod security context", mutate: func(p *corev1.Pod) { p.Spec.SecurityContext.RunAsUser = ptr.To(int64(0)) }, wantField: "spec.securityContext"},
		{name: "host network", mutate: func(p *corev1.Pod) { p.Spec.HostNetwork = true }, wantField: "spec.hostNetwork"},
		{name: "host users", mutate: func(p *corev1.Pod) { p.Spec.HostUsers = ptr.To(false) }, wantField: "spec.hostUsers"},
		{name: "submitted node selector dropped", mutate: func(p *corev1.Pod) { delete(p.Spec.NodeSelector, "kubernetes.io/arch") }, wantField: "spec.nodeSelector: expected at least"},
		{name: "service links enabled", mutate: func(p *corev1.Pod) { p.Spec.EnableServiceLinks = ptr.To(true) }, wantField: "spec.enableServiceLinks: expected false, observed true"},
		{name: "overhead removed", mutate: func(p *corev1.Pod) { p.Spec.Overhead = nil }, wantField: "spec.overhead[eks.amazonaws.com/nitro-sandbox]: expected present, observed absent"},
		{name: "overhead without the sandbox resource", mutate: func(p *corev1.Pod) {
			p.Spec.Overhead = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}
		}, wantField: "spec.overhead[eks.amazonaws.com/nitro-sandbox]: expected present, observed absent"},
		{name: "runtime class selector removed and toleration added", mutate: func(p *corev1.Pod) {
			delete(p.Spec.NodeSelector, "eks.amazonaws.com/nitro-sandbox")
			p.Spec.Tolerations = append(p.Spec.Tolerations, corev1.Toleration{Operator: corev1.TolerationOpExists})
		}, wantField: `spec.nodeSelector[eks.amazonaws.com/nitro-sandbox]: expected "true", observed absent`},
		{name: "runtime class selector value changed", mutate: func(p *corev1.Pod) { p.Spec.NodeSelector["eks.amazonaws.com/nitro-sandbox"] = "false" }, wantField: `spec.nodeSelector[eks.amazonaws.com/nitro-sandbox]: expected "true", observed "false"`},
		{name: "submitted node selector value changed", mutate: func(p *corev1.Pod) { p.Spec.NodeSelector["kubernetes.io/arch"] = "arm64" }, wantField: "spec.nodeSelector: expected at least"},
		{name: "submitted label removed", mutate: func(p *corev1.Pod) { delete(p.Labels, k8sRunIDLabel) }, wantField: "metadata.labels[" + k8sRunIDLabel + `]: expected "` + testK8sRunID + `", observed absent`},
		{name: "container renamed", mutate: func(p *corev1.Pod) { p.Spec.Containers[0].Name = "other" }, wantField: "spec.containers: expected containers [agent], observed containers [other]"},
	}
}

func TestPodAdmission_StrictRejectsAdmittedPodMutations(t *testing.T) {
	profile := k8sRuntimeProfiles["nitro-isolated"]
	for _, tc := range strictAdmissionMutations() {
		t.Run(tc.name+" at creation", func(t *testing.T) {
			must := require.New(t)
			submitted := buildSandboxPod(nitroFixtureCfg(), profile, testK8sPodName, nil)
			admitted := decodeAdmittedNitroPod(t)
			tc.mutate(admitted)

			err := newPodAdmission(submitted, admitted, profile).checkCreate()

			must.ErrorContains(err, tc.wantField)
			must.NotContains(err.Error(), "init-secret-value")
		})
		t.Run(tc.name+" during the readiness wait", func(t *testing.T) {
			must := require.New(t)
			submitted := buildSandboxPod(nitroFixtureCfg(), profile, testK8sPodName, nil)
			admitted := decodeAdmittedNitroPod(t)
			admission := newPodAdmission(submitted, admitted, profile)
			must.NoError(admission.checkCreate())
			observed := admitted.DeepCopy()
			tc.mutate(observed)

			err := admission.checkPoll(observed)

			must.ErrorContains(err, strings.Split(tc.wantField, ":")[0])
		})
	}
}

func TestPodAdmission_StrictNeverPrintsEnvValues(t *testing.T) {
	must := require.New(t)
	cfg := nitroFixtureCfg()
	profile := k8sRuntimeProfiles["nitro-isolated"]
	submitted := buildSandboxPod(cfg, profile, testK8sPodName, []corev1.EnvVar{{Name: "SUBMITTED", Value: "submitted-secret-value"}})
	admitted := submitted.DeepCopy()
	admitted.UID = "uid-1"
	admitted.Spec.Containers[0].Env[0].Value = "observed-secret-value"

	err := newPodAdmission(submitted, admitted, profile).checkCreate()

	must.ErrorContains(err, "spec.containers[agent].env[SUBMITTED].value: expected the submitted value, observed a different value")
	must.NotContains(err.Error(), "submitted-secret-value")
	must.NotContains(err.Error(), "observed-secret-value")

	admitted.Spec.Containers[0].Env[0].Value = "submitted-secret-value"
	admitted.Spec.Containers[0].Env[0].ValueFrom = &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{Key: "k"}}
	err = newPodAdmission(submitted, admitted, profile).checkCreate()
	must.ErrorContains(err, "spec.containers[agent].env[SUBMITTED].valueFrom: expected the submitted source, observed a different source")
}

func TestPodAdmission_StrictBoundsObservedValues(t *testing.T) {
	must := require.New(t)
	profile := k8sRuntimeProfiles["nitro-isolated"]
	submitted := buildSandboxPod(nitroFixtureCfg(), profile, testK8sPodName, nil)
	admitted := decodeAdmittedNitroPod(t)
	admitted.Spec.Containers[0].Image = "evil\n" + strings.Repeat("x", 4*k8sClusterTextMax)

	err := newPodAdmission(submitted, admitted, profile).checkCreate()

	must.Error(err)
	must.NotContains(err.Error(), "\n")
	must.Less(len(err.Error()), 3*k8sClusterTextMax+300)
}

func TestPodAdmission_StrictBoundsTheNumberOfMismatchesShown(t *testing.T) {
	must := require.New(t)
	profile := k8sRuntimeProfiles["nitro-isolated"]
	submitted, admitted := syntheticAdmittedNitroPod()
	for i := range 5000 {
		admitted.Annotations[fmt.Sprintf("sandbox.amazonaws.com/k%05d", i)] = "x"
	}

	err := newPodAdmission(submitted, admitted, profile).checkCreate()

	must.ErrorContains(err, "metadata.annotations[sandbox.amazonaws.com/k00009]")
	must.NotContains(err.Error(), "sandbox.amazonaws.com/k00010")
	must.ErrorContains(err, "; and 4990 more (5000 mismatches)")
	must.Less(len(err.Error()), 20*k8sClusterTextMax)
}

func TestPodAdmission_BaseProfileIgnoresStrictFields(t *testing.T) {
	must := require.New(t)
	submitted := submittedPod("gvisor")
	admitted := admittedCopy(submitted)
	admitted.Spec.Containers = append(admitted.Spec.Containers, corev1.Container{Name: "sidecar"})
	must.NoError(newPodAdmission(submitted, admitted, k8sRuntimeProfiles["gvisor"]).checkCreate())
}

// nitroConstructorCfg is nitroCfg with a recording security emitter.
func nitroConstructorCfg() (K8sExecutorConfig, *recordingSecurityEmitter) {
	cfg := nitroCfg()
	security := &recordingSecurityEmitter{}
	cfg.Security = security
	return cfg, security
}

// TestNewK8sExecutor_StrictAdmissionMismatchTearsDownAndReports runs every
// named mutation through the constructor: the run fails, the Pod is torn
// down in order, and one admitted_pod event names the first field.
func TestNewK8sExecutor_StrictAdmissionMismatchTearsDownAndReports(t *testing.T) {
	for _, tc := range strictAdmissionMutations() {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			cs := fake.NewClientset()
			admitPods(cs, tc.mutate)
			lingering := lingerOnDelete(t, cs, 3)
			cfg := nitroFixtureCfg()
			security := &recordingSecurityEmitter{}
			cfg.Security = security

			_, err := newFakeK8sExecutor(context.Background(), cs, cfg)

			must.ErrorContains(err, tc.wantField)
			must.NotContains(err.Error(), "init-secret-value")
			must.Zero(lingering.readsBeforeDelete, "readiness wait ran after an admission mismatch")
			requireTornDown(t, cs)
			requireRemovedAfterRelease(t, lingering)
			must.Len(security.runtimeDiagnostics, 1)
			event := security.runtimeDiagnostics[0]
			must.False(event.passed)
			must.Equal("admitted_pod", event.check)
			must.Equal("nitro-isolated", event.runtime)
			must.Contains(event.observed["field"], strings.Split(strings.Split(tc.wantField, ":")[0], "[")[0])
		})
	}
}

func TestNewK8sExecutor_StrictAdmissionRechecksDuringReadiness(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(pod *corev1.Pod)
		wantField string
	}{
		{name: "annotation stripped", mutate: func(p *corev1.Pod) { delete(p.Annotations, "sandbox.amazonaws.com/network-isolated") }, wantField: "metadata.annotations[sandbox.amazonaws.com/network-isolated]"},
		{name: "overhead removed", mutate: func(p *corev1.Pod) { p.Spec.Overhead = nil }, wantField: "spec.overhead[eks.amazonaws.com/nitro-sandbox]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			cs := fake.NewClientset()
			admitPods(cs, nil)
			lingering := lingerOnDelete(t, cs, 3)
			cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
				if lingering.terminating {
					return false, nil, nil
				}
				obj, err := cs.Tracker().Get(podsResource, testK8sNamespace, action.(clienttesting.GetAction).GetName())
				if err != nil {
					return true, nil, err
				}
				changed := obj.(*corev1.Pod).DeepCopy()
				tc.mutate(changed)
				return true, changed, nil
			})
			cfg, security := nitroConstructorCfg()

			_, err := newFakeK8sExecutor(context.Background(), cs, cfg)

			must.ErrorIs(err, errPodNotReady)
			must.ErrorContains(err, "no longer matches the admitted pod: "+tc.wantField)
			requireTornDown(t, cs)
			requireRemovedAfterRelease(t, lingering)
			must.Len(security.runtimeDiagnostics, 1)
			must.Equal("admitted_pod", security.runtimeDiagnostics[0].check)
		})
	}
}

// syntheticAdmittedNitroPod returns the nitro-isolated Pod as submitted and a
// copy with a UID and the RuntimeClass marks added; no read-back underlies
// the copy.
func syntheticAdmittedNitroPod() (*corev1.Pod, *corev1.Pod) {
	submitted := buildSandboxPod(nitroFixtureCfg(), k8sRuntimeProfiles["nitro-isolated"], testK8sPodName, nil)
	admitted := admittedCopy(submitted)
	applyRuntimeClassMarks(admitted)
	return submitted, admitted
}

func TestPodAdmission_StrictSyntheticSandboxAnnotationRules(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(pod *corev1.Pod)
		wantField string
	}{
		{name: "throttle value retuned", mutate: func(p *corev1.Pod) { p.Annotations["sandbox.amazonaws.com/log-bandwidth"] = "40M" }},
		{name: "throttle key added", mutate: func(p *corev1.Pod) { p.Annotations["sandbox.amazonaws.com/overlay-disk-iops"] = "3000" }},
		{name: "annotation outside the prefix added", mutate: func(p *corev1.Pod) { p.Annotations["example.com/injected"] = "true" }},
		{name: "submitted throttle key removed", mutate: func(p *corev1.Pod) { delete(p.Annotations, "sandbox.amazonaws.com/log-bandwidth") }, wantField: "metadata.annotations[sandbox.amazonaws.com/log-bandwidth]: expected present with any value, observed absent"},
		{name: "unknown sandbox key added", mutate: func(p *corev1.Pod) { p.Annotations["sandbox.amazonaws.com/policy"] = "permissive" }, wantField: `metadata.annotations[sandbox.amazonaws.com/policy]: expected absent, observed "permissive"`},
		{name: "no-network annotation changed", mutate: func(p *corev1.Pod) { p.Annotations["sandbox.amazonaws.com/network-isolated"] = "false" }, wantField: `metadata.annotations[sandbox.amazonaws.com/network-isolated]: expected "true", observed "false"`},
		{name: "capitalised prefix added", mutate: func(p *corev1.Pod) { p.Annotations["Sandbox.amazonaws.com/network-isolated"] = "false" }, wantField: `metadata.annotations[Sandbox.amazonaws.com/network-isolated]: expected absent, observed "false"`},
		{name: "upper-case prefix added", mutate: func(p *corev1.Pod) { p.Annotations["SANDBOX.AMAZONAWS.COM/network-isolated"] = "false" }, wantField: `metadata.annotations[SANDBOX.AMAZONAWS.COM/network-isolated]: expected absent, observed "false"`},
		{name: "case variant of a throttle key added", mutate: func(p *corev1.Pod) { p.Annotations["sandbox.amazonaws.com/Log-Bandwidth"] = "1M" }, wantField: `metadata.annotations[sandbox.amazonaws.com/Log-Bandwidth]: expected absent, observed "1M"`},
	}
	profile := k8sRuntimeProfiles["nitro-isolated"]
	for _, tc := range cases {
		t.Run(tc.name+" at creation", func(t *testing.T) {
			must := require.New(t)
			submitted, admitted := syntheticAdmittedNitroPod()
			tc.mutate(admitted)

			err := newPodAdmission(submitted, admitted, profile).checkCreate()

			if tc.wantField == "" {
				must.NoError(err)
				return
			}
			must.ErrorContains(err, tc.wantField)
		})
		t.Run(tc.name+" during the readiness wait", func(t *testing.T) {
			must := require.New(t)
			submitted, admitted := syntheticAdmittedNitroPod()
			admission := newPodAdmission(submitted, admitted, profile)
			must.NoError(admission.checkCreate())
			observed := admitted.DeepCopy()
			tc.mutate(observed)

			err := admission.checkPoll(observed)

			if tc.wantField == "" {
				must.NoError(err)
				return
			}
			must.ErrorContains(err, tc.wantField)
		})
	}
}

func TestK8sRuntimeProfiles_StrictProfileAnnotationsAreGoverned(t *testing.T) {
	for runtime, profile := range k8sRuntimeProfiles {
		if !profile.strictAdmission {
			continue
		}
		t.Run(runtime, func(t *testing.T) {
			must := require.New(t)
			must.NotEmpty(profile.governedAnnotationPrefix)
			for _, annotations := range []map[string]string{profile.podAnnotations, profile.noNetworkAnnotations} {
				for key := range annotations {
					must.True(strings.HasPrefix(key, profile.governedAnnotationPrefix), "%s is not under %s, so admission could change it unnoticed", key, profile.governedAnnotationPrefix)
				}
			}
			for key := range profile.noNetworkAnnotations {
				must.NotContains(profile.retunableAnnotations, key, "a no-network annotation must keep its value")
			}
		})
	}
}

// A readiness failure that is not an admission mismatch is an ordinary
// start failure, not a runtime diagnostic.
func TestNewK8sExecutor_StrictReadinessFailureEmitsNoDiagnostic(t *testing.T) {
	cases := []struct {
		name            string
		cancelOnPodRead bool
	}{
		{name: "readiness timeout"},
		{name: "caller cancels during the wait", cancelOnPodRead: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			cs := fake.NewClientset()
			admitPods(cs, func(p *corev1.Pod) { p.Status = corev1.PodStatus{Phase: corev1.PodPending} })
			cfg, security := nitroConstructorCfg()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelOnPodRead {
				cs.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
					cancel()
					return false, nil, nil
				})
			}

			_, err := newFakeK8sExecutor(ctx, cs, cfg)

			must.ErrorIs(err, errPodNotReady)
			must.Empty(security.runtimeDiagnostics)
			requireTornDown(t, cs)
		})
	}
}

func TestK8sRuntimeProfiles_PlatformMarksOnlyForNitroIsolated(t *testing.T) {
	must := require.New(t)
	nitro := k8sRuntimeProfiles["nitro-isolated"]
	must.Equal([]corev1.ResourceName{"eks.amazonaws.com/nitro-sandbox"}, nitro.requiredOverheadResources)
	must.Equal(map[string]string{"eks.amazonaws.com/nitro-sandbox": "true"}, nitro.requiredNodeSelector)
	for runtime, profile := range k8sRuntimeProfiles {
		if runtime == "nitro-isolated" {
			continue
		}
		must.Empty(profile.requiredOverheadResources, runtime)
		must.Empty(profile.requiredNodeSelector, runtime)
	}
}

func TestPodAdmission_BaseProfilesAcceptAPodWithoutPlatformMarks(t *testing.T) {
	for _, runtime := range []string{"", "gvisor"} {
		t.Run("runtime "+runtime, func(t *testing.T) {
			must := require.New(t)
			cfg := constructorCfg(runtime)
			profile := k8sRuntimeProfiles[runtime]
			submitted := buildSandboxPod(cfg, profile, testK8sPodName, nil)
			admitted := admittedCopy(submitted)
			must.Nil(admitted.Spec.Overhead)

			admission := newPodAdmission(submitted, admitted, profile)

			must.NoError(admission.checkCreate())
			must.NoError(admission.checkPoll(admitted.DeepCopy()))
		})
	}
}

func TestNewK8sExecutor_BaseAdmissionMismatchEmitsNoDiagnostic(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset()
	admitPods(cs, func(p *corev1.Pod) { p.Spec.RuntimeClassName = ptr.To("runc") })
	cfg := constructorCfg("gvisor")
	security := &recordingSecurityEmitter{}
	cfg.Security = security

	_, err := newFakeK8sExecutor(context.Background(), cs, cfg)

	must.ErrorContains(err, "spec.runtimeClassName")
	must.Empty(security.runtimeDiagnostics)
}

// mutateForAdmission changes v in place to a value the semantic comparison
// sees as different, and reports false for a kind it cannot synthesise.
func mutateForAdmission(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Pointer:
		changed := reflect.New(v.Type().Elem())
		if !v.IsNil() {
			changed.Elem().Set(v.Elem())
			if !mutateForAdmission(changed.Elem()) {
				return false
			}
		}
		v.Set(changed)
	case reflect.String:
		v.SetString(v.String() + "-changed")
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.Int, reflect.Int32, reflect.Int64:
		v.SetInt(v.Int() + 1)
	case reflect.Slice:
		v.Set(reflect.Append(v, reflect.Zero(v.Type().Elem())))
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return false
		}
		if v.IsNil() {
			v.Set(reflect.MakeMap(v.Type()))
		}
		v.SetMapIndex(reflect.ValueOf("stirrup-test").Convert(v.Type().Key()), reflect.Zero(v.Type().Elem()))
	default:
		return false
	}
	return true
}

// explicitPodSpecChanges and explicitContainerChanges change the compared
// fields whose fixture value mutateForAdmission cannot synthesise a change
// for: a set struct.
var explicitPodSpecChanges = map[string]func(spec *corev1.PodSpec){
	"SecurityContext": func(spec *corev1.PodSpec) { spec.SecurityContext.RunAsGroup = ptr.To(int64(0)) },
}

var explicitContainerChanges = map[string]func(c *corev1.Container){
	"SecurityContext": func(c *corev1.Container) { c.SecurityContext.ReadOnlyRootFilesystem = ptr.To(true) },
	"Resources": func(c *corev1.Container) {
		c.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("2")
	},
}

// comparedFieldExceptions are compared fields whose change another check
// reports first, so the table entry alone is not what catches it.
var comparedFieldExceptions = map[string]string{
	"pod.RuntimeClassName": "checkRuntimeClassKept, a base check, reports it at both stages",
	"container.Name":       "firstContainersMismatch reports a renamed container as a containers mismatch",
}

// wantUncomparedFields lists, independently of the classification tables,
// the fields the whole-Pod admission check must not compare, with the rule
// each follows. Every other PodSpec and Container field must be compared, so
// reclassifying a field in either direction breaks a test.
var wantUncomparedFields = map[string]admissionFieldRule{
	"pod.DNSPolicy":                      fieldPlatformOwned,
	"pod.DeprecatedServiceAccount":       fieldPlatformOwned,
	"pod.NodeName":                       fieldPlatformOwned,
	"pod.ImagePullSecrets":               fieldPlatformOwned,
	"pod.SchedulerName":                  fieldPlatformOwned,
	"pod.PriorityClassName":              fieldPlatformOwned,
	"pod.Priority":                       fieldPlatformOwned,
	"pod.PreemptionPolicy":               fieldPlatformOwned,
	"pod.Overhead":                       fieldPlatformOwned,
	"pod.NodeSelector":                   fieldSubmittedSubset,
	"pod.Tolerations":                    fieldSubmittedSubset,
	"container.TerminationMessagePath":   fieldPlatformOwned,
	"container.TerminationMessagePolicy": fieldPlatformOwned,
	"container.ImagePullPolicy":          fieldPlatformOwned,
}

func TestStrictAdmissionFields_UncomparedFieldsAreTheReasonedSet(t *testing.T) {
	must := require.New(t)
	got := map[string]admissionFieldRule{}
	for name, field := range strictPodSpecFields {
		if field.rule != fieldCompared {
			got["pod."+name] = field.rule
		}
	}
	for name, field := range strictContainerFields {
		if field.rule != fieldCompared {
			got["container."+name] = field.rule
		}
	}
	must.Equal(wantUncomparedFields, got, "the classification tables no longer match wantUncomparedFields; change the list only for a field that truly must not be compared, and give it a reason in the table")
}

// TestPodAdmission_StrictComparesEveryComparedField changes each field that
// is not in wantUncomparedFields, one at a time, and requires both
// admission stages to name it.
func TestPodAdmission_StrictComparesEveryComparedField(t *testing.T) {
	profile := k8sRuntimeProfiles["nitro-isolated"]
	type fieldCase struct {
		key, path string
		change    func(t *testing.T, pod *corev1.Pod)
	}
	var cases []fieldCase
	for _, f := range reflect.VisibleFields(reflect.TypeOf(corev1.PodSpec{})) {
		name := f.Name
		if _, ok := wantUncomparedFields["pod."+name]; ok {
			continue
		}
		cases = append(cases, fieldCase{key: "pod." + name, path: "spec." + jsonFieldName(f) + ":", change: func(t *testing.T, pod *corev1.Pod) {
			if change, ok := explicitPodSpecChanges[name]; ok {
				change(&pod.Spec)
				return
			}
			require.New(t).True(mutateForAdmission(reflect.ValueOf(&pod.Spec).Elem().FieldByName(name)), "no change for PodSpec.%s; add one to explicitPodSpecChanges", name)
		}})
	}
	for _, f := range reflect.VisibleFields(reflect.TypeOf(corev1.Container{})) {
		name := f.Name
		if _, ok := wantUncomparedFields["container."+name]; ok {
			continue
		}
		cases = append(cases, fieldCase{key: "container." + name, path: "spec.containers[agent]." + jsonFieldName(f) + ":", change: func(t *testing.T, pod *corev1.Pod) {
			if change, ok := explicitContainerChanges[name]; ok {
				change(&pod.Spec.Containers[0])
				return
			}
			require.New(t).True(mutateForAdmission(reflect.ValueOf(&pod.Spec.Containers[0]).Elem().FieldByName(name)), "no change for Container.%s; add one to explicitContainerChanges", name)
		}})
	}
	for _, tc := range cases {
		if _, ok := comparedFieldExceptions[tc.key]; ok {
			continue
		}
		path := tc.path
		if tc.key == "pod.Containers" {
			path = "spec.containers: expected containers [agent], observed containers [agent, ]"
		}
		t.Run(tc.key+" at creation", func(t *testing.T) {
			must := require.New(t)
			submitted := buildSandboxPod(nitroFixtureCfg(), profile, testK8sPodName, nil)
			admitted := decodeAdmittedNitroPod(t)
			tc.change(t, admitted)

			must.ErrorContains(newPodAdmission(submitted, admitted, profile).checkCreate(), path)
		})
		t.Run(tc.key+" during the readiness wait", func(t *testing.T) {
			must := require.New(t)
			submitted := buildSandboxPod(nitroFixtureCfg(), profile, testK8sPodName, nil)
			admitted := decodeAdmittedNitroPod(t)
			admission := newPodAdmission(submitted, admitted, profile)
			must.NoError(admission.checkCreate())
			observed := admitted.DeepCopy()
			tc.change(t, observed)

			must.ErrorContains(admission.checkPoll(observed), path)
		})
	}
}

// TestPodAdmission_StrictIgnoresPlatformOwnedFields is the converse: a
// change to each field wantUncomparedFields lists as platform-owned passes
// both stages.
func TestPodAdmission_StrictIgnoresPlatformOwnedFields(t *testing.T) {
	profile := k8sRuntimeProfiles["nitro-isolated"]
	for _, table := range []struct {
		kind   string
		target func(pod *corev1.Pod) reflect.Value
	}{
		{kind: "pod", target: func(pod *corev1.Pod) reflect.Value { return reflect.ValueOf(&pod.Spec).Elem() }},
		{kind: "container", target: func(pod *corev1.Pod) reflect.Value { return reflect.ValueOf(&pod.Spec.Containers[0]).Elem() }},
	} {
		for key, rule := range wantUncomparedFields {
			name, ok := strings.CutPrefix(key, table.kind+".")
			if !ok || rule != fieldPlatformOwned {
				continue
			}
			t.Run(key, func(t *testing.T) {
				must := require.New(t)
				submitted := buildSandboxPod(nitroFixtureCfg(), profile, testK8sPodName, nil)
				admitted := decodeAdmittedNitroPod(t)
				must.True(mutateForAdmission(table.target(admitted).FieldByName(name)), "no change for %s", name)

				admission := newPodAdmission(submitted, admitted, profile)

				must.NoError(admission.checkCreate())
				observed := admitted.DeepCopy()
				must.True(mutateForAdmission(table.target(observed).FieldByName(name)))
				must.NoError(admission.checkPoll(observed))
			})
		}
	}
}

func TestPodAdmission_StrictSubsetFields(t *testing.T) {
	profile := k8sRuntimeProfiles["nitro-isolated"]
	cases := []struct {
		name      string
		mutate    func(pod *corev1.Pod)
		wantField string
	}{
		{name: "node selector key added", mutate: func(p *corev1.Pod) { p.Spec.NodeSelector["topology.kubernetes.io/zone"] = "eu-west-1a" }},
		{name: "toleration added", mutate: func(p *corev1.Pod) {
			p.Spec.Tolerations = append(p.Spec.Tolerations, corev1.Toleration{Key: "example.com/pool", Operator: corev1.TolerationOpExists})
		}},
		{name: "node selector value changed", mutate: func(p *corev1.Pod) { p.Spec.NodeSelector["kubernetes.io/arch"] = "arm64" }, wantField: "spec.nodeSelector: expected at least"},
		{name: "node selector key removed", mutate: func(p *corev1.Pod) { delete(p.Spec.NodeSelector, "kubernetes.io/arch") }, wantField: "spec.nodeSelector: expected at least"},
		{name: "toleration removed", mutate: func(p *corev1.Pod) { p.Spec.Tolerations = p.Spec.Tolerations[1:] }, wantField: "spec.tolerations: expected at least"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			submitted := buildSandboxPod(nitroFixtureCfg(), profile, testK8sPodName, nil)
			admitted := decodeAdmittedNitroPod(t)
			must.NotEmpty(admitted.Spec.Tolerations, "the fixture carries the default tolerations")
			admission := newPodAdmission(submitted, admitted, profile)
			must.NoError(admission.checkCreate())
			observed := admitted.DeepCopy()
			tc.mutate(observed)

			err := admission.checkPoll(observed)

			if tc.wantField == "" {
				must.NoError(err)
				return
			}
			must.ErrorContains(err, tc.wantField)
		})
	}
}

func TestPodAdmission_StrictRejectsAChangedSubmittedOwnerReference(t *testing.T) {
	must := require.New(t)
	profile := k8sRuntimeProfiles["nitro-isolated"]
	cfg := nitroFixtureCfg()
	cfg.HarnessPod = &HarnessPod{Namespace: testK8sNamespace, Name: "harness", UID: "harness-uid"}
	submitted := buildSandboxPod(cfg, profile, testK8sPodName, nil)
	must.Len(submitted.OwnerReferences, 1)
	admitted := admittedCopy(submitted)
	applyRuntimeClassMarks(admitted)
	must.NoError(newPodAdmission(submitted, admitted, profile).checkCreate())

	admitted.OwnerReferences[0].UID = "other-uid"

	must.ErrorContains(newPodAdmission(submitted, admitted, profile).checkCreate(), "metadata.ownerReferences: expected")
}
