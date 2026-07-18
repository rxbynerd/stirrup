package spec

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// TestLoadSuiteHCL_RunConfigFileOnly asserts a suite-level
// `run_config_file` attribute populates EvalSuite.RunConfigFile
// (resolved against the suite file's directory) and leaves
// EvalSuite.RunConfig and task RunConfigOverrides nil.
func TestLoadSuiteHCL_RunConfigFileOnly(t *testing.T) {
	src := `
suite "s" {
  description     = "file-baseline"
  run_config_file = "configs/openai-base.json"

  task "t1" {
    mode   = "execution"
    prompt = "p"
    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
`
	path := writeTemp(t, "runcfg-file.hcl", src)
	got, err := LoadSuiteHCL(path)
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	want := filepath.Join(filepath.Dir(path), "configs/openai-base.json")
	if got.RunConfigFile != want {
		t.Errorf("RunConfigFile = %q, want %q", got.RunConfigFile, want)
	}
	if got.RunConfig != nil {
		t.Errorf("RunConfig should be nil when only run_config_file set, got %#v", got.RunConfig)
	}
	if len(got.Tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(got.Tasks))
	}
	if got.Tasks[0].RunConfigOverrides != nil {
		t.Errorf("task RunConfigOverrides should be nil, got %#v", got.Tasks[0].RunConfigOverrides)
	}
}

// TestLoadSuiteHCL_RunConfigFileAbsolutePath confirms absolute paths in
// `run_config_file` are preserved verbatim.
func TestLoadSuiteHCL_RunConfigFileAbsolutePath(t *testing.T) {
	abs := "/etc/stirrup/baseline.json"
	src := `
suite "s" {
  run_config_file = "` + abs + `"

  task "t1" {
    prompt = "p"
    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
`
	path := writeTemp(t, "runcfg-abs.hcl", src)
	got, err := LoadSuiteHCL(path)
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	if got.RunConfigFile != abs {
		t.Errorf("RunConfigFile = %q, want %q (absolute path should pass through)", got.RunConfigFile, abs)
	}
}

// TestLoadSuiteHCL_InlineRunConfigBlock asserts an inline `run_config`
// block's nested provider / model_router / scalar fields decode into a
// non-nil *types.RunConfig matching a hand-built equivalent.
func TestLoadSuiteHCL_InlineRunConfigBlock(t *testing.T) {
	src := `
suite "s" {
  run_config {
    mode      = "execution"
    max_turns = 10

    provider {
      type        = "openai-responses"
      api_key_ref = "secret://OPENAI_KEY"
      base_url    = "https://api.openai.com/v1"
    }

    model_router {
      type     = "static"
      provider = "openai-responses"
      model    = "gpt-5.4-nano"
    }

    permission_policy {
      type = "deny-side-effects"
    }
  }

  task "t1" {
    mode   = "execution"
    prompt = "p"
    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
`
	path := writeTemp(t, "runcfg-inline.hcl", src)
	got, err := LoadSuiteHCL(path)
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	if got.RunConfigFile != "" {
		t.Errorf("RunConfigFile should be empty, got %q", got.RunConfigFile)
	}
	if got.RunConfig == nil {
		t.Fatalf("RunConfig should be non-nil")
	}
	want := &types.RunConfig{
		Mode:     "execution",
		MaxTurns: 10,
		Provider: types.ProviderConfig{
			Type:      "openai-responses",
			APIKeyRef: "secret://OPENAI_KEY",
			BaseURL:   "https://api.openai.com/v1",
		},
		ModelRouter: types.ModelRouterConfig{
			Type:     "static",
			Provider: "openai-responses",
			Model:    "gpt-5.4-nano",
		},
		PermissionPolicy: types.PermissionPolicyConfig{
			Type: "deny-side-effects",
		},
	}
	if !reflect.DeepEqual(got.RunConfig, want) {
		t.Fatalf("RunConfig mismatch\n got:  %#v\n want: %#v", got.RunConfig, want)
	}
}

// TestLoadSuiteHCL_RunConfigMutuallyExclusive asserts that setting both
// `run_config_file` and `run_config` on the same suite is a parse error
// naming the suite ID and both offending fields.
func TestLoadSuiteHCL_RunConfigMutuallyExclusive(t *testing.T) {
	src := `
suite "dual-baseline" {
  run_config_file = "configs/base.json"

  run_config {
    max_turns = 5
  }

  task "t1" {
    mode   = "execution"
    prompt = "p"
    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
`
	path := writeTemp(t, "runcfg-both.hcl", src)
	_, err := LoadSuiteHCL(path)
	if err == nil {
		t.Fatal("expected error for suite setting both run_config_file and run_config")
	}
	frags := []string{"dual-baseline", "run_config_file", "run_config", "mutually exclusive"}
	for _, f := range frags {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("error = %q, want it to contain %q", err.Error(), f)
		}
	}
}

// TestLoadSuiteHCL_TaskRunConfigOverrides asserts the per-task overlay is
// sparse: fields not set in the HCL remain zero/nil on the resulting
// *types.RunConfigOverrides.
func TestLoadSuiteHCL_TaskRunConfigOverrides(t *testing.T) {
	src := `
suite "s" {
  task "t1" {
    mode   = "execution"
    prompt = "p"

    run_config_overrides {
      max_turns = 4

      provider {
        type        = "anthropic"
        api_key_ref = "secret://ANTHROPIC_KEY"
      }
    }

    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
`
	path := writeTemp(t, "task-overrides.hcl", src)
	got, err := LoadSuiteHCL(path)
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	if len(got.Tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(got.Tasks))
	}
	ov := got.Tasks[0].RunConfigOverrides
	if ov == nil {
		t.Fatalf("RunConfigOverrides should be non-nil")
	}
	if ov.MaxTurns == nil || *ov.MaxTurns != 4 {
		t.Errorf("MaxTurns = %v, want pointer to 4", ov.MaxTurns)
	}
	if ov.Provider == nil {
		t.Fatalf("Provider override should be non-nil")
	}
	if ov.Provider.Type != "anthropic" {
		t.Errorf("Provider.Type = %q, want %q", ov.Provider.Type, "anthropic")
	}
	if ov.Provider.APIKeyRef != "secret://ANTHROPIC_KEY" {
		t.Errorf("Provider.APIKeyRef = %q, want %q", ov.Provider.APIKeyRef, "secret://ANTHROPIC_KEY")
	}
	if ov.ModelRouter != nil {
		t.Errorf("ModelRouter should be nil, got %#v", ov.ModelRouter)
	}
	if ov.ContextStrategy != nil {
		t.Errorf("ContextStrategy should be nil, got %#v", ov.ContextStrategy)
	}
	if ov.EditStrategy != nil {
		t.Errorf("EditStrategy should be nil, got %#v", ov.EditStrategy)
	}
	if ov.Verifier != nil {
		t.Errorf("Verifier should be nil, got %#v", ov.Verifier)
	}
	if ov.Mode != "" {
		t.Errorf("Mode should be empty, got %q", ov.Mode)
	}
}

// TestLoadSuiteHCL_TaskRunConfigOverridesAllPointerBlocks pins the
// pointer-typed overlay branches in runConfigOverridesSpecToType
// (model_router, context_strategy, edit_strategy, verifier): each must
// round-trip into a non-nil pointer with the named fields preserved.
func TestLoadSuiteHCL_TaskRunConfigOverridesAllPointerBlocks(t *testing.T) {
	src := `
suite "s" {
  task "t1" {
    mode   = "execution"
    prompt = "p"

    run_config_overrides {
      model_router {
        type     = "static"
        provider = "anthropic"
        model    = "claude-sonnet-4-6"
      }

      context_strategy {
        type       = "truncate"
        max_tokens = 8000
      }

      edit_strategy {
        type            = "fuzzy"
        fuzzy_threshold = 0.85
      }

      verifier {
        type    = "test-command"
        command = "true"
      }
    }

    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
`
	path := writeTemp(t, "task-overrides-all-blocks.hcl", src)
	got, err := LoadSuiteHCL(path)
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	if len(got.Tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(got.Tasks))
	}
	ov := got.Tasks[0].RunConfigOverrides
	if ov == nil {
		t.Fatalf("RunConfigOverrides should be non-nil")
	}
	if ov.ModelRouter == nil || ov.ModelRouter.Type != "static" || ov.ModelRouter.Model != "claude-sonnet-4-6" {
		t.Errorf("ModelRouter overlay = %#v, want {Type:static Model:claude-sonnet-4-6}", ov.ModelRouter)
	}
	if ov.ContextStrategy == nil || ov.ContextStrategy.Type != "truncate" || ov.ContextStrategy.MaxTokens != 8000 {
		t.Errorf("ContextStrategy overlay = %#v, want {Type:truncate MaxTokens:8000}", ov.ContextStrategy)
	}
	if ov.EditStrategy == nil || ov.EditStrategy.Type != "fuzzy" {
		t.Errorf("EditStrategy overlay = %#v, want Type:fuzzy", ov.EditStrategy)
	}
	if ov.EditStrategy != nil && (ov.EditStrategy.FuzzyThreshold == nil || *ov.EditStrategy.FuzzyThreshold != 0.85) {
		t.Errorf("EditStrategy.FuzzyThreshold = %v, want pointer to 0.85", ov.EditStrategy.FuzzyThreshold)
	}
	if ov.Verifier == nil || ov.Verifier.Type != "test-command" || ov.Verifier.Command != "true" {
		t.Errorf("Verifier overlay = %#v, want {Type:test-command Command:true}", ov.Verifier)
	}
}

// TestLoadSuiteHCL_RunConfigOverridesRejectsMode asserts
// `run_config_overrides { mode = "..." }` is a parse error: the HCL
// surface omits mode entirely, so gohcl rejects it as unknown.
func TestLoadSuiteHCL_RunConfigOverridesRejectsMode(t *testing.T) {
	src := `
suite "s" {
  task "t1" {
    mode   = "execution"
    prompt = "p"

    run_config_overrides {
      mode = "planning"
    }

    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
`
	path := writeTemp(t, "task-overrides-mode.hcl", src)
	_, err := LoadSuiteHCL(path)
	if err == nil {
		t.Fatal("expected error for mode attribute inside run_config_overrides")
	}
	if !strings.Contains(err.Error(), "mode") {
		t.Errorf("error = %q, want it to name the rejected attribute", err.Error())
	}
}

// TestLoadSuiteHCL_ProviderWithCredential covers credentialSpecToType:
// a field-name typo here would silently drop an auth parameter.
func TestLoadSuiteHCL_ProviderWithCredential(t *testing.T) {
	src := `
suite "s" {
  run_config {
    provider {
      type = "bedrock"

      credential {
        type     = "web-identity"
        role_arn = "arn:aws:iam::123456789012:role/eval"

        token_source {
          type    = "env_var"
          env_var = "AWS_WEB_IDENTITY_TOKEN"
        }
      }
    }
  }

  task "t1" {
    mode   = "execution"
    prompt = "p"
    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
`
	path := writeTemp(t, "runcfg-credential.hcl", src)
	got, err := LoadSuiteHCL(path)
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	if got.RunConfig == nil {
		t.Fatal("RunConfig should be non-nil")
	}
	cred := got.RunConfig.Provider.Credential
	if cred == nil {
		t.Fatal("Provider.Credential should be non-nil")
	}
	if cred.Type != "web-identity" {
		t.Errorf("Credential.Type = %q, want web-identity", cred.Type)
	}
	if cred.RoleARN != "arn:aws:iam::123456789012:role/eval" {
		t.Errorf("Credential.RoleARN = %q, want canonical ARN", cred.RoleARN)
	}
	if cred.TokenSource == nil {
		t.Fatal("Credential.TokenSource should be non-nil")
	}
	if cred.TokenSource.Type != "env_var" {
		t.Errorf("Credential.TokenSource.Type = %q, want env_var", cred.TokenSource.Type)
	}
	if cred.TokenSource.EnvVar != "AWS_WEB_IDENTITY_TOKEN" {
		t.Errorf("Credential.TokenSource.EnvVar = %q, want AWS_WEB_IDENTITY_TOKEN", cred.TokenSource.EnvVar)
	}
}

// TestLoadSuiteHCL_RunConfigUnknownAttribute asserts unknown attributes
// in the inline `run_config` block (e.g. a `max_turn` typo) are parse
// errors, not silently dropped.
func TestLoadSuiteHCL_RunConfigUnknownAttribute(t *testing.T) {
	src := `
suite "s" {
  run_config {
    max_turn = 10
  }

  task "t1" {
    mode   = "execution"
    prompt = "p"
    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
`
	path := writeTemp(t, "runcfg-typo.hcl", src)
	_, err := LoadSuiteHCL(path)
	if err == nil {
		t.Fatal("expected error for unknown attribute in run_config block")
	}
	if !strings.Contains(err.Error(), "max_turn") {
		t.Errorf("error = %q, want it to name the offending attribute", err.Error())
	}
}

// TestLoadSuiteHCL_RunConfigOverridesUnknownAttribute mirrors the
// previous test for `run_config_overrides`.
func TestLoadSuiteHCL_RunConfigOverridesUnknownAttribute(t *testing.T) {
	src := `
suite "s" {
  task "t1" {
    mode   = "execution"
    prompt = "p"

    run_config_overrides {
      maxturns = 4
    }

    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
`
	path := writeTemp(t, "task-overrides-typo.hcl", src)
	_, err := LoadSuiteHCL(path)
	if err == nil {
		t.Fatal("expected error for unknown attribute in run_config_overrides block")
	}
	if !strings.Contains(err.Error(), "maxturns") {
		t.Errorf("error = %q, want it to name the offending attribute", err.Error())
	}
}

// TestLoadSuiteHCL_ExistingSuitesParse asserts that a suite declaring
// none of the RunConfig fields still parses and produces zero-valued
// RunConfig fields (backwards compatibility).
func TestLoadSuiteHCL_ExistingSuitesParse(t *testing.T) {
	cases := []string{
		"../suites/guardrail.hcl",
	}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			got, err := LoadSuiteHCL(path)
			if err != nil {
				t.Fatalf("LoadSuiteHCL(%s): %v", path, err)
			}
			if got.RunConfigFile != "" {
				t.Errorf("RunConfigFile should be empty (backwards-compat), got %q", got.RunConfigFile)
			}
			if got.RunConfig != nil {
				t.Errorf("RunConfig should be nil (backwards-compat), got %#v", got.RunConfig)
			}
			for _, task := range got.Tasks {
				if task.RunConfigOverrides != nil {
					t.Errorf("task %q RunConfigOverrides should be nil (backwards-compat), got %#v", task.ID, task.RunConfigOverrides)
				}
			}
			if len(got.Tasks) == 0 {
				t.Errorf("expected at least one task")
			}
		})
	}
}

// TestLoadSuiteHCL_ToolUseSuiteParses asserts the tool-use reliability
// suite parses and that its tool-trace judges decode with their
// sequence / call expectations intact.
func TestLoadSuiteHCL_ToolUseSuiteParses(t *testing.T) {
	got, err := LoadSuiteHCL("../suites/tooluse.hcl")
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	if got.ID != "tooluse-reliability" {
		t.Errorf("suite ID = %q, want tooluse-reliability", got.ID)
	}
	if len(got.Tasks) == 0 {
		t.Fatal("expected at least one task")
	}

	sawSequence := false
	var walk func(j types.EvalJudge)
	walk = func(j types.EvalJudge) {
		if j.Type == "tool-trace" {
			if j.ToolTrace == nil {
				t.Errorf("tool-trace judge in task has nil ToolTrace")
				return
			}
			if len(j.ToolTrace.Sequence) > 0 {
				sawSequence = true
			}
		}
		for _, sub := range j.Judges {
			walk(sub)
		}
	}
	for _, task := range got.Tasks {
		walk(task.Judge)
	}
	if !sawSequence {
		t.Error("expected at least one tool-trace judge with a sequence constraint")
	}
}

// TestLoadSuiteHCL_OpenAIResponsesSuiteUsesInlineRunConfig asserts the
// openai-responses regression suite pins its provider type and
// model_router via an inline `run_config` block, so the regression
// scenario can't be silently nullified by an operator's environment.
func TestLoadSuiteHCL_OpenAIResponsesSuiteUsesInlineRunConfig(t *testing.T) {
	got, err := LoadSuiteHCL("../suites/openai-responses-empty-tool-output.hcl")
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	if got.RunConfigFile != "" {
		t.Errorf("RunConfigFile should be empty (suite uses inline block), got %q", got.RunConfigFile)
	}
	if got.RunConfig == nil {
		t.Fatal("RunConfig should be non-nil: the suite declares an inline run_config block")
	}
	if got.RunConfig.Provider.Type != "openai-responses" {
		t.Errorf("Provider.Type = %q, want %q", got.RunConfig.Provider.Type, "openai-responses")
	}
	if got.RunConfig.Provider.APIKeyRef != "secret://OPENAI_KEY" {
		t.Errorf("Provider.APIKeyRef = %q, want %q", got.RunConfig.Provider.APIKeyRef, "secret://OPENAI_KEY")
	}
	if got.RunConfig.ModelRouter.Model == "" {
		t.Errorf("ModelRouter.Model should be non-empty")
	}
	if len(got.Tasks) == 0 {
		t.Errorf("expected at least one task")
	}
}

// TestLoadSuiteHCL_RuleOfTwoSuitesParse asserts the Rule-of-Two
// runtime-classifier suites keep their arming shape: the enforcing suite
// must keep web_fetch + run_command with no declared sensitivity and no
// enforce override; the observe-only companion must keep enforce:false.
func TestLoadSuiteHCL_RuleOfTwoSuitesParse(t *testing.T) {
	enforcing, err := LoadSuiteHCL("../suites/ruleoftwo.hcl")
	if err != nil {
		t.Fatalf("LoadSuiteHCL(ruleoftwo.hcl): %v", err)
	}
	if enforcing.ID != "ruleoftwo-enforcing" {
		t.Errorf("suite ID = %q, want ruleoftwo-enforcing", enforcing.ID)
	}
	if len(enforcing.Tasks) == 0 {
		t.Fatal("expected at least one task in ruleoftwo.hcl")
	}
	rc := enforcing.RunConfig
	if rc == nil {
		t.Fatal("ruleoftwo.hcl must declare an inline run_config block")
	}
	// The factory auto-arms enforcing only when untrusted input and
	// external comms both hold without a declared sensitivity; web_fetch
	// supplies both legs, run_command supplies external comms.
	hasWebFetch, hasRunCommand := false, false
	for _, name := range rc.Tools.BuiltIn {
		switch name {
		case "web_fetch":
			hasWebFetch = true
		case "run_command":
			hasRunCommand = true
		}
	}
	if !hasWebFetch || !hasRunCommand {
		t.Errorf("ruleoftwo.hcl run_config must keep web_fetch + run_command to auto-arm enforcing; got %v", rc.Tools.BuiltIn)
	}
	if rc.SensitiveData != nil && *rc.SensitiveData {
		t.Error("ruleoftwo.hcl must not declare sensitiveData: that would arm observe-only, not enforcing")
	}
	if rc.RuleOfTwo != nil && rc.RuleOfTwo.Enforce != nil && !*rc.RuleOfTwo.Enforce {
		t.Error("ruleoftwo.hcl must not set enforce:false: that is the observe-only companion's job")
	}

	observe, err := LoadSuiteHCL("../suites/ruleoftwo-observe.hcl")
	if err != nil {
		t.Fatalf("LoadSuiteHCL(ruleoftwo-observe.hcl): %v", err)
	}
	if observe.ID != "ruleoftwo-observe-only" {
		t.Errorf("suite ID = %q, want ruleoftwo-observe-only", observe.ID)
	}
	if observe.RunConfig == nil || observe.RunConfig.RuleOfTwo == nil ||
		observe.RunConfig.RuleOfTwo.Enforce == nil || *observe.RunConfig.RuleOfTwo.Enforce {
		t.Error("ruleoftwo-observe.hcl must set ruleOfTwo.enforce = false")
	}
}

// TestLoadSuiteHCL_RunConfigDeepBlocks exercises a richer inline
// run_config with nested blocks to catch field-mapping mistakes in
// runConfigSpecToType across the full recursive spec -> types shape.
func TestLoadSuiteHCL_RunConfigDeepBlocks(t *testing.T) {
	src := `
suite "s" {
  run_config {
    mode = "execution"

    prompt_builder {
      type     = "template"
      template = "hello"
    }

    context_strategy {
      type       = "truncate"
      max_tokens = 8000
    }

    edit_strategy {
      type            = "fuzzy"
      fuzzy_threshold = 0.85
    }

    verifier {
      type = "composite"

      verifier {
        type    = "test-command"
        command = "true"
      }
    }

    executor {
      type      = "container"
      image     = "stirrup/sandbox:latest"
      runtime   = "runsc"

      network {
        mode      = "allowlist"
        allowlist = ["api.openai.com"]
      }

      resources {
        cpus      = 2.0
        memory_mb = 1024
        disk_mb   = 2048
        pids      = 256
      }
    }

    permission_policy {
      type        = "policy-engine"
      policy_file = "policies/default.cedar"
      fallback    = "deny-side-effects"
    }

    code_scanner {
      type          = "composite"
      scanners      = ["patterns", "semgrep"]
      block_on_warn = true
    }

    rule_of_two {
      enforce = true

      runtime {
        classifier     = "patterns"
        on_detect      = "block-external"
        guard_criteria = ["sensitive_data", "pii"]
      }
    }

    guard_rail {
      type = "composite"

      stage {
        type     = "granite-guardian"
        endpoint = "http://localhost:8000/v1/chat/completions"
      }
    }

    trace_emitter {
      type     = "otel"
      endpoint = "http://collector:4317"
      protocol = "grpc"
      headers = {
        "Authorization" = "secret://GRAFANA_CLOUD_AUTH"
      }
    }

    observability {
      environment       = "staging"
      service_namespace = "stirrup-eval"
    }
  }

  task "t1" {
    mode   = "execution"
    prompt = "p"
    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
`
	path := writeTemp(t, "runcfg-deep.hcl", src)
	got, err := LoadSuiteHCL(path)
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	rc := got.RunConfig
	if rc == nil {
		t.Fatal("RunConfig should be non-nil")
	}
	if rc.PromptBuilder.Type != "template" || rc.PromptBuilder.Template != "hello" {
		t.Errorf("PromptBuilder = %#v, want {Type:template Template:hello}", rc.PromptBuilder)
	}
	if rc.ContextStrategy.Type != "truncate" || rc.ContextStrategy.MaxTokens != 8000 {
		t.Errorf("ContextStrategy = %#v, want {Type:truncate MaxTokens:8000}", rc.ContextStrategy)
	}
	if rc.EditStrategy.Type != "fuzzy" {
		t.Errorf("EditStrategy.Type = %q, want fuzzy", rc.EditStrategy.Type)
	}
	if rc.EditStrategy.FuzzyThreshold == nil || *rc.EditStrategy.FuzzyThreshold != 0.85 {
		t.Errorf("EditStrategy.FuzzyThreshold = %v, want pointer to 0.85", rc.EditStrategy.FuzzyThreshold)
	}
	if rc.Verifier.Type != "composite" {
		t.Errorf("Verifier.Type = %q, want composite", rc.Verifier.Type)
	}
	if len(rc.Verifier.Verifiers) != 1 || rc.Verifier.Verifiers[0].Type != "test-command" || rc.Verifier.Verifiers[0].Command != "true" {
		t.Errorf("Verifier.Verifiers = %#v, want one test-command child", rc.Verifier.Verifiers)
	}
	if rc.Executor.Type != "container" {
		t.Errorf("Executor.Type = %q, want %q", rc.Executor.Type, "container")
	}
	if rc.Executor.Network == nil || rc.Executor.Network.Mode != "allowlist" {
		t.Errorf("Executor.Network = %#v, want allowlist", rc.Executor.Network)
	}
	if rc.Executor.Resources == nil || rc.Executor.Resources.CPUs != 2.0 {
		t.Errorf("Executor.Resources = %#v, want cpus=2.0", rc.Executor.Resources)
	}
	if rc.PermissionPolicy.PolicyFile != "policies/default.cedar" {
		t.Errorf("PermissionPolicy.PolicyFile = %q, want policies/default.cedar", rc.PermissionPolicy.PolicyFile)
	}
	if rc.CodeScanner == nil || !rc.CodeScanner.BlockOnWarn {
		t.Errorf("CodeScanner = %#v, want BlockOnWarn=true", rc.CodeScanner)
	}
	if rc.RuleOfTwo == nil || rc.RuleOfTwo.Enforce == nil || !*rc.RuleOfTwo.Enforce {
		t.Errorf("RuleOfTwo = %#v, want Enforce=&true", rc.RuleOfTwo)
	}
	if rc.RuleOfTwo == nil || rc.RuleOfTwo.Runtime == nil {
		t.Fatalf("RuleOfTwo.Runtime should be non-nil, got %#v", rc.RuleOfTwo)
	}
	if rc.RuleOfTwo.Runtime.Classifier != "patterns" || rc.RuleOfTwo.Runtime.OnDetect != "block-external" {
		t.Errorf("RuleOfTwo.Runtime = %#v, want {Classifier:patterns OnDetect:block-external}", rc.RuleOfTwo.Runtime)
	}
	if got := rc.RuleOfTwo.Runtime.GuardCriteria; len(got) != 2 || got[0] != "sensitive_data" || got[1] != "pii" {
		t.Errorf("RuleOfTwo.Runtime.GuardCriteria = %v, want [sensitive_data pii]", got)
	}
	if rc.GuardRail == nil || rc.GuardRail.Type != "composite" {
		t.Errorf("GuardRail = %#v, want composite", rc.GuardRail)
	}
	if rc.GuardRail == nil || len(rc.GuardRail.Stages) != 1 || rc.GuardRail.Stages[0].Type != "granite-guardian" {
		t.Errorf("GuardRail.Stages = %#v, want one granite-guardian stage", rc.GuardRail.Stages)
	}
	if rc.TraceEmitter.Type != "otel" || rc.TraceEmitter.Endpoint != "http://collector:4317" || rc.TraceEmitter.Protocol != "grpc" {
		t.Errorf("TraceEmitter = %#v, want otel/grpc/collector", rc.TraceEmitter)
	}
	if rc.TraceEmitter.Headers["Authorization"] != "secret://GRAFANA_CLOUD_AUTH" {
		t.Errorf("TraceEmitter.Headers[Authorization] = %q, want secret://GRAFANA_CLOUD_AUTH", rc.TraceEmitter.Headers["Authorization"])
	}
	if rc.Observability.Environment != "staging" {
		t.Errorf("Observability.Environment = %q, want staging", rc.Observability.Environment)
	}
}

// TestLoadSuiteHCL_RejectsRawProviderAPIKey asserts a pasted-in literal
// API key on the inline run_config provider block is a parse-time error
// naming the offending field, rather than surviving into the merged
// config and being masked by redaction.
func TestLoadSuiteHCL_RejectsRawProviderAPIKey(t *testing.T) {
	src := `
suite "s" {
  run_config {
    provider {
      type        = "openai-responses"
      api_key_ref = "sk-live-abc123"
    }
  }

  task "t1" {
    prompt = "p"
    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
`
	path := writeTemp(t, "raw-provider-key.hcl", src)
	_, err := LoadSuiteHCL(path)
	if err == nil {
		t.Fatalf("LoadSuiteHCL: expected error for raw provider api_key_ref, got nil")
	}
	if !strings.Contains(err.Error(), "secret://") {
		t.Errorf("error message %q does not mention secret:// scheme", err.Error())
	}
	if !strings.Contains(err.Error(), "provider.api_key_ref") {
		t.Errorf("error message %q does not name the offending field", err.Error())
	}
}

// TestLoadSuiteHCL_RejectsRawVcsBackendAPIKey extends the same guard to
// the executor.vcs_backend nested block.
func TestLoadSuiteHCL_RejectsRawVcsBackendAPIKey(t *testing.T) {
	src := `
suite "s" {
  run_config {
    executor {
      type = "container"
      vcs_backend {
        type        = "github"
        api_key_ref = "ghp_literaltokenvalue"
      }
    }
  }

  task "t1" {
    prompt = "p"
    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
`
	path := writeTemp(t, "raw-vcs-key.hcl", src)
	_, err := LoadSuiteHCL(path)
	if err == nil {
		t.Fatalf("LoadSuiteHCL: expected error for raw vcs_backend api_key_ref, got nil")
	}
	if !strings.Contains(err.Error(), "secret://") {
		t.Errorf("error message %q does not mention secret:// scheme", err.Error())
	}
	if !strings.Contains(err.Error(), "executor.vcs_backend.api_key_ref") {
		t.Errorf("error message %q does not name the offending field", err.Error())
	}
}

// TestLoadSuiteHCL_RejectsRawAPIKeyInTaskOverrides asserts the same guard
// on per-task run_config_overrides, which produce a
// *types.RunConfigOverrides rather than a *types.RunConfig.
func TestLoadSuiteHCL_RejectsRawAPIKeyInTaskOverrides(t *testing.T) {
	src := `
suite "s" {
  task "t1" {
    prompt = "p"
    run_config_overrides {
      provider {
        type        = "anthropic"
        api_key_ref = "sk-ant-rawvalue"
      }
    }
    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
`
	path := writeTemp(t, "raw-override-key.hcl", src)
	_, err := LoadSuiteHCL(path)
	if err == nil {
		t.Fatalf("LoadSuiteHCL: expected error for raw task-overrides api_key_ref, got nil")
	}
	if !strings.Contains(err.Error(), "secret://") {
		t.Errorf("error message %q does not mention secret:// scheme", err.Error())
	}
	if !strings.Contains(err.Error(), "run_config_overrides.provider.api_key_ref") {
		t.Errorf("error message %q does not name the offending field", err.Error())
	}
}
