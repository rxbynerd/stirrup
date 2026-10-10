package spec

import (
	"reflect"
	"testing"

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
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	want := &types.RepositoryGuidanceConfig{
		Files:        []string{"AGENTS.md", "CLAUDE.md"},
		MaxBytes:     2048,
		PinnedSHA256: map[string]string{"AGENTS.md": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"},
	}
	if got.RunConfig == nil || !reflect.DeepEqual(got.RunConfig.RepositoryGuidance, want) {
		t.Fatalf("RepositoryGuidance mismatch\n got:  %#v\n want: %#v", got.RunConfig, want)
	}
}

func TestLoadSuiteHCL_GuidanceSuiteParses(t *testing.T) {
	got, err := LoadSuiteHCL("../suites/guidance.hcl")
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	if got.ID != "repository-guidance" || len(got.Tasks) != 2 {
		t.Fatalf("suite = %q with %d tasks, want repository-guidance with 2", got.ID, len(got.Tasks))
	}
	for _, task := range got.Tasks {
		if _, ok := task.Files["AGENTS.md"]; !ok {
			t.Errorf("task %q must seed an AGENTS.md for the guidance loader to read", task.ID)
		}
	}
}
