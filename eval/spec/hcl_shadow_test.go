package spec

import (
	"strings"
	"testing"
)

func TestLoadSuiteHCL_ShadowSubJudge(t *testing.T) {
	src := `
suite "s" {
  task "t1" {
    prompt = "p"
    judge {
      type = "composite"
      judge {
        type  = "file-exists"
        paths = ["a.txt"]
      }
      judge {
        type     = "diff-review"
        criteria = "the change adds a test"
        shadow   = true
      }
    }
  }
}
`
	suite, err := LoadSuiteHCL(writeTemp(t, "shadow.hcl", src))
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	subs := suite.Tasks[0].Judge.Judges
	if len(subs) != 2 || subs[0].Shadow || !subs[1].Shadow {
		t.Fatalf("sub-judges = %+v, want the second one marked shadow", subs)
	}
}

func TestLoadSuiteHCL_ShadowRulesRejected(t *testing.T) {
	cases := map[string]string{
		"top-level shadow": `
    judge {
      type   = "file-exists"
      paths  = ["a.txt"]
      shadow = true
    }`,
		"composite of shadows": `
    judge {
      type = "composite"
      judge {
        type   = "file-exists"
        paths  = ["a.txt"]
        shadow = true
      }
    }`,
	}
	wants := map[string]string{
		"top-level shadow":     "only valid on a composite sub-judge",
		"composite of shadows": "at least one sub-judge that is not a shadow",
	}
	for name, judge := range cases {
		t.Run(name, func(t *testing.T) {
			src := "suite \"s\" {\n  task \"t1\" {\n    prompt = \"p\"\n" + judge + "\n  }\n}\n"
			_, err := LoadSuiteHCL(writeTemp(t, "shadow.hcl", src))
			if err == nil || !strings.Contains(err.Error(), wants[name]) || !strings.Contains(err.Error(), `task "t1"`) {
				t.Fatalf("LoadSuiteHCL error = %v, want a task-scoped error containing %q", err, wants[name])
			}
		})
	}
}
