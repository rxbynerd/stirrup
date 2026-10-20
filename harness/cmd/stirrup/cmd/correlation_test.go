package cmd

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func setFlags(t *testing.T, cmdFlags interface{ Set(string, string) error }, kvs [][2]string) {
	t.Helper()
	must := require.New(t)
	for _, kv := range kvs {
		must.NoError(cmdFlags.Set(kv[0], kv[1]), "set --%s", kv[0])
	}
}

func TestApplyOverrides_CorrelationDefaultLeavesBlockNil(t *testing.T) {
	must := require.New(t)
	cmd := newTestHarnessCommand()
	cfg := baseFileConfig()
	must.NoError(applyOverrides(cmd, cfg, nil))
	must.Nil(cfg.Correlation)
}

func TestApplyOverrides_CorrelationFlagsMergeWithFile(t *testing.T) {
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
			must := require.New(t)
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
			must.NoError(applyOverrides(cmd, cfg, nil))
			must.Equal(tc.want, cfg.Correlation)
		})
	}
}

func TestApplyOverrides_CorrelationLabelRejectsMalformedPair(t *testing.T) {
	cases := []struct {
		name  string
		entry string
	}{
		{name: "missing equals sign", entry: "agent_profile"},
		{name: "empty key", entry: "=value"},
		{name: "empty entry", entry: ""},
		{name: "empty value", entry: "agent_profile="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			cmd := newTestHarnessCommand()
			setFlags(t, cmd.Flags(), [][2]string{{"correlation-label", tc.entry}})
			err := applyOverrides(cmd, baseFileConfig(), nil)
			must.ErrorContains(err, "--correlation-label")
		})
	}
}

func TestBuildRunConfig_CorrelationFlagsValidate(t *testing.T) {
	t.Run("rejects an invalid label key", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		setFlags(t, cmd.Flags(), [][2]string{
			{"prompt", "do the thing"},
			{"correlation-session-id", "s1"},
			{"correlation-label", "Bad-Key=v"},
		})
		_, err := BuildRunConfig(RunConfigSources{Cmd: cmd, Resolve: ResolveAll})
		must.ErrorContains(err, "correlation.labels key")
	})

	t.Run("builds the block from valid flags", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		setFlags(t, cmd.Flags(), [][2]string{
			{"prompt", "do the thing"},
			{"correlation-session-id", "s1"},
			{"correlation-label", "agent_profile=reviewer"},
		})
		cfg, err := BuildRunConfig(RunConfigSources{Cmd: cmd, Resolve: ResolveAll})
		must.NoError(err)
		want := &types.CorrelationConfig{SessionID: "s1", Labels: map[string]string{"agent_profile": "reviewer"}}
		must.Equal(want, cfg.Correlation)
	})
}

func TestRunRunConfig_CorrelationFlagsEmitBlock(t *testing.T) {
	must := require.New(t)
	out, err := runRunConfigForTest(t, []string{
		"--correlation-session-id", "s1",
		"--correlation-label", "agent_profile=reviewer",
	}, `{"correlation":{"taskId":"from-stdin","sessionId":"stdin-session"}}`)
	must.NoError(err)

	var cfg types.RunConfig
	must.NoError(json.Unmarshal([]byte(out), &cfg), "emitted output must be valid JSON:\n%s", out)
	want := &types.CorrelationConfig{
		SessionID: "s1",
		TaskID:    "from-stdin",
		Labels:    map[string]string{"agent_profile": "reviewer"},
	}
	must.Equal(want, cfg.Correlation)
}
