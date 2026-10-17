package cmd

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

func setFlags(t *testing.T, cmdFlags interface{ Set(string, string) error }, kvs [][2]string) {
	t.Helper()
	for _, kv := range kvs {
		if err := cmdFlags.Set(kv[0], kv[1]); err != nil {
			t.Fatalf("set --%s: %v", kv[0], err)
		}
	}
}

func TestApplyOverrides_CorrelationDefaultLeavesBlockNil(t *testing.T) {
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	if err := applyOverrides(cmd, cfg, nil); err != nil {
		t.Fatalf("applyOverrides: %v", err)
	}
	if cfg.Correlation != nil {
		t.Errorf("Correlation must stay nil without the flags, got %+v", cfg.Correlation)
	}
}

func TestApplyOverrides_CorrelationFlags(t *testing.T) {
	fileBlock := &types.CorrelationConfig{
		SessionID: "file-session",
		TaskID:    "file-task",
		UserID:    "file-user",
		Labels:    map[string]string{"agent_profile": "file", "team": "core"},
	}

	cases := []struct {
		name  string
		file  *types.CorrelationConfig
		flags [][2]string
		want  *types.CorrelationConfig
	}{
		{
			name: "flags only build the block",
			flags: [][2]string{
				{"correlation-session-id", "s1"},
				{"correlation-task-id", "t1"},
				{"correlation-attempt-id", "a1"},
				{"correlation-user-id", "u1"},
				{"correlation-url", "https://cp.example.com/runs/1"},
				{"correlation-label", "agent_profile=reviewer"},
				{"correlation-label", "env=prod"},
			},
			want: &types.CorrelationConfig{
				SessionID: "s1",
				TaskID:    "t1",
				AttemptID: "a1",
				UserID:    "u1",
				URL:       "https://cp.example.com/runs/1",
				Labels:    map[string]string{"agent_profile": "reviewer", "env": "prod"},
			},
		},
		{
			name:  "unset flags leave a file block untouched",
			file:  fileBlock,
			flags: [][2]string{{"correlation-attempt-id", "a2"}},
			want: &types.CorrelationConfig{
				SessionID: "file-session",
				TaskID:    "file-task",
				AttemptID: "a2",
				UserID:    "file-user",
				Labels:    map[string]string{"agent_profile": "file", "team": "core"},
			},
		},
		{
			name:  "flag overrides the file value for its own field",
			file:  fileBlock,
			flags: [][2]string{{"correlation-session-id", "flag-session"}},
			want: &types.CorrelationConfig{
				SessionID: "flag-session",
				TaskID:    "file-task",
				UserID:    "file-user",
				Labels:    map[string]string{"agent_profile": "file", "team": "core"},
			},
		},
		{
			name: "labels merge key by key and the last repeat wins",
			file: fileBlock,
			flags: [][2]string{
				{"correlation-label", "agent_profile=flag"},
				{"correlation-label", "env=a"},
				{"correlation-label", "env=b"},
			},
			want: &types.CorrelationConfig{
				SessionID: "file-session",
				TaskID:    "file-task",
				UserID:    "file-user",
				Labels:    map[string]string{"agent_profile": "flag", "team": "core", "env": "b"},
			},
		},
		{
			name:  "label value may contain an equals sign",
			flags: [][2]string{{"correlation-label", "q=a=b"}},
			want:  &types.CorrelationConfig{Labels: map[string]string{"q": "a=b"}},
		},
		{
			name:  "an explicitly empty flag clears only its own file field",
			file:  fileBlock,
			flags: [][2]string{{"correlation-user-id", ""}},
			want: &types.CorrelationConfig{
				SessionID: "file-session",
				TaskID:    "file-task",
				Labels:    map[string]string{"agent_profile": "file", "team": "core"},
			},
		},
		{
			name:  "an explicitly empty flag with nothing else set leaves no block",
			flags: [][2]string{{"correlation-session-id", ""}},
			want:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newTestHarnessCommand()
			cfg := baseFileConfig()
			if tc.file != nil {
				copied := *tc.file
				copied.Labels = map[string]string{}
				for k, v := range tc.file.Labels {
					copied.Labels[k] = v
				}
				cfg.Correlation = &copied
			}
			setFlags(t, cmd.Flags(), tc.flags)
			if err := applyOverrides(cmd, cfg, nil); err != nil {
				t.Fatalf("applyOverrides: %v", err)
			}
			if !reflect.DeepEqual(cfg.Correlation, tc.want) {
				t.Errorf("Correlation = %+v, want %+v", cfg.Correlation, tc.want)
			}
		})
	}
}

func TestApplyOverrides_CorrelationLabelRejectsMalformedPair(t *testing.T) {
	for _, entry := range []string{"agent_profile", "=value", "", "agent_profile="} {
		t.Run(entry, func(t *testing.T) {
			cmd := newTestHarnessCommand()
			setFlags(t, cmd.Flags(), [][2]string{{"correlation-label", entry}})
			err := applyOverrides(cmd, baseFileConfig(), nil)
			if err == nil || !strings.Contains(err.Error(), "--correlation-label") {
				t.Fatalf("applyOverrides error = %v, want a --correlation-label parse error", err)
			}
		})
	}
}

func TestBuildRunConfig_CorrelationFlagsValidate(t *testing.T) {
	cmd := newTestHarnessCommand()
	setFlags(t, cmd.Flags(), [][2]string{
		{"prompt", "do the thing"},
		{"correlation-session-id", "s1"},
		{"correlation-label", "Bad-Key=v"},
	})
	_, err := BuildRunConfig(RunConfigSources{Cmd: cmd, Resolve: ResolveAll})
	if err == nil || !strings.Contains(err.Error(), "correlation.labels key") {
		t.Fatalf("BuildRunConfig error = %v, want the label key validation error", err)
	}

	cmd = newTestHarnessCommand()
	setFlags(t, cmd.Flags(), [][2]string{
		{"prompt", "do the thing"},
		{"correlation-session-id", "s1"},
		{"correlation-label", "agent_profile=reviewer"},
	})
	cfg, err := BuildRunConfig(RunConfigSources{Cmd: cmd, Resolve: ResolveAll})
	if err != nil {
		t.Fatalf("BuildRunConfig: %v", err)
	}
	want := &types.CorrelationConfig{SessionID: "s1", Labels: map[string]string{"agent_profile": "reviewer"}}
	if !reflect.DeepEqual(cfg.Correlation, want) {
		t.Errorf("Correlation = %+v, want %+v", cfg.Correlation, want)
	}
}

func TestRunRunConfig_CorrelationFlagsEmitBlock(t *testing.T) {
	out, err := runRunConfigForTest(t, []string{
		"--correlation-session-id", "s1",
		"--correlation-label", "agent_profile=reviewer",
	}, `{"correlation":{"taskId":"from-stdin","sessionId":"stdin-session"}}`)
	if err != nil {
		t.Fatalf("runRunConfig: %v", err)
	}
	var cfg types.RunConfig
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("emitted output is not parseable JSON: %v\n%s", err, out)
	}
	want := &types.CorrelationConfig{
		SessionID: "s1",
		TaskID:    "from-stdin",
		Labels:    map[string]string{"agent_profile": "reviewer"},
	}
	if !reflect.DeepEqual(cfg.Correlation, want) {
		t.Errorf("Correlation = %+v, want %+v", cfg.Correlation, want)
	}
}
