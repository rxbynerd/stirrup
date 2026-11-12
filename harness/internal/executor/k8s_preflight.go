package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/rxbynerd/stirrup/types"
)

// K8sProbeConfig selects what ProbeK8sAccess checks. Type is the executor
// type, "k8s" or "k8s-sandbox".
type K8sProbeConfig struct {
	Type             string
	Namespace        string
	Kubeconfig       string
	RuntimeClassName string
}

// k8sAccess is one permission the executor needs in its namespace.
// subresource is empty for a plain resource.
type k8sAccess struct {
	group, resource, subresource, verb string
}

func (a k8sAccess) String() string {
	name := a.resource
	if a.subresource != "" {
		name += "/" + a.subresource
	}
	if a.group != "" {
		name += "." + a.group
	}
	return a.verb + " " + name
}

// k8sExecAccess is checked as create alone: the SPDY transport authorises POST
// on pods/exec as create, and the WebSocket transport falls back to SPDY when
// its upgrade is refused (see newFallbackRemoteExecutor).
var k8sExecAccess = k8sAccess{resource: "pods", subresource: "exec", verb: "create"}

var k8sNetworkPolicyAccess = []k8sAccess{
	{group: "networking.k8s.io", resource: "networkpolicies", verb: "create"},
	{group: "networking.k8s.io", resource: "networkpolicies", verb: "delete"},
}

// k8sRequiredAccess returns the namespaced permissions the selected executor
// uses at run time: the calls NewK8sExecutor, Close and the pods/exec streams
// make for "k8s", and the Sandbox calls in NewAgentSandboxExecutor and Close
// for "k8s-sandbox".
func k8sRequiredAccess(executorType string) []k8sAccess {
	var workload []k8sAccess
	if executorType == "k8s-sandbox" {
		workload = []k8sAccess{
			{group: sandboxGVR.Group, resource: sandboxGVR.Resource, verb: "create"},
			{group: sandboxGVR.Group, resource: sandboxGVR.Resource, verb: "get"},
			{group: sandboxGVR.Group, resource: sandboxGVR.Resource, verb: "delete"},
		}
	} else {
		workload = []k8sAccess{
			{resource: "pods", verb: "create"},
			{resource: "pods", verb: "get"},
			{resource: "pods", verb: "delete"},
		}
	}
	required := append(workload, k8sExecAccess)
	return append(required, k8sNetworkPolicyAccess...)
}

// ValidateK8sNetwork applies the offline checks the k8s and k8s-sandbox
// constructors make on the egress posture, without contacting a cluster: the
// mode must be supported and allowlist mode needs a proxy URL.
func ValidateK8sNetwork(network *types.NetworkConfig, proxyURL string) error {
	if _, err := proxyEnvFor(network, proxyURL); err != nil {
		return err
	}
	_, err := egressPolicyFor(network, "", "", "")
	return err
}

// ProbeK8sAccess is the dry-run check for the k8s and k8s-sandbox executors.
// It asks the API server whether the configured credentials hold every
// permission the executor uses, through SelfSubjectAccessReview, and never
// creates, updates or deletes a Pod, Sandbox or NetworkPolicy. The returned
// detail summarises what passed and which optional checks could not be made.
func ProbeK8sAccess(ctx context.Context, cfg K8sProbeConfig) (string, error) {
	restCfg, err := buildRESTConfig(cfg.Kubeconfig)
	if err != nil {
		return "", fmt.Errorf("build kube rest config: %w", err)
	}
	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return "", fmt.Errorf("build kube clientset: %w", err)
	}
	return probeK8sAccess(ctx, kube, cfg)
}

func probeK8sAccess(ctx context.Context, kube kubernetes.Interface, cfg K8sProbeConfig) (string, error) {
	required := k8sRequiredAccess(cfg.Type)
	var denied []string
	for _, access := range required {
		allowed, reason, err := reviewAccess(ctx, kube, cfg.Namespace, access)
		if err != nil {
			return "", fmt.Errorf("check permission %q in namespace %q: %w", access, cfg.Namespace, err)
		}
		if !allowed {
			entry := fmt.Sprintf("%q", access.String())
			if reason != "" {
				entry += " (" + reason + ")"
			}
			denied = append(denied, entry)
		}
	}

	var problems, notes []string
	if len(denied) > 0 {
		problems = append(problems, fmt.Sprintf("credentials lack permission in namespace %q: %s", cfg.Namespace, strings.Join(denied, ", ")))
	}

	if cfg.Type == "k8s-sandbox" {
		problem, note := checkSandboxAPIServed(ctx, kube)
		problems, notes = appendResult(problems, notes, problem, note)
	}
	if runtimeClass := effectiveRuntimeClass(cfg); runtimeClass != "" {
		problem, note := checkRuntimeClassExists(ctx, kube, runtimeClass)
		problems, notes = appendResult(problems, notes, problem, note)
	}

	if len(problems) > 0 {
		return "", errors.New(strings.Join(problems, "; "))
	}
	detail := fmt.Sprintf("%d required permissions granted in namespace %q", len(required), cfg.Namespace)
	if len(notes) > 0 {
		detail += "; " + strings.Join(notes, "; ")
	}
	return detail, nil
}

func appendResult(problems, notes []string, problem, note string) ([]string, []string) {
	if problem != "" {
		problems = append(problems, problem)
	}
	if note != "" {
		notes = append(notes, note)
	}
	return problems, notes
}

// effectiveRuntimeClass is the RuntimeClass the executor's Pods run under:
// k8s-sandbox always uses the gVisor class, k8s uses executor.runtime.
func effectiveRuntimeClass(cfg K8sProbeConfig) string {
	if cfg.Type == "k8s-sandbox" {
		return gkeSandboxRuntimeValue
	}
	return cfg.RuntimeClassName
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

// checkSandboxAPIServed confirms the Agent Sandbox API group and the sandboxes
// resource are served. Discovery is readable by every authenticated user by
// default; a refusal or transport error is reported as not checked.
func checkSandboxAPIServed(ctx context.Context, kube kubernetes.Interface) (problem, note string) {
	gv := sandboxGVR.GroupVersion().String()
	resources, err := kube.Discovery().ServerResourcesForGroupVersionWithContext(ctx, gv)
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Sprintf("the Agent Sandbox API %s is not served by the cluster (install the Agent Sandbox controller)", gv), ""
	case err != nil:
		return "", fmt.Sprintf("Agent Sandbox API %s not checked (%v)", gv, err)
	}
	for _, r := range resources.APIResources {
		if r.Name == sandboxGVR.Resource {
			return "", fmt.Sprintf("Agent Sandbox API %s served", gv)
		}
	}
	return fmt.Sprintf("the cluster serves %s but not the %s resource", gv, sandboxGVR.Resource), ""
}

// checkRuntimeClassExists reads the RuntimeClass the Pods will request. The
// reference Role grants no cluster-scoped reads, so a refusal is reported as
// not checked rather than as a failure.
func checkRuntimeClassExists(ctx context.Context, kube kubernetes.Interface, name string) (problem, note string) {
	_, err := kube.NodeV1().RuntimeClasses().Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		return "", fmt.Sprintf("RuntimeClass %q exists", name)
	case apierrors.IsNotFound(err):
		return fmt.Sprintf("RuntimeClass %q does not exist in the cluster", name), ""
	case apierrors.IsForbidden(err):
		return "", fmt.Sprintf("RuntimeClass %q not checked (the credentials may not read runtimeclasses)", name)
	default:
		return "", fmt.Sprintf("RuntimeClass %q not checked (%v)", name, err)
	}
}
