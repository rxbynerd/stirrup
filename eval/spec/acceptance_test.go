package spec

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

const acceptanceSuitePath = "../suites/acceptance.hcl"

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// TestSuites_PinClonedReposToAFullCommitSHA scans every shipped suite: a task
// that clones a repository must name a full 40-character commit SHA, never a
// branch or tag that can move.
func TestSuites_PinClonedReposToAFullCommitSHA(t *testing.T) {
	paths, err := filepath.Glob("../suites/*.hcl")
	if err != nil || len(paths) == 0 {
		t.Fatalf("globbing suites: %v (found %d)", err, len(paths))
	}
	for _, path := range paths {
		suite, err := LoadSuiteHCL(path)
		if err != nil {
			t.Fatalf("LoadSuiteHCL(%s): %v", path, err)
		}
		for _, task := range suite.Tasks {
			if task.Repo != "" && !fullSHA.MatchString(task.Ref) {
				t.Errorf("%s: task %q clones %s at ref %q, want a full 40-character commit SHA", path, task.ID, task.Repo, task.Ref)
			}
		}
	}
}

func TestAcceptanceSuite_EveryTaskClonesAPinnedRepo(t *testing.T) {
	suite, err := LoadSuiteHCL(acceptanceSuitePath)
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	if n := len(suite.Tasks); n < 8 || n > 10 {
		t.Errorf("suite has %d tasks, want 8 to 10", n)
	}
	for _, task := range suite.Tasks {
		if !strings.HasPrefix(task.Repo, "https://github.com/") {
			t.Errorf("task %q repo = %q, want a public https://github.com/ URL", task.ID, task.Repo)
		}
		if !fullSHA.MatchString(task.Ref) {
			t.Errorf("task %q ref = %q, want a full 40-character commit SHA", task.ID, task.Ref)
		}
	}
}

func TestAcceptanceSuite_JudgesAreDeterministic(t *testing.T) {
	suite, err := LoadSuiteHCL(acceptanceSuitePath)
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	deterministic := map[string]bool{
		"test-command": true, "file-exists": true, "file-contains": true,
		"tool-trace": true, "composite": true,
	}
	var walk func(taskID string, j types.EvalJudge)
	walk = func(taskID string, j types.EvalJudge) {
		if !deterministic[j.Type] {
			t.Errorf("task %q uses judge type %q, want a deterministic judge", taskID, j.Type)
		}
		if j.Shadow {
			t.Errorf("task %q carries a shadow judge", taskID)
		}
		for _, sub := range j.Judges {
			walk(taskID, sub)
		}
	}
	for _, task := range suite.Tasks {
		walk(task.ID, task.Judge)
	}
}

// TestAcceptanceSuite_PerTaskOverridesHaveABaseline guards the verifier task:
// the runner drops a task's run_config_overrides when the suite declares no
// baseline run_config, which would silently disable the failing verifier.
func TestAcceptanceSuite_PerTaskOverridesHaveABaseline(t *testing.T) {
	suite, err := LoadSuiteHCL(acceptanceSuitePath)
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	var verifierTasks int
	for _, task := range suite.Tasks {
		if task.RunConfigOverrides == nil {
			continue
		}
		if suite.RunConfig == nil && suite.RunConfigFile == "" {
			t.Errorf("task %q sets run_config_overrides but the suite has no baseline", task.ID)
		}
		if v := task.RunConfigOverrides.Verifier; v != nil && v.Type == "test-runner" && v.Command != "" {
			verifierTasks++
		}
	}
	if verifierTasks == 0 {
		t.Error("no task overrides the verifier with a test-runner command")
	}
}

// TestAcceptanceSuite_JudgeHeredocsTerminate keeps the shell heredocs that
// write a hidden judge-owned test file well formed after HCL strips the
// indentation: an unterminated heredoc would fail the judge, not the task.
func TestAcceptanceSuite_JudgeHeredocsTerminate(t *testing.T) {
	suite, err := LoadSuiteHCL(acceptanceSuitePath)
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	var seen int
	var walk func(taskID string, j types.EvalJudge)
	walk = func(taskID string, j types.EvalJudge) {
		if strings.Contains(j.Command, "<<'JUDGE_EOF'") {
			seen++
			if !strings.Contains(j.Command, "\nJUDGE_EOF\n") {
				t.Errorf("task %q: judge heredoc has no JUDGE_EOF terminator at column 0:\n%s", taskID, j.Command)
			}
			if !strings.Contains(j.Command, "trap 'rm -f zz_judge_test.go' EXIT") {
				t.Errorf("task %q: judge writes zz_judge_test.go without removing it afterwards", taskID)
			}
		}
		for _, sub := range j.Judges {
			walk(taskID, sub)
		}
	}
	for _, task := range suite.Tasks {
		walk(task.ID, task.Judge)
	}
	if seen == 0 {
		t.Error("no judge writes a hidden test file; the heredoc check has nothing to cover")
	}
}
