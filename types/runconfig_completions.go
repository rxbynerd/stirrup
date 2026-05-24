package types

import "sort"

// Shell-completion helpers.
//
// These functions expose the closed-set Type enumerations as sorted
// string slices so callers (today: harness/cmd/stirrup/cmd, used to
// register cobra dynamic-flag completions) can offer tab-completion
// without duplicating the value lists. Each helper is sourced from
// the same package-private map the validator consults, so adding a
// new valid value in one place automatically extends the completion
// surface.
//
// Sorted output is required so cobra emits a stable, deterministic
// completion order. Map iteration is non-deterministic; emitting
// completions in shuffled order would make shell snapshots and the
// "did the operator type the same prefix and get a different result"
// test loop flaky.
//
// The empty-string entry that some maps carry (e.g.
// validExecutorRuntimes, validTraceEmitterProtocols) is filtered out
// here because a completion value of "" is not useful to a tab-
// completing operator — it offers no character to type.

// ValidRunModeValues returns the run modes accepted on RunConfig.Mode.
func ValidRunModeValues() []string { return sortedKeys(validRunModes) }

// ValidProviderTypeValues returns the provider types accepted on
// RunConfig.Provider.Type.
func ValidProviderTypeValues() []string { return sortedKeys(validProviderTypes) }

// ValidExecutorTypeValues returns the executor types accepted on
// RunConfig.Executor.Type.
func ValidExecutorTypeValues() []string { return sortedKeys(validExecutorTypes) }

// ValidExecutorRuntimeValues returns the OCI runtimes accepted on
// RunConfig.Executor.Runtime. The "" (engine-default) entry is
// filtered out so the completion list contains only the typeable
// runtimes.
func ValidExecutorRuntimeValues() []string { return sortedNonEmptyKeys(validExecutorRuntimes) }

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
// accepted on RunConfig.TraceEmitter.Protocol. The "" (defaults-to-grpc)
// entry is filtered out so the completion list contains only the
// typeable protocols.
func ValidTraceEmitterProtocolValues() []string {
	return sortedNonEmptyKeys(validTraceEmitterProtocols)
}

// ValidCodeScannerTypeValues returns the code-scanner types accepted
// on RunConfig.CodeScanner.Type.
func ValidCodeScannerTypeValues() []string { return sortedKeys(validCodeScannerTypes) }

// ValidGuardRailTypeValues returns the guard-rail types accepted on
// RunConfig.GuardRail.Type.
func ValidGuardRailTypeValues() []string { return sortedKeys(validGuardRailTypes) }

// ValidCompatProfileValues returns the provider compat profiles
// accepted on RunConfig.Provider.CompatProfile. The "" (no profile)
// entry is filtered out so the completion list contains only the
// typeable values.
func ValidCompatProfileValues() []string { return sortedNonEmptyKeys(validCompatProfiles) }

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
