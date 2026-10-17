package cmd

import (
	"reflect"
	"strings"
	"testing"
)

func TestApplyOverrides_ProtectedWriteTargetDefaultLeavesItEmpty(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}
	if cfg.Tools.ProtectedWriteTargets != nil {
		t.Errorf("ProtectedWriteTargets must stay nil without the flag, got %v", cfg.Tools.ProtectedWriteTargets)
	}
}

func TestApplyOverrides_ProtectedWriteTargetReplacesFileList(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	cfg.Tools.ProtectedWriteTargets = []string{"CLAUDE.md"}
	for _, v := range []string{"AGENTS.md", "config/policies/"} {
		if err := cmd.Flags().Set("protected-write-target", v); err != nil {
			t.Fatal(err)
		}
	}
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}
	want := []string{"AGENTS.md", "config/policies/"}
	if !reflect.DeepEqual(cfg.Tools.ProtectedWriteTargets, want) {
		t.Errorf("ProtectedWriteTargets = %v, want %v", cfg.Tools.ProtectedWriteTargets, want)
	}
}

func TestBuildRunConfig_ProtectedWriteTargetValidates(t *testing.T) {
	cmd := newTestHarnessCommand()
	for _, kv := range [][2]string{
		{"prompt", "do the thing"},
		{"protected-write-target", "../outside.md"},
	} {
		if err := cmd.Flags().Set(kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	_, err := BuildRunConfig(RunConfigSources{Cmd: cmd, Resolve: ResolveAll})
	if err == nil || !strings.Contains(err.Error(), "tools.protectedWriteTargets[0]") {
		t.Fatalf("BuildRunConfig error = %v, want a tools.protectedWriteTargets validation error", err)
	}

	cmd = newTestHarnessCommand()
	for _, kv := range [][2]string{
		{"prompt", "do the thing"},
		{"protected-write-target", "AGENTS.md"},
	} {
		if err := cmd.Flags().Set(kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := BuildRunConfig(RunConfigSources{Cmd: cmd, Resolve: ResolveAll})
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	if !reflect.DeepEqual(cfg.Tools.ProtectedWriteTargets, []string{"AGENTS.md"}) {
		t.Errorf("ProtectedWriteTargets = %v, want [AGENTS.md]", cfg.Tools.ProtectedWriteTargets)
	}
}
