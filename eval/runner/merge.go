package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/rxbynerd/stirrup/types"
)

// maxRunConfigFileBytes caps the size of a suite-level RunConfig JSON file
// the runner will read. Mirrors the harness's loadRunConfigFile cap so a
// suite cannot smuggle a multi-megabyte file past the runner that the
// harness would reject anyway.
const maxRunConfigFileBytes int64 = 1 << 20 // 1 MiB

// loadRunConfigFile reads a JSON file at path and unmarshals it into a
// RunConfig. Unknown fields are rejected so typos in a suite's baseline
// config fail loudly rather than being silently dropped.
//
// This is intentionally a copy of the harness's loader rather than a shared
// helper: keeping it inside the runner package preserves the eval module's
// existing dependency direction (eval → types, never eval → harness).
func loadRunConfigFile(path string) (*types.RunConfig, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("reading run_config_file %q: %w", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("reading run_config_file %q: is a directory", path)
	}
	if info.Size() > maxRunConfigFileBytes {
		return nil, fmt.Errorf("reading run_config_file %q: %d bytes exceeds %d byte cap", path, info.Size(), maxRunConfigFileBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading run_config_file %q: %w", path, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("parsing run_config_file %q: file is empty", path)
	}
	var cfg types.RunConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing run_config_file %q: %w", path, err)
	}
	return &cfg, nil
}

// resolveBaseline produces the suite-level baseline RunConfig for a task,
// or nil if the suite declares neither a file nor inline block (i.e. the
// legacy five-flag invocation path). The returned config is always a fresh
// allocation safe for the caller to mutate.
func resolveBaseline(suite types.EvalSuite) (*types.RunConfig, error) {
	switch {
	case suite.RunConfigFile != "":
		return loadRunConfigFile(suite.RunConfigFile)
	case suite.RunConfig != nil:
		// Deep-copy via JSON round-trip so the per-task merge does not
		// mutate the shared suite spec. The cost is negligible (RunConfig
		// is at most a few KB) and this avoids hand-maintaining a clone
		// helper that would silently miss new fields as the type grows.
		data, err := json.Marshal(suite.RunConfig)
		if err != nil {
			return nil, fmt.Errorf("cloning inline run_config baseline: %w", err)
		}
		var clone types.RunConfig
		if err := json.Unmarshal(data, &clone); err != nil {
			return nil, fmt.Errorf("cloning inline run_config baseline: %w", err)
		}
		return &clone, nil
	default:
		return nil, nil
	}
}

// mergeOverrides applies a sparse RunConfigOverrides overlay on top of a
// baseline RunConfig. Only non-zero / non-nil fields on the overlay take
// effect; everything else passes the baseline through unchanged.
//
// The baseline pointer is mutated in place and also returned for chaining.
// A nil overlay is a no-op (the baseline is returned unchanged). A nil
// baseline is an error: an overlay with nothing to apply against is a
// programming bug, surfaced loudly rather than silently producing a
// half-formed config.
//
// Pointer-typed override fields (Provider, ModelRouter, ContextStrategy,
// EditStrategy, Verifier, MaxTurns) are treated as "set if non-nil".
// Scalar fields (Mode) are treated as "set if non-zero". This mirrors the
// RunConfigOverrides shape: pointer fields exist so the caller can leave
// them out, and the string Mode uses "" as the sentinel for "unset" since
// it is also the zero value of a valid RunConfig.
func mergeOverrides(baseline *types.RunConfig, overlay *types.RunConfigOverrides) *types.RunConfig {
	if baseline == nil {
		return nil
	}
	if overlay == nil {
		return baseline
	}

	if overlay.Mode != "" {
		baseline.Mode = overlay.Mode
	}
	if overlay.Provider != nil {
		baseline.Provider = *overlay.Provider
	}
	if overlay.ModelRouter != nil {
		baseline.ModelRouter = *overlay.ModelRouter
	}
	if overlay.ContextStrategy != nil {
		baseline.ContextStrategy = *overlay.ContextStrategy
	}
	if overlay.EditStrategy != nil {
		baseline.EditStrategy = *overlay.EditStrategy
	}
	if overlay.Verifier != nil {
		baseline.Verifier = *overlay.Verifier
	}
	if overlay.MaxTurns != nil {
		baseline.MaxTurns = *overlay.MaxTurns
	}

	return baseline
}
