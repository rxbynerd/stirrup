package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// TestLoadSuiteHCL_StructEquality parses an HCL fixture and asserts
// it deep-equals a hand-rolled types.EvalSuite literal with the same
// logical content. This is the core guarantee: HCL is the canonical
// authoring surface and must deserialise into the documented struct
// shape exactly. The literal lives in this test (rather than a JSON
// fixture) now that the legacy JSON loader has been removed.
func TestLoadSuiteHCL_StructEquality(t *testing.T) {
	hclPath := filepath.Join("testdata", "sample.hcl")

	got, err := LoadSuiteHCL(hclPath)
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}

	want := types.EvalSuite{
		ID:          "sample-suite",
		Description: "round-trip fixture",
		Tasks: []types.EvalTask{
			{
				ID:          "single-judge",
				Description: "exercises a non-composite judge",
				Repo:        "",
				Ref:         "",
				Prompt:      "line one\nline two\n",
				Mode:        "execution",
				Judge: types.EvalJudge{
					Type:    "test-command",
					Command: "test ! -f EXFILTRATED",
				},
			},
			{
				ID:          "composite-judge",
				Description: "exercises composite + nested judges",
				Repo:        "",
				Ref:         "",
				Prompt:      "write brief.md",
				Mode:        "execution",
				Judge: types.EvalJudge{
					Type:    "composite",
					Require: "all",
					Judges: []types.EvalJudge{
						{Type: "file-exists", Paths: []string{"brief.md"}},
						{Type: "file-contains", Path: "brief.md", Pattern: "(?i)token"},
					},
				},
			},
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("HCL parse mismatch\n got:  %#v\n want: %#v", got, want)
	}
}

// TestLoadSuiteHCL_CommentsTolerated confirms that all three HCL comment
// styles parse without affecting the resulting suite.
func TestLoadSuiteHCL_CommentsTolerated(t *testing.T) {
	src := `
# leading hash comment
// leading slash comment
/* leading
   block comment */
suite "with-comments" {
  description = "ok" # trailing hash
  // trailing slash
  task "t1" {
    /* inline block */
    mode   = "execution"
    prompt = "hello"
    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
`
	path := writeTemp(t, "comments.hcl", src)
	got, err := LoadSuiteHCL(path)
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	if got.ID != "with-comments" || len(got.Tasks) != 1 {
		t.Fatalf("unexpected suite: %#v", got)
	}
	if got.Tasks[0].Prompt != "hello" {
		t.Errorf("Prompt = %q, want %q", got.Tasks[0].Prompt, "hello")
	}
}

// TestLoadSuiteHCL_HeredocPreserved asserts that a `<<-EOT` heredoc
// preserves its multi-line content (with the trailing newline) byte for
// byte. The leading-tab stripping rule is part of HCL semantics; we
// just want the documented behaviour to be predictable.
func TestLoadSuiteHCL_HeredocPreserved(t *testing.T) {
	src := "suite \"hd\" {\n" +
		"  task \"t1\" {\n" +
		"    mode   = \"execution\"\n" +
		"    prompt = <<-EOT\n" +
		"      first\n" +
		"      second\n" +
		"    EOT\n" +
		"    judge {\n" +
		"      type    = \"test-command\"\n" +
		"      command = \"true\"\n" +
		"    }\n" +
		"  }\n" +
		"}\n"
	path := writeTemp(t, "heredoc.hcl", src)
	got, err := LoadSuiteHCL(path)
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	want := "first\nsecond\n"
	if got.Tasks[0].Prompt != want {
		t.Fatalf("Prompt = %q, want %q", got.Tasks[0].Prompt, want)
	}
}

// TestLoadSuiteHCL_CompositeChildrenOrdered asserts that composite
// judges with two `judge` child blocks decode to a Judges slice of
// length 2 in source order.
func TestLoadSuiteHCL_CompositeChildrenOrdered(t *testing.T) {
	src := `
suite "c" {
  task "t1" {
    mode   = "execution"
    prompt = "p"
    judge {
      type    = "composite"
      require = "all"
      judge {
        type  = "file-exists"
        paths = ["a.txt"]
      }
      judge {
        type    = "file-contains"
        path    = "a.txt"
        pattern = "x"
      }
    }
  }
}
`
	path := writeTemp(t, "composite.hcl", src)
	got, err := LoadSuiteHCL(path)
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	j := got.Tasks[0].Judge
	if j.Type != "composite" || j.Require != "all" {
		t.Fatalf("unexpected outer judge: %#v", j)
	}
	if len(j.Judges) != 2 {
		t.Fatalf("got %d sub-judges, want 2", len(j.Judges))
	}
	if j.Judges[0].Type != "file-exists" {
		t.Errorf("Judges[0].Type = %q, want %q", j.Judges[0].Type, "file-exists")
	}
	if j.Judges[1].Type != "file-contains" {
		t.Errorf("Judges[1].Type = %q, want %q", j.Judges[1].Type, "file-contains")
	}
}

// TestLoadSuiteHCL_ValidationErrors covers all the guard-rails: missing
// suite ID, missing task ID, no tasks, invalid judge.type, invalid
// composite require value, and recursive convertJudge errors surfacing
// through the composite branch. Each error message must mention every
// listed fragment so authors can find the problem.
func TestLoadSuiteHCL_ValidationErrors(t *testing.T) {
	cases := []struct {
		name         string
		src          string
		wantErrFrags []string
	}{
		{
			name: "missing suite id (empty label)",
			src: `
suite "" {
  task "t1" {
    mode   = "execution"
    prompt = "p"
    judge {
      type    = "test-command"
      command = "true"
    }
  }
}`,
			wantErrFrags: []string{"suite ID is required"},
		},
		{
			name: "no tasks",
			src: `
suite "empty" {
  description = "no tasks"
}`,
			wantErrFrags: []string{"must contain at least one task"},
		},
		{
			name: "missing task id (empty label)",
			src: `
suite "s" {
  task "" {
    mode   = "execution"
    prompt = "p"
    judge {
      type    = "test-command"
      command = "true"
    }
  }
}`,
			wantErrFrags: []string{"is missing an id label"},
		},
		{
			name: "invalid judge type",
			src: `
suite "s" {
  task "t1" {
    mode   = "execution"
    prompt = "p"
    judge { type = "made-up" }
  }
}`,
			wantErrFrags: []string{"invalid judge.type"},
		},
		{
			name: "invalid composite require",
			src: `
suite "s" {
  task "t1" {
    mode   = "execution"
    prompt = "p"
    judge {
      type    = "composite"
      require = "some"
      judge {
      type    = "test-command"
      command = "true"
    }
    }
  }
}`,
			wantErrFrags: []string{"judge.require"},
		},
		{
			name: "composite with invalid child type",
			src: `
suite "s" {
  task "t1" {
    mode   = "execution"
    prompt = "p"
    judge {
      type    = "composite"
      require = "all"
      judge { type = "bad" }
    }
  }
}`,
			// The recursive convertJudge call must surface the child
			// error with its context-prefixed path so authors can find
			// the offending nested judge by index.
			wantErrFrags: []string{"invalid judge.type", "judge[0]"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTemp(t, "case.hcl", tc.src)
			_, err := LoadSuiteHCL(path)
			if err == nil {
				t.Fatalf("expected error containing %v, got nil", tc.wantErrFrags)
			}
			for _, frag := range tc.wantErrFrags {
				if !strings.Contains(err.Error(), frag) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), frag)
				}
			}
		})
	}
}

// TestLoadSuiteHCL_UnknownTopLevelBlock rejects a `variable` (or any
// other non-`suite`) top-level block with a clear forward-looking
// message. The grammar may grow these later; until then they are off
// limits.
func TestLoadSuiteHCL_UnknownTopLevelBlock(t *testing.T) {
	src := `
variable "foo" {
  default = "bar"
}

suite "s" {
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
	path := writeTemp(t, "unknown.hcl", src)
	_, err := LoadSuiteHCL(path)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported top-level block") {
		t.Fatalf("error = %q, want it to mention unsupported top-level block", err.Error())
	}
	if !strings.Contains(err.Error(), "variable") {
		t.Fatalf("error = %q, want it to name the offending block", err.Error())
	}
}

// TestLoadSuiteHCL_UnknownTopLevelAttributesDeterministic asserts that
// when multiple stray top-level attributes are present, the reported
// error names them in a deterministic (sorted) order. Map iteration
// order would otherwise make future tests flaky.
func TestLoadSuiteHCL_UnknownTopLevelAttributesDeterministic(t *testing.T) {
	src := `
zeta = "z"
alpha = "a"

suite "s" {
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
	path := writeTemp(t, "attrs.hcl", src)
	_, err := LoadSuiteHCL(path)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), `unsupported top-level attribute "alpha"`) {
		t.Fatalf("error = %q, want it to name alpha first (sorted)", err.Error())
	}
}

// TestLoadSuiteHCL_UnknownTopLevelBlockOutsideProbeList ensures the
// catch-all branch in rejectUnsupportedTopLevel handles block types
// that aren't part of the named probe list (e.g. `output`, `resource`).
func TestLoadSuiteHCL_UnknownTopLevelBlockOutsideProbeList(t *testing.T) {
	src := `
output "x" {
  value = "y"
}

suite "s" {
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
	path := writeTemp(t, "output-block.hcl", src)
	_, err := LoadSuiteHCL(path)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported top-level block") {
		t.Fatalf("error = %q, want it to mention unsupported top-level block", err.Error())
	}
	if !strings.Contains(err.Error(), "output") {
		t.Fatalf("error = %q, want it to name the offending block", err.Error())
	}
}

// TestLoadSuiteHCL_EmptyFile fails clearly rather than panicking when
// handed a zero-byte source.
func TestLoadSuiteHCL_EmptyFile(t *testing.T) {
	path := writeTemp(t, "empty.hcl", "")
	_, err := LoadSuiteHCL(path)
	if err == nil {
		t.Fatal("expected error for empty file")
	}
	if !strings.Contains(err.Error(), "no suite block found") {
		t.Fatalf("error = %q, want it to say no suite block was found", err.Error())
	}
}

// TestLoadSuiteHCL_TwoSuiteBlocks fails clearly: exactly one suite per
// file is the contract.
func TestLoadSuiteHCL_TwoSuiteBlocks(t *testing.T) {
	src := `
suite "a" {
  task "t1" {
    mode   = "execution"
    prompt = "p"
    judge {
      type    = "test-command"
      command = "true"
    }
  }
}

suite "b" {
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
	path := writeTemp(t, "two.hcl", src)
	_, err := LoadSuiteHCL(path)
	if err == nil {
		t.Fatal("expected error for two suite blocks")
	}
	if !strings.Contains(err.Error(), "exactly one suite block") {
		t.Fatalf("error = %q, want it to mention the one-suite contract", err.Error())
	}
}

// TestLoadSuiteHCL_MalformedBytes returns a clear parser error rather
// than panicking when the input isn't valid HCL.
func TestLoadSuiteHCL_MalformedBytes(t *testing.T) {
	path := writeTemp(t, "broken.hcl", `suite "x" { not even close to valid {{`)
	_, err := LoadSuiteHCL(path)
	if err == nil {
		t.Fatal("expected error for malformed HCL")
	}
}

// TestLoadSuiteHCL_MissingFile surfaces the underlying os error.
func TestLoadSuiteHCL_MissingFile(t *testing.T) {
	_, err := LoadSuiteHCL("/nonexistent/missing.hcl")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// TestLoadSuiteHCL_TaskMissingJudgeBlock pins the gohcl.DecodeBody
// error path: the most common structural authoring mistake (forgetting
// the `judge {}` sub-block) must surface a clear diagnostic. The error
// originates inside hcl/v2 so we only assert that "judge" appears in
// the message — the wording is library-controlled.
func TestLoadSuiteHCL_TaskMissingJudgeBlock(t *testing.T) {
	src := `suite "s" { task "t" { prompt = "x" } }`
	path := writeTemp(t, "no-judge.hcl", src)
	_, err := LoadSuiteHCL(path)
	if err == nil {
		t.Fatal("expected error for task missing judge block")
	}
	if !strings.Contains(err.Error(), "judge") {
		t.Fatalf("error = %q, want it to mention judge", err.Error())
	}
}

// TestLoadSuiteHCL_NonCompositeWithChildJudge pins the contract that
// nesting `judge` blocks inside a non-composite parent is rejected
// loudly rather than silently dropped (gohcl decodes the field for
// every judge type).
func TestLoadSuiteHCL_NonCompositeWithChildJudge(t *testing.T) {
	src := `
suite "s" {
  task "t1" {
    mode   = "execution"
    prompt = "p"
    judge {
      type  = "file-exists"
      paths = ["a.txt"]
      judge {
        type    = "test-command"
        command = "true"
      }
    }
  }
}
`
	path := writeTemp(t, "non-composite-with-child.hcl", src)
	_, err := LoadSuiteHCL(path)
	if err == nil {
		t.Fatal("expected error for non-composite judge with nested judge block")
	}
	if !strings.Contains(err.Error(), "does not support nested judge blocks") {
		t.Fatalf("error = %q, want it to mention nested judge blocks", err.Error())
	}
}

// TestLoadSuiteHCL_EmptyCompositeJudge pins the contract that a
// composite block with zero children is rejected. The runtime evaluator
// would otherwise treat `all` of zero as a vacuous pass, producing
// misleading green CI for what is almost always a forgotten typo.
func TestLoadSuiteHCL_EmptyCompositeJudge(t *testing.T) {
	src := `
suite "s" {
  task "t1" {
    mode   = "execution"
    prompt = "p"
    judge {
      type    = "composite"
      require = "all"
    }
  }
}
`
	path := writeTemp(t, "empty-composite.hcl", src)
	_, err := LoadSuiteHCL(path)
	if err == nil {
		t.Fatal("expected error for empty composite judge")
	}
	if !strings.Contains(err.Error(), "at least one nested judge block") {
		t.Fatalf("error = %q, want it to mention nested judge requirement", err.Error())
	}
}

// TestLoadSuiteHCL_CompositeRequireDefaultsToAll pins the contract that
// omitting `require` on a composite judge defaults to "all". A change
// of the default to "" or "any" must surface here.
func TestLoadSuiteHCL_CompositeRequireDefaultsToAll(t *testing.T) {
	src := `
suite "s" {
  task "t" {
    mode   = "execution"
    prompt = "p"
    judge {
      type = "composite"
      judge {
        type  = "file-exists"
        paths = ["a.txt"]
      }
    }
  }
}
`
	path := writeTemp(t, "default-require.hcl", src)
	got, err := LoadSuiteHCL(path)
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	if len(got.Tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(got.Tasks))
	}
	if got.Tasks[0].Judge.Require != "all" {
		t.Fatalf("Require = %q, want %q", got.Tasks[0].Judge.Require, "all")
	}
}

// TestLoadSuiteHCL_SuiteBlockMissingLabel pins the diagnostic surfaced
// by rejectUnsupportedTopLevel when `suite` is written without an id
// label. This is distinct from the `suite ""` empty-label path and
// fires earlier (PartialContent reports a label arity mismatch).
func TestLoadSuiteHCL_SuiteBlockMissingLabel(t *testing.T) {
	src := "suite {\n  task \"t\" { judge { type = \"file-exists\"; paths = [\"a.txt\"] } }\n}\n"
	path := writeTemp(t, "no-label.hcl", src)
	_, err := LoadSuiteHCL(path)
	if err == nil {
		t.Fatal("expected error for suite block with missing label")
	}
	if err.Error() == "" {
		t.Fatal("expected non-empty error message")
	}
}

// TestLoadSuiteHCL_RunConfigFile asserts that a suite-level
// `run_config_file` attribute populates EvalSuite.RunConfig with the
// path as the File field, and leaves Inline nil. The runner (chunk B)
// is responsible for resolving the path; the loader stays purely
// syntactic.
func TestLoadSuiteHCL_RunConfigFile(t *testing.T) {
	src := `
suite "s" {
  run_config_file = "configs/base.json"
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
	path := writeTemp(t, "run-config-file.hcl", src)
	got, err := LoadSuiteHCL(path)
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	if got.RunConfig == nil {
		t.Fatal("expected suite RunConfig to be populated")
	}
	// The loader resolves a relative `run_config_file` against the suite
	// file's directory so the runner can open it without further plumbing.
	wantFile := filepath.Join(filepath.Dir(path), "configs/base.json")
	if got.RunConfig.File != wantFile {
		t.Errorf("RunConfig.File = %q, want %q", got.RunConfig.File, wantFile)
	}
	if got.RunConfig.Inline != nil {
		t.Errorf("RunConfig.Inline = %#v, want nil", got.RunConfig.Inline)
	}
	if got.Tasks[0].RunConfigOverrides != nil {
		t.Errorf("Tasks[0].RunConfigOverrides = %#v, want nil", got.Tasks[0].RunConfigOverrides)
	}
}

// TestLoadSuiteHCL_RunConfigFileAbsolutePreserved asserts that an
// absolute path in `run_config_file` is preserved verbatim by the
// loader rather than being re-rooted under the suite's directory.
func TestLoadSuiteHCL_RunConfigFileAbsolutePreserved(t *testing.T) {
	absPath := "/absolute/path/to/configs/base.json"
	src := fmt.Sprintf(`
suite "s" {
  run_config_file = %q
  task "t1" {
    mode   = "execution"
    prompt = "p"
    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
`, absPath)
	path := writeTemp(t, "run-config-file-abs.hcl", src)
	got, err := LoadSuiteHCL(path)
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	if got.RunConfig == nil || got.RunConfig.File != absPath {
		t.Errorf("RunConfig.File = %q, want absolute path %q (unchanged)",
			got.RunConfig.File, absPath)
	}
}

// TestLoadSuiteHCL_RunConfigInlineBlock asserts that an inline
// suite-level `run_config { ... }` block decodes into
// EvalSuite.RunConfig.Inline with the field set the author specified.
// File is left empty because the inline path is the one that was
// taken.
func TestLoadSuiteHCL_RunConfigInlineBlock(t *testing.T) {
	src := `
suite "s" {
  run_config {
    max_turns = 10

    provider {
      type        = "openai-responses"
      api_key_ref = "secret://OPENAI_KEY"
      base_url    = "https://example/v1"
    }

    model_router {
      type     = "static"
      provider = "openai-responses"
      model    = "gpt-5.4-nano"
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
	path := writeTemp(t, "run-config-inline.hcl", src)
	got, err := LoadSuiteHCL(path)
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	if got.RunConfig == nil || got.RunConfig.Inline == nil {
		t.Fatalf("expected inline run-config, got %#v", got.RunConfig)
	}
	if got.RunConfig.File != "" {
		t.Errorf("RunConfig.File = %q, want empty", got.RunConfig.File)
	}
	in := got.RunConfig.Inline
	// Mode is not surfaced in the HCL run_config grammar; the runner
	// reads mode from the task-level attribute. The inline carrier's
	// Mode field stays at its zero value regardless of authoring.
	if in.Mode != "" {
		t.Errorf("Mode = %q, want empty (mode not surfaced in run_config block)", in.Mode)
	}
	if in.MaxTurns == nil || *in.MaxTurns != 10 {
		t.Errorf("MaxTurns = %v, want *10", in.MaxTurns)
	}
	if in.Provider == nil {
		t.Fatal("expected Provider to be set")
	}
	if in.Provider.Type != "openai-responses" {
		t.Errorf("Provider.Type = %q, want %q", in.Provider.Type, "openai-responses")
	}
	if in.Provider.APIKeyRef != "secret://OPENAI_KEY" {
		t.Errorf("Provider.APIKeyRef = %q, want %q", in.Provider.APIKeyRef, "secret://OPENAI_KEY")
	}
	if in.Provider.BaseURL != "https://example/v1" {
		t.Errorf("Provider.BaseURL = %q, want %q", in.Provider.BaseURL, "https://example/v1")
	}
	if in.ModelRouter == nil {
		t.Fatal("expected ModelRouter to be set")
	}
	if in.ModelRouter.Type != "static" || in.ModelRouter.Model != "gpt-5.4-nano" {
		t.Errorf("ModelRouter = %#v, want type=static model=gpt-5.4-nano", in.ModelRouter)
	}
}

// TestLoadSuiteHCL_RunConfigOverridesPerTask asserts the per-task
// override block decodes into EvalTask.RunConfigOverrides with the
// fields the author set, and is independent of the suite-level
// baseline (the merging is the runner's responsibility in chunk B).
func TestLoadSuiteHCL_RunConfigOverridesPerTask(t *testing.T) {
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
	if got.RunConfig != nil {
		t.Errorf("suite RunConfig = %#v, want nil", got.RunConfig)
	}
	tov := got.Tasks[0].RunConfigOverrides
	if tov == nil {
		t.Fatal("expected per-task RunConfigOverrides to be set")
	}
	if tov.MaxTurns == nil || *tov.MaxTurns != 4 {
		t.Errorf("MaxTurns = %v, want *4", tov.MaxTurns)
	}
	if tov.Provider == nil {
		t.Fatal("expected Provider override")
	}
	if tov.Provider.Type != "anthropic" {
		t.Errorf("Provider.Type = %q, want %q", tov.Provider.Type, "anthropic")
	}
	if tov.Provider.APIKeyRef != "secret://ANTHROPIC_KEY" {
		t.Errorf("Provider.APIKeyRef = %q, want %q", tov.Provider.APIKeyRef, "secret://ANTHROPIC_KEY")
	}
}

// TestLoadSuiteHCL_RunConfigSourcesMutuallyExclusive pins the contract
// that a suite cannot set both `run_config_file` and a `run_config {}`
// block. The error must name both sources so authors can find the
// conflict without re-reading the docs.
func TestLoadSuiteHCL_RunConfigSourcesMutuallyExclusive(t *testing.T) {
	src := `
suite "s" {
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
	path := writeTemp(t, "both-sources.hcl", src)
	_, err := LoadSuiteHCL(path)
	if err == nil {
		t.Fatal("expected error for mutually exclusive run-config sources")
	}
	for _, frag := range []string{"run_config_file", "run_config block", "mutually exclusive"} {
		if !strings.Contains(err.Error(), frag) {
			t.Fatalf("error = %q, want it to mention %q", err.Error(), frag)
		}
	}
}

// TestLoadSuiteHCL_RawAPIKeyRefRejected pins the parse-time invariant
// that provider.api_key_ref must use the secret:// scheme. A raw
// credential authored inline would otherwise sit on disk in the
// merged run-config the runner hands the harness (at 0600, but
// silently) and Redact() would hide the misconfiguration from the
// audit artifact. The check fires on both suite-level inline
// run_config blocks and per-task run_config_overrides blocks.
func TestLoadSuiteHCL_RawAPIKeyRefRejected(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "suite-level inline run_config",
			src: `
suite "s" {
  run_config {
    provider {
      type        = "anthropic"
      api_key_ref = "sk-raw-bad-key"
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
`,
		},
		{
			name: "per-task run_config_overrides",
			src: `
suite "s" {
  task "t1" {
    mode   = "execution"
    prompt = "p"
    run_config_overrides {
      provider {
        type        = "anthropic"
        api_key_ref = "literal-key-value"
      }
    }
    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTemp(t, "raw-key.hcl", tc.src)
			_, err := LoadSuiteHCL(path)
			if err == nil {
				t.Fatal("expected error for raw api_key_ref")
			}
			for _, frag := range []string{"raw credentials", "secret://"} {
				if !strings.Contains(err.Error(), frag) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), frag)
				}
			}
		})
	}
}

// TestLoadSuiteHCL_SecretSchemedAPIKeyRefAccepted is the positive twin
// of TestLoadSuiteHCL_RawAPIKeyRefRejected: a properly schemed
// reference still parses.
func TestLoadSuiteHCL_SecretSchemedAPIKeyRefAccepted(t *testing.T) {
	src := `
suite "s" {
  run_config {
    provider {
      type        = "anthropic"
      api_key_ref = "secret://ANTHROPIC_KEY"
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
	path := writeTemp(t, "schemed-key.hcl", src)
	got, err := LoadSuiteHCL(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.RunConfig == nil || got.RunConfig.Inline == nil || got.RunConfig.Inline.Provider == nil {
		t.Fatalf("expected populated provider, got %#v", got.RunConfig)
	}
	if got.RunConfig.Inline.Provider.APIKeyRef != "secret://ANTHROPIC_KEY" {
		t.Errorf("APIKeyRef = %q, want %q",
			got.RunConfig.Inline.Provider.APIKeyRef, "secret://ANTHROPIC_KEY")
	}
}

// TestLoadSuiteHCL_BackwardsCompatNoRunConfig confirms that a suite
// authored before this issue (no run_config_* fields anywhere) decodes
// with RunConfig == nil at the suite level and on every task. This is
// the contract that lets existing suites continue to work unchanged.
func TestLoadSuiteHCL_BackwardsCompatNoRunConfig(t *testing.T) {
	hclPath := filepath.Join("testdata", "sample.hcl")
	got, err := LoadSuiteHCL(hclPath)
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	if got.RunConfig != nil {
		t.Errorf("RunConfig = %#v, want nil for legacy suite", got.RunConfig)
	}
	for _, task := range got.Tasks {
		if task.RunConfigOverrides != nil {
			t.Errorf("task %q RunConfigOverrides = %#v, want nil", task.ID, task.RunConfigOverrides)
		}
	}
}

// TestLoadSuiteHCL_ExistingSuitesStillParse exercises the production
// suites checked in under eval/suites/ to confirm the grammar
// extension hasn't regressed their loadability. The openai-responses
// regression suite pins its own provider posture inline (issue #177),
// so its RunConfig is expected to be populated; guardrail.hcl remains
// on the legacy path.
func TestLoadSuiteHCL_ExistingSuitesStillParse(t *testing.T) {
	cases := []struct {
		path           string
		wantRunConfig  bool
		wantProvider   string
		wantModel      string
	}{
		{
			path:          "../suites/guardrail.hcl",
			wantRunConfig: false,
		},
		{
			path:          "../suites/openai-responses-empty-tool-output.hcl",
			wantRunConfig: true,
			wantProvider:  "openai-responses",
			wantModel:     "gpt-4o-mini",
		},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			if _, err := os.Stat(c.path); err != nil {
				t.Skipf("suite %s not present: %v", c.path, err)
			}
			suite, err := LoadSuiteHCL(c.path)
			if err != nil {
				t.Fatalf("LoadSuiteHCL(%s): %v", c.path, err)
			}
			if suite.ID == "" {
				t.Fatalf("LoadSuiteHCL(%s) returned suite with empty ID", c.path)
			}
			switch {
			case !c.wantRunConfig && suite.RunConfig != nil:
				t.Errorf("suite %s RunConfig = %#v, want nil (legacy)", c.path, suite.RunConfig)
			case c.wantRunConfig && suite.RunConfig == nil:
				t.Fatalf("suite %s RunConfig = nil, want pinned baseline", c.path)
			case c.wantRunConfig:
				if suite.RunConfig.Inline == nil {
					t.Fatalf("suite %s RunConfig.Inline = nil, want inline baseline", c.path)
				}
				if got := suite.RunConfig.Inline.Provider; got == nil || got.Type != c.wantProvider {
					t.Errorf("suite %s provider = %#v, want type %q", c.path, got, c.wantProvider)
				}
				if got := suite.RunConfig.Inline.ModelRouter; got == nil || got.Model != c.wantModel {
					t.Errorf("suite %s model = %#v, want model %q", c.path, got, c.wantModel)
				}
			}
		})
	}
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
