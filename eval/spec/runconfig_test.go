package spec

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// TestLoadSuiteHCL_RunConfigFileOnly asserts a suite-level
// `run_config_file` attribute populates EvalSuite.RunConfigFile
// (resolved against the suite file's directory) and leaves
// EvalSuite.RunConfig and task RunConfigOverrides nil.
func TestLoadSuiteHCL_RunConfigFileOnly(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err)
	check.Equal(filepath.Join(filepath.Dir(path), "configs/openai-base.json"), got.RunConfigFile)
	check.Nil(got.RunConfig, "run_config_file alone must leave RunConfig nil")
	must.Len(got.Tasks, 1)
	check.Nil(got.Tasks[0].RunConfigOverrides)
}

// TestLoadSuiteHCL_RunConfigFileAbsolutePath confirms absolute paths in
// `run_config_file` are preserved verbatim.
func TestLoadSuiteHCL_RunConfigFileAbsolutePath(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	must.Equal(abs, got.RunConfigFile, "absolute paths pass through verbatim")
}

// TestLoadSuiteHCL_InlineRunConfigBlock asserts an inline `run_config`
// block's nested provider / model_router / scalar fields decode into a
// non-nil *types.RunConfig matching a hand-built equivalent.
func TestLoadSuiteHCL_InlineRunConfigBlock(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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
	must.NoError(err)
	check.Empty(got.RunConfigFile)
	must.NotNil(got.RunConfig)
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
	must.Equal(want, got.RunConfig)
}

func TestLoadSuiteHCL_ToolsCommandOutputBlock(t *testing.T) {
	must := require.New(t)
	src := `
suite "s" {
  run_config {
    mode = "execution"

    provider {
      type        = "anthropic"
      api_key_ref = "secret://ANTHROPIC_KEY"
    }

    model_router {
      type     = "static"
      provider = "anthropic"
      model    = "claude-haiku-5-5"
    }

    tools {
      built_in = ["run_command", "read_file"]

      command_output {
        enabled          = false
        failure_posture  = "bestEffort"
        inline_max_bytes = 4096
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
	path := writeTemp(t, "runcfg-command-output.hcl", src)
	got, err := LoadSuiteHCL(path)
	must.NoError(err)
	must.NotNil(got.RunConfig)
	enabled := false
	wantTools := types.ToolsConfig{
		BuiltIn: []string{"run_command", "read_file"},
		CommandOutput: types.CommandOutputConfig{
			Enabled:        &enabled,
			FailurePosture: "bestEffort",
			InlineMaxBytes: 4096,
		},
	}
	must.Equal(wantTools, got.RunConfig.Tools)
}

func TestLoadSuiteHCL_ToolsPostEditBlocks(t *testing.T) {
	must := require.New(t)
	src := `
suite "s" {
  run_config {
    mode = "execution"

    provider {
      type        = "anthropic"
      api_key_ref = "secret://ANTHROPIC_KEY"
    }

    tools {
      post_edit {
        glob    = "*.go"
        command = "gofmt -l {path}"
      }
      post_edit {
        glob            = "cmd/*.go"
        command         = "go vet ./cmd/..."
        timeout_seconds = 120
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
	got, err := LoadSuiteHCL(writeTemp(t, "runcfg-post-edit.hcl", src))
	must.NoError(err)
	want := []types.PostEditCheckConfig{
		{Glob: "*.go", Command: "gofmt -l {path}"},
		{Glob: "cmd/*.go", Command: "go vet ./cmd/...", TimeoutSeconds: 120},
	}
	must.NotNil(got.RunConfig)
	must.Equal(want, got.RunConfig.Tools.PostEdit)
}

// TestLoadSuiteHCL_PostEditABArmsDifferOnlyInChecks pins the paired
// post-edit suites: identical tasks and run config apart from
// tools.postEdit, which only the on arm sets.
func TestLoadSuiteHCL_PostEditABArmsDifferOnlyInChecks(t *testing.T) {
	must := require.New(t)
	on, err := LoadSuiteHCL("../suites/post-edit-ab-on.hcl")
	must.NoError(err, "loading on arm")
	off, err := LoadSuiteHCL("../suites/post-edit-ab-off.hcl")
	must.NoError(err, "loading off arm")
	must.NotEmpty(on.Tasks, "post-edit A/B arms must carry a non-empty task list")
	must.Equal(off.Tasks, on.Tasks, "post-edit A/B arms must carry identical task lists")
	must.NotEmpty(on.RunConfig.Tools.PostEdit, "only the on arm may configure checks")
	must.Empty(off.RunConfig.Tools.PostEdit, "only the on arm may configure checks")
	onConfig := *on.RunConfig
	onConfig.Tools.PostEdit = nil
	must.Equal(*off.RunConfig, onConfig, "arms differ beyond tools.postEdit")
}

// TestLoadSuiteHCL_TraceEmitterArchiveBlock asserts a gcs trace_emitter
// archive block round-trips into a *types.TraceArchiveConfig. The local
// archive type is covered by TestLoadSuiteHCL_TraceEmitterArchiveLocalBlock.
func TestLoadSuiteHCL_TraceEmitterArchiveBlock(t *testing.T) {
	must := require.New(t)
	src := `
suite "s" {
  run_config {
    mode = "execution"

    provider {
      type        = "anthropic"
      api_key_ref = "secret://ANTHROPIC_KEY"
    }

    model_router {
      type     = "static"
      provider = "anthropic"
      model    = "claude-haiku-5-5"
    }

    trace_emitter {
      type = "jsonl"

      archive {
        type          = "gcs"
        bucket        = "stirrup-traces"
        object_prefix = "command-output/"
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
	path := writeTemp(t, "runcfg-trace-archive.hcl", src)
	got, err := LoadSuiteHCL(path)
	must.NoError(err)
	must.NotNil(got.RunConfig)
	wantArchive := &types.TraceArchiveConfig{
		Type:         "gcs",
		Bucket:       "stirrup-traces",
		ObjectPrefix: "command-output/",
	}
	must.Equal(wantArchive, got.RunConfig.TraceEmitter.Archive)
}

// TestLoadSuiteHCL_TraceEmitterArchiveLocalBlock covers the "local" archive
// type, whose field set (file_path) is disjoint from the "gcs" type
// exercised in TestLoadSuiteHCL_TraceEmitterArchiveBlock.
func TestLoadSuiteHCL_TraceEmitterArchiveLocalBlock(t *testing.T) {
	must := require.New(t)
	src := `
suite "s" {
  run_config {
    mode = "execution"

    provider {
      type        = "anthropic"
      api_key_ref = "secret://ANTHROPIC_KEY"
    }

    model_router {
      type     = "static"
      provider = "anthropic"
      model    = "claude-haiku-5-5"
    }

    trace_emitter {
      type = "jsonl"

      archive {
        type      = "local"
        file_path = "/var/log/stirrup/command-output.tar.gz"
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
	path := writeTemp(t, "runcfg-trace-archive-local.hcl", src)
	got, err := LoadSuiteHCL(path)
	must.NoError(err)
	must.NotNil(got.RunConfig)
	wantArchive := &types.TraceArchiveConfig{
		Type:     "local",
		FilePath: "/var/log/stirrup/command-output.tar.gz",
	}
	must.Equal(wantArchive, got.RunConfig.TraceEmitter.Archive)
}

// TestLoadSuiteHCL_TraceEmitterNoArchiveIsNil asserts a trace_emitter block
// with no archive sub-block leaves TraceEmitter.Archive nil. A non-nil
// archive with an empty type fails validateTraceArchiveConfig, so a
// zero-value struct would reject every suite that omits archive.
func TestLoadSuiteHCL_TraceEmitterNoArchiveIsNil(t *testing.T) {
	must := require.New(t)
	src := `
suite "s" {
  run_config {
    mode = "execution"

    provider {
      type        = "anthropic"
      api_key_ref = "secret://ANTHROPIC_KEY"
    }

    model_router {
      type     = "static"
      provider = "anthropic"
      model    = "claude-haiku-5-5"
    }

    trace_emitter {
      type = "jsonl"
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
	path := writeTemp(t, "runcfg-trace-no-archive.hcl", src)
	got, err := LoadSuiteHCL(path)
	must.NoError(err)
	must.NotNil(got.RunConfig)
	must.Nil(got.RunConfig.TraceEmitter.Archive)
}

// TestLoadSuiteHCL_RunConfigMutuallyExclusive asserts that setting both
// `run_config_file` and `run_config` on the same suite is a parse error
// naming the suite ID and both offending fields.
func TestLoadSuiteHCL_RunConfigMutuallyExclusive(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.Error(err, "a suite setting both run_config_file and run_config must fail")
	for _, frag := range []string{"dual-baseline", "run_config_file", "run_config", "mutually exclusive"} {
		check.ErrorContains(err, frag)
	}
}

// TestLoadSuiteHCL_TaskRunConfigOverrides asserts the per-task overlay is
// sparse: fields not set in the HCL remain zero/nil on the resulting
// *types.RunConfigOverrides.
func TestLoadSuiteHCL_TaskRunConfigOverrides(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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
	must.NoError(err)
	must.Len(got.Tasks, 1)
	ov := got.Tasks[0].RunConfigOverrides
	must.NotNil(ov)

	must.NotNil(ov.MaxTurns)
	check.Equal(4, *ov.MaxTurns)
	must.NotNil(ov.Provider)
	check.Equal("anthropic", ov.Provider.Type)
	check.Equal("secret://ANTHROPIC_KEY", ov.Provider.APIKeyRef)

	check.Nil(ov.ModelRouter)
	check.Nil(ov.ContextStrategy)
	check.Nil(ov.EditStrategy)
	check.Nil(ov.Verifier)
	check.Empty(ov.Mode)
}

// TestLoadSuiteHCL_TaskRunConfigOverridesAllPointerBlocks pins the
// pointer-typed overlay branches in runConfigOverridesSpecToType
// (model_router, context_strategy, edit_strategy, verifier): each must
// round-trip into a non-nil pointer with the named fields preserved.
func TestLoadSuiteHCL_TaskRunConfigOverridesAllPointerBlocks(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err)
	must.Len(got.Tasks, 1)
	ov := got.Tasks[0].RunConfigOverrides
	must.NotNil(ov)

	must.NotNil(ov.ModelRouter)
	check.Equal("static", ov.ModelRouter.Type)
	check.Equal("claude-sonnet-4-6", ov.ModelRouter.Model)

	must.NotNil(ov.ContextStrategy)
	check.Equal("truncate", ov.ContextStrategy.Type)
	check.Equal(8000, ov.ContextStrategy.MaxTokens)

	must.NotNil(ov.EditStrategy)
	check.Equal("fuzzy", ov.EditStrategy.Type)
	must.NotNil(ov.EditStrategy.FuzzyThreshold)
	check.InDelta(0.85, *ov.EditStrategy.FuzzyThreshold, 1e-9)

	must.NotNil(ov.Verifier)
	check.Equal("test-command", ov.Verifier.Type)
	check.Equal("true", ov.Verifier.Command)
}

// TestLoadSuiteHCL_RunConfigOverridesRejectsMode asserts
// `run_config_overrides { mode = "..." }` is a parse error: the HCL
// surface omits mode entirely, so gohcl rejects it as unknown.
func TestLoadSuiteHCL_RunConfigOverridesRejectsMode(t *testing.T) {
	must := require.New(t)
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
	must.Error(err, "mode inside run_config_overrides must be rejected")
	must.ErrorContains(err, "mode")
}

// TestLoadSuiteHCL_ProviderCredentialMapsFields covers credentialSpecToType:
// a field-name typo here would silently drop an auth parameter.
func TestLoadSuiteHCL_ProviderCredentialMapsFields(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err)
	must.NotNil(got.RunConfig)
	cred := got.RunConfig.Provider.Credential
	must.NotNil(cred)
	check.Equal("web-identity", cred.Type)
	check.Equal("arn:aws:iam::123456789012:role/eval", cred.RoleARN)
	must.NotNil(cred.TokenSource)
	check.Equal("env_var", cred.TokenSource.Type)
	check.Equal("AWS_WEB_IDENTITY_TOKEN", cred.TokenSource.EnvVar)
}

// TestLoadSuiteHCL_RunConfigUnknownAttribute asserts unknown attributes
// in the inline `run_config` block (e.g. a `max_turn` typo) are parse
// errors, not silently dropped.
func TestLoadSuiteHCL_RunConfigUnknownAttribute(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(err, "max_turn")
}

// TestLoadSuiteHCL_RunConfigOverridesUnknownAttribute asserts unknown
// attributes in a task's `run_config_overrides` block are parse errors.
func TestLoadSuiteHCL_RunConfigOverridesUnknownAttribute(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(err, "maxturns")
}

// TestLoadSuiteHCL_ExistingSuitesParse asserts that a suite declaring
// none of the RunConfig fields still parses and produces zero-valued
// RunConfig fields (backwards compatibility).
func TestLoadSuiteHCL_ExistingSuitesParse(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{name: "guardrail suite", path: "../suites/guardrail.hcl"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got, err := LoadSuiteHCL(tc.path)
			must.NoError(err)
			must.Empty(got.RunConfigFile)
			must.Nil(got.RunConfig)
			for _, task := range got.Tasks {
				must.Nil(task.RunConfigOverrides, "task %q", task.ID)
			}
			must.NotEmpty(got.Tasks)
		})
	}
}

// TestLoadSuiteHCL_ToolUseSuiteParses asserts the tool-use reliability
// suite parses and that its tool-trace judges decode with their
// sequence / call expectations intact.
func TestLoadSuiteHCL_ToolUseSuiteParses(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	got, err := LoadSuiteHCL("../suites/tooluse.hcl")
	must.NoError(err)
	check.Equal("tooluse-reliability", got.ID)
	must.NotEmpty(got.Tasks)

	sawSequence := false
	var walk func(j types.EvalJudge)
	walk = func(j types.EvalJudge) {
		if j.Type == "tool-trace" {
			must.NotNil(j.ToolTrace, "tool-trace judge should carry a ToolTrace")
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
	check.True(sawSequence, "expected at least one tool-trace judge with a sequence constraint")
}

// TestLoadSuiteHCL_OpenAIResponsesSuite_UsesInlineRunConfig asserts the
// openai-responses regression suite pins its provider type and
// model_router via an inline `run_config` block, so the regression
// scenario can't be silently nullified by an operator's environment.
func TestLoadSuiteHCL_OpenAIResponsesSuite_UsesInlineRunConfig(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	got, err := LoadSuiteHCL("../suites/openai-responses-empty-tool-output.hcl")
	must.NoError(err)
	check.Empty(got.RunConfigFile, "suite uses an inline run_config block")
	must.NotNil(got.RunConfig, "suite declares an inline run_config block")
	check.Equal("openai-responses", got.RunConfig.Provider.Type)
	check.Equal("secret://OPENAI_KEY", got.RunConfig.Provider.APIKeyRef)
	check.NotEmpty(got.RunConfig.ModelRouter.Model)
	check.NotEmpty(got.Tasks)
}

// TestLoadSuiteHCL_RuleOfTwoSuitesParse asserts the Rule-of-Two
// runtime-classifier suites keep their arming shape: the enforcing suite
// must keep web_fetch + run_command with no declared sensitivity and no
// enforce override; the observe-only companion must keep enforce:false.
func TestLoadSuiteHCL_RuleOfTwoSuitesParse(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	enforcing, err := LoadSuiteHCL("../suites/ruleoftwo.hcl")
	must.NoError(err, "LoadSuiteHCL(ruleoftwo.hcl)")
	check.Equal("ruleoftwo-enforcing", enforcing.ID)
	must.NotEmpty(enforcing.Tasks, "ruleoftwo.hcl should declare a task")
	rc := enforcing.RunConfig
	must.NotNil(rc, "ruleoftwo.hcl must declare an inline run_config block")
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
	check.True(hasWebFetch && hasRunCommand,
		"ruleoftwo.hcl run_config must keep web_fetch + run_command to auto-arm enforcing; got %v", rc.Tools.BuiltIn)
	if rc.SensitiveData != nil {
		check.False(*rc.SensitiveData, "ruleoftwo.hcl must not declare sensitiveData: that would arm observe-only, not enforcing")
	}
	if rc.RuleOfTwo != nil && rc.RuleOfTwo.Enforce != nil {
		check.True(*rc.RuleOfTwo.Enforce, "ruleoftwo.hcl must not set enforce:false: that is the observe-only companion's job")
	}

	observe, err := LoadSuiteHCL("../suites/ruleoftwo-observe.hcl")
	must.NoError(err, "LoadSuiteHCL(ruleoftwo-observe.hcl)")
	check.Equal("ruleoftwo-observe-only", observe.ID)
	must.NotNil(observe.RunConfig)
	must.NotNil(observe.RunConfig.RuleOfTwo)
	must.NotNil(observe.RunConfig.RuleOfTwo.Enforce)
	check.False(*observe.RunConfig.RuleOfTwo.Enforce, "ruleoftwo-observe.hcl must set ruleOfTwo.enforce = false")
}

// TestLoadSuiteHCL_RunConfigNestedBlocks exercises a richer inline
// run_config with nested blocks to catch field-mapping mistakes in
// runConfigSpecToType across the full recursive spec -> types shape.
func TestLoadSuiteHCL_RunConfigNestedBlocks(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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

      stage {
        type        = "shieldstral"
        endpoint    = "https://api.mistral.ai"
        api_key_ref = "secret://MISTRAL_API_KEY"
        criteria    = ["jailbreak", "no_internal_hosts"]
        custom_criteria = {
          "no_internal_hosts" = "The content references internal hostnames."
        }
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
	must.NoError(err)
	rc := got.RunConfig
	must.NotNil(rc)

	check.Equal("template", rc.PromptBuilder.Type)
	check.Equal("hello", rc.PromptBuilder.Template)
	check.Equal("truncate", rc.ContextStrategy.Type)
	check.Equal(8000, rc.ContextStrategy.MaxTokens)
	check.Equal("fuzzy", rc.EditStrategy.Type)
	must.NotNil(rc.EditStrategy.FuzzyThreshold)
	check.InDelta(0.85, *rc.EditStrategy.FuzzyThreshold, 1e-9)
	check.Equal("composite", rc.Verifier.Type)
	must.Len(rc.Verifier.Verifiers, 1)
	check.Equal("test-command", rc.Verifier.Verifiers[0].Type)
	check.Equal("true", rc.Verifier.Verifiers[0].Command)
	check.Equal("container", rc.Executor.Type)
	must.NotNil(rc.Executor.Network)
	check.Equal("allowlist", rc.Executor.Network.Mode)
	must.NotNil(rc.Executor.Resources)
	check.InDelta(2.0, rc.Executor.Resources.CPUs, 1e-9)
	check.Equal("policies/default.cedar", rc.PermissionPolicy.PolicyFile)
	must.NotNil(rc.CodeScanner)
	check.True(rc.CodeScanner.BlockOnWarn)
	must.NotNil(rc.RuleOfTwo)
	must.NotNil(rc.RuleOfTwo.Enforce)
	check.True(*rc.RuleOfTwo.Enforce)
	must.NotNil(rc.RuleOfTwo.Runtime)
	check.Equal("patterns", rc.RuleOfTwo.Runtime.Classifier)
	check.Equal("block-external", rc.RuleOfTwo.Runtime.OnDetect)
	check.Equal([]string{"sensitive_data", "pii"}, rc.RuleOfTwo.Runtime.GuardCriteria)
	must.NotNil(rc.GuardRail)
	check.Equal("composite", rc.GuardRail.Type)
	must.Len(rc.GuardRail.Stages, 2)
	check.Equal("granite-guardian", rc.GuardRail.Stages[0].Type)
	shieldstral := rc.GuardRail.Stages[1]
	check.Equal("shieldstral", shieldstral.Type)
	check.Equal("secret://MISTRAL_API_KEY", shieldstral.APIKeyRef)
	check.Len(shieldstral.Criteria, 2)
	check.Equal("The content references internal hostnames.", shieldstral.CustomCriteria["no_internal_hosts"])
	check.Equal("otel", rc.TraceEmitter.Type)
	check.Equal("http://collector:4317", rc.TraceEmitter.Endpoint)
	check.Equal("grpc", rc.TraceEmitter.Protocol)
	check.Equal("secret://GRAFANA_CLOUD_AUTH", rc.TraceEmitter.Headers["Authorization"])
	check.Equal("staging", rc.Observability.Environment)
}

// TestLoadSuiteHCL_RejectsRawProviderAPIKey asserts a pasted-in literal
// API key on the inline run_config provider block is a parse-time error
// naming the offending field, rather than surviving into the merged
// config and being masked by redaction.
func TestLoadSuiteHCL_RejectsRawProviderAPIKey(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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
	must.Error(err, "raw provider api_key_ref should be rejected")
	must.ErrorContains(err, "secret://")
	check.ErrorContains(err, "provider.api_key_ref")
}

// TestLoadSuiteHCL_RejectsRawGuardRailAPIKey asserts the same parse-time
// secret:// enforcement covers guard_rail.api_key_ref, including inside
// composite stages.
func TestLoadSuiteHCL_RejectsRawGuardRailAPIKey(t *testing.T) {
	must := require.New(t)
	src := `
suite "s" {
  run_config {
    guard_rail {
      type = "composite"

      stage {
        type        = "shieldstral"
        endpoint    = "https://api.mistral.ai"
        api_key_ref = "sk-live-abc123"
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
	path := writeTemp(t, "raw-guardrail-key.hcl", src)
	_, err := LoadSuiteHCL(path)
	must.ErrorContains(err, "guard_rail.stage[0].api_key_ref")
}

// TestLoadSuiteHCL_RejectsRawVcsBackendAPIKey extends the same guard to
// the executor.vcs_backend nested block.
func TestLoadSuiteHCL_RejectsRawVcsBackendAPIKey(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.Error(err, "raw vcs_backend api_key_ref should be rejected")
	must.ErrorContains(err, "secret://")
	check.ErrorContains(err, "executor.vcs_backend.api_key_ref")
}

// TestLoadSuiteHCL_RejectsRawAPIKeyInTaskOverrides asserts the same guard
// on per-task run_config_overrides, which produce a
// *types.RunConfigOverrides rather than a *types.RunConfig.
func TestLoadSuiteHCL_RejectsRawAPIKeyInTaskOverrides(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.Error(err, "raw task-overrides api_key_ref should be rejected")
	must.ErrorContains(err, "secret://")
	check.ErrorContains(err, "run_config_overrides.provider.api_key_ref")
}

// TestAnchorRunConfigPaths_ResolvesRelativeReplayFiles pins the anchoring
// rule for the default provider and the providers map: relative paths
// become absolute under baseDir, absolute paths and empty values pass
// through, and a nil config is a no-op.
func TestAnchorRunConfigPaths_ResolvesRelativeReplayFiles(t *testing.T) {
	must := require.New(t)
	base := t.TempDir()
	cfg := &types.RunConfig{
		Provider: types.ProviderConfig{Type: "replay", ReplayFile: "rec/a.jsonl"},
		Providers: map[string]types.ProviderConfig{
			"abs":  {Type: "replay", ReplayFile: "/abs/b.jsonl"},
			"rel":  {Type: "replay", ReplayFile: "rec/c.jsonl"},
			"live": {Type: "anthropic"},
		},
	}
	AnchorRunConfigPaths(cfg, base)
	must.Equal(filepath.Join(base, "rec/a.jsonl"), cfg.Provider.ReplayFile)
	must.Equal("/abs/b.jsonl", cfg.Providers["abs"].ReplayFile)
	must.Equal(filepath.Join(base, "rec/c.jsonl"), cfg.Providers["rel"].ReplayFile)
	must.Empty(cfg.Providers["live"].ReplayFile)
	AnchorRunConfigPaths(nil, base)
}
