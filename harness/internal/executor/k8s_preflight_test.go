package executor

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const probeTestNamespace = "stirrup-sandbox"

// reviewReactor answers access reviews: every permission is allowed except
// those named (in k8sAccess.String form).
func reviewReactor(denied ...string) k8stesting.ReactionFunc {
	deny := map[string]bool{}
	for _, d := range denied {
		deny[d] = true
	}
	return func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		attrs := review.Spec.ResourceAttributes
		access := k8sAccess{group: attrs.Group, resource: attrs.Resource, subresource: attrs.Subresource, verb: attrs.Verb}
		review.Status.Allowed = !deny[access.String()]
		if !review.Status.Allowed {
			review.Status.Reason = "no RBAC policy matched"
		}
		return true, review, nil
	}
}

// accessClient returns a fake clientset holding the probe namespace whose
// access reviews allow every permission except those named.
func accessClient(denied ...string) *fake.Clientset {
	kube := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: probeTestNamespace}})
	kube.PrependReactor("create", "selfsubjectaccessreviews", reviewReactor(denied...))
	return kube
}

func serveSandboxAPI(kube *fake.Clientset) {
	kube.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{{
		GroupVersion: sandboxGVR.GroupVersion().String(),
		APIResources: []metav1.APIResource{{Name: sandboxGVR.Resource}},
	}}
}

func addRuntimeClass(t *testing.T, kube *fake.Clientset, name string) {
	t.Helper()
	must := require.New(t)
	_, err := kube.NodeV1().RuntimeClasses().Create(context.Background(), &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	must.NoError(err)
}

// refuseRead makes reads of the resource return err.
func refuseRead(kube *fake.Clientset, resource string, err error) {
	kube.PrependReactor("get", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, err
	})
}

// unexpectedActions lists recorded actions other than access reviews and the
// reads the probe is allowed to make: the namespace, the RuntimeClass and API
// discovery.
func unexpectedActions(kube *fake.Clientset) []string {
	var out []string
	for _, a := range kube.Actions() {
		resource := a.GetResource().Resource
		switch {
		case a.GetVerb() == "create" && resource == "selfsubjectaccessreviews":
		case a.GetVerb() == "get" && (resource == "namespaces" || resource == "runtimeclasses" || resource == "resource"):
		default:
			out = append(out, a.GetVerb()+" "+resource)
		}
	}
	return out
}

func probeErrorKinds(t *testing.T, err error) []K8sProbeKind {
	t.Helper()
	must := require.New(t)
	var probeErr *K8sProbeError
	must.ErrorAs(err, &probeErr)
	kinds := append([]K8sProbeKind(nil), probeErr.Kinds...)
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	return kinds
}

func TestProbeK8sAccess_AllGrantedReportsOK(t *testing.T) {
	for _, executorType := range []string{"k8s", "k8s-sandbox"} {
		t.Run(executorType, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			kube := accessClient()
			serveSandboxAPI(kube)
			addRuntimeClass(t, kube, "gvisor")
			kube.ClearActions()

			detail, err := probeK8sAccess(context.Background(), kube, K8sProbeConfig{Type: executorType, Namespace: probeTestNamespace, RuntimeClassName: "gvisor"})

			must.NoError(err)
			check.Contains(detail, "6 required permissions granted")
			check.Contains(detail, `namespace "stirrup-sandbox" exists`)
			check.Empty(unexpectedActions(kube), "the probe must only review access and make the documented reads")
		})
	}
}

func TestProbeK8sAccess_ReviewsEveryRequiredVerbInTheNamespace(t *testing.T) {
	tests := []struct {
		executorType string
		want         []string
	}{
		{"k8s", []string{
			"create pods", "get pods", "delete pods", "create pods/exec",
			"create networkpolicies.networking.k8s.io", "delete networkpolicies.networking.k8s.io",
		}},
		{"k8s-sandbox", []string{
			"create sandboxes.agents.x-k8s.io", "get sandboxes.agents.x-k8s.io", "delete sandboxes.agents.x-k8s.io",
			"create pods/exec",
			"create networkpolicies.networking.k8s.io", "delete networkpolicies.networking.k8s.io",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.executorType, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			kube := accessClient()
			serveSandboxAPI(kube)
			addRuntimeClass(t, kube, "gvisor")

			_, err := probeK8sAccess(context.Background(), kube, K8sProbeConfig{Type: tt.executorType, Namespace: probeTestNamespace})
			must.NoError(err)

			var got []string
			for _, a := range kube.Actions() {
				if a.GetResource().Resource != "selfsubjectaccessreviews" {
					continue
				}
				review := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
				attrs := review.Spec.ResourceAttributes
				check.Equal(probeTestNamespace, attrs.Namespace)
				got = append(got, k8sAccess{group: attrs.Group, resource: attrs.Resource, subresource: attrs.Subresource, verb: attrs.Verb}.String())
			}
			sort.Strings(got)
			want := append([]string(nil), tt.want...)
			sort.Strings(want)
			check.Equal(want, got)
		})
	}
}

func TestProbeK8sAccess_DeniedPermissionFailsNamingVerbResourceAndNamespace(t *testing.T) {
	tests := []struct {
		name         string
		executorType string
		denied       []string
		wantInError  []string
	}{
		{"k8s pod delete", "k8s", []string{"delete pods"}, []string{`"delete pods"`, probeTestNamespace}},
		{"k8s pod exec", "k8s", []string{"create pods/exec"}, []string{`"create pods/exec"`}},
		{"k8s network policy", "k8s", []string{"create networkpolicies.networking.k8s.io"}, []string{`"create networkpolicies.networking.k8s.io"`}},
		{"k8s-sandbox without sandbox permission", "k8s-sandbox", []string{"create sandboxes.agents.x-k8s.io"}, []string{`"create sandboxes.agents.x-k8s.io"`, probeTestNamespace}},
		{"several denials are all named", "k8s", []string{"create pods", "delete pods"}, []string{`"create pods"`, `"delete pods"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			kube := accessClient(tt.denied...)
			serveSandboxAPI(kube)
			addRuntimeClass(t, kube, "gvisor")

			_, err := probeK8sAccess(context.Background(), kube, K8sProbeConfig{Type: tt.executorType, Namespace: probeTestNamespace})

			must.Error(err)
			for _, want := range tt.wantInError {
				check.Contains(err.Error(), want)
			}
			check.Contains(err.Error(), "no RBAC policy matched")
			check.Equal([]K8sProbeKind{K8sProbePermissionDenied}, probeErrorKinds(t, err))
		})
	}
}

func TestProbeK8sAccess_EvaluationErrorReachesTheDenial(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	kube := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: probeTestNamespace}})
	kube.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		review.Status.Allowed = false
		review.Status.EvaluationError = "authorizer unavailable"
		return true, review, nil
	})

	_, err := probeK8sAccess(context.Background(), kube, K8sProbeConfig{Type: "k8s", Namespace: probeTestNamespace})

	must.Error(err)
	check.Contains(err.Error(), "authorizer unavailable")
	check.Equal([]K8sProbeKind{K8sProbePermissionDenied}, probeErrorKinds(t, err))
}

func TestProbeK8sAccess_ReviewCallErrorFailsAsAFailedCall(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	kube := fake.NewClientset()
	kube.PrependReactor("create", "selfsubjectaccessreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "authorization.k8s.io", Resource: "selfsubjectaccessreviews"}, "", errors.New("denied"))
	})

	_, err := probeK8sAccess(context.Background(), kube, K8sProbeConfig{Type: "k8s", Namespace: probeTestNamespace})

	must.Error(err)
	check.Contains(err.Error(), `access review for "create pods" in namespace "stirrup-sandbox" failed`)
	check.Contains(err.Error(), "selfsubjectaccessreviews")
	check.Equal([]K8sProbeKind{K8sProbeReviewFailed}, probeErrorKinds(t, err))
}

func TestProbeK8sAccess_UnsupportedTypeFailsWithoutCalls(t *testing.T) {
	for _, executorType := range []string{"", "container", "local"} {
		t.Run("type="+executorType, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			kube := accessClient()

			_, err := probeK8sAccess(context.Background(), kube, K8sProbeConfig{Type: executorType, Namespace: probeTestNamespace})
			must.Error(err)
			check.Contains(err.Error(), "unsupported executor type")
			check.Empty(kube.Actions())

			_, err = ProbeK8sAccess(context.Background(), K8sProbeConfig{Type: executorType, Namespace: probeTestNamespace})
			must.Error(err)
			check.Contains(err.Error(), "unsupported executor type")
		})
	}
}

func TestProbeK8sAccess_ClientBuildFailureIsClassified(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	_, err := ProbeK8sAccess(context.Background(), K8sProbeConfig{Type: "k8s", Namespace: probeTestNamespace, Kubeconfig: filepath.Join(t.TempDir(), "missing")})

	must.Error(err)
	check.Contains(err.Error(), "build kube rest config")
	check.Equal([]K8sProbeKind{K8sProbeClientBuild}, probeErrorKinds(t, err))
}

func TestProbeK8sAccess_RuntimeClassOutcomes(t *testing.T) {
	cfg := K8sProbeConfig{Type: "k8s", Namespace: probeTestNamespace, RuntimeClassName: "gvisor"}

	t.Run("existing class is reported", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		kube := accessClient()
		addRuntimeClass(t, kube, "gvisor")

		detail, err := probeK8sAccess(context.Background(), kube, cfg)

		must.NoError(err)
		check.Contains(detail, `RuntimeClass "gvisor" exists`)
	})

	t.Run("missing class fails", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		kube := accessClient()

		_, err := probeK8sAccess(context.Background(), kube, cfg)

		must.ErrorContains(err, `RuntimeClass "gvisor" does not exist`)
		check.Equal([]K8sProbeKind{K8sProbeRuntimeClassMissing}, probeErrorKinds(t, err))
	})

	t.Run("forbidden read is reported as not checked", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		kube := accessClient()
		refuseRead(kube, "runtimeclasses", apierrors.NewForbidden(schema.GroupResource{Group: "node.k8s.io", Resource: "runtimeclasses"}, "gvisor", errors.New("denied")))

		detail, err := probeK8sAccess(context.Background(), kube, cfg)

		must.NoError(err)
		check.Contains(detail, `RuntimeClass "gvisor" not checked`)
		check.NotContains(detail, `RuntimeClass "gvisor" exists`)
	})

	t.Run("other read errors are reported as not checked", func(t *testing.T) {
		for name, readErr := range map[string]error{
			"server error": apierrors.NewInternalError(errors.New("etcd timeout")),
			"unauthorized": apierrors.NewUnauthorized("token expired"),
			"transport":    errors.New("connection reset"),
		} {
			t.Run(name, func(t *testing.T) {
				must := require.New(t)
				check := assert.New(t)
				kube := accessClient()
				refuseRead(kube, "runtimeclasses", readErr)

				detail, err := probeK8sAccess(context.Background(), kube, cfg)

				must.NoError(err)
				check.Contains(detail, `RuntimeClass "gvisor" not checked`)
				check.NotContains(detail, `RuntimeClass "gvisor" exists`)
			})
		}
	})

	t.Run("no requested class makes no read", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		kube := accessClient()

		_, err := probeK8sAccess(context.Background(), kube, K8sProbeConfig{Type: "k8s", Namespace: probeTestNamespace})

		must.NoError(err)
		for _, a := range kube.Actions() {
			check.NotEqual("runtimeclasses", a.GetResource().Resource)
		}
	})

	t.Run("k8s-sandbox checks the gVisor class whatever executor.runtime says", func(t *testing.T) {
		must := require.New(t)
		kube := accessClient()
		serveSandboxAPI(kube)

		_, err := probeK8sAccess(context.Background(), kube, K8sProbeConfig{Type: "k8s-sandbox", Namespace: probeTestNamespace, RuntimeClassName: "other"})

		must.ErrorContains(err, `RuntimeClass "gvisor" does not exist`)
	})
}

func TestProbeK8sAccess_NamespaceOutcomes(t *testing.T) {
	cfg := K8sProbeConfig{Type: "k8s", Namespace: probeTestNamespace}

	t.Run("missing namespace fails naming it", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		kube := fake.NewClientset()
		kube.PrependReactor("create", "selfsubjectaccessreviews", reviewReactor())

		_, err := probeK8sAccess(context.Background(), kube, cfg)

		must.ErrorContains(err, `namespace "stirrup-sandbox" does not exist`)
		check.Equal([]K8sProbeKind{K8sProbeNamespaceMissing}, probeErrorKinds(t, err))
	})

	t.Run("forbidden read is reported as not checked", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		kube := accessClient()
		refuseRead(kube, "namespaces", apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, probeTestNamespace, errors.New("denied")))

		detail, err := probeK8sAccess(context.Background(), kube, cfg)

		must.NoError(err)
		check.Contains(detail, `namespace "stirrup-sandbox" not checked`)
		check.NotContains(detail, `namespace "stirrup-sandbox" exists`)
	})

	t.Run("other read errors are reported as not checked", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		kube := accessClient()
		refuseRead(kube, "namespaces", apierrors.NewInternalError(errors.New("etcd timeout")))

		detail, err := probeK8sAccess(context.Background(), kube, cfg)

		must.NoError(err)
		check.Contains(detail, `namespace "stirrup-sandbox" not checked`)
	})
}

func TestProbeK8sAccess_SandboxAPIOutcomes(t *testing.T) {
	cfg := K8sProbeConfig{Type: "k8s-sandbox", Namespace: probeTestNamespace}

	t.Run("an unserved API group fails", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		kube := accessClient()
		addRuntimeClass(t, kube, "gvisor")

		_, err := probeK8sAccess(context.Background(), kube, cfg)

		must.ErrorContains(err, "agents.x-k8s.io/v1alpha1 is not served")
		check.Equal([]K8sProbeKind{K8sProbeSandboxAPIMissing}, probeErrorKinds(t, err))
	})

	t.Run("a served group without the sandboxes resource fails", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		kube := accessClient()
		addRuntimeClass(t, kube, "gvisor")
		kube.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{{GroupVersion: sandboxGVR.GroupVersion().String()}}

		_, err := probeK8sAccess(context.Background(), kube, cfg)

		must.ErrorContains(err, "not the sandboxes resource")
		check.Equal([]K8sProbeKind{K8sProbeSandboxAPIMissing}, probeErrorKinds(t, err))
	})

	t.Run("a refused or failed discovery request is reported as not checked", func(t *testing.T) {
		for name, readErr := range map[string]error{
			"forbidden":   apierrors.NewForbidden(schema.GroupResource{Resource: "resource"}, "", errors.New("denied")),
			"unavailable": apierrors.NewServiceUnavailable("apiserver restarting"),
			"transport":   errors.New("connection reset"),
		} {
			t.Run(name, func(t *testing.T) {
				must := require.New(t)
				check := assert.New(t)
				kube := accessClient()
				addRuntimeClass(t, kube, "gvisor")
				refuseRead(kube, "resource", readErr)

				detail, err := probeK8sAccess(context.Background(), kube, cfg)

				must.NoError(err)
				check.Contains(detail, "Agent Sandbox API agents.x-k8s.io/v1alpha1 not checked")
				check.NotContains(detail, "served")
			})
		}
	})

	t.Run("a served API is reported", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		kube := accessClient()
		serveSandboxAPI(kube)
		addRuntimeClass(t, kube, "gvisor")

		detail, err := probeK8sAccess(context.Background(), kube, cfg)

		must.NoError(err)
		check.Contains(detail, "Agent Sandbox API agents.x-k8s.io/v1alpha1 served")
	})
}

func TestProbeK8sAccess_SimultaneousFailuresAreAllReported(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	kube := fake.NewClientset()
	kube.PrependReactor("create", "selfsubjectaccessreviews", reviewReactor("create sandboxes.agents.x-k8s.io"))

	_, err := probeK8sAccess(context.Background(), kube, K8sProbeConfig{Type: "k8s-sandbox", Namespace: probeTestNamespace})

	must.Error(err)
	check.Contains(err.Error(), `"create sandboxes.agents.x-k8s.io"`)
	check.Contains(err.Error(), `namespace "stirrup-sandbox" does not exist`)
	check.Contains(err.Error(), "is not served")
	check.Contains(err.Error(), `RuntimeClass "gvisor" does not exist`)
	check.NotContains(err.Error(), "not checked", "optional notes are dropped when the probe fails")
	check.Equal([]K8sProbeKind{
		K8sProbeNamespaceMissing, K8sProbePermissionDenied, K8sProbeRuntimeClassMissing, K8sProbeSandboxAPIMissing,
	}, probeErrorKinds(t, err))
}

// referenceRolePermissions flattens the Role in a reference RBAC manifest
// into k8sAccess.String entries.
func referenceRolePermissions(t *testing.T, manifest string) map[string]bool {
	t.Helper()
	must := require.New(t)
	f, err := os.Open(filepath.Join("..", "..", "..", "examples", "k8s", manifest))
	must.NoError(err)
	defer func() { _ = f.Close() }()

	got := map[string]bool{}
	decoder := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
	for {
		var doc struct {
			Kind  string              `json:"kind"`
			Rules []rbacv1.PolicyRule `json:"rules"`
		}
		if err := decoder.Decode(&doc); errors.Is(err, io.EOF) {
			break
		} else {
			must.NoError(err)
		}
		if doc.Kind != "Role" {
			continue
		}
		for _, rule := range doc.Rules {
			for _, group := range rule.APIGroups {
				for _, res := range rule.Resources {
					resource, subresource, _ := strings.Cut(res, "/")
					for _, verb := range rule.Verbs {
						got[k8sAccess{group: group, resource: resource, subresource: subresource, verb: verb}.String()] = true
					}
				}
			}
		}
	}
	must.NotEmpty(got, "no Role rules found in %s", manifest)
	return got
}

// TestK8sRequiredAccess_MatchesReferenceRoles pins the probe's permission list
// to the reference Roles: every probed permission must be granted, and every
// granted permission the probe does not check must be listed here with the
// reason the executor does not use it.
func TestK8sRequiredAccess_MatchesReferenceRoles(t *testing.T) {
	tests := []struct {
		executorType string
		manifest     string
		unprobed     map[string]string
	}{
		{"k8s", "rbac.yaml", map[string]string{
			"get pods/log": "the executor reads no Pod logs; granted for operator debugging",
		}},
		{"k8s-sandbox", filepath.Join("agent-sandbox", "rbac-agent-sandbox.yaml"), map[string]string{
			"list sandboxes.agents.x-k8s.io":  "the executor gets and deletes its own Sandbox by name and never lists",
			"watch sandboxes.agents.x-k8s.io": "readiness is polled with get, not watched",
			"get pods":                        "the executor never reads the controller-created Pod; it only execs into it",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.executorType, func(t *testing.T) {
			must := require.New(t)
			role := referenceRolePermissions(t, tt.manifest)

			want := map[string]bool{}
			for _, access := range k8sRequiredAccess(tt.executorType) {
				must.Truef(role[access.String()], "the probe checks %q but %s does not grant it", access, tt.manifest)
				want[access.String()] = true
			}
			for k := range tt.unprobed {
				want[k] = true
			}
			must.Equal(want, role, "%s grants permissions that are neither probed nor listed as unprobed", tt.manifest)
		})
	}
}
