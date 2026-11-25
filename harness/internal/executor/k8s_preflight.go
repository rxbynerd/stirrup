package executor

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

// K8sDefaultRuntimeClassWarning is the isolation warning for a k8s executor
// that requests no RuntimeClass; the constructor logs it and the dry run
// reports it.
const K8sDefaultRuntimeClassWarning = "k8s executor running with the cluster-default RuntimeClass; sandbox isolation is not guaranteed — set executor.runtime (e.g. gvisor) for a sandboxed RuntimeClass"

// K8sProbeConfig selects what ProbeK8sAccess checks. Type is the executor
// type, "k8s" or "k8s-sandbox"; Runtime is executor.runtime.
type K8sProbeConfig struct {
	Type       string
	Namespace  string
	Kubeconfig string
	Runtime    string
}

// K8sProbeKind classifies one reason ProbeK8sAccess failed.
type K8sProbeKind string

const (
	// K8sProbeClientBuild means the kubeconfig or in-cluster configuration
	// could not be turned into a client.
	K8sProbeClientBuild K8sProbeKind = "client-build"
	// K8sProbeCredentialPlugin means the kubeconfig's exec credential plugin
	// did not return before the probe's context ended.
	K8sProbeCredentialPlugin K8sProbeKind = "credential-plugin"
	// K8sProbePermissionDenied means the credentials lack a required permission.
	K8sProbePermissionDenied K8sProbeKind = "permission-denied"
	// K8sProbeReviewFailed means an access review request itself failed.
	K8sProbeReviewFailed K8sProbeKind = "review-failed"
	// K8sProbeRuntimeClassMissing means the requested RuntimeClass does not exist.
	K8sProbeRuntimeClassMissing K8sProbeKind = "runtime-class-missing"
	// K8sProbeNamespaceMissing means the target namespace does not exist.
	K8sProbeNamespaceMissing K8sProbeKind = "namespace-missing"
	// K8sProbeSandboxAPIMissing means the cluster does not serve the Agent
	// Sandbox sandboxes resource.
	K8sProbeSandboxAPIMissing K8sProbeKind = "sandbox-api-missing"
	// K8sProbeEgressBaselineMissing means no standing NetworkPolicy in the
	// namespace isolates every sandbox Pod for egress.
	K8sProbeEgressBaselineMissing K8sProbeKind = "egress-baseline-missing"
)

// K8sProbeError is the error ProbeK8sAccess returns. Kinds lists every
// distinct reason the probe failed, so callers can tailor remediation without
// matching on the message.
type K8sProbeError struct {
	Kinds []K8sProbeKind
	msg   string
	err   error
}

func (e *K8sProbeError) Error() string { return e.msg }

func (e *K8sProbeError) Unwrap() error { return e.err }

// HasKind reports whether the probe failed for the given reason.
func (e *K8sProbeError) HasKind(kind K8sProbeKind) bool {
	for _, k := range e.Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// k8sAccess is one permission the executor needs in its namespace.
// subresource is empty for a plain resource; name is empty unless the
// permission is for one named object.
type k8sAccess struct {
	group, resource, subresource, verb, name string
}

func (a k8sAccess) String() string {
	name := a.resource
	if a.subresource != "" {
		name += "/" + a.subresource
	}
	if a.group != "" {
		name += "." + a.group
	}
	if a.name != "" {
		name += " named " + strconv.Quote(a.name)
	}
	return a.verb + " " + name
}

// k8sExecAccess is checked as create alone: SPDY exec is a POST authorised as
// create, and the WebSocket path falls back to it (newFallbackRemoteExecutor).
var k8sExecAccess = k8sAccess{resource: "pods", subresource: "exec", verb: "create"}

// k8sStandingEgressPolicyName is the name of the reference standing
// default-deny egress policy (examples/k8s/sandbox-egress-default-deny.yaml).
const k8sStandingEgressPolicyName = "stirrup-sandbox-default-deny-egress"

// k8sStandingPolicyAccess is the read the dry run makes of the standing egress
// policy when the credentials cannot list NetworkPolicies. RBAC can scope a
// get to one name, so this adds no read of the per-run policies.
var k8sStandingPolicyAccess = k8sAccess{group: "networking.k8s.io", resource: "networkpolicies", verb: "get", name: k8sStandingEgressPolicyName}

var k8sNetworkPolicyAccess = []k8sAccess{
	{group: "networking.k8s.io", resource: "networkpolicies", verb: "create"},
	{group: "networking.k8s.io", resource: "networkpolicies", verb: "delete"},
}

// k8sRequiredAccess returns the namespaced permissions the selected executor
// uses at run time: the calls NewK8sExecutor, Close and the pods/exec streams
// make for "k8s", and the Sandbox calls in NewAgentSandboxExecutor and Close
// plus the teardown read of the backing Pod for "k8s-sandbox". Any other type
// has none.
func k8sRequiredAccess(executorType string) []k8sAccess {
	var workload []k8sAccess
	switch executorType {
	case "k8s":
		workload = []k8sAccess{
			{resource: "pods", verb: "create"},
			{resource: "pods", verb: "get"},
			{resource: "pods", verb: "delete"},
			{group: "networking.k8s.io", resource: "networkpolicies", verb: "patch"},
		}
	case "k8s-sandbox":
		workload = []k8sAccess{
			{group: sandboxGVR.Group, resource: sandboxGVR.Resource, verb: "create"},
			{group: sandboxGVR.Group, resource: sandboxGVR.Resource, verb: "get"},
			{group: sandboxGVR.Group, resource: sandboxGVR.Resource, verb: "delete"},
			{resource: "pods", verb: "get"},
		}
	default:
		return nil
	}
	required := append(workload, k8sExecAccess)
	return append(required, k8sNetworkPolicyAccess...)
}

// k8sOptionalAccess returns the permissions the dry run uses for checks that
// may be skipped. The reference Roles grant them, and a Role without them
// still passes the probe.
func k8sOptionalAccess(executorType string) []k8sAccess {
	if len(k8sRequiredAccess(executorType)) == 0 {
		return nil
	}
	return []k8sAccess{k8sStandingPolicyAccess}
}

// ProbeK8sAccess is the dry-run check for the k8s and k8s-sandbox executors.
// It asks the API server whether the configured credentials hold every
// permission the executor uses, through SelfSubjectAccessReview, and never
// creates, updates or deletes a Pod, Sandbox or NetworkPolicy. The returned
// detail summarises what passed and which optional checks could not be made;
// a failure is a *K8sProbeError.
func ProbeK8sAccess(ctx context.Context, cfg K8sProbeConfig) (string, error) {
	if len(k8sRequiredAccess(cfg.Type)) == 0 {
		return "", errUnsupportedProbeType(cfg.Type)
	}
	restCfg, err := buildRESTConfig(cfg.Kubeconfig)
	if err != nil {
		return "", &K8sProbeError{Kinds: []K8sProbeKind{K8sProbeClientBuild}, msg: fmt.Sprintf("build kube rest config: %v", err), err: err}
	}
	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return "", &K8sProbeError{Kinds: []K8sProbeKind{K8sProbeClientBuild}, msg: fmt.Sprintf("build kube clientset: %v", err), err: err}
	}
	if err := awaitCredentialPlugin(ctx, restCfg); err != nil {
		return "", &K8sProbeError{Kinds: []K8sProbeKind{K8sProbeCredentialPlugin}, msg: err.Error(), err: err}
	}
	return probeK8sAccess(ctx, kube, cfg)
}

func errUnsupportedProbeType(executorType string) error {
	return fmt.Errorf("k8s access probe: unsupported executor type %q", executorType)
}

// optionalCheck is the outcome of a read that is attempted but not required.
// A non-empty problem fails the probe; a non-empty note is reported in the
// detail.
type optionalCheck struct {
	kind    K8sProbeKind
	problem string
	note    string
}

func probeK8sAccess(ctx context.Context, kube kubernetes.Interface, cfg K8sProbeConfig) (string, error) {
	required := k8sRequiredAccess(cfg.Type)
	if len(required) == 0 {
		return "", errUnsupportedProbeType(cfg.Type)
	}
	runtimeClass, err := effectiveRuntimeClass(cfg)
	if err != nil {
		return "", err
	}

	var denied []string
	for _, access := range required {
		allowed, reason, err := reviewAccess(ctx, kube, cfg.Namespace, access)
		if err != nil {
			return "", &K8sProbeError{
				Kinds: []K8sProbeKind{K8sProbeReviewFailed},
				msg:   fmt.Sprintf("access review for %q in namespace %q failed: %v", access, cfg.Namespace, err),
				err:   err,
			}
		}
		if !allowed {
			entry := fmt.Sprintf("%q", access.String())
			if reason != "" {
				entry += " (" + reason + ")"
			}
			denied = append(denied, entry)
		}
	}

	var problems []string
	var kinds []K8sProbeKind
	if len(denied) > 0 {
		problems = append(problems, fmt.Sprintf("credentials lack permission in namespace %q: %s", cfg.Namespace, strings.Join(denied, ", ")))
		kinds = append(kinds, K8sProbePermissionDenied)
	}

	namespaceCheck := checkNamespaceExists(ctx, kube, cfg.Namespace)
	checks := []optionalCheck{namespaceCheck}
	if namespaceCheck.kind != K8sProbeNamespaceMissing {
		checks = append(checks, checkStandingEgressPolicy(ctx, kube, cfg.Namespace))
	}
	if cfg.Type == "k8s-sandbox" {
		checks = append(checks, checkSandboxAPIServed(ctx, kube))
	}
	if runtimeClass != "" {
		checks = append(checks, checkRuntimeClassExists(ctx, kube, runtimeClass))
	}
	var notes []string
	for _, check := range checks {
		if check.problem != "" {
			problems = append(problems, check.problem)
			kinds = append(kinds, check.kind)
		}
		if check.note != "" {
			notes = append(notes, check.note)
		}
	}

	if len(problems) > 0 {
		return "", &K8sProbeError{Kinds: kinds, msg: strings.Join(problems, "; ")}
	}
	detail := fmt.Sprintf("%d required permissions granted in namespace %q", len(required), cfg.Namespace)
	if len(notes) > 0 {
		detail += "; " + strings.Join(notes, "; ")
	}
	return detail, nil
}

// effectiveRuntimeClass is the RuntimeClass the executor's Pods run under:
// k8s-sandbox always uses the gVisor class, k8s the class its runtime
// profile maps executor.runtime to.
func effectiveRuntimeClass(cfg K8sProbeConfig) (string, error) {
	if cfg.Type == "k8s-sandbox" {
		return gkeSandboxRuntimeValue, nil
	}
	profile, err := k8sRuntimeProfileFor(cfg.Runtime)
	if err != nil {
		return "", err
	}
	return profile.runtimeClassName, nil
}

// reviewAccess asks whether the current credentials may perform access in the
// namespace. A SelfSubjectAccessReview persists nothing.
func reviewAccess(ctx context.Context, kube kubernetes.Interface, namespace string, access k8sAccess) (bool, string, error) {
	review := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace:   namespace,
				Verb:        access.verb,
				Group:       access.group,
				Resource:    access.resource,
				Subresource: access.subresource,
				Name:        access.name,
			},
		},
	}
	got, err := kube.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, "", err
	}
	reason := got.Status.Reason
	if reason == "" {
		reason = got.Status.EvaluationError
	}
	return got.Status.Allowed, reason, nil
}

// checkNamespaceExists reads the target namespace. A refused or failed read is
// reported as not checked rather than as a failure.
func checkNamespaceExists(ctx context.Context, kube kubernetes.Interface, name string) optionalCheck {
	_, err := kube.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		return optionalCheck{note: fmt.Sprintf("namespace %q exists", name)}
	case apierrors.IsNotFound(err):
		return optionalCheck{kind: K8sProbeNamespaceMissing, problem: fmt.Sprintf("namespace %q does not exist in the cluster", name)}
	case apierrors.IsForbidden(err):
		return optionalCheck{note: fmt.Sprintf("namespace %q not checked (the credentials may not read namespaces)", name)}
	default:
		return optionalCheck{note: fmt.Sprintf("namespace %q not checked (%v)", name, err)}
	}
}

// checkStandingEgressPolicy looks for a NetworkPolicy that isolates every
// sandbox Pod for egress independently of the per-run policy, so that removing
// a per-run policy while its Pod still runs can only narrow egress. Credentials
// that may list NetworkPolicies are checked against every policy; credentials
// that may only get the reference policy by name are checked against that one.
// A refused or failed read is reported as not checked rather than as a failure.
func checkStandingEgressPolicy(ctx context.Context, kube kubernetes.Interface, namespace string) optionalCheck {
	list, err := kube.NetworkingV1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{})
	switch {
	case apierrors.IsForbidden(err):
		return checkNamedStandingPolicy(ctx, kube, namespace)
	case err != nil:
		return optionalCheck{note: fmt.Sprintf("standing egress policy not checked (%v)", err)}
	}
	var names []string
	for i := range list.Items {
		if isolatesEverySandboxPod(&list.Items[i]) {
			names = append(names, fmt.Sprintf("%q", list.Items[i].Name))
		}
	}
	if len(names) == 0 {
		return optionalCheck{kind: K8sProbeEgressBaselineMissing, problem: noStandingPolicyProblem(namespace)}
	}
	return optionalCheck{note: "standing egress policy " + strings.Join(names, ", ") + " selects every sandbox Pod"}
}

// checkNamedStandingPolicy reads the reference standing policy by name. A
// policy under another name is not seen, which the missing-policy problem says.
func checkNamedStandingPolicy(ctx context.Context, kube kubernetes.Interface, namespace string) optionalCheck {
	name := k8sStandingPolicyAccess.name
	policy, err := kube.NetworkingV1().NetworkPolicies(namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return optionalCheck{
			kind:    K8sProbeEgressBaselineMissing,
			problem: fmt.Sprintf("%s (the credentials may only get the policy named %q, so a standing policy under another name is not detected)", noStandingPolicyProblem(namespace), name),
		}
	case apierrors.IsForbidden(err):
		return optionalCheck{note: fmt.Sprintf("standing egress policy not checked (the credentials may not list networkpolicies or get %q)", name)}
	case err != nil:
		return optionalCheck{note: fmt.Sprintf("standing egress policy not checked (%v)", err)}
	case !isolatesEverySandboxPod(policy):
		return optionalCheck{
			kind:    K8sProbeEgressBaselineMissing,
			problem: fmt.Sprintf("NetworkPolicy %q in namespace %q does not isolate every %s=true Pod for egress", name, namespace, k8sSandboxLabel),
		}
	}
	return optionalCheck{note: fmt.Sprintf("standing egress policy %q selects every sandbox Pod", name)}
}

func noStandingPolicyProblem(namespace string) string {
	return fmt.Sprintf("no standing NetworkPolicy in namespace %q isolates every %s=true Pod for egress", namespace, k8sSandboxLabel)
}

// isolatesEverySandboxPod reports whether policy is an Egress policy whose Pod
// selector matches any sandbox Pod, whatever its per-Pod and run labels.
// Policies the executor installs select one Pod by name and never match.
func isolatesEverySandboxPod(policy *networkingv1.NetworkPolicy) bool {
	if !hasEgressPolicyType(policy) {
		return false
	}
	selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.PodSelector)
	if err != nil {
		return false
	}
	return selector.Matches(labels.Set(sandboxLabels(""))) &&
		selector.Matches(labels.Set(podLabels("stirrup-probe", "probe")))
}

// hasEgressPolicyType applies the API server's defaulting: a policy with no
// policyTypes is an Egress policy exactly when it has egress rules.
func hasEgressPolicyType(policy *networkingv1.NetworkPolicy) bool {
	if len(policy.Spec.PolicyTypes) == 0 {
		return len(policy.Spec.Egress) > 0
	}
	return slices.Contains(policy.Spec.PolicyTypes, networkingv1.PolicyTypeEgress)
}

// checkSandboxAPIServed confirms the Agent Sandbox API group and the sandboxes
// resource are served. A refused or failed discovery request is reported as
// not checked rather than as a failure.
func checkSandboxAPIServed(ctx context.Context, kube kubernetes.Interface) optionalCheck {
	gv := sandboxGVR.GroupVersion().String()
	resources, err := kube.Discovery().ServerResourcesForGroupVersionWithContext(ctx, gv)
	switch {
	case apierrors.IsNotFound(err):
		return optionalCheck{kind: K8sProbeSandboxAPIMissing, problem: fmt.Sprintf("the Agent Sandbox API %s is not served by the cluster (install the Agent Sandbox controller)", gv)}
	case err != nil:
		return optionalCheck{note: fmt.Sprintf("Agent Sandbox API %s not checked (%v)", gv, err)}
	}
	for _, r := range resources.APIResources {
		if r.Name == sandboxGVR.Resource {
			return optionalCheck{note: fmt.Sprintf("Agent Sandbox API %s served", gv)}
		}
	}
	return optionalCheck{kind: K8sProbeSandboxAPIMissing, problem: fmt.Sprintf("the cluster serves %s but not the %s resource", gv, sandboxGVR.Resource)}
}

// checkRuntimeClassExists reads the RuntimeClass the Pods will request. A
// refused or failed read is reported as not checked rather than as a failure.
func checkRuntimeClassExists(ctx context.Context, kube kubernetes.Interface, name string) optionalCheck {
	_, err := kube.NodeV1().RuntimeClasses().Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		return optionalCheck{note: fmt.Sprintf("RuntimeClass %q exists", name)}
	case apierrors.IsNotFound(err):
		return optionalCheck{kind: K8sProbeRuntimeClassMissing, problem: fmt.Sprintf("RuntimeClass %q does not exist in the cluster", name)}
	case apierrors.IsForbidden(err):
		return optionalCheck{note: fmt.Sprintf("RuntimeClass %q not checked (the credentials may not read runtimeclasses)", name)}
	default:
		return optionalCheck{note: fmt.Sprintf("RuntimeClass %q not checked (%v)", name, err)}
	}
}
