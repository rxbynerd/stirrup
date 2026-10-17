package types

import (
	"encoding/json"
	"strings"
	"testing"
)

func validPostEditCheck() PostEditCheckConfig {
	return PostEditCheckConfig{Glob: "*.go", Command: "gofmt -l {path}"}
}

func TestValidateRunConfig_PostEdit_AcceptedInExecutionMode(t *testing.T) {
	for _, executorType := range []string{"", "local", "container"} {
		c := validConfig()
		c.Executor = ExecutorConfig{Type: executorType}
		c.Tools.PostEdit = []PostEditCheckConfig{
			validPostEditCheck(),
			{Glob: "cmd/*.go", Command: "go vet ./cmd/...", TimeoutSeconds: MaxPostEditTimeoutSeconds},
			{Glob: "*.go", Command: `test -f "{path}" && sh -c 'gofmt -l {path}'`},
		}
		if err := ValidateRunConfig(c); err != nil {
			t.Errorf("executor %q: valid postEdit rejected: %v", executorType, err)
		}
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
			err := ValidateRunConfig(c)
			if err == nil {
				t.Fatalf("postEdit in read-only mode %q must fail validation", mode)
			}
			if !strings.Contains(err.Error(), "tools.postEdit is not allowed in read-only mode") {
				t.Errorf("unexpected error: %v", err)
			}

			c.Tools.PostEdit = nil
			if err := ValidateRunConfig(c); err != nil {
				t.Fatalf("baseline read-only config must validate without postEdit: %v", err)
			}
		})
	}
}

func TestValidateRunConfig_PostEdit_RejectedForExecutorsWithoutExec(t *testing.T) {
	for _, executorType := range []string{"api", "none"} {
		t.Run(executorType, func(t *testing.T) {
			c := validConfig()
			c.Executor = ExecutorConfig{Type: executorType}
			c.Tools.PostEdit = []PostEditCheckConfig{validPostEditCheck()}
			err := ValidateRunConfig(c)
			if err == nil {
				t.Fatalf("postEdit with executor %q must fail validation", executorType)
			}
			if !strings.Contains(err.Error(), "tools.postEdit requires an exec-capable executor") {
				t.Errorf("unexpected error: %v", err)
			}
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
			err := ValidateRunConfig(c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestValidateRunConfig_PostEdit_EntryCountCapped(t *testing.T) {
	c := validConfig()
	for i := 0; i <= maxPostEditChecks; i++ {
		c.Tools.PostEdit = append(c.Tools.PostEdit, validPostEditCheck())
	}
	err := ValidateRunConfig(c)
	if err == nil || !strings.Contains(err.Error(), "tools.postEdit must have <=") {
		t.Fatalf("error = %v", err)
	}
}

func TestEffectivePostEditTimeout(t *testing.T) {
	if got := EffectivePostEditTimeout(PostEditCheckConfig{}); got != DefaultPostEditTimeoutSeconds {
		t.Errorf("zero timeout resolved to %d", got)
	}
	if got := EffectivePostEditTimeout(PostEditCheckConfig{TimeoutSeconds: 7}); got != 7 {
		t.Errorf("explicit timeout resolved to %d", got)
	}
}

func TestToolsConfig_PostEditJSONShape(t *testing.T) {
	var tc ToolsConfig
	raw := `{"postEdit":[{"glob":"*.go","command":"gofmt -l {path}","timeoutSeconds":30}]}`
	if err := json.Unmarshal([]byte(raw), &tc); err != nil {
		t.Fatal(err)
	}
	want := PostEditCheckConfig{Glob: "*.go", Command: "gofmt -l {path}", TimeoutSeconds: 30}
	if len(tc.PostEdit) != 1 || tc.PostEdit[0] != want {
		t.Fatalf("decoded %+v", tc.PostEdit)
	}
}

func TestPostEditCheckRecord_Failed(t *testing.T) {
	cases := []struct {
		rec  PostEditCheckRecord
		want bool
	}{
		{PostEditCheckRecord{}, false},
		{PostEditCheckRecord{ExitCode: 1}, true},
		{PostEditCheckRecord{Error: "timed out", TimedOut: true}, true},
	}
	for _, tc := range cases {
		if got := tc.rec.Failed(); got != tc.want {
			t.Errorf("%+v.Failed() = %v, want %v", tc.rec, got, tc.want)
		}
	}
}
