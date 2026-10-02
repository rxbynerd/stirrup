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
	cases["top-level decision judge"] = `
    judge {
      type     = "diff-review"
      criteria = "the change adds a test"
      llm {
        provider = "decision"
        model    = "jev-latest"
      }
    }`
	cases["deciding decision sub-judge"] = `
    judge {
      type = "composite"
      judge {
        type  = "file-exists"
        paths = ["a.txt"]
      }
      judge {
        type     = "diff-review"
        criteria = "the change adds a test"
        llm {
          provider = "decision"
          model    = "jev-latest"
        }
      }
    }`
	cases["shadow test-command"] = `
    judge {
      type = "composite"
      judge {
        type  = "file-exists"
        paths = ["a.txt"]
      }
      judge {
        type    = "test-command"
        command = "make fmt"
        shadow  = true
      }
    }`
	wants := map[string]string{
		"shadow test-command":         `sub-judge 2: a "test-command" judge cannot be a shadow`,
		"top-level shadow":            "only valid on a composite sub-judge",
		"composite of shadows":        "at least one sub-judge that is not a shadow",
		"top-level decision judge":    `provider "decision" is usable only on a shadow judge`,
		"deciding decision sub-judge": `sub-judge 2: llm provider "decision"`,
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

func TestLoadSuiteHCL_ShadowDecisionJudge(t *testing.T) {
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
        llm {
          provider = "decision"
          model    = "jev-latest"
        }
      }
    }
  }
}
`
	suite, err := LoadSuiteHCL(writeTemp(t, "decision.hcl", src))
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	sub := suite.Tasks[0].Judge.Judges[1]
	if !sub.Shadow || sub.LLM == nil || sub.LLM.Provider != "decision" {
		t.Fatalf("sub-judge = %+v, want a shadow decision judge", sub)
	}
}
