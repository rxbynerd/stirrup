package spec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// TestLoadSuiteHCL_RoundTripWithJSON parses an HCL fixture and asserts
// it deep-equals a JSON fixture with the same logical content. This is
// the core guarantee: HCL is just an authoring surface that produces
// the same types.EvalSuite the JSON loader produces.
func TestLoadSuiteHCL_RoundTripWithJSON(t *testing.T) {
	hclPath := filepath.Join("testdata", "sample.hcl")
	jsonPath := filepath.Join("testdata", "sample.json")

	got, err := LoadSuiteHCL(hclPath)
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}

	var want types.EvalSuite
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read JSON fixture: %v", err)
	}
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatalf("unmarshal JSON fixture: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("HCL/JSON mismatch\n got:  %#v\n want: %#v", got, want)
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
// composite require value. Each error message must mention the offending
// field so authors can find the problem.
func TestLoadSuiteHCL_ValidationErrors(t *testing.T) {
	cases := []struct {
		name        string
		src         string
		wantErrFrag string
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
			wantErrFrag: "suite ID is required",
		},
		{
			name: "no tasks",
			src: `
suite "empty" {
  description = "no tasks"
}`,
			wantErrFrag: "must contain at least one task",
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
			wantErrFrag: "is missing an id label",
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
			wantErrFrag: "invalid judge.type",
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
			wantErrFrag: "judge.require",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTemp(t, "case.hcl", tc.src)
			_, err := LoadSuiteHCL(path)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrFrag)
			}
			if !strings.Contains(err.Error(), tc.wantErrFrag) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErrFrag)
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

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
