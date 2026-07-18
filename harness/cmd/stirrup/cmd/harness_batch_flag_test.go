package cmd

import (
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// TestBuildHarnessRunConfig_BatchFlagEnabledOnly verifies that --batch
// alone produces a BatchProviderConfig with Enabled=true and every
// other field at its zero value; operators reach for --config to set
// the rest.
func TestBuildHarnessRunConfig_BatchFlagEnabledOnly(t *testing.T) {
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
		RunID:         "test-run",
		Mode:          "research",
		Prompt:        "test",
		ProviderType:  "anthropic",
		APIKeyRef:     "secret://ANTHROPIC_API_KEY",
		Model:         "claude-sonnet-4-6",
		MaxTurns:      5,
		Timeout:       600,
		TransportType: "grpc",
		TransportAddr: "control-plane:443",
		LogLevel:      "info",
		Batch:         true,
	})
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}
	if cfg.Provider.Batch == nil {
		t.Fatal("expected non-nil Provider.Batch when --batch is set")
	}
	if !cfg.Provider.Batch.Enabled {
		t.Errorf("Provider.Batch.Enabled = false, want true")
	}
	if cfg.Provider.Batch.MaxWaitSeconds != nil {
		t.Errorf("Provider.Batch.MaxWaitSeconds = %v, want nil (defaulted by ValidateRunConfig)", *cfg.Provider.Batch.MaxWaitSeconds)
	}
	if cfg.Provider.Batch.HarnessSidePolling {
		t.Errorf("Provider.Batch.HarnessSidePolling = true, want false (operators set this via --config)")
	}
	if cfg.Provider.Batch.FallbackOnTimeout {
		t.Errorf("Provider.Batch.FallbackOnTimeout = true, want false")
	}
	if cfg.Provider.Batch.CancelBundleOnRunCancel {
		t.Errorf("Provider.Batch.CancelBundleOnRunCancel = true, want false")
	}
	if cfg.Provider.Batch.AllowInteractiveModes {
		t.Errorf("Provider.Batch.AllowInteractiveModes = true, want false")
	}
}

// TestBuildHarnessRunConfig_BatchFlagUnsetLeavesNil verifies the
// default-off posture: a build without --batch leaves Provider.Batch
// nil so the validator's MaxWaitSeconds default does not fire and the
// factory routes through the streaming adapter.
func TestBuildHarnessRunConfig_BatchFlagUnsetLeavesNil(t *testing.T) {
	cfg, err := buildHarnessRunConfig(harnessCLIOptions{
		RunID:         "test-run",
		Mode:          "research",
		Prompt:        "test",
		ProviderType:  "anthropic",
		APIKeyRef:     "secret://ANTHROPIC_API_KEY",
		Model:         "claude-sonnet-4-6",
		MaxTurns:      20,
		Timeout:       600,
		TransportType: "stdio",
		LogLevel:      "info",
	})
	if err != nil {
		t.Fatalf("buildHarnessRunConfig: %v", err)
	}
	if cfg.Provider.Batch != nil {
		t.Errorf("expected Provider.Batch to be nil without --batch, got %+v", cfg.Provider.Batch)
	}
}

// TestApplyOverrides_BatchFlagMergesWithFile pins the merge semantics
// against a --config file that already carries a partial Batch block:
// setting --batch on top must flip Enabled to true without disturbing
// the file's other fields.
func TestApplyOverrides_BatchFlagMergesWithFile(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider.Batch = &types.BatchProviderConfig{
		Enabled:            false,
		HarnessSidePolling: true,
	}

	if err := cmd.Flags().Set("batch", "true"); err != nil {
		t.Fatalf("set --batch: %v", err)
	}
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.Provider.Batch == nil {
		t.Fatal("expected Provider.Batch to remain non-nil after override")
	}
	if !cfg.Provider.Batch.Enabled {
		t.Errorf("Provider.Batch.Enabled: --batch override failed, got false")
	}
	if !cfg.Provider.Batch.HarnessSidePolling {
		t.Errorf("Provider.Batch.HarnessSidePolling: file value should survive, got false")
	}
}

// TestApplyOverrides_BatchFlagAllocatesWhenFileOmits covers the path
// where the --config file omits the Batch block entirely. The flag
// alone must allocate a fresh BatchProviderConfig with Enabled=true.
func TestApplyOverrides_BatchFlagAllocatesWhenFileOmits(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	if cfg.Provider.Batch != nil {
		t.Fatalf("baseFileConfig should not set Batch; got %+v", cfg.Provider.Batch)
	}

	if err := cmd.Flags().Set("batch", "true"); err != nil {
		t.Fatalf("set --batch: %v", err)
	}
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.Provider.Batch == nil {
		t.Fatal("expected Provider.Batch to be allocated when --batch flips on an empty slot")
	}
	if !cfg.Provider.Batch.Enabled {
		t.Errorf("Provider.Batch.Enabled = false, want true")
	}
}

// TestApplyOverrides_BatchFlagUnsetPreservesFile pins the precedence
// rule: an unset --batch flag must not clobber a Batch block the file
// supplied.
func TestApplyOverrides_BatchFlagUnsetPreservesFile(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider.Batch = &types.BatchProviderConfig{
		Enabled:            true,
		HarnessSidePolling: true,
	}

	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.Provider.Batch == nil {
		t.Fatal("expected Provider.Batch to survive when --batch is unset")
	}
	if !cfg.Provider.Batch.Enabled {
		t.Errorf("Provider.Batch.Enabled: file value should survive, got false")
	}
	if !cfg.Provider.Batch.HarnessSidePolling {
		t.Errorf("Provider.Batch.HarnessSidePolling: file value should survive, got false")
	}
}

// TestHarnessCmd_BatchFlagHelpText pins the operator-facing flag
// description so a future cosmetic edit does not silently drop the
// cost/latency tradeoff statement or the doc link that operators rely
// on to find the cross-field invariants.
func TestHarnessCmd_BatchFlagHelpText(t *testing.T) {
	flag := harnessCmd.Flags().Lookup("batch")
	if flag == nil {
		t.Fatal("expected --batch flag to be registered on harnessCmd")
	}
	usage := flag.Usage
	wantSubstrings := []string{
		"async batch submission",
		"50% cost reduction",
		"24h latency",
		"transport=grpc",
		"harnessSidePolling=true",
		"docs/batch.md",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(usage, want) {
			t.Errorf("--batch usage text missing %q; got: %s", want, usage)
		}
	}
	if flag.DefValue != "false" {
		t.Errorf("--batch default = %q, want \"false\"", flag.DefValue)
	}
}

// TestApplyOverrides_BatchFlagExplicitFalseClears pins that
// --batch=false explicitly passed on top of a file with
// Batch.Enabled=true flips Enabled to false while the rest of the
// struct (e.g. HarnessSidePolling) survives.
func TestApplyOverrides_BatchFlagExplicitFalseClears(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Provider.Batch = &types.BatchProviderConfig{
		Enabled:            true,
		HarnessSidePolling: true,
	}

	if err := cmd.Flags().Set("batch", "false"); err != nil {
		t.Fatalf("set --batch=false: %v", err)
	}
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}

	if cfg.Provider.Batch == nil {
		t.Fatal("expected Provider.Batch to remain non-nil after --batch=false")
	}
	if cfg.Provider.Batch.Enabled {
		t.Errorf("Provider.Batch.Enabled = true, want false after --batch=false")
	}
	if !cfg.Provider.Batch.HarnessSidePolling {
		t.Errorf("Provider.Batch.HarnessSidePolling: file value should survive --batch=false, got false")
	}
}
