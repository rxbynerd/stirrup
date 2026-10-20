package spec

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// TestLoadSuiteHCL_DecodesFullSuite parses an HCL fixture and asserts it
// deep-equals a hand-rolled types.EvalSuite literal with the same
// logical content.
func TestLoadSuiteHCL_DecodesFullSuite(t *testing.T) {
	must := require.New(t)
	hclPath := filepath.Join("testdata", "sample.hcl")

	got, err := LoadSuiteHCL(hclPath)
	must.NoError(err)

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

	must.Equal(want, got)
}

// TestLoadSuiteHCL_CommentsTolerated confirms that all three HCL comment
// styles parse without affecting the resulting suite.
func TestLoadSuiteHCL_CommentsTolerated(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	must.Equal("with-comments", got.ID)
	must.Len(got.Tasks, 1)
	must.Equal("hello", got.Tasks[0].Prompt)
}

// TestLoadSuiteHCL_HeredocPreserved asserts that a `<<-EOT` heredoc
// preserves its multi-line content (with the trailing newline) byte for
// byte. The leading-tab stripping rule is part of HCL semantics; we
// just want the documented behaviour to be predictable.
func TestLoadSuiteHCL_HeredocPreserved(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	must.Equal("first\nsecond\n", got.Tasks[0].Prompt)
}

// TestLoadSuiteHCL_DecodesSeedFilesByPath asserts that labelled `file`
// blocks decode into EvalTask.Files keyed by the (cleaned)
// workspace-relative path, with the heredoc content preserved and nested
// paths supported.
func TestLoadSuiteHCL_DecodesSeedFilesByPath(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	src := "suite \"seed\" {\n" +
		"  task \"t1\" {\n" +
		"    mode   = \"execution\"\n" +
		"    prompt = \"p\"\n" +
		"    file \"README.md\" {\n" +
		"      content = <<-EOT\n" +
		"        # Title\n" +
		"        body\n" +
		"      EOT\n" +
		"    }\n" +
		"    file \"docs/guide.txt\" {\n" +
		"      content = \"hello\"\n" +
		"    }\n" +
		"    judge {\n" +
		"      type    = \"test-command\"\n" +
		"      command = \"true\"\n" +
		"    }\n" +
		"  }\n" +
		"}\n"
	path := writeTemp(t, "seed.hcl", src)
	got, err := LoadSuiteHCL(path)
	must.NoError(err)
	files := got.Tasks[0].Files
	must.Len(files, 2)
	check.Equal("# Title\nbody\n", files["README.md"])
	check.Equal("hello", files["docs/guide.txt"])
}

// TestLoadSuiteHCL_CompositeChildrenOrdered asserts that composite
// judges with two `judge` child blocks decode to a Judges slice of
// length 2 in source order.
func TestLoadSuiteHCL_CompositeChildrenOrdered(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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
	must.NoError(err)
	j := got.Tasks[0].Judge
	must.Equal("composite", j.Type)
	must.Equal("all", j.Require)
	must.Len(j.Judges, 2)
	check.Equal("file-exists", j.Judges[0].Type)
	check.Equal("file-contains", j.Judges[1].Type)
}

// TestLoadSuiteHCL_DecodesToolTraceJudge parses a tool-trace judge with a
// sequence, two per-tool call expectations, and forbid_unknown, asserting
// the block decodes into types.ToolTraceCriteria with the call labels and
// bounds preserved.
func TestLoadSuiteHCL_DecodesToolTraceJudge(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	src := `
suite "tt" {
  task "t1" {
    mode   = "execution"
    prompt = "p"
    judge {
      type = "tool-trace"
      tool_trace {
        sequence       = ["read_file", "edit_file"]
        forbid_unknown = true
        call "edit_file" {
          min_calls     = 1
          all_succeeded = true
        }
        call "write_file" {
          max_calls = 0
        }
      }
    }
  }
}
`
	path := writeTemp(t, "tooltrace.hcl", src)
	got, err := LoadSuiteHCL(path)
	must.NoError(err)
	j := got.Tasks[0].Judge
	must.Equal("tool-trace", j.Type)
	must.NotNil(j.ToolTrace)
	check.Equal([]string{"read_file", "edit_file"}, j.ToolTrace.Sequence)
	check.True(j.ToolTrace.ForbidUnknown)
	must.Len(j.ToolTrace.Calls, 2)

	editCall := j.ToolTrace.Calls[0]
	check.Equal("edit_file", editCall.Name)
	check.Equal(1, editCall.MinCalls)
	check.True(editCall.AllSucceeded)

	writeCall := j.ToolTrace.Calls[1]
	check.Equal("write_file", writeCall.Name)
	must.NotNil(writeCall.MaxCalls)
	check.Equal(0, *writeCall.MaxCalls)
}

// TestLoadSuiteHCL_ToolTraceRejectedOnWrongType asserts a tool_trace block
// under a non-tool-trace judge type is a parse error, mirroring the nested
// judge-block guard.
func TestLoadSuiteHCL_ToolTraceRejectedOnWrongType(t *testing.T) {
	must := require.New(t)
	src := `
suite "tt" {
  task "t1" {
    mode   = "execution"
    prompt = "p"
    judge {
      type = "file-exists"
      paths = ["a.txt"]
      tool_trace {
        sequence = ["read_file"]
      }
    }
  }
}
`
	path := writeTemp(t, "tooltrace-bad.hcl", src)
	_, err := LoadSuiteHCL(path)
	must.Error(err)
}

// TestLoadSuiteHCL_ToolTraceRequiresBlock asserts a tool-trace judge with
// no tool_trace block is rejected at parse time.
func TestLoadSuiteHCL_ToolTraceRequiresBlock(t *testing.T) {
	must := require.New(t)
	src := `
suite "tt" {
  task "t1" {
    mode   = "execution"
    prompt = "p"
    judge {
      type = "tool-trace"
    }
  }
}
`
	path := writeTemp(t, "tooltrace-empty.hcl", src)
	_, err := LoadSuiteHCL(path)
	must.Error(err)
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
		{
			name: "seed file with absolute path",
			src: `
suite "s" {
  task "t1" {
    mode   = "execution"
    prompt = "p"
    file "/etc/passwd" { content = "x" }
    judge {
      type    = "test-command"
      command = "true"
    }
  }
}`,
			wantErrFrags: []string{"must be relative"},
		},
		{
			name: "seed file escaping via traversal",
			src: `
suite "s" {
  task "t1" {
    mode   = "execution"
    prompt = "p"
    file "../escape.txt" { content = "x" }
    judge {
      type    = "test-command"
      command = "true"
    }
  }
}`,
			wantErrFrags: []string{"escapes the workspace"},
		},
		{
			name: "duplicate seed file path",
			src: `
suite "s" {
  task "t1" {
    mode   = "execution"
    prompt = "p"
    file "a.txt" { content = "one" }
    file "a.txt" { content = "two" }
    judge {
      type    = "test-command"
      command = "true"
    }
  }
}`,
			wantErrFrags: []string{"duplicate file path"},
		},
		{
			name: "duplicate tool_trace call block",
			src: `
suite "s" {
  task "t1" {
    mode   = "execution"
    prompt = "p"
    judge {
      type = "tool-trace"
      tool_trace {
        call "edit_file" { min_calls = 1 }
        call "edit_file" { max_calls = 2 }
      }
    }
  }
}`,
			wantErrFrags: []string{"duplicate call block", "edit_file"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			path := writeTemp(t, "case.hcl", tc.src)
			_, err := LoadSuiteHCL(path)
			must.Error(err)
			for _, frag := range tc.wantErrFrags {
				must.ErrorContains(err, frag)
			}
		})
	}
}

// TestLoadSuiteHCL_UnknownTopLevelBlock rejects a `variable` (or any
// other non-`suite`) top-level block with a clear forward-looking
// message. The grammar may grow these later; until then they are off
// limits.
func TestLoadSuiteHCL_UnknownTopLevelBlock(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(err, "unsupported top-level block")
	must.ErrorContains(err, "variable")
}

// TestLoadSuiteHCL_UnknownTopLevelAttributesDeterministic asserts that
// when multiple stray top-level attributes are present, the reported
// error names them in a deterministic (sorted) order. Map iteration
// order would otherwise make future tests flaky.
func TestLoadSuiteHCL_UnknownTopLevelAttributesDeterministic(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(err, `unsupported top-level attribute "alpha"`)
}

// TestLoadSuiteHCL_RejectsOtherTopLevelBlockTypes rejects top-level block
// types beyond the named ones (for example `output`) with the same
// unsupported-block error.
func TestLoadSuiteHCL_RejectsOtherTopLevelBlockTypes(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(err, "unsupported top-level block")
	must.ErrorContains(err, "output")
}

// TestLoadSuiteHCL_EmptyFile fails clearly rather than panicking when
// handed a zero-byte source.
func TestLoadSuiteHCL_EmptyFile(t *testing.T) {
	must := require.New(t)
	path := writeTemp(t, "empty.hcl", "")
	_, err := LoadSuiteHCL(path)
	must.ErrorContains(err, "no suite block found")
}

// TestLoadSuiteHCL_RejectsTwoSuiteBlocks fails clearly: exactly one suite
// per file is the contract.
func TestLoadSuiteHCL_RejectsTwoSuiteBlocks(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(err, "exactly one suite block")
}

// TestLoadSuiteHCL_RejectsMalformedHCL returns a clear parser error rather
// than panicking when the input isn't valid HCL.
func TestLoadSuiteHCL_RejectsMalformedHCL(t *testing.T) {
	must := require.New(t)
	path := writeTemp(t, "broken.hcl", `suite "x" { not even close to valid {{`)
	_, err := LoadSuiteHCL(path)
	must.Error(err)
}

// TestLoadSuiteHCL_ReportsMissingFile surfaces the underlying os error.
func TestLoadSuiteHCL_ReportsMissingFile(t *testing.T) {
	must := require.New(t)
	_, err := LoadSuiteHCL("/nonexistent/missing.hcl")
	must.Error(err)
}

// TestLoadSuiteHCL_TaskMissingJudgeBlock pins the gohcl.DecodeBody
// error path: the most common structural authoring mistake (forgetting
// the `judge {}` sub-block) must surface a clear diagnostic. The error
// originates inside hcl/v2 so we only assert that "judge" appears in
// the message — the wording is library-controlled.
func TestLoadSuiteHCL_TaskMissingJudgeBlock(t *testing.T) {
	must := require.New(t)
	src := `suite "s" { task "t" { prompt = "x" } }`
	path := writeTemp(t, "no-judge.hcl", src)
	_, err := LoadSuiteHCL(path)
	must.ErrorContains(err, "judge")
}

// TestLoadSuiteHCL_RejectsNestedJudgeOnNonComposite pins the contract that
// nesting `judge` blocks inside a non-composite parent is rejected
// loudly rather than silently dropped (gohcl decodes the field for
// every judge type).
func TestLoadSuiteHCL_RejectsNestedJudgeOnNonComposite(t *testing.T) {
	must := require.New(t)
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
	must.Error(err)
	must.ErrorContains(err, "does not support nested judge blocks")
}

// TestLoadSuiteHCL_RejectsEmptyCompositeJudge pins the contract that a
// composite block with zero children is rejected. The runtime evaluator
// would otherwise treat `all` of zero as a vacuous pass, producing
// misleading green CI for what is almost always a forgotten typo.
func TestLoadSuiteHCL_RejectsEmptyCompositeJudge(t *testing.T) {
	must := require.New(t)
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
	must.Error(err)
	must.ErrorContains(err, "at least one nested judge block")
}

// TestLoadSuiteHCL_CompositeRequireDefaultsToAll pins the contract that
// omitting `require` on a composite judge defaults to "all". A change
// of the default to "" or "any" must surface here.
func TestLoadSuiteHCL_CompositeRequireDefaultsToAll(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	must.Len(got.Tasks, 1)
	must.Equal("all", got.Tasks[0].Judge.Require)
}

// TestLoadSuiteHCL_RejectsSuiteBlockWithoutLabel pins the diagnostic surfaced
// by rejectUnsupportedTopLevel when `suite` is written without an id
// label. This is distinct from the `suite ""` empty-label path and
// fires earlier (PartialContent reports a label arity mismatch).
func TestLoadSuiteHCL_RejectsSuiteBlockWithoutLabel(t *testing.T) {
	must := require.New(t)
	src := "suite {\n  task \"t\" { judge { type = \"file-exists\"; paths = [\"a.txt\"] } }\n}\n"
	path := writeTemp(t, "no-label.hcl", src)
	_, err := LoadSuiteHCL(path)
	must.Error(err)
	must.NotEmpty(err.Error(), "error message for a labelless suite block")
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	must := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	must.NoError(os.WriteFile(path, []byte(content), 0o644))
	return path
}
