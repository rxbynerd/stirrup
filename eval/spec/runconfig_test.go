package spec

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// TestLoadSuiteHCL_RunConfigFileOnly pins the simplest baseline shape:
// a suite-level `run_config_file = "..."` attribute populates the
// corresponding EvalSuite field and leaves EvalSuite.RunConfig nil.
// Tasks without per-task overrides must still have a nil
// RunConfigOverrides.
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
	if got.RunConfigFile != "configs/openai-base.json" {
		t.Errorf("RunConfigFile = %q, want %q", got.RunConfigFile, "configs/openai-base.json")
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

// TestLoadSuiteHCL_ExistingSuitesParse asserts that the live
// guardrail.hcl and openai-responses-empty-tool-output.hcl files in
// eval/suites/ continue to parse with the new code path and produce
// zero-valued RunConfig fields (no new fields populated). This is the
// backwards-compat contract from the issue.
func TestLoadSuiteHCL_ExistingSuitesParse(t *testing.T) {
	cases := []string{
		"../suites/guardrail.hcl",
		"../suites/openai-responses-empty-tool-output.hcl",
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

// TestLoadSuiteHCL_RunConfigDeepBlocks exercises a richer inline
// run_config — nested blocks (executor, network, resources,
// permission_policy, code_scanner, guard_rail with stages) — to pin
// that the recursive spec → types conversion preserves the full
// shape. The intent is to catch field-mapping mistakes in
// runConfigSpecToType without listing every leaf field in every test.
func TestLoadSuiteHCL_RunConfigDeepBlocks(t *testing.T) {
	src := `
suite "s" {
  run_config {
    mode = "execution"

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
	if rc.Observability.Environment != "staging" {
		t.Errorf("Observability.Environment = %q, want staging", rc.Observability.Environment)
	}
}
