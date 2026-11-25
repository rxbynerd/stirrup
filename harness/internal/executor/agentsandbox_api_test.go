package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1unstructured "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var (
	testSandboxGVR      = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}
	testSandboxGVRAlpha = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1alpha1", Resource: "sandboxes"}
)

// serveSandboxVersions makes the fake cluster's discovery serve the Sandbox
// API at exactly the given versions, each with the sandboxes resource.
func serveSandboxVersions(kube kubernetes.Interface, versions ...string) {
	var lists []*metav1.APIResourceList
	for _, v := range versions {
		lists = append(lists, &metav1.APIResourceList{
			GroupVersion: sandboxGroup + "/" + v,
			APIResources: []metav1.APIResource{{Name: sandboxResource}},
		})
	}
	kube.Discovery().(*fakediscovery.FakeDiscovery).Resources = lists
}

func TestResolveSandboxGVR_PicksTheMostPreferredServedVersion(t *testing.T) {
	tests := []struct {
		name   string
		served []string
		want   schema.GroupVersionResource
	}{
		{name: "both served", served: []string{"v1alpha1", "v1beta1"}, want: testSandboxGVR},
		{name: "only v1beta1", served: []string{"v1beta1"}, want: testSandboxGVR},
		{name: "only v1alpha1", served: []string{"v1alpha1"}, want: testSandboxGVRAlpha},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			kube := fake.NewClientset()
			serveSandboxVersions(kube, tt.served...)

			got, err := resolveSandboxGVR(context.Background(), kube.Discovery())

			must.NoError(err)
			check.Equal(tt.want, got)
		})
	}
}

func TestResolveSandboxGVR_FallsBackPastAVersionWithoutTheResource(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	kube := fake.NewClientset()
	serveSandboxVersions(kube, "v1alpha1")
	disco := kube.Discovery().(*fakediscovery.FakeDiscovery)
	disco.Resources = append(disco.Resources, &metav1.APIResourceList{GroupVersion: sandboxGroup + "/v1beta1"})

	got, err := resolveSandboxGVR(context.Background(), disco)

	must.NoError(err)
	check.Equal(testSandboxGVRAlpha, got)
}

func TestResolveSandboxGVR_NeitherVersionServedIsAnError(t *testing.T) {
	must := require.New(t)
	kube := fake.NewClientset()

	_, err := resolveSandboxGVR(context.Background(), kube.Discovery())

	must.ErrorIs(err, errSandboxAPINotServed)
	must.ErrorContains(err, "agents.x-k8s.io/v1beta1 or agents.x-k8s.io/v1alpha1 is not served")
	must.ErrorContains(err, "install the Agent Sandbox controller")
}

func TestResolveSandboxGVR_ServedWithoutTheResourceIsAnError(t *testing.T) {
	must := require.New(t)
	kube := fake.NewClientset()
	kube.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{
		{GroupVersion: sandboxGroup + "/v1beta1"},
		{GroupVersion: sandboxGroup + "/v1alpha1"},
	}

	_, err := resolveSandboxGVR(context.Background(), kube.Discovery())

	must.ErrorIs(err, errSandboxResourceMissing)
	must.EqualError(err, "the cluster serves agents.x-k8s.io/v1beta1 and agents.x-k8s.io/v1alpha1 but not the sandboxes resource")
}

func TestResolveSandboxGVR_OtherDiscoveryErrorsFailClosedWithoutFallback(t *testing.T) {
	tests := map[string]error{
		"forbidden":   apierrors.NewForbidden(schema.GroupResource{Resource: "resource"}, "", errors.New("denied")),
		"unavailable": apierrors.NewServiceUnavailable("apiserver restarting"),
		"transport":   errors.New("connection reset"),
	}
	for name, discoErr := range tests {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			kube := fake.NewClientset()
			serveSandboxVersions(kube, "v1alpha1")
			kube.PrependReactor("get", "resource", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, discoErr
			})

			_, err := resolveSandboxGVR(context.Background(), kube.Discovery())

			must.ErrorIs(err, discoErr)
			must.NotErrorIs(err, errSandboxAPINotServed)
			must.ErrorContains(err, "agents.x-k8s.io/v1beta1")
			check.Len(kube.Actions(), 1, "discovery fell back to another version after the failure")
		})
	}
}

// readyCondition models a single status.conditions entry as the Agent Sandbox
// controller emits it: {type, status, reason, message,
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
// status.conditions list. Note the deliberate absence of status.podIPs: the
// controller does not emit it, and sandboxReady must not depend on it.
func sandboxWithConditions(conditions []any) *metav1unstructured.Unstructured {
	return &metav1unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "agents.x-k8s.io/v1beta1",
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

func TestSandboxReady_RequiresReadyTrueCondition(t *testing.T) {
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
			name: "conditions present but no ready type",
			obj: sandboxWithConditions([]any{
				readyCondition("Initialized", "True"),
				readyCondition("PodScheduled", "True"),
			}),
			want: false,
		},
		{
			name: "non-ready condition true but ready false",
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
			name: "ready type but status missing",
			obj: sandboxWithConditions([]any{
				map[string]any{"type": "Ready"},
			}),
			want: false,
		},
		{
			name: "ready type but status wrong type",
			obj: sandboxWithConditions([]any{
				map[string]any{"type": "Ready", "status": true},
			}),
			want: false,
		},
		{
			name: "malformed entry skipped before valid ready true",
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
			must := require.New(t)
			must.Equal(tt.want, sandboxReady(tt.obj))
		})
	}
}

func TestResolveSandboxPodName_PrefersNonEmptyAnnotation(t *testing.T) {
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
			must := require.New(t)
			must.Equal(tt.want, resolveSandboxPodName(tt.obj))
		})
	}
}
