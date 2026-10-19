package spec

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadSuiteHCL_AcceptsShadowSubJudge(t *testing.T) {
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
	require.NoError(t, err)
	subs := suite.Tasks[0].Judge.Judges
	require.Len(t, subs, 2, "sub-judges = %+v", subs)
	require.False(t, subs[0].Shadow, "first sub-judge should not be a shadow")
	require.True(t, subs[1].Shadow, "second sub-judge should be a shadow")
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
			src := "suite \"s\" {\n  task \"t1\" {\n    prompt = \"p\"\n" + tc.judge + "\n  }\n}\n"
			_, err := LoadSuiteHCL(writeTemp(t, "shadow.hcl", src))
			require.ErrorContains(t, err, tc.wantErr)
			require.ErrorContains(t, err, `task "t1"`)
		})
	}
}

func TestLoadSuiteHCL_AcceptsShadowDecisionJudge(t *testing.T) {
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
	require.NoError(t, err)
	sub := suite.Tasks[0].Judge.Judges[1]
	require.True(t, sub.Shadow, "sub-judge = %+v", sub)
	require.NotNil(t, sub.LLM, "sub-judge = %+v", sub)
	require.Equal(t, "decision", sub.LLM.Provider)
}
