package types

import "sort"

// Shell-completion helpers.
//
// These expose the closed-set Type enumerations as sorted string
// slices for cobra dynamic-flag completions, sourced from the same
// maps the validator consults. Output is sorted because map iteration
// is non-deterministic and completions must be stable. Maps that carry
// an empty-string "default" entry are filtered to non-empty keys since
// "" offers nothing to tab-complete.

// ValidRunModeValues returns the run modes accepted on RunConfig.Mode.
func ValidRunModeValues() []string { return sortedKeys(validRunModes) }

// ValidProviderTypeValues returns the provider types accepted on
// RunConfig.Provider.Type.
func ValidProviderTypeValues() []string { return sortedKeys(validProviderTypes) }

// ValidExecutorTypeValues returns the executor types accepted on
// RunConfig.Executor.Type.
func ValidExecutorTypeValues() []string { return sortedKeys(validExecutorTypes) }

// ValidExecutorRuntimeValues returns the OCI runtimes accepted on
// RunConfig.Executor.Runtime for the container executor. The k8s
// executor's RuntimeClass names are a separate set (validK8sRuntimes).
func ValidExecutorRuntimeValues() []string { return sortedNonEmptyKeys(validContainerRuntimes) }

// ValidEditStrategyTypeValues returns the edit-strategy types accepted
// on RunConfig.EditStrategy.Type.
func ValidEditStrategyTypeValues() []string { return sortedKeys(validEditStrategyTypes) }

// ValidVerifierTypeValues returns the verifier types accepted on
// RunConfig.Verifier.Type.
func ValidVerifierTypeValues() []string { return sortedKeys(validVerifierTypes) }

// ValidGitStrategyTypeValues returns the git-strategy types accepted
// on RunConfig.GitStrategy.Type.
func ValidGitStrategyTypeValues() []string { return sortedKeys(validGitStrategyTypes) }

// ValidTransportTypeValues returns the transport types accepted on
// RunConfig.Transport.Type.
func ValidTransportTypeValues() []string { return sortedKeys(validTransportTypes) }

// ValidTraceEmitterTypeValues returns the trace-emitter types accepted
// on RunConfig.TraceEmitter.Type.
func ValidTraceEmitterTypeValues() []string { return sortedKeys(validTraceEmitterTypes) }

// ValidTraceEmitterProtocolValues returns the OTLP wire protocols
// accepted on RunConfig.TraceEmitter.Protocol.
func ValidTraceEmitterProtocolValues() []string {
	return sortedNonEmptyKeys(validTraceEmitterProtocols)
}

// ValidLogsExportTypeValues returns the log-export types accepted on
// RunConfig.Observability.LogsExport.Type.
func ValidLogsExportTypeValues() []string {
	return sortedNonEmptyKeys(validLogsExportTypes)
}

// ValidCodeScannerTypeValues returns the code-scanner types accepted
// on RunConfig.CodeScanner.Type.
func ValidCodeScannerTypeValues() []string { return sortedKeys(validCodeScannerTypes) }

// ValidGuardRailTypeValues returns the guard-rail types accepted on
// RunConfig.GuardRail.Type.
func ValidGuardRailTypeValues() []string { return sortedKeys(validGuardRailTypes) }

// ValidCompatProfileValues returns the provider compat profiles
// accepted on RunConfig.Provider.CompatProfile.
func ValidCompatProfileValues() []string { return sortedNonEmptyKeys(validCompatProfiles) }

// ValidToolsProfileValues returns the toolset profiles accepted on
// RunConfig.Tools.Profile. "default" is retained as a typeable synonym
// for the empty value.
func ValidToolsProfileValues() []string { return sortedNonEmptyKeys(validToolsProfiles) }

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedNonEmptyKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if k == "" {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
