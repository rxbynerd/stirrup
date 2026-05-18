package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"syscall"

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
// The file must be a regular file: FIFOs, sockets, and device files are
// rejected before the read. Without that guard a worker pool entering
// loadRunConfigFile on a path like `/tmp/evil-fifo` would block
// indefinitely on os.ReadFile, deadlocking RunSuite for the duration
// of the worker pool's lifetime. The size cap and io.LimitReader give a
// second layer of defence in case a regular file's reported size is
// raced after the fstat (small TOCTOU window — the loader is still
// authoritative on the bytes it actually consumed).
//
// This is intentionally a copy of the harness's loader rather than a shared
// helper: keeping it inside the runner package preserves the eval module's
// existing dependency direction (eval → types, never eval → harness). The
// harness copy still uses the two-syscall Stat+ReadFile shape; tracked as
// a separate hardening pass under issue #177 follow-ups.
func loadRunConfigFile(path string) (*types.RunConfig, error) {
	// O_NONBLOCK is the load-bearing flag: on POSIX, opening a FIFO
	// for read without a connected writer blocks indefinitely. With
	// O_NONBLOCK the open returns immediately for any path type, so
	// the worker pool cannot be parked by a hostile or accidental
	// FIFO at the configured run_config_file path. Regular files
	// ignore O_NONBLOCK.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("reading run_config_file %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("reading run_config_file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("reading run_config_file %q: not a regular file (mode %s)", path, info.Mode())
	}
	if info.Size() > maxRunConfigFileBytes {
		return nil, fmt.Errorf("reading run_config_file %q: %d bytes exceeds %d byte cap", path, info.Size(), maxRunConfigFileBytes)
	}

	// io.LimitReader bounds the read regardless of what fstat reported —
	// a file that grew between stat and read still cannot smuggle a
	// multi-MiB blob through this loader. Read one byte past the cap so
	// we can distinguish "exactly cap bytes" from "exceeded cap".
	data, err := io.ReadAll(io.LimitReader(f, maxRunConfigFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading run_config_file %q: %w", path, err)
	}
	if int64(len(data)) > maxRunConfigFileBytes {
		return nil, fmt.Errorf("reading run_config_file %q: exceeds %d byte cap", path, maxRunConfigFileBytes)
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
//
// Mutual exclusion is enforced at HCL parse time, but a Go caller
// constructing EvalSuite directly (integration tests, the experiment
// runner, future callers) can still set both fields. resolveBaseline
// surfaces that as an error so the inline block is never silently
// discarded in favour of the file.
func resolveBaseline(suite types.EvalSuite) (*types.RunConfig, error) {
	if suite.RunConfigFile != "" && suite.RunConfig != nil {
		return nil, fmt.Errorf("suite %q: run_config_file and run_config are mutually exclusive", suite.ID)
	}
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
// baseline returns nil: the legacy invocation path (no suite-level
// baseline) has nothing for the overlay to land on, and the caller is
// expected to treat (nil, _) as "no merged config — fall back to the
// five-flag harness invocation". The runner already enforces that
// contract in buildMergedConfig.
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
