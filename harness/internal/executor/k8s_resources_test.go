package executor

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/rxbynerd/stirrup/types"
)

// requireQuantity asserts that the ResourceList entry at name equals the
// quantity parsed from want. Comparison is by numeric value (Quantity.Cmp)
// rather than string, because resource.Quantity canonicalises its suffix
// on String(): "1024Mi" round-trips as "1Gi", which is the same value.
func requireQuantity(t *testing.T, list corev1.ResourceList, name corev1.ResourceName, want string) {
	t.Helper()
	must := require.New(t)
	got, ok := list[name]
	must.True(ok, "%s is not set, want %s", name, want)
	wantQ := resource.MustParse(want)
	must.Equal(0, got.Cmp(wantQ), "%s = %s, want %s", name, got.String(), want)
}

// TestResourcesToPodResources_RendersEachLimit covers nil input, whole and
// fractional CPU, memory-only, and the full four-field case where DiskMB
// lands on limits.ephemeral-storage only and PIDs is ignored.
func TestResourcesToPodResources_RendersEachLimit(t *testing.T) {
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
			name:       "whole cpu renders as integer",
			limits:     &types.ResourceLimits{CPUs: 2},
			wantReqCPU: "2",
			wantLimCPU: "2",
		},
		{
			name:       "fractional cpu renders as milli",
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
			name: "all four fields: disk is limit-only, pids ignored",
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
			must := require.New(t)
			got := resourcesToPodResources(tc.limits)

			if tc.wantNoReq {
				must.Nil(got.Requests)
			}
			if tc.wantNoLim {
				must.Nil(got.Limits)
			}

			if tc.wantReqCPU != "" {
				requireQuantity(t, got.Requests, corev1.ResourceCPU, tc.wantReqCPU)
			}
			if tc.wantLimCPU != "" {
				requireQuantity(t, got.Limits, corev1.ResourceCPU, tc.wantLimCPU)
			}
			if tc.wantReqMem != "" {
				requireQuantity(t, got.Requests, corev1.ResourceMemory, tc.wantReqMem)
			}
			if tc.wantLimMem != "" {
				requireQuantity(t, got.Limits, corev1.ResourceMemory, tc.wantLimMem)
			}

			// DiskMB must land on limits.ephemeral-storage ONLY — never on
			// requests, where it would force the scheduler to find a node
			// with that much free scratch space.
			if tc.wantLimEph != "" {
				requireQuantity(t, got.Limits, corev1.ResourceEphemeralStorage, tc.wantLimEph)
				must.NotContains(got.Requests, corev1.ResourceEphemeralStorage)
			}

			// PIDs is never enforced as a container resource, regardless of
			// the input value.
			must.NotContains(got.Limits, corev1.ResourceName("pids"))
		})
	}
}

// TestCPUQuantity_RendersReadableQuantities pins the readable-rendering
// contract independently of the surrounding ResourceRequirements assembly.
func TestCPUQuantity_RendersReadableQuantities(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want string
	}{
		{name: "one core", in: 1, want: "1"},
		{name: "two cores", in: 2, want: "2"},
		{name: "half a core", in: 0.5, want: "500m"},
		{name: "quarter of a core", in: 0.25, want: "250m"},
		{name: "one and a half cores", in: 1.5, want: "1500m"},
		{name: "one millicore", in: 0.001, want: "1m"},
		// A positive sub-millicore share rounds to zero millis; the 1m
		// floor keeps the render from being MustParse("0m"), which panics.
		{name: "0.0004 cores floors to 1m", in: 0.0004, want: "1m"},
		{name: "0.0001 cores floors to 1m", in: 0.0001, want: "1m"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got := cpuQuantity(tc.in)
			must.Equal(tc.want, got.String())

			// Round-trip through resource.Quantity so a malformed render is
			// caught here rather than panicking later in MustParse.
			_, err := resource.ParseQuantity(got.String())
			must.NoError(err)
		})
	}
}

// TestResourcesToPodResources_SubMillicoreCPUFloorsToOneMilli checks that a
// positive CPU value below 1m does not panic in resource.MustParse and
// floors to 1m on both requests and limits.
func TestResourcesToPodResources_SubMillicoreCPUFloorsToOneMilli(t *testing.T) {
	got := resourcesToPodResources(&types.ResourceLimits{CPUs: 0.0004})
	requireQuantity(t, got.Requests, corev1.ResourceCPU, "1m")
	requireQuantity(t, got.Limits, corev1.ResourceCPU, "1m")
}
