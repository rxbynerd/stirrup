package executor

import (
	"testing"

	metav1unstructured "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// readyCondition models a single status.conditions entry as GKE's v1alpha1
// Sandbox controller emits it: {type, status, reason, message,
// lastTransitionTime, observedGeneration}. Only type and status are
// load-bearing for sandboxReady; the rest are carried so the test documents
// the real wire shape.
func readyCondition(condType, status string) map[string]any {
	return map[string]any{
		"type":               condType,
		"status":             status,
		"reason":             "PodReady",
		"message":            "backing pod is ready",
		"lastTransitionTime": "2026-06-15T00:00:00Z",
		"observedGeneration": int64(1),
	}
}

// sandboxWithConditions builds a Sandbox unstructured object carrying the given
// status.conditions list. Note the deliberate absence of status.podIPs: GKE's
// v1alpha1 does not emit it, and sandboxReady must not depend on it.
func sandboxWithConditions(conditions []any) *metav1unstructured.Unstructured {
	return &metav1unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "agents.x-k8s.io/v1alpha1",
			"kind":       "Sandbox",
			"metadata": map[string]any{
				"name": "demo",
			},
			"status": map[string]any{
				"conditions": conditions,
			},
		},
	}
}

func TestSandboxReady(t *testing.T) {
	tests := []struct {
		name string
		obj  *metav1unstructured.Unstructured
		want bool
	}{
		{
			name: "ready true",
			obj:  sandboxWithConditions([]any{readyCondition("Ready", "True")}),
			want: true,
		},
		{
			name: "ready false",
			obj:  sandboxWithConditions([]any{readyCondition("Ready", "False")}),
			want: false,
		},
		{
			name: "ready unknown",
			obj:  sandboxWithConditions([]any{readyCondition("Ready", "Unknown")}),
			want: false,
		},
		{
			name: "no conditions field",
			obj: &metav1unstructured.Unstructured{Object: map[string]any{
				"status": map[string]any{},
			}},
			want: false,
		},
		{
			name: "no status at all",
			obj:  &metav1unstructured.Unstructured{Object: map[string]any{}},
			want: false,
		},
		{
			name: "empty conditions list",
			obj:  sandboxWithConditions([]any{}),
			want: false,
		},
		{
			name: "conditions present but no Ready type",
			obj: sandboxWithConditions([]any{
				readyCondition("Initialized", "True"),
				readyCondition("PodScheduled", "True"),
			}),
			want: false,
		},
		{
			name: "non-Ready condition True but Ready False",
			obj: sandboxWithConditions([]any{
				readyCondition("Initialized", "True"),
				readyCondition("Ready", "False"),
			}),
			want: false,
		},
		{
			name: "malformed entry: not a map",
			obj:  sandboxWithConditions([]any{"not-a-condition"}),
			want: false,
		},
		{
			name: "malformed entry: missing type key",
			obj: sandboxWithConditions([]any{
				map[string]any{"status": "True"},
			}),
			want: false,
		},
		{
			name: "malformed entry: type wrong type",
			obj: sandboxWithConditions([]any{
				map[string]any{"type": 42, "status": "True"},
			}),
			want: false,
		},
		{
			name: "Ready type but status missing",
			obj: sandboxWithConditions([]any{
				map[string]any{"type": "Ready"},
			}),
			want: false,
		},
		{
			name: "Ready type but status wrong type",
			obj: sandboxWithConditions([]any{
				map[string]any{"type": "Ready", "status": true},
			}),
			want: false,
		},
		{
			name: "malformed entry skipped before valid Ready True",
			obj: sandboxWithConditions([]any{
				"junk",
				map[string]any{"type": 7},
				readyCondition("Ready", "True"),
			}),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sandboxReady(tt.obj); got != tt.want {
				t.Errorf("sandboxReady() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveSandboxPodName(t *testing.T) {
	sandbox := func(name string, annotations map[string]any) *metav1unstructured.Unstructured {
		meta := map[string]any{"name": name}
		if annotations != nil {
			meta["annotations"] = annotations
		}
		return &metav1unstructured.Unstructured{Object: map[string]any{
			"metadata": meta,
		}}
	}

	tests := []struct {
		name string
		obj  *metav1unstructured.Unstructured
		want string
	}{
		{
			name: "annotation present and non-empty wins",
			obj: sandbox("cold-name", map[string]any{
				sandboxPodNameAnnotation: "warm-pool-pod-xyz",
			}),
			want: "warm-pool-pod-xyz",
		},
		{
			name: "annotation absent falls back to name",
			obj:  sandbox("cold-name", nil),
			want: "cold-name",
		},
		{
			name: "annotation present but empty falls back to name",
			obj: sandbox("cold-name", map[string]any{
				sandboxPodNameAnnotation: "",
			}),
			want: "cold-name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveSandboxPodName(tt.obj); got != tt.want {
				t.Errorf("resolveSandboxPodName() = %q, want %q", got, tt.want)
			}
		})
	}
}
