package executor

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/rxbynerd/stirrup/types"
)

// quantityEquals reports whether the ResourceList entry at name equals the
// quantity parsed from want. Comparison is by numeric value (Quantity.Cmp)
// rather than string, because resource.Quantity canonicalises its suffix
// on String() — "1024Mi" round-trips as "1Gi", which is the same value.
// A missing key never equals a non-empty want.
func quantityEquals(t *testing.T, list corev1.ResourceList, name corev1.ResourceName, want string) bool {
	t.Helper()
	q, ok := list[name]
	if !ok {
		return false
	}
	wantQ := resource.MustParse(want)
	return q.Cmp(wantQ) == 0
}

// TestResourcesToPodResources exercises every branch of the mapping:
// nil input, whole-vs-fractional CPU rendering, memory-only, and the
// full four-field case where DiskMB lands on limits.ephemeral-storage
// only and PIDs is ignored without panicking.
func TestResourcesToPodResources(t *testing.T) {
	cases := []struct {
		name       string
		limits     *types.ResourceLimits
		wantReqCPU string
		wantLimCPU string
		wantReqMem string
		wantLimMem string
		wantLimEph string
		wantNoReq  bool
		wantNoLim  bool
	}{
		{
			name:      "nil yields empty requirements",
			limits:    nil,
			wantNoReq: true,
			wantNoLim: true,
		},
		{
			name:       "whole CPU renders as integer",
			limits:     &types.ResourceLimits{CPUs: 2},
			wantReqCPU: "2",
			wantLimCPU: "2",
		},
		{
			name:       "fractional CPU renders as milli",
			limits:     &types.ResourceLimits{CPUs: 0.5},
			wantReqCPU: "500m",
			wantLimCPU: "500m",
		},
		{
			name:       "memory only",
			limits:     &types.ResourceLimits{MemoryMB: 512},
			wantReqMem: "512Mi",
			wantLimMem: "512Mi",
		},
		{
			name: "all four: disk is limit-only, pids ignored",
			limits: &types.ResourceLimits{
				CPUs:     1.5,
				MemoryMB: 1024,
				DiskMB:   2048,
				PIDs:     256,
			},
			wantReqCPU: "1500m",
			wantLimCPU: "1500m",
			wantReqMem: "1024Mi",
			wantLimMem: "1024Mi",
			wantLimEph: "2048Mi",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resourcesToPodResources(tc.limits)

			if tc.wantNoReq && got.Requests != nil {
				t.Errorf("expected nil Requests, got %v", got.Requests)
			}
			if tc.wantNoLim && got.Limits != nil {
				t.Errorf("expected nil Limits, got %v", got.Limits)
			}

			if tc.wantReqCPU != "" && !quantityEquals(t, got.Requests, corev1.ResourceCPU, tc.wantReqCPU) {
				t.Errorf("requests.cpu = %v, want %q", got.Requests[corev1.ResourceCPU], tc.wantReqCPU)
			}
			if tc.wantLimCPU != "" && !quantityEquals(t, got.Limits, corev1.ResourceCPU, tc.wantLimCPU) {
				t.Errorf("limits.cpu = %v, want %q", got.Limits[corev1.ResourceCPU], tc.wantLimCPU)
			}
			if tc.wantReqMem != "" && !quantityEquals(t, got.Requests, corev1.ResourceMemory, tc.wantReqMem) {
				t.Errorf("requests.memory = %v, want %q", got.Requests[corev1.ResourceMemory], tc.wantReqMem)
			}
			if tc.wantLimMem != "" && !quantityEquals(t, got.Limits, corev1.ResourceMemory, tc.wantLimMem) {
				t.Errorf("limits.memory = %v, want %q", got.Limits[corev1.ResourceMemory], tc.wantLimMem)
			}

			// DiskMB must land on limits.ephemeral-storage ONLY — never on
			// requests, where it would force the scheduler to find a node
			// with that much free scratch space.
			if tc.wantLimEph != "" {
				if !quantityEquals(t, got.Limits, corev1.ResourceEphemeralStorage, tc.wantLimEph) {
					t.Errorf("limits.ephemeral-storage = %v, want %q", got.Limits[corev1.ResourceEphemeralStorage], tc.wantLimEph)
				}
				if _, ok := got.Requests[corev1.ResourceEphemeralStorage]; ok {
					t.Error("ephemeral-storage must not appear in requests")
				}
			}

			// PIDs is never enforced as a container resource, regardless of
			// the input value.
			if _, ok := got.Limits["pids"]; ok {
				t.Error("pids must not appear in limits")
			}
		})
	}
}

// TestCPUQuantity pins the readable-rendering contract independently of
// the surrounding ResourceRequirements assembly.
func TestCPUQuantity(t *testing.T) {
	cases := map[float64]string{
		1:    "1",
		2:    "2",
		0.5:  "500m",
		0.25: "250m",
		1.5:  "1500m",
		// A positive sub-millicore share rounds to zero millis; the 1m
		// floor turns what would be a MustParse("0m") panic into the
		// smallest representable reservation.
		0.001:  "1m",
		0.0004: "1m",
		0.0001: "1m",
	}
	for in, want := range cases {
		got := cpuQuantity(in)
		if got.String() != want {
			t.Errorf("cpuQuantity(%v) = %q, want %q", in, got.String(), want)
		}
		// Round-trips through resource.Quantity so a malformed render
		// (which MustParse would have panicked on) is caught here too.
		if _, err := resource.ParseQuantity(got.String()); err != nil {
			t.Errorf("cpuQuantity(%v) produced unparseable quantity %q: %v", in, got.String(), err)
		}
	}
}

// TestResourcesToPodResources_SubMillicoreCPU is the regression guard for
// the panic path: a positive CPU value that rounds below 1m must not
// panic inside resource.MustParse and must surface as a 1m floor on both
// requests and limits.
func TestResourcesToPodResources_SubMillicoreCPU(t *testing.T) {
	got := resourcesToPodResources(&types.ResourceLimits{CPUs: 0.0004})
	if !quantityEquals(t, got.Requests, corev1.ResourceCPU, "1m") {
		t.Errorf("requests.cpu = %v, want 1m", got.Requests[corev1.ResourceCPU])
	}
	if !quantityEquals(t, got.Limits, corev1.ResourceCPU, "1m") {
		t.Errorf("limits.cpu = %v, want 1m", got.Limits[corev1.ResourceCPU])
	}
}
