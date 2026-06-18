package executor

import (
	metav1unstructured "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// sandboxGVR is the GroupVersionResource for the Agent Sandbox CRD as served
// by GKE Agent Sandbox. GKE installs v1alpha1; upstream HEAD has moved to
// v1beta1 with a different field shape, so this is pinned to what the cluster
// actually serves. Changing this is the single edit needed to track a GKE bump.
var sandboxGVR = schema.GroupVersionResource{
	Group: "agents.x-k8s.io", Version: "v1alpha1", Resource: "sandboxes",
}

const (
	// sandboxNameHashLabel is the label the Sandbox controller puts on every
	// backing Pod (value = FNV-1a hash of the Sandbox name). It is the
	// controller-guaranteed selector for the Pod.
	sandboxNameHashLabel = "agents.x-k8s.io/sandbox-name-hash" //nolint:unused // documents the controller-guaranteed Pod label; becomes load-bearing for the warm-pool follow-up, which selects the backing Pod by this hash instead of the cold-path name==Sandbox identity.
	// sandboxPodNameAnnotation, when present on the Sandbox, names the backing
	// Pod. It is set for warm-pool-adopted Sandboxes (random Pod name); for a
	// cold-created Sandbox it is absent and the Pod name equals the Sandbox name.
	sandboxPodNameAnnotation = "agents.x-k8s.io/pod-name"
	// sandboxReadyConditionType is the Sandbox status condition that gates
	// readiness; the controller sets it True only once the backing Pod is Ready.
	sandboxReadyConditionType = "Ready"
)

// sandboxReady reports whether the Sandbox's status carries a "Ready" condition
// with status "True". It gates purely on that condition and deliberately
// ignores status.podIPs, which GKE's v1alpha1 controller does not emit. A
// missing or malformed status yields false rather than a panic, so a partially
// populated object observed mid-reconcile is simply treated as not-yet-ready.
func sandboxReady(obj *metav1unstructured.Unstructured) bool {
	// NestedFieldNoCopy, not NestedSlice: the latter deep-copies the slice and
	// panics on any element that is not a JSON-native type (e.g. a stray int),
	// whereas this reader only inspects the conditions and must never panic on a
	// malformed status. The no-copy accessor returns the raw value untouched.
	raw, found, err := metav1unstructured.NestedFieldNoCopy(obj.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	conditions, ok := raw.([]any)
	if !ok {
		return false
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		condType, ok := cond["type"].(string)
		if !ok || condType != sandboxReadyConditionType {
			continue
		}
		condStatus, ok := cond["status"].(string)
		if !ok {
			return false
		}
		return condStatus == "True"
	}
	return false
}

// resolveSandboxPodName returns the name of the Pod backing the Sandbox. It
// matches the upstream controller's own resolvePodName: the pod-name annotation
// wins when set (the warm-pool-adopted path, where the Pod has a random name),
// otherwise the Pod name equals the Sandbox name (the cold-created path).
func resolveSandboxPodName(obj *metav1unstructured.Unstructured) string {
	if name := obj.GetAnnotations()[sandboxPodNameAnnotation]; name != "" {
		return name
	}
	return obj.GetName()
}
