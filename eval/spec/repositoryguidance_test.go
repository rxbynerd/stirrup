package spec

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func TestLoadSuiteHCL_RepositoryGuidanceBlock(t *testing.T) {
	src := `
suite "s" {
  run_config {
    mode = "execution"

    provider {
      type        = "anthropic"
      api_key_ref = "secret://ANTHROPIC_KEY"
    }

    repository_guidance {
      files         = ["AGENTS.md", "CLAUDE.md"]
      max_bytes     = 2048
      pinned_sha256 = { "AGENTS.md" = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" }
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
	got, err := LoadSuiteHCL(writeTemp(t, "runcfg-repository-guidance.hcl", src))
	require.NoError(t, err)

	want := &types.RepositoryGuidanceConfig{
		Files:        []string{"AGENTS.md", "CLAUDE.md"},
		MaxBytes:     2048,
		PinnedSHA256: map[string]string{"AGENTS.md": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"},
	}
	require.NotNil(t, got.RunConfig)
	require.Equal(t, want, got.RunConfig.RepositoryGuidance)
}

func TestLoadSuiteHCL_GuidanceSuiteParses(t *testing.T) {
	got, err := LoadSuiteHCL("../suites/guidance.hcl")
	require.NoError(t, err)

	require.Equal(t, "repository-guidance", got.ID)
	require.Len(t, got.Tasks, 2)

	for _, task := range got.Tasks {
		t.Run(task.ID, func(t *testing.T) {
			require.Contains(t, task.Files, "AGENTS.md", "task must seed an AGENTS.md for the guidance loader to read")
		})
	}
}
