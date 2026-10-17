package cmd

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

const cliGuidanceDigest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

func TestApplyOverrides_RepositoryGuidanceDefaultLeavesItOff(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}
	if cfg.RepositoryGuidance != nil {
		t.Errorf("RepositoryGuidance must stay nil without the flags, got %+v", cfg.RepositoryGuidance)
	}
}

func TestApplyOverrides_RepositoryGuidanceFlags(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	for _, kv := range [][2]string{
		{"repository-guidance-file", "AGENTS.md"},
		{"repository-guidance-file", "CLAUDE.md"},
		{"repository-guidance-max-bytes", "4096"},
		{"repository-guidance-pin", "AGENTS.md=" + cliGuidanceDigest},
	} {
		if err := cmd.Flags().Set(kv[0], kv[1]); err != nil {
			t.Fatalf("set %s: %v", kv[0], err)
		}
	}
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}
	want := &types.RepositoryGuidanceConfig{
		Files:        []string{"AGENTS.md", "CLAUDE.md"},
		MaxBytes:     4096,
		PinnedSHA256: map[string]string{"AGENTS.md": cliGuidanceDigest},
	}
	if !reflect.DeepEqual(cfg.RepositoryGuidance, want) {
		t.Errorf("RepositoryGuidance = %+v, want %+v", cfg.RepositoryGuidance, want)
	}
}

func TestApplyOverrides_RepositoryGuidanceFileReplacesAndPinMerges(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.RepositoryGuidance = &types.RepositoryGuidanceConfig{
		Files:        []string{"CLAUDE.md"},
		MaxBytes:     1000,
		PinnedSHA256: map[string]string{"CLAUDE.md": strings.Repeat("a", 64)},
	}
	if err := cmd.Flags().Set("repository-guidance-file", "AGENTS.md"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("repository-guidance-pin", "AGENTS.md="+cliGuidanceDigest); err != nil {
		t.Fatal(err)
	}
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}
	rg := cfg.RepositoryGuidance
	if !reflect.DeepEqual(rg.Files, []string{"AGENTS.md"}) {
		t.Errorf("Files = %v, want the flag list to replace the file list", rg.Files)
	}
	if rg.MaxBytes != 1000 {
		t.Errorf("MaxBytes = %d, want the file value to survive", rg.MaxBytes)
	}
	if len(rg.PinnedSHA256) != 2 || rg.PinnedSHA256["AGENTS.md"] != cliGuidanceDigest {
		t.Errorf("PinnedSHA256 = %v, want the flag pin merged over the file pins", rg.PinnedSHA256)
	}
}

func TestApplyOverrides_RepositoryGuidancePinRejectsMalformedEntry(t *testing.T) {
	cmd := newTestHarnessCommand()
	if err := cmd.Flags().Set("repository-guidance-pin", "AGENTS.md"); err != nil {
		t.Fatal(err)
	}
	err := applyOverrides(cmd, baseFileConfig(), nil)
	if err == nil || !strings.Contains(err.Error(), "--repository-guidance-pin") {
		t.Fatalf("applyOverrides error = %v, want a --repository-guidance-pin parse error", err)
	}
}

func TestBuildRunConfig_RepositoryGuidanceFlagsValidate(t *testing.T) {
	cmd := newTestHarnessCommand()
	for _, kv := range [][2]string{
		{"prompt", "do the thing"},
		{"repository-guidance-file", "AGENTS.md"},
		{"repository-guidance-pin", "CLAUDE.md=" + cliGuidanceDigest},
	} {
		if err := cmd.Flags().Set(kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	_, err := BuildRunConfig(RunConfigSources{Cmd: cmd, Resolve: ResolveAll})
	if err == nil || !strings.Contains(err.Error(), "must also appear in repositoryGuidance.files") {
		t.Fatalf("BuildRunConfig error = %v, want the pin-not-listed validation error", err)
	}

	cmd = newTestHarnessCommand()
	for _, kv := range [][2]string{
		{"prompt", "do the thing"},
		{"repository-guidance-file", "AGENTS.md"},
	} {
		if err := cmd.Flags().Set(kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := BuildRunConfig(RunConfigSources{Cmd: cmd, Resolve: ResolveAll})
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	if cfg.RepositoryGuidance == nil || !reflect.DeepEqual(cfg.RepositoryGuidance.Files, []string{"AGENTS.md"}) {
		t.Errorf("RepositoryGuidance = %+v, want files [AGENTS.md]", cfg.RepositoryGuidance)
	}
}
