package spec

import (
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

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
