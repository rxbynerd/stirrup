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
// the runner will read, mirroring the harness's loadRunConfigFile cap.
const maxRunConfigFileBytes int64 = 1 << 20 // 1 MiB

// loadRunConfigFile reads a JSON file at path and unmarshals it into a
// RunConfig, rejecting unknown fields so authoring typos fail loudly.
//
// The file must be a regular file: FIFOs, sockets, and device files are
// rejected before the read, since opening one without O_NONBLOCK would
// block os.ReadFile indefinitely and deadlock the worker pool. The size
// cap and io.LimitReader are a second layer of defence against a
// regular file that grows between the fstat and the read.
//
// This is intentionally a copy of the harness's loader rather than a
// shared helper: it preserves the eval module's dependency direction
// (eval → types, never eval → harness).
func loadRunConfigFile(path string) (*types.RunConfig, error) {
	// O_NONBLOCK makes the open return immediately for a FIFO instead
	// of blocking for a connected writer; regular files ignore it.
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

	// Read one byte past the cap so "exactly cap bytes" is distinguishable
	// from "exceeded cap" regardless of what fstat reported.
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
// or nil if the suite declares neither a file nor inline block (the
// legacy five-flag invocation path). The returned config is always a
// fresh allocation safe for the caller to mutate.
//
// Mutual exclusion is enforced at HCL parse time, but a Go caller
// constructing EvalSuite directly can still set both fields; this
// surfaces that as an error rather than silently discarding one.
func resolveBaseline(suite types.EvalSuite) (*types.RunConfig, error) {
	if suite.RunConfigFile != "" && suite.RunConfig != nil {
		return nil, fmt.Errorf("suite %q: run_config_file and run_config are mutually exclusive", suite.ID)
	}
	switch {
	case suite.RunConfigFile != "":
		return loadRunConfigFile(suite.RunConfigFile)
	case suite.RunConfig != nil:
		// Deep-copy via JSON round-trip so the per-task merge does not
		// mutate the shared suite spec.
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
// effect; everything else passes the baseline through unchanged. The
// baseline pointer is mutated in place and also returned for chaining. A
// nil overlay is a no-op; a nil baseline returns nil (buildMergedConfig
// treats that as "fall back to the legacy harness invocation").
//
// Pointer-typed override fields are "set if non-nil"; the scalar Mode
// field is "set if non-zero", using "" as its unset sentinel.
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
