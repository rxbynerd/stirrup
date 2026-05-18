package spec

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// TestLoadSuiteHCL_RunConfigFileOnly pins the simplest baseline shape:
// a suite-level `run_config_file = "..."` attribute populates the
// corresponding EvalSuite field and leaves EvalSuite.RunConfig nil.
// Tasks without per-task overrides must still have a nil
// RunConfigOverrides. The relative path is resolved against the suite
// file's directory so authors can write intuitive relative paths.
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

// TestLoadSuiteHCL_RunConfigFileAbsolutePath confirms that absolute paths
// in `run_config_file` are preserved verbatim — the relative-resolution
// pass must only join paths that are not already absolute.
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

// TestLoadSuiteHCL_InlineRunConfigBlock pins the inline `run_config` shape:
// nested provider / model_router / scalar fields all decode into a
// non-nil *types.RunConfig matching what callers would build by hand.
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
// that names the suite ID and both offending field names — operators
// must be able to locate the conflict from the error alone.
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

// TestLoadSuiteHCL_TaskRunConfigOverrides exercises the sparse
// per-task overlay: a subset of fields are set, the rest must remain
// unset (zero values / nil pointers) on the resulting
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
	// Fields not set must stay nil/zero — the overrides surface is
	// sparse by contract.
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
// pointer-typed overlay branches in runConfigOverridesSpecToType:
// model_router, context_strategy, edit_strategy, and verifier. Each
// must round-trip into a non-nil pointer on the resulting
// *types.RunConfigOverrides with the named fields preserved.
//
// The sub-blocks exist on the runConfigOverridesSpec for parity with
// the suite-level runConfigSpec; without this test, a typo in any of
// the four converter branches (e.g. assigning to the wrong target
// field) would silently drop the overlay.
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

// TestLoadSuiteHCL_RunConfigOverridesRejectsMode pins the post-B2
// invariant: run_config_overrides { mode = "..." } must be a parse
// error. Accepting the field opens a silent-conflict footgun where
// the overlay's mode is overwritten by the runner's --mode flag.
// The check is structural — the HCL surface omits mode entirely, so
// gohcl rejects it with an "unknown attribute" diagnostic.
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

// TestLoadSuiteHCL_ProviderWithCredential closes the
// credentialSpecToType coverage gap. The credential block is a
// security-critical path: a field-name typo silently drops an auth
// parameter and the live run picks up a misconfigured credential at
// runtime rather than at parse time.
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

// TestLoadSuiteHCL_RunConfigUnknownAttribute pins the "unknown
// attributes are errors" contract on the inline `run_config` block.
// Silently dropping a typo (e.g. `max_turn` instead of `max_turns`)
// would let a regression suite be validated against the wrong config;
// the parser must reject the construct.
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
// previous test for the per-task overlay: typos in
// `run_config_overrides` must fail loudly.
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

// TestLoadSuiteHCL_ExistingSuitesParse asserts that a suite which
// never declared any of the chunk-2 RunConfig fields continues to
// parse with the new code path and produce zero-valued RunConfig
// fields. This is the backwards-compat contract from the issue.
//
// `openai-responses-empty-tool-output.hcl` was updated in chunk 4
// to demonstrate the new authoring surface (it now sets a
// suite-level `run_config` block); its parse is covered by
// TestLoadSuiteHCL_OpenAIResponsesSuiteUsesInlineRunConfig below.
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

// TestLoadSuiteHCL_OpenAIResponsesSuiteUsesInlineRunConfig pins the
// openai-responses regression suite's chunk-4 update: the suite now
// authors a suite-level inline `run_config` block that nails the
// provider type and model_router so the regression scenario cannot
// be silently nullified by an operator's environment. The check is
// deliberately shallow (presence + provider type + model) — the
// full decoding surface is exhaustively tested elsewhere in this
// file; here we only care that the live suite uses the new flow.
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

// TestLoadSuiteHCL_RunConfigDeepBlocks exercises a richer inline
// run_config — nested blocks (executor, network, resources,
// permission_policy, code_scanner, guard_rail with stages,
// prompt_builder, context_strategy, edit_strategy, verifier with
// recursive child, trace_emitter with headers) — to pin that the
// recursive spec → types conversion preserves the full shape. The
// intent is to catch field-mapping mistakes in runConfigSpecToType
// without listing every leaf field in every test.
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

// TestLoadSuiteHCL_RejectsRawProviderAPIKey pins the parse-time
// secret:// scheme guard on the inline run_config provider block.
// Without this check a pasted-in literal API key would survive parse,
// land in the merged run_config.json the harness receives, and the
// retained redacted artifact would quietly rewrite it to "[REDACTED]"
// — masking the misconfiguration from audit. ValidateRunConfig
// catches the same shape at the types layer, but the parse-time
// error names the offending field so authors see the diagnostic where
// they wrote the value.
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

// TestLoadSuiteHCL_RejectsRawVcsBackendAPIKey extends the parse-time
// guard to the executor.vcs_backend nested block — same invariant,
// different field, same reasoning.
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

// TestLoadSuiteHCL_RejectsRawAPIKeyInTaskOverrides pins the parse-time
// guard on per-task run_config_overrides. The task-level overrides
// produce a *types.RunConfigOverrides rather than a *types.RunConfig,
// so the validator must walk both shapes.
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
