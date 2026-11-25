package executor

import (
	"context"
	"errors"
	"fmt"
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
	networkingv1 "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
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
		access := k8sAccess{group: attrs.Group, resource: attrs.Resource, subresource: attrs.Subresource, verb: attrs.Verb, name: attrs.Name}
		review.Status.Allowed = !deny[access.String()]
		if !review.Status.Allowed {
			review.Status.Reason = "no RBAC policy matched"
		}
		return true, review, nil
	}
}

// standingDenyPolicy is the reference standing default-deny egress policy.
func standingDenyPolicy() *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "stirrup-sandbox-default-deny-egress", Namespace: probeTestNamespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{k8sSandboxLabel: "true"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		},
	}
}

// accessClient returns a fake clientset holding the probe namespace and the
// standing default-deny egress policy, whose access reviews allow every
// permission except those named.
func accessClient(denied ...string) *fake.Clientset {
	kube := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: probeTestNamespace}},
		standingDenyPolicy(),
	)
	kube.PrependReactor("create", "selfsubjectaccessreviews", reviewReactor(denied...))
	return kube
}

func serveSandboxAPI(kube *fake.Clientset) {
	serveSandboxVersions(kube, "v1beta1")
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
// reads the probe is allowed to make: the namespace, the RuntimeClass, the
// namespace's NetworkPolicies, the standing policy by name and API discovery.
func unexpectedActions(kube *fake.Clientset) []string {
	var out []string
	for _, a := range kube.Actions() {
		resource := a.GetResource().Resource
		switch {
		case a.GetVerb() == "create" && resource == "selfsubjectaccessreviews":
		case a.GetVerb() == "get" && (resource == "namespaces" || resource == "runtimeclasses" || resource == "resource"):
		case (a.GetVerb() == "list" || a.GetVerb() == "get") && resource == "networkpolicies":
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

			detail, err := probeK8sAccess(context.Background(), kube, K8sProbeConfig{Type: executorType, Namespace: probeTestNamespace, Runtime: "gvisor"})

			must.NoError(err)
			check.Contains(detail, fmt.Sprintf("%d required permissions granted", len(k8sRequiredAccess(executorType))))
			check.Contains(detail, `namespace "stirrup-sandbox" exists`)
			check.Contains(detail, `standing egress policy "stirrup-sandbox-default-deny-egress" selects every sandbox Pod`)
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
			"create networkpolicies.networking.k8s.io", "delete networkpolicies.networking.k8s.io", "patch networkpolicies.networking.k8s.io",
		}},
		{"k8s-sandbox", []string{
			"create sandboxes.agents.x-k8s.io", "get sandboxes.agents.x-k8s.io", "delete sandboxes.agents.x-k8s.io",
			"get pods", "create pods/exec",
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
	kube := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: probeTestNamespace}}, standingDenyPolicy())
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
	cfg := K8sProbeConfig{Type: "k8s", Namespace: probeTestNamespace, Runtime: "gvisor"}

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

		_, err := probeK8sAccess(context.Background(), kube, K8sProbeConfig{Type: "k8s-sandbox", Namespace: probeTestNamespace, Runtime: "other"})

		must.ErrorContains(err, `RuntimeClass "gvisor" does not exist`)
	})

	t.Run("nitro-isolated checks the class its profile maps to", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		kube := accessClient()
		addRuntimeClass(t, kube, "isolated-sandbox")

		detail, err := probeK8sAccess(context.Background(), kube, K8sProbeConfig{Type: "k8s", Namespace: probeTestNamespace, Runtime: "nitro-isolated"})

		must.NoError(err)
		check.Contains(detail, `RuntimeClass "isolated-sandbox" exists`)
	})

	t.Run("a runtime without a profile fails before any call", func(t *testing.T) {
		must := require.New(t)
		kube := accessClient()

		_, err := probeK8sAccess(context.Background(), kube, K8sProbeConfig{Type: "k8s", Namespace: probeTestNamespace, Runtime: "isolated-sandbox"})

		must.ErrorContains(err, `unsupported executor.runtime "isolated-sandbox"`)
		must.Empty(kube.Actions())
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

func TestProbeK8sAccess_StandingEgressPolicyOutcomes(t *testing.T) {
	sandboxSelector := metav1.LabelSelector{MatchLabels: map[string]string{k8sSandboxLabel: "true"}}
	egressOnly := []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}
	dnsRule := []networkingv1.NetworkPolicyEgressRule{{Ports: []networkingv1.NetworkPolicyPort{{Port: ptr.To(intstr.FromInt32(53))}}}}
	named := func(name string, spec networkingv1.NetworkPolicySpec) *networkingv1.NetworkPolicy {
		return &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: probeTestNamespace}, Spec: spec}
	}
	clientWith := func(policies ...*networkingv1.NetworkPolicy) *fake.Clientset {
		objects := []runtime.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: probeTestNamespace}}}
		for _, p := range policies {
			objects = append(objects, p)
		}
		kube := fake.NewClientset(objects...)
		kube.PrependReactor("create", "selfsubjectaccessreviews", reviewReactor())
		return kube
	}

	accepted := map[string]*networkingv1.NetworkPolicy{
		"a deny-all selecting the sandbox label": named("baseline", networkingv1.NetworkPolicySpec{PodSelector: sandboxSelector, PolicyTypes: egressOnly}),
		"a namespace-wide deny-all":              named("baseline", networkingv1.NetworkPolicySpec{PolicyTypes: egressOnly}),
		"an allowlist baseline":                  named("baseline", networkingv1.NetworkPolicySpec{PodSelector: sandboxSelector, PolicyTypes: egressOnly, Egress: dnsRule}),
		"egress rules with defaulted types":      named("baseline", networkingv1.NetworkPolicySpec{PodSelector: sandboxSelector, Egress: dnsRule}),
	}
	for name, policy := range accepted {
		t.Run("accepts "+name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)

			detail, err := probeK8sAccess(context.Background(), clientWith(policy), K8sProbeConfig{Type: "k8s", Namespace: probeTestNamespace})

			must.NoError(err)
			check.Contains(detail, `standing egress policy "baseline" selects every sandbox Pod`)
		})
	}

	rejected := map[string][]*networkingv1.NetworkPolicy{
		"no policy":             nil,
		"only a per-run policy": {denyAllEgressPolicy(probeTestNamespace, "stirrup-abc", "run-1")},
		"an ingress-only policy": {named("ingress", networkingv1.NetworkPolicySpec{
			PodSelector: sandboxSelector, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}, Egress: dnsRule,
		})},
		"a policy selecting other Pods": {named("other", networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}, PolicyTypes: egressOnly,
		})},
		"a policy excluding Pods with a per-Pod label": {named("narrow", networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels:      map[string]string{k8sSandboxLabel: "true"},
				MatchExpressions: []metav1.LabelSelectorRequirement{{Key: k8sPodNameLabel, Operator: metav1.LabelSelectorOpDoesNotExist}},
			},
			PolicyTypes: egressOnly,
		})},
	}
	for name, policies := range rejected {
		t.Run("fails with "+name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)

			_, err := probeK8sAccess(context.Background(), clientWith(policies...), K8sProbeConfig{Type: "k8s", Namespace: probeTestNamespace})

			must.ErrorContains(err, `no standing NetworkPolicy in namespace "stirrup-sandbox" isolates every stirrup-sandbox=true Pod for egress`)
			check.Equal([]K8sProbeKind{K8sProbeEgressBaselineMissing}, probeErrorKinds(t, err))
		})
	}

	listForbidden := apierrors.NewForbidden(schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, "", errors.New("denied"))
	refuseList := func(kube *fake.Clientset, err error) {
		kube.PrependReactor("list", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, err
		})
	}

	t.Run("reports an unavailable list as not checked", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		kube := clientWith()
		refuseList(kube, apierrors.NewServiceUnavailable("apiserver restarting"))

		detail, err := probeK8sAccess(context.Background(), kube, K8sProbeConfig{Type: "k8s", Namespace: probeTestNamespace})

		must.NoError(err)
		check.Contains(detail, "standing egress policy not checked")
	})

	t.Run("a refused list falls back to a get of the reference policy by name", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		kube := clientWith(standingDenyPolicy())
		refuseList(kube, listForbidden)
		kube.ClearActions()

		detail, err := probeK8sAccess(context.Background(), kube, K8sProbeConfig{Type: "k8s", Namespace: probeTestNamespace})

		must.NoError(err)
		check.Contains(detail, `standing egress policy "stirrup-sandbox-default-deny-egress" selects every sandbox Pod`)
		var gets []string
		for _, a := range kube.Actions() {
			if get, ok := a.(k8stesting.GetAction); ok && a.GetResource().Resource == "networkpolicies" {
				gets = append(gets, get.GetName())
			}
		}
		check.Equal([]string{k8sStandingEgressPolicyName}, gets)
	})

	t.Run("a refused list and an absent reference policy fail naming the limit", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		kube := clientWith(named("baseline", networkingv1.NetworkPolicySpec{PodSelector: sandboxSelector, PolicyTypes: egressOnly}))
		refuseList(kube, listForbidden)

		_, err := probeK8sAccess(context.Background(), kube, K8sProbeConfig{Type: "k8s", Namespace: probeTestNamespace})

		must.ErrorContains(err, `no standing NetworkPolicy in namespace "stirrup-sandbox" isolates every stirrup-sandbox=true Pod for egress`)
		check.Contains(err.Error(), "a standing policy under another name is not detected")
		check.Equal([]K8sProbeKind{K8sProbeEgressBaselineMissing}, probeErrorKinds(t, err))
	})

	t.Run("a refused list and a reference policy that does not isolate fail", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		kube := clientWith(named(k8sStandingEgressPolicyName, networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}, PolicyTypes: egressOnly,
		}))
		refuseList(kube, listForbidden)

		_, err := probeK8sAccess(context.Background(), kube, K8sProbeConfig{Type: "k8s", Namespace: probeTestNamespace})

		must.ErrorContains(err, `NetworkPolicy "stirrup-sandbox-default-deny-egress" in namespace "stirrup-sandbox" does not isolate every stirrup-sandbox=true Pod for egress`)
		check.Equal([]K8sProbeKind{K8sProbeEgressBaselineMissing}, probeErrorKinds(t, err))
	})

	for name, getErr := range map[string]error{
		"forbidden":   apierrors.NewForbidden(schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, k8sStandingEgressPolicyName, errors.New("denied")),
		"unavailable": apierrors.NewServiceUnavailable("apiserver restarting"),
	} {
		t.Run("reports a refused list and a "+name+" get as not checked", func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			kube := clientWith()
			refuseList(kube, listForbidden)
			refuseRead(kube, "networkpolicies", getErr)

			detail, err := probeK8sAccess(context.Background(), kube, K8sProbeConfig{Type: "k8s", Namespace: probeTestNamespace})

			must.NoError(err)
			check.Contains(detail, "standing egress policy not checked")
		})
	}

	t.Run("is not looked for in a missing namespace", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		kube := fake.NewClientset()
		kube.PrependReactor("create", "selfsubjectaccessreviews", reviewReactor())

		_, err := probeK8sAccess(context.Background(), kube, K8sProbeConfig{Type: "k8s", Namespace: probeTestNamespace})

		must.Error(err)
		check.Equal([]K8sProbeKind{K8sProbeNamespaceMissing}, probeErrorKinds(t, err))
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

		must.ErrorContains(err, "agents.x-k8s.io/v1beta1 or agents.x-k8s.io/v1alpha1 is not served")
		check.Equal([]K8sProbeKind{K8sProbeSandboxAPIMissing}, probeErrorKinds(t, err))
	})

	t.Run("a served group without the sandboxes resource fails", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		kube := accessClient()
		addRuntimeClass(t, kube, "gvisor")
		kube.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{{GroupVersion: testSandboxGVR.GroupVersion().String()}}

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
				check.Contains(detail, "Agent Sandbox API not checked (discover agents.x-k8s.io/v1beta1: ")
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
		check.Contains(detail, "Agent Sandbox API agents.x-k8s.io/v1beta1 served")
	})

	t.Run("a cluster serving only v1alpha1 is reported with that version", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		kube := accessClient()
		serveSandboxVersions(kube, "v1alpha1")
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
// into k8sAccess.String entries, one per resource name a rule is restricted
// to. A rule with no resourceNames yields unnamed entries, so a restricted
// rule never satisfies an unrestricted probe check.
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
		must.Contains([]string{"ServiceAccount", "Role", "RoleBinding"}, doc.Kind, "%s holds a %s the pin does not read", manifest, doc.Kind)
		if doc.Kind != "Role" {
			continue
		}
		for _, rule := range doc.Rules {
			names := rule.ResourceNames
			if len(names) == 0 {
				names = []string{""}
			}
			for _, group := range rule.APIGroups {
				for _, res := range rule.Resources {
					resource, subresource, _ := strings.Cut(res, "/")
					for _, verb := range rule.Verbs {
						for _, name := range names {
							got[k8sAccess{group: group, resource: resource, subresource: subresource, verb: verb, name: name}.String()] = true
						}
					}
				}
			}
		}
	}
	must.NotEmpty(got, "no Role rules found in %s", manifest)
	return got
}

// TestK8sRequiredAccess_MatchesReferenceRoles pins the probe's permission lists
// to the reference Roles: each Role grants every required permission and every
// optional one, and nothing else.
func TestK8sRequiredAccess_MatchesReferenceRoles(t *testing.T) {
	tests := []struct {
		executorType string
		manifest     string
	}{
		{"k8s", "rbac.yaml"},
		{"k8s-sandbox", filepath.Join("agent-sandbox", "rbac-agent-sandbox.yaml")},
	}
	for _, tt := range tests {
		t.Run(tt.executorType, func(t *testing.T) {
			must := require.New(t)
			role := referenceRolePermissions(t, tt.manifest)

			want := map[string]bool{}
			for _, access := range k8sRequiredAccess(tt.executorType) {
				must.Truef(role[access.String()], "the probe requires %q but %s does not grant it", access, tt.manifest)
				want[access.String()] = true
			}
			must.NotEmpty(k8sOptionalAccess(tt.executorType))
			for _, access := range k8sOptionalAccess(tt.executorType) {
				must.Truef(role[access.String()], "the probe reads %q when it can but %s does not grant it", access, tt.manifest)
				want[access.String()] = true
			}
			must.Equal(want, role, "%s grants permissions the probe does not use", tt.manifest)
		})
	}
}

func TestK8sStandingEgressPolicyName_MatchesReferenceManifest(t *testing.T) {
	must := require.New(t)
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "examples", "k8s", "sandbox-egress-default-deny.yaml"))
	must.NoError(err)
	var policy networkingv1.NetworkPolicy
	must.NoError(utilyaml.Unmarshal(data, &policy))

	must.Equal(k8sStandingEgressPolicyName, policy.Name)
}

func TestReviewAccess_CarriesTheResourceName(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	kube := fake.NewClientset()
	var got *authorizationv1.ResourceAttributes
	kube.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		got = review.Spec.ResourceAttributes
		review.Status.Allowed = true
		return true, review, nil
	})

	allowed, _, err := reviewAccess(context.Background(), kube, probeTestNamespace, k8sStandingPolicyAccess)

	must.NoError(err)
	check.True(allowed)
	must.NotNil(got)
	check.Equal(k8sStandingEgressPolicyName, got.Name)
	check.Equal("get", got.Verb)
}
