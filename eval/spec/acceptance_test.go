package spec

import (
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

const acceptanceSuitePath = "../suites/acceptance.hcl"

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func loadAcceptanceSuite(t *testing.T) types.EvalSuite {
	t.Helper()
	must := require.New(t)
	suite, err := LoadSuiteHCL(acceptanceSuitePath)
	must.NoError(err, "LoadSuiteHCL(%s)", acceptanceSuitePath)
	return suite
}

// TestSuites_PinClonedReposToAFullCommitSHA scans every shipped suite: a task
// that clones a repository must name a full 40-character commit SHA, never a
// branch or tag that can move.
func TestSuites_PinClonedReposToAFullCommitSHA(t *testing.T) {
	must := require.New(t)
	paths, err := filepath.Glob("../suites/*.hcl")
	must.NoError(err, "globbing suites")
	must.NotEmpty(paths, "no suites found under ../suites")
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			must := require.New(t)
			suite, err := LoadSuiteHCL(path)
			must.NoError(err, "LoadSuiteHCL(%s)", path)
			for _, task := range suite.Tasks {
				if task.Repo == "" {
					continue
				}
				t.Run(task.ID, func(t *testing.T) {
					must := require.New(t)
					must.Regexp(fullSHA, task.Ref, "task clones %s, which must name a full 40-character commit SHA", task.Repo)
				})
			}
		})
	}
}

func TestAcceptanceSuite_EveryTaskClonesAPinnedRepo(t *testing.T) {
	suite := loadAcceptanceSuite(t)
	t.Run("task count", func(t *testing.T) {
		must := require.New(t)
		n := len(suite.Tasks)
		must.True(n >= 8 && n <= 10, "suite has %d tasks, want 8 to 10", n)
	})
	for _, task := range suite.Tasks {
		t.Run(task.ID, func(t *testing.T) {
			must := require.New(t)
			must.True(strings.HasPrefix(task.Repo, "https://github.com/"), "repo %q, want a public https://github.com/ URL", task.Repo)
			must.Regexp(fullSHA, task.Ref, "ref %q, want a full 40-character commit SHA", task.Ref)
		})
	}
}

func TestAcceptanceSuite_JudgesAreDeterministic(t *testing.T) {
	suite := loadAcceptanceSuite(t)
	deterministic := map[string]bool{
		"test-command": true, "file-exists": true, "file-contains": true,
		"tool-trace": true, "composite": true,
	}
	var walk func(t *testing.T, j types.EvalJudge)
	walk = func(t *testing.T, j types.EvalJudge) {
		must := require.New(t)
		must.True(deterministic[j.Type], "judge type %q, want a deterministic judge", j.Type)
		must.False(j.Shadow, "judge must not be a shadow judge")
		for _, sub := range j.Judges {
			walk(t, sub)
		}
	}
	for _, task := range suite.Tasks {
		t.Run(task.ID, func(t *testing.T) {
			walk(t, task.Judge)
		})
	}
}

// TestAcceptanceSuite_PerTaskOverridesHaveABaseline guards the verifier task:
// the runner drops a task's run_config_overrides when the suite declares no
// baseline run_config, which would silently disable the failing verifier.
func TestAcceptanceSuite_PerTaskOverridesHaveABaseline(t *testing.T) {
	must := require.New(t)
	suite := loadAcceptanceSuite(t)
	hasBaseline := suite.RunConfig != nil || suite.RunConfigFile != ""
	var verifierTasks int
	for _, task := range suite.Tasks {
		if task.RunConfigOverrides == nil {
			continue
		}
		t.Run(task.ID, func(t *testing.T) {
			must := require.New(t)
			must.True(hasBaseline, "task sets run_config_overrides but the suite has no baseline")
		})
		if v := task.RunConfigOverrides.Verifier; v != nil && v.Type == "test-runner" && v.Command != "" {
			verifierTasks++
		}
	}
	must.NotZero(verifierTasks, "no task overrides the verifier with a test-runner command")
}

// TestAcceptanceSuite_JudgeHeredocsTerminate keeps the shell heredocs that
// write a hidden judge-owned test file well formed after HCL strips the
// indentation: an unterminated heredoc would fail the judge, not the task.
func TestAcceptanceSuite_JudgeHeredocsTerminate(t *testing.T) {
	must := require.New(t)
	suite := loadAcceptanceSuite(t)
	var seen int
	var walk func(t *testing.T, j types.EvalJudge)
	walk = func(t *testing.T, j types.EvalJudge) {
		must := require.New(t)
		if strings.Contains(j.Command, "<<'JUDGE_EOF'") {
			seen++
			must.Contains(j.Command, "\nJUDGE_EOF\n", "judge heredoc must terminate with JUDGE_EOF at column 0")
			must.Contains(j.Command, "trap 'rm -f zz_judge_test.go' EXIT", "judge must remove zz_judge_test.go on exit")
		}
		for _, sub := range j.Judges {
			walk(t, sub)
		}
	}
	for _, task := range suite.Tasks {
		t.Run(task.ID, func(t *testing.T) {
			walk(t, task.Judge)
		})
	}
	must.NotZero(seen, "no judge writes a hidden test file; the heredoc check has nothing to cover")
}

// TestAcceptanceSuite_PostEditChecksTargetOnlyTheirTasks keeps the
// suite-level post-edit checks well formed and scoped: each hidden-test
// heredoc terminates and is removed on exit, and each glob matches the
// two files its task creates and no file another task's prompt names.
func TestAcceptanceSuite_PostEditChecksTargetOnlyTheirTasks(t *testing.T) {
	must := require.New(t)
	suite := loadAcceptanceSuite(t)
	must.NotNil(suite.RunConfig, "the suite has no baseline run_config")
	targets := map[string][]string{
		"*percent*.go": {"percent.go", "example_percent_test.go"},
		"*lenient*.go": {"lenient.go", "example_lenient_test.go"},
	}
	checks := suite.RunConfig.Tools.PostEdit
	must.Len(checks, len(targets))
	for _, check := range checks {
		t.Run(check.Glob, func(t *testing.T) {
			must := require.New(t)
			files, ok := targets[check.Glob]
			must.True(ok, "unexpected post-edit glob %q", check.Glob)
			must.Contains(check.Command, "\nCHECK_EOF\n", "check heredoc must terminate with CHECK_EOF at column 0")
			must.Contains(check.Command, "trap 'rm -f zz_", "check must remove its hidden test file on exit")
			for _, f := range files {
				ok, err := path.Match(check.Glob, f)
				must.NoError(err)
				must.True(ok, "glob %q must match %s", check.Glob, f)
			}
			for _, task := range suite.Tasks {
				if strings.HasSuffix(task.ID, "-under-post-edit-check") {
					continue
				}
				for _, word := range strings.Fields(task.Prompt) {
					word = strings.Trim(word, "`.,:;()")
					if !strings.HasSuffix(word, ".go") {
						continue
					}
					ok, err := path.Match(check.Glob, path.Base(word))
					must.NoError(err)
					must.False(ok, "glob %q matches %s, named by task %s", check.Glob, word, task.ID)
				}
			}
		})
	}
}
