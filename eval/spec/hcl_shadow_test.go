package spec

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadSuiteHCL_AcceptsShadowSubJudge(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	subs := suite.Tasks[0].Judge.Judges
	must.Len(subs, 2, "sub-judges = %+v", subs)
	must.False(subs[0].Shadow, "first sub-judge should not be a shadow")
	must.True(subs[1].Shadow, "second sub-judge should be a shadow")
}

func TestLoadSuiteHCL_RejectsInvalidShadowJudges(t *testing.T) {
	cases := []struct {
		name    string
		wantErr string
		judge   string
	}{
		{
			name:    "top-level shadow",
			wantErr: "only valid on a composite sub-judge",
			judge: `
    judge {
      type   = "file-exists"
      paths  = ["a.txt"]
      shadow = true
    }`,
		},
		{
			name:    "composite of shadows",
			wantErr: "at least one sub-judge that is not a shadow",
			judge: `
    judge {
      type = "composite"
      judge {
        type   = "file-exists"
        paths  = ["a.txt"]
        shadow = true
      }
    }`,
		},
		{
			name:    "top-level decision judge",
			wantErr: `provider "decision" is usable only on a shadow judge`,
			judge: `
    judge {
      type     = "diff-review"
      criteria = "the change adds a test"
      llm {
        provider = "decision"
        model    = "jev-latest"
      }
    }`,
		},
		{
			name:    "deciding decision sub-judge",
			wantErr: `sub-judge 2: llm provider "decision"`,
			judge: `
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
    }`,
		},
		{
			name:    "shadow test-command",
			wantErr: `sub-judge 2: a "test-command" judge cannot be a shadow`,
			judge: `
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
    }`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			src := "suite \"s\" {\n  task \"t1\" {\n    prompt = \"p\"\n" + tc.judge + "\n  }\n}\n"
			_, err := LoadSuiteHCL(writeTemp(t, "shadow.hcl", src))
			must.ErrorContains(err, tc.wantErr)
			must.ErrorContains(err, `task "t1"`)
		})
	}
}

func TestLoadSuiteHCL_AcceptsShadowDecisionJudge(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	sub := suite.Tasks[0].Judge.Judges[1]
	must.True(sub.Shadow, "sub-judge = %+v", sub)
	must.NotNil(sub.LLM, "sub-judge = %+v", sub)
	must.Equal("decision", sub.LLM.Provider)
}
