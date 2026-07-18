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
	// k8sSandboxLabel marks every sandbox Pod the k8s-family executors create;
	// the per-Pod NetworkPolicy selects on it (combined with the pod-name label).
	k8sSandboxLabel = "stirrup-sandbox"

	// k8sPodNameLabel mirrors the Pod name into a label since metadata.name is
	// not selectable by a LabelSelector.
	k8sPodNameLabel = "stirrup.dev/pod"

	// k8sDNSPort is the standard DNS port; the allowlist policy opens it
	// unconditionally alongside the proxy.
	k8sDNSPort = 53

	// k8sEgressProxyLabel is the label (app=stirrup-egress-proxy) the allowlist
	// policy's egress peer selects on; must match the manifests in
	// examples/k8s/egress-proxy/.
	k8sEgressProxyLabel = "stirrup-egress-proxy"

	// k8sEgressProxyPort is the egress proxy's listen port; the allowlist
	// policy confines egress to this port so an enforcing CNI does not permit
	// reaching the proxy Pod on any other port.
	k8sEgressProxyPort = 8080
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
// permits no egress at all (installed for Mode=="none"). Enforcement depends
// on the cluster CNI supporting NetworkPolicy; see docs/executors/k8s.md.
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
// permits egress only to (a) the in-namespace egress proxy and (b) DNS
// (installed for Mode=="allowlist"). The FQDN allowlist itself lives in the
// proxy, not the policy — NetworkPolicy operates on IPs/ports/selectors. The
// proxy peer selector has no NamespaceSelector, so the egress proxy
// Deployment must run in the sandbox's namespace. See docs/executors/k8s.md.
func allowlistEgressPolicy(namespace, podName string) *networkingv1.NetworkPolicy {
	dnsPort := intstr.FromInt32(k8sDNSPort)
	proxyPort := intstr.FromInt32(k8sEgressProxyPort)
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
					// Peer-unrestricted (To omitted) so this matches whichever
					// Pod/CIDR hosts kube-dns.
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &udp, Port: &dnsPort},
						{Protocol: &tcp, Port: &dnsPort},
					},
				},
				{
					// Confined to the proxy's listen port so an enforcing CNI
					// can't let the sandbox reach the proxy Pod on any port.
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &tcp, Port: &proxyPort},
					},
					To: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": k8sEgressProxyLabel,
								},
							},
						},
					},
				},
			},
		},
	}
}

// egressPolicyFor returns the NetworkPolicy for the given network mode:
// deny-all for "none", proxy+DNS allowlist for "allowlist". Any other mode
// is a programming error and is surfaced as an error rather than silently
// installing no policy.
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

// proxyEnvFor returns the HTTP_PROXY/HTTPS_PROXY/NO_PROXY entries to inject
// for the given network mode. Mode=="allowlist" requires a non-empty
// proxyURL; mode=="none" injects nothing.
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
