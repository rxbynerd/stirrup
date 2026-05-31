package executor

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/rxbynerd/stirrup/types"
)

const (
	// k8sSandboxLabel marks every Pod the K8sExecutor creates. It is the
	// stable selector a cluster operator can target with their own policies
	// and the key the per-Pod NetworkPolicy selects on (combined with the
	// unique pod name) so the policy binds to exactly this Pod.
	k8sSandboxLabel = "stirrup-sandbox"

	// k8sPodNameLabel carries the generated Pod name as a label so the
	// NetworkPolicy's podSelector can bind to this single Pod rather than
	// every sandbox in the namespace. metadata.name is not selectable by a
	// LabelSelector, so the name is mirrored into a label.
	k8sPodNameLabel = "stirrup.dev/pod"

	// k8sDNSPort is the standard DNS port. Both egress modes that permit any
	// traffic still need DNS resolution to function, so the allowlist policy
	// opens UDP/TCP 53 to kube-dns alongside the proxy.
	k8sDNSPort = 53
)

// podLabels returns the label set applied to every sandbox Pod. The sandbox
// marker is constant; the pod-name label is unique per Pod so a per-Pod
// NetworkPolicy can select exactly this Pod.
func podLabels(podName string) map[string]string {
	return map[string]string{
		k8sSandboxLabel: "true",
		k8sPodNameLabel: podName,
	}
}

// networkPolicyName derives the NetworkPolicy object name from the Pod name.
// One policy is created per Pod and torn down with it, so the names are
// coupled 1:1.
func networkPolicyName(podName string) string {
	return podName + "-egress"
}

// denyAllEgressPolicy builds a NetworkPolicy that selects the named Pod and
// permits no egress at all. An Egress policy type with an empty egress rule
// list is the canonical Kubernetes idiom for "deny all outgoing traffic" for
// the selected Pods (see the NetworkPolicySpec.Egress and PolicyTypes
// documentation). It is installed for Mode=="none".
//
// CAVEAT: enforcement depends on the cluster CNI. kindnet (kind's default
// CNI) accepts NetworkPolicy objects but does NOT enforce them, so this
// policy is inert on a stock kind cluster. A CNI that enforces NetworkPolicy
// (Cilium, Calico) is required for the deny to take effect. This mirrors the
// container executor's honest fail-open caveat around host.docker.internal.
func denyAllEgressPolicy(namespace, podName string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      networkPolicyName(podName),
			Namespace: namespace,
			Labels:    map[string]string{k8sSandboxLabel: "true"},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{k8sPodNameLabel: podName},
			},
			// No egress rules + an explicit Egress policy type = deny all
			// outgoing traffic. Omitting PolicyTypes would default to
			// Ingress-only, leaving egress unrestricted.
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		},
	}
}

// allowlistEgressPolicy builds a NetworkPolicy that selects the named Pod and
// permits egress only to (a) the in-namespace egress proxy and (b) DNS, so
// every other destination is forced through the proxy where the FQDN
// allowlist applies. It is installed for Mode=="allowlist".
//
// The policy intentionally does NOT encode the FQDN allowlist itself:
// NetworkPolicy operates on IPs/ports/selectors, not hostnames, so the
// hostname allowlist lives in the proxy (egressproxy.Matcher). This policy's
// job is the complementary half — guarantee the Pod cannot reach the network
// except via that proxy. DNS (UDP+TCP 53) is opened unconditionally because
// the in-Pod client must resolve the proxy's address and the proxy itself
// resolves upstream names.
//
// CAVEAT: as with denyAllEgressPolicy, enforcement depends on a
// NetworkPolicy-enforcing CNI. On kindnet the policy is accepted but inert.
func allowlistEgressPolicy(namespace, podName string) *networkingv1.NetworkPolicy {
	dnsPort := intstr.FromInt32(k8sDNSPort)
	udp := corev1.ProtocolUDP
	tcp := corev1.ProtocolTCP

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      networkPolicyName(podName),
			Namespace: namespace,
			Labels:    map[string]string{k8sSandboxLabel: "true"},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{k8sPodNameLabel: podName},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					// DNS resolution. Left peer-unrestricted (To omitted) so
					// the rule matches whichever Pod/CIDR hosts kube-dns; the
					// port restriction confines it to 53.
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &udp, Port: &dnsPort},
						{Protocol: &tcp, Port: &dnsPort},
					},
				},
				{
					// Egress to the proxy Deployment, selected by the label
					// the proxy manifests set. The proxy then enforces the
					// FQDN allowlist on the operator's behalf.
					To: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": "stirrup-egress-proxy",
								},
							},
						},
					},
				},
			},
		},
	}
}

// egressPolicyFor returns the NetworkPolicy to install for the given network
// mode, or nil when no policy applies. Mode=="none" yields a deny-all egress
// policy; Mode=="allowlist" yields the proxy+DNS allowlist policy. Any other
// (non-empty) mode is a programming error reachable only if validation is
// bypassed; it is surfaced as an error rather than silently installing no
// policy, which would leave the Pod with cluster-default egress while
// CanNetwork might report otherwise.
func egressPolicyFor(network *types.NetworkConfig, namespace, podName string) (*networkingv1.NetworkPolicy, error) {
	if network == nil {
		return nil, fmt.Errorf("k8s executor: network config is required (fail-closed)")
	}
	switch network.Mode {
	case "none":
		return denyAllEgressPolicy(namespace, podName), nil
	case "allowlist":
		return allowlistEgressPolicy(namespace, podName), nil
	default:
		return nil, fmt.Errorf("k8s executor: unsupported network mode %q (want \"none\" or \"allowlist\")", network.Mode)
	}
}

// proxyEnvFor returns the HTTP_PROXY/HTTPS_PROXY/NO_PROXY environment entries
// to inject into the sandbox container for the given network mode. Mode==
// "allowlist" requires a non-empty proxyURL (the run cannot reach the network
// any other way once the NetworkPolicy is enforced) and returns the three
// proxy variables. Mode=="none" injects nothing — a denied Pod has no egress
// to proxy. A nil network is rejected for symmetry with egressPolicyFor.
func proxyEnvFor(network *types.NetworkConfig, proxyURL string) ([]corev1.EnvVar, error) {
	if network == nil {
		return nil, fmt.Errorf("k8s executor: network config is required (fail-closed)")
	}
	switch network.Mode {
	case "none":
		return nil, nil
	case "allowlist":
		if proxyURL == "" {
			return nil, fmt.Errorf("k8s executor: network mode \"allowlist\" requires an egress proxy URL (set executor.k8sEgressProxyUrl / --k8s-egress-proxy-url)")
		}
		return []corev1.EnvVar{
			{Name: "HTTP_PROXY", Value: proxyURL},
			{Name: "HTTPS_PROXY", Value: proxyURL},
			{Name: "NO_PROXY", Value: "localhost,127.0.0.1,::1"},
		}, nil
	default:
		return nil, fmt.Errorf("k8s executor: unsupported network mode %q (want \"none\" or \"allowlist\")", network.Mode)
	}
}
