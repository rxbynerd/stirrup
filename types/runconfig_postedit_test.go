package types

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func validPostEditCheck() PostEditCheckConfig {
	return PostEditCheckConfig{Glob: "*.go", Command: "gofmt -l {path}"}
}

func TestValidateRunConfig_PostEdit_AcceptedInExecutionMode(t *testing.T) {
	cases := []struct {
		name         string
		executorType string
	}{
		{"unset executor type", ""},
		{"local executor", "local"},
		{"container executor", "container"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.Executor = ExecutorConfig{Type: tc.executorType}
			c.Tools.PostEdit = []PostEditCheckConfig{
				validPostEditCheck(),
				{Glob: "cmd/*.go", Command: "go vet ./cmd/...", TimeoutSeconds: MaxPostEditTimeoutSeconds},
				{Glob: "*.go", Command: `test -f "{path}" && sh -c 'gofmt -l {path}'`},
			}
			require.NoError(t, ValidateRunConfig(c), "valid postEdit must be accepted")
		})
	}
}

func TestValidateRunConfig_PostEdit_RejectedInReadOnlyModes(t *testing.T) {
	for _, mode := range []string{"planning", "review", "research", "toil"} {
		t.Run(mode, func(t *testing.T) {
			c := validConfig()
			c.Mode = mode
			c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
			c.Tools.BuiltIn = []string{"read_file"}
			c.Tools.PostEdit = []PostEditCheckConfig{validPostEditCheck()}
			require.ErrorContains(t, ValidateRunConfig(c), "tools.postEdit is not allowed in read-only mode")

			c.Tools.PostEdit = nil
			require.NoError(t, ValidateRunConfig(c), "baseline read-only config without postEdit must validate")
		})
	}
}

func TestValidateRunConfig_PostEdit_RejectedForExecutorsWithoutExec(t *testing.T) {
	for _, executorType := range []string{"api", "none"} {
		t.Run(executorType, func(t *testing.T) {
			c := validConfig()
			c.Executor = ExecutorConfig{Type: executorType}
			c.Tools.PostEdit = []PostEditCheckConfig{validPostEditCheck()}
			require.ErrorContains(t, ValidateRunConfig(c), "tools.postEdit requires an exec-capable executor")
		})
	}
}

func TestValidateRunConfig_PostEdit_EntryRules(t *testing.T) {
	cases := []struct {
		name  string
		check PostEditCheckConfig
		want  string
	}{
		{"missing glob", PostEditCheckConfig{Command: "true"}, "tools.postEdit[0].glob is required"},
		{"bad glob", PostEditCheckConfig{Glob: "[", Command: "true"}, "is not a valid pattern"},
		{"long glob", PostEditCheckConfig{Glob: strings.Repeat("a", maxPostEditGlobBytes+1), Command: "true"}, "glob must be <="},
		{"missing command", PostEditCheckConfig{Glob: "*", Command: "  "}, "tools.postEdit[0].command is required"},
		{"long command", PostEditCheckConfig{Glob: "*", Command: strings.Repeat("a", maxHookCommandBytes+1)}, "command must be <="},
		{"secret ref", PostEditCheckConfig{Glob: "*", Command: "curl SECRET://x"}, "secret://"},
		{"single-quoted placeholder", PostEditCheckConfig{Glob: "*", Command: "gofmt -l '{path}'"}, "must not wrap {path} in single quotes"},
		{"negative timeout", PostEditCheckConfig{Glob: "*", Command: "true", TimeoutSeconds: -1}, "timeoutSeconds must be 0"},
		{"timeout over max", PostEditCheckConfig{Glob: "*", Command: "true", TimeoutSeconds: MaxPostEditTimeoutSeconds + 1}, "timeoutSeconds must be 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.Tools.PostEdit = []PostEditCheckConfig{tc.check}
			require.ErrorContains(t, ValidateRunConfig(c), tc.want)
		})
	}
}

func TestValidateRunConfig_PostEdit_EntryCountCapped(t *testing.T) {
	c := validConfig()
	for range maxPostEditChecks + 1 {
		c.Tools.PostEdit = append(c.Tools.PostEdit, validPostEditCheck())
	}
	require.ErrorContains(t, ValidateRunConfig(c), "tools.postEdit must have <=")
}

func TestEffectivePostEditTimeout_DefaultAndExplicit(t *testing.T) {
	t.Run("zero timeout uses default", func(t *testing.T) {
		require.Equal(t, DefaultPostEditTimeoutSeconds, EffectivePostEditTimeout(PostEditCheckConfig{}))
	})
	t.Run("explicit timeout is kept", func(t *testing.T) {
		require.Equal(t, 7, EffectivePostEditTimeout(PostEditCheckConfig{TimeoutSeconds: 7}))
	})
}

func TestToolsConfig_PostEdit_DecodesJSONShape(t *testing.T) {
	var tc ToolsConfig
	raw := `{"postEdit":[{"glob":"*.go","command":"gofmt -l {path}","timeoutSeconds":30}]}`
	require.NoError(t, json.Unmarshal([]byte(raw), &tc))

	want := PostEditCheckConfig{Glob: "*.go", Command: "gofmt -l {path}", TimeoutSeconds: 30}
	require.Len(t, tc.PostEdit, 1)
	require.Equal(t, want, tc.PostEdit[0])
}

func TestPostEditCheckRecord_Failed(t *testing.T) {
	cases := []struct {
		name string
		rec  PostEditCheckRecord
		want bool
	}{
		{"zero record is not failed", PostEditCheckRecord{}, false},
		{"non-zero exit code is failed", PostEditCheckRecord{ExitCode: 1}, true},
		{"timed out record is failed", PostEditCheckRecord{Error: "timed out", TimedOut: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.rec.Failed())
		})
	}
}
