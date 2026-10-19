package spec

import (
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
	suite, err := LoadSuiteHCL(acceptanceSuitePath)
	require.NoError(t, err, "LoadSuiteHCL(%s)", acceptanceSuitePath)
	return suite
}

// TestSuites_PinClonedReposToAFullCommitSHA scans every shipped suite: a task
// that clones a repository must name a full 40-character commit SHA, never a
// branch or tag that can move.
func TestSuites_PinClonedReposToAFullCommitSHA(t *testing.T) {
	paths, err := filepath.Glob("../suites/*.hcl")
	require.NoError(t, err, "globbing suites")
	require.NotEmpty(t, paths, "no suites found under ../suites")
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			suite, err := LoadSuiteHCL(path)
			require.NoError(t, err, "LoadSuiteHCL(%s)", path)
			for _, task := range suite.Tasks {
				if task.Repo == "" {
					continue
				}
				t.Run(task.ID, func(t *testing.T) {
					require.Regexp(t, fullSHA, task.Ref, "task clones %s, which must name a full 40-character commit SHA", task.Repo)
				})
			}
		})
	}
}

func TestAcceptanceSuite_EveryTaskClonesAPinnedRepo(t *testing.T) {
	suite := loadAcceptanceSuite(t)
	t.Run("task count", func(t *testing.T) {
		n := len(suite.Tasks)
		require.True(t, n >= 8 && n <= 10, "suite has %d tasks, want 8 to 10", n)
	})
	for _, task := range suite.Tasks {
		t.Run(task.ID, func(t *testing.T) {
			require.True(t, strings.HasPrefix(task.Repo, "https://github.com/"), "repo %q, want a public https://github.com/ URL", task.Repo)
			require.Regexp(t, fullSHA, task.Ref, "ref %q, want a full 40-character commit SHA", task.Ref)
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
		require.True(t, deterministic[j.Type], "judge type %q, want a deterministic judge", j.Type)
		require.False(t, j.Shadow, "judge must not be a shadow judge")
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
	suite := loadAcceptanceSuite(t)
	hasBaseline := suite.RunConfig != nil || suite.RunConfigFile != ""
	var verifierTasks int
	for _, task := range suite.Tasks {
		if task.RunConfigOverrides == nil {
			continue
		}
		t.Run(task.ID, func(t *testing.T) {
			require.True(t, hasBaseline, "task sets run_config_overrides but the suite has no baseline")
		})
		if v := task.RunConfigOverrides.Verifier; v != nil && v.Type == "test-runner" && v.Command != "" {
			verifierTasks++
		}
	}
	require.NotZero(t, verifierTasks, "no task overrides the verifier with a test-runner command")
}

// TestAcceptanceSuite_JudgeHeredocsTerminate keeps the shell heredocs that
// write a hidden judge-owned test file well formed after HCL strips the
// indentation: an unterminated heredoc would fail the judge, not the task.
func TestAcceptanceSuite_JudgeHeredocsTerminate(t *testing.T) {
	suite := loadAcceptanceSuite(t)
	var seen int
	var walk func(t *testing.T, j types.EvalJudge)
	walk = func(t *testing.T, j types.EvalJudge) {
		if strings.Contains(j.Command, "<<'JUDGE_EOF'") {
			seen++
			require.Contains(t, j.Command, "\nJUDGE_EOF\n", "judge heredoc must terminate with JUDGE_EOF at column 0")
			require.Contains(t, j.Command, "trap 'rm -f zz_judge_test.go' EXIT", "judge must remove zz_judge_test.go on exit")
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
	require.NotZero(t, seen, "no judge writes a hidden test file; the heredoc check has nothing to cover")
}
