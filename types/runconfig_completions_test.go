package types

import (
	"sort"
	"testing"
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
		{"logs export types", ValidLogsExportTypeValues()},
		{"code scanners", ValidCodeScannerTypeValues()},
		{"guard rails", ValidGuardRailTypeValues()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.got) == 0 {
				t.Fatalf("%s: empty slice", tc.name)
			}
			if !sort.StringsAreSorted(tc.got) {
				t.Errorf("%s: not sorted: %v", tc.name, tc.got)
			}
			for _, v := range tc.got {
				if v == "" {
					t.Errorf("%s: contains empty string", tc.name)
				}
			}
		})
	}
}

// TestCompletionValues_TrackValidatorMaps pins that every key in the
// validator's closed-set map (minus the filtered empty string) appears
// in the matching completion helper output.
func TestCompletionValues_TrackValidatorMaps(t *testing.T) {
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
		{name: "logs export types", backing: validLogsExportTypes, got: ValidLogsExportTypeValues(), filterEmpty: true},
		{name: "code scanners", backing: validCodeScannerTypes, got: ValidCodeScannerTypeValues()},
		{name: "guard rails", backing: validGuardRailTypes, got: ValidGuardRailTypeValues()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expected := make(map[string]struct{}, len(tc.backing))
			for k := range tc.backing {
				if tc.filterEmpty && k == "" {
					continue
				}
				expected[k] = struct{}{}
			}
			if len(tc.got) != len(expected) {
				t.Fatalf("%s: got %d values, want %d (backing map %v)",
					tc.name, len(tc.got), len(expected), tc.backing)
			}
			for _, v := range tc.got {
				if _, ok := expected[v]; !ok {
					t.Errorf("%s: helper returned %q not present in backing map", tc.name, v)
				}
			}
		})
	}
}
