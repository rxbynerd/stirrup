package types

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCompletionValues_SortedAndNonEmpty pins that completion output is
// sorted lexicographically and contains no empty strings.
func TestCompletionValues_SortedAndNonEmpty(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  []string
	}{
		{"run modes", ValidRunModeValues()},
		{"provider types", ValidProviderTypeValues()},
		{"executor types", ValidExecutorTypeValues()},
		{"executor runtimes", ValidExecutorRuntimeValues()},
		{"edit strategies", ValidEditStrategyTypeValues()},
		{"verifiers", ValidVerifierTypeValues()},
		{"git strategies", ValidGitStrategyTypeValues()},
		{"transports", ValidTransportTypeValues()},
		{"trace emitters", ValidTraceEmitterTypeValues()},
		{"trace emitter protocols", ValidTraceEmitterProtocolValues()},
		{"trace emitter metrics temporalities", ValidTraceEmitterMetricsTemporalityValues()},
		{"trace emitter platforms", ValidTraceEmitterPlatformValues()},
		{"logs export types", ValidLogsExportTypeValues()},
		{"code scanners", ValidCodeScannerTypeValues()},
		{"guard rails", ValidGuardRailTypeValues()},
		{"endpoint profiles", ValidEndpointProfileValues()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.NotEmpty(tc.got)
			must.True(sort.StringsAreSorted(tc.got), "not sorted: %v", tc.got)
			must.NotContains(tc.got, "", "contains an empty string")
		})
	}
}

// TestCompletionValues_MatchValidatorMaps pins that every key in the
// validator's closed-set map (minus the filtered empty string) appears
// in the matching completion helper output.
func TestCompletionValues_MatchValidatorMaps(t *testing.T) {
	for _, tc := range []struct {
		name        string
		backing     map[string]bool
		got         []string
		filterEmpty bool
	}{
		{name: "run modes", backing: validRunModes, got: ValidRunModeValues()},
		{name: "provider types", backing: validProviderTypes, got: ValidProviderTypeValues()},
		{name: "executor types", backing: validExecutorTypes, got: ValidExecutorTypeValues()},
		{name: "executor runtimes", backing: validContainerRuntimes, got: ValidExecutorRuntimeValues(), filterEmpty: true},
		{name: "edit strategies", backing: validEditStrategyTypes, got: ValidEditStrategyTypeValues()},
		{name: "verifiers", backing: validVerifierTypes, got: ValidVerifierTypeValues()},
		{name: "git strategies", backing: validGitStrategyTypes, got: ValidGitStrategyTypeValues()},
		{name: "transports", backing: validTransportTypes, got: ValidTransportTypeValues()},
		{name: "trace emitters", backing: validTraceEmitterTypes, got: ValidTraceEmitterTypeValues()},
		{name: "trace emitter protocols", backing: validTraceEmitterProtocols, got: ValidTraceEmitterProtocolValues(), filterEmpty: true},
		{name: "trace emitter metrics temporalities", backing: validTraceEmitterMetricsTemporalities, got: ValidTraceEmitterMetricsTemporalityValues(), filterEmpty: true},
		{name: "trace emitter platforms", backing: validTraceEmitterPlatforms, got: ValidTraceEmitterPlatformValues(), filterEmpty: true},
		{name: "logs export types", backing: validLogsExportTypes, got: ValidLogsExportTypeValues(), filterEmpty: true},
		{name: "code scanners", backing: validCodeScannerTypes, got: ValidCodeScannerTypeValues()},
		{name: "guard rails", backing: validGuardRailTypes, got: ValidGuardRailTypeValues()},
		{name: "endpoint profiles", backing: validEndpointProfiles, got: ValidEndpointProfileValues(), filterEmpty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			expected := make(map[string]struct{}, len(tc.backing))
			for k := range tc.backing {
				if tc.filterEmpty && k == "" {
					continue
				}
				expected[k] = struct{}{}
			}
			must.Len(tc.got, len(expected), "backing map %v", tc.backing)
			for _, v := range tc.got {
				must.Contains(expected, v, "helper returned %q not present in backing map", v)
			}
		})
	}
}
