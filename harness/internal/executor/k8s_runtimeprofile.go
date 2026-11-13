package executor

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/rxbynerd/stirrup/types"
)

// k8sRuntimeProfile declares how the k8s executor adapts a sandbox Pod to
// one executor.runtime value. The requirement rules for a runtime live in
// types.K8sRuntimeRequirementErrors, not here.
type k8sRuntimeProfile struct {
	// runtimeClassName is the Pod RuntimeClassName; empty leaves it unset.
	runtimeClassName string
	// runtimeClassMissingHint says where the class comes from, for a run or
	// dry run that finds it missing; empty gives the generic advice.
	runtimeClassMissingHint string
	// podAnnotations are set on every sandbox Pod for this runtime.
	podAnnotations map[string]string
	// noNetworkAnnotations are added when network.mode is "none".
	noNetworkAnnotations map[string]string
	// strictAdmission adds the whole-Pod admission checks
	// (strictAdmissionChecks) at creation and during the readiness wait,
	// and reports an admission failure as a runtime diagnostic event.
	strictAdmission bool
	// requiredOverheadResources must each be a key of the admitted Pod's
	// spec.overhead, and requiredNodeSelector entries must be in its
	// spec.nodeSelector with exactly these values: the marks the platform's
	// own RuntimeClass sets, which a same-named class elsewhere lacks.
	// Checked only under strictAdmission.
	requiredOverheadResources []corev1.ResourceName
	requiredNodeSelector      map[string]string
	// governedAnnotationPrefix names the annotations the platform acts on.
	// Under strictAdmission every annotation with this prefix must keep its
	// submitted presence and value, except retunableAnnotations, which
	// admission may add or change but not remove. The profile's own
	// annotations all carry the prefix, so they are covered.
	governedAnnotationPrefix string
	retunableAnnotations     []string
	// diagnose runs runtime diagnostics after readiness and returns what the
	// guest reported; nil means the runtime has none. A failed check is a
	// *k8sRuntimeDiagnosticError.
	diagnose func(ctx context.Context, core *podExecCore) (map[string]string, error)
}

// k8sRuntimeProfiles has one entry per accepted k8s executor.runtime value,
// including "". The shared maps are read-only; callers copy them.
var k8sRuntimeProfiles = map[string]k8sRuntimeProfile{
	"":          {},
	"runc":      {runtimeClassName: "runc"},
	"gvisor":    {runtimeClassName: "gvisor"},
	"kata-qemu": {runtimeClassName: "kata-qemu"},
	"kata-fc":   {runtimeClassName: "kata-fc"},
	"kata-clh":  {runtimeClassName: "kata-clh"},
	"nitro-isolated": {
		runtimeClassName:        "isolated-sandbox",
		runtimeClassMissingHint: "the isolated-sandbox RuntimeClass is installed by EKS Auto Mode on Kubernetes 1.36 or later; see docs/executors/k8s.md#eks-with-nitro-sandboxes",
		// The default 2M log throttle also throttles exec stdout, the only
		// path results leave an offline guest by.
		podAnnotations:            map[string]string{"sandbox.amazonaws.com/log-bandwidth": "20M"},
		noNetworkAnnotations:      map[string]string{"sandbox.amazonaws.com/network-isolated": "true"},
		strictAdmission:           true,
		requiredOverheadResources: []corev1.ResourceName{"eks.amazonaws.com/nitro-sandbox"},
		requiredNodeSelector:      map[string]string{"eks.amazonaws.com/nitro-sandbox": "true"},
		governedAnnotationPrefix:  "sandbox.amazonaws.com/",
		// The throttle keys AWS documents for Nitro Sandboxes.
		retunableAnnotations: []string{
			"sandbox.amazonaws.com/log-bandwidth",
			"sandbox.amazonaws.com/ingress-bandwidth",
			"sandbox.amazonaws.com/egress-bandwidth",
			"sandbox.amazonaws.com/ingress-packets-per-second",
			"sandbox.amazonaws.com/egress-packets-per-second",
			"sandbox.amazonaws.com/dns-packets-per-second",
			"sandbox.amazonaws.com/overlay-disk-bandwidth",
			"sandbox.amazonaws.com/overlay-disk-iops",
			"sandbox.amazonaws.com/rootfs-disk-bandwidth",
			"sandbox.amazonaws.com/rootfs-disk-iops",
			"sandbox.amazonaws.com/conntrack-limit",
		},
		diagnose: diagnoseNitroIsolatedGuest,
	},
}

// k8sRuntimeProfileFor returns the profile for an executor.runtime value.
// A value without a profile is an error rather than a fallback, so a caller
// that bypasses ValidateRunConfig cannot run under assumptions the executor
// does not hold for that runtime.
func k8sRuntimeProfileFor(runtime string) (k8sRuntimeProfile, error) {
	profile, ok := k8sRuntimeProfiles[runtime]
	if !ok {
		return k8sRuntimeProfile{}, fmt.Errorf(
			"k8s executor: unsupported executor.runtime %q (accepted: %s, or empty)",
			runtime, strings.Join(types.ValidK8sExecutorRuntimeValues(), ", "))
	}
	return profile, nil
}

// K8sRuntimeClassMissingHint is the remediation for a missing RuntimeClass
// that is specific to a k8s executor.runtime value, or "" when the runtime
// has none and the generic advice applies.
func K8sRuntimeClassMissingHint(runtime string) string {
	return k8sRuntimeProfiles[runtime].runtimeClassMissingHint
}
