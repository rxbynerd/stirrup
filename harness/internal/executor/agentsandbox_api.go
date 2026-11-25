package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1unstructured "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

const (
	sandboxGroup    = "agents.x-k8s.io"
	sandboxResource = "sandboxes"
)

// sandboxVersions lists the Sandbox API versions the executor can drive, most
// preferred first. The object shape the executor writes and reads is the same
// in each, so only the apiVersion and the dynamic client's resource differ.
var sandboxVersions = []string{"v1beta1", "v1alpha1"}

var (
	// errSandboxAPINotServed ends the error for a cluster that serves none of
	// sandboxVersions.
	errSandboxAPINotServed = errors.New("is not served by the cluster (install the Agent Sandbox controller)")
	// errSandboxResourceMissing ends the error for a cluster that serves a
	// version without the sandboxes resource.
	errSandboxResourceMissing = errors.New("not the " + sandboxResource + " resource")
)

// sandboxGroupVersions renders the supported versions as full group/version
// strings joined by sep.
func sandboxGroupVersions(versions []string, sep string) string {
	out := make([]string, len(versions))
	for i, v := range versions {
		out[i] = sandboxGroup + "/" + v
	}
	return strings.Join(out, sep)
}

// resolveSandboxGVR returns the most preferred supported Sandbox version the
// cluster serves with the sandboxes resource. A version the cluster does not
// serve is skipped; any other discovery error is returned without falling
// back. With no usable version the error wraps errSandboxAPINotServed, or
// errSandboxResourceMissing if some version lacked the resource.
func resolveSandboxGVR(ctx context.Context, disco discovery.DiscoveryInterfaces) (schema.GroupVersionResource, error) {
	var withoutResource []string
	for _, version := range sandboxVersions {
		gv := schema.GroupVersion{Group: sandboxGroup, Version: version}
		resources, err := disco.ServerResourcesForGroupVersionWithContext(ctx, gv.String())
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return schema.GroupVersionResource{}, fmt.Errorf("discover %s: %w", gv, err)
		}
		if slices.ContainsFunc(resources.APIResources, func(r metav1.APIResource) bool { return r.Name == sandboxResource }) {
			return gv.WithResource(sandboxResource), nil
		}
		withoutResource = append(withoutResource, version)
	}
	if len(withoutResource) > 0 {
		return schema.GroupVersionResource{}, fmt.Errorf("the cluster serves %s but %w", sandboxGroupVersions(withoutResource, " and "), errSandboxResourceMissing)
	}
	return schema.GroupVersionResource{}, fmt.Errorf("the Agent Sandbox API %s %w", sandboxGroupVersions(sandboxVersions, " or "), errSandboxAPINotServed)
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
// ignores status.podIPs, which the controller does not emit. A missing or
// malformed status yields false rather than a panic, so a partially populated
// object observed mid-reconcile is simply treated as not-yet-ready.
func sandboxReady(obj *metav1unstructured.Unstructured) bool {
	cond, ok := sandboxReadyCondition(obj)
	if !ok {
		return false
	}
	condStatus, ok := cond["status"].(string)
	return ok && condStatus == "True"
}

// sandboxReadyCondition returns the first "Ready" entry of the Sandbox's
// status.conditions, if any.
func sandboxReadyCondition(obj *metav1unstructured.Unstructured) (map[string]any, bool) {
	// NestedFieldNoCopy, not NestedSlice: the latter deep-copies the slice and
	// panics on any element that is not a JSON-native type (e.g. a stray int),
	// whereas this reader only inspects the conditions and must never panic on a
	// malformed status. The no-copy accessor returns the raw value untouched.
	raw, found, err := metav1unstructured.NestedFieldNoCopy(obj.Object, "status", "conditions")
	if err != nil || !found {
		return nil, false
	}
	conditions, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if condType, ok := cond["type"].(string); ok && condType == sandboxReadyConditionType {
			return cond, true
		}
	}
	return nil, false
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
