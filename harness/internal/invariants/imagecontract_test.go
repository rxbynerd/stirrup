package invariants

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// sandboxExecSites are the production functions that run a command inside
// the sandbox with a command the harness chooses. Every command run there is
// part of the sandbox image contract in docs/executors/image-contract.md,
// so a new site is a decision to make about the contract, not an
// allow-list edit: state the command in the document, bump the contract
// version, and extend the command lists in
// harness/internal/executor/imagecontract_test.go.
//
// Sites that run a command the operator or the model supplies (run_command,
// hooks, verifiers, post-edit checks) are not part of the contract; their
// entries say so.
var sandboxExecSites = map[string]string{
	"harness/internal/executor/container.go:ContainerExecutor.prepareScratch":            "contract, every run: scratch setup (mkdir)",
	"harness/internal/executor/container.go:ContainerExecutor.ReadFile":                  "contract, every run: workspace read script (readlink, tar)",
	"harness/internal/executor/container.go:ContainerExecutor.WriteFile":                 "contract, every run: workspace write guard (readlink, mkdir)",
	"harness/internal/executor/container.go:ContainerExecutor.ListDirectory":             "contract, every run: ls",
	"harness/internal/executor/container.go:ContainerExecutor.Exec":                      "contract, every run: process-group script around the command (sh, rm, setsid)",
	"harness/internal/executor/container.go:ContainerExecutor.ExecStream":                "contract, every run: process-group script around the command",
	"harness/internal/executor/container.go:ContainerExecutor.killGroup":                 "contract, every run: process-group kill script (sleep, rm)",
	"harness/internal/executor/container.go:ContainerExecutor.WriteSandboxIdentityToken": "contract, sandboxIdentity: token write script (cat, wc, tr, mv, rm)",
	"harness/internal/executor/workspacearchive.go:ContainerExecutor.ArchiveWorkspace":   "contract, every run: workspace archive (tar)",
	"harness/internal/executor/workspacearchive.go:podExecCore.ArchiveWorkspace":         "contract, every run: workspace archive (tar)",
	"harness/internal/executor/k8s_execcore.go:podExecCore.WriteSandboxIdentityToken":    "contract, sandboxIdentity: token write script",
	"harness/internal/executor/k8s_execcore.go:podExecCore.prepareScratch":               "contract, every run: scratch setup (mkdir)",
	"harness/internal/executor/k8s_execcore.go:podExecCore.ReadFile":                     "contract, every run: workspace read script",
	"harness/internal/executor/k8s_execcore.go:podExecCore.WriteFile":                    "contract, every run: workspace write guard and tar -x",
	"harness/internal/executor/k8s_execcore.go:podExecCore.ListDirectory":                "contract, every run: ls",
	"harness/internal/executor/k8s_execcore.go:podExecCore.Exec":                         "contract, every run: process-group script around the command",
	"harness/internal/executor/k8s_execcore.go:podExecCore.ExecStream":                   "contract, every run: process-group script around the command",
	"harness/internal/executor/k8s_execcore.go:podExecCore.killGroup":                    "contract, every run: process-group kill script",
	"harness/internal/executor/k8s_readiness.go:waitForExecReachable":                    "contract, every run: /bin/sh -c : readiness wait",
	"harness/internal/executor/k8s_runtimediagnostics.go:readGuestFiles":                 "contract, runtime diagnostics: shell-builtin file reader",

	"harness/internal/git/deterministic.go:DeterministicGitStrategy.refuseUnresolvableGitEntry": "contract, git: test",
	"harness/internal/git/deterministic.go:DeterministicGitStrategy.run":                        "contract, git: git",
	"harness/internal/git/root.go:WorkspaceRoot":                                                "contract, git: pwd -P",
	"harness/internal/prompt/composed.go:runGit":                                                "contract, git: git status for the prompt",
	"harness/internal/tool/builtins/git.go:runGit":                                              "contract, git: the git tools",
	"harness/internal/tool/builtins/search.go:grepViaRipgrep":                                   "contract, search tools: rg",
	"harness/internal/tool/builtins/search.go:sandboxRipgrepProbe.detect":                       "contract, search tools: rg --version",
	"harness/internal/tool/builtins/search.go:grepViaShellGrep":                                 "contract, search tools: grep",
	"harness/internal/tool/builtins/search.go:findViaExec":                                      "contract, search tools: find",

	"harness/internal/hook/runner.go:ExecRunner.runOne":                 "not contract: operator-supplied hook command",
	"harness/internal/postedit/postedit.go:Checker.runOne":              "not contract: operator-supplied post-edit check",
	"harness/internal/verifier/testrunner.go:TestRunnerVerifier.Verify": "not contract: operator-supplied verifier command",
	"harness/internal/tool/builtins/shell.go:RunCommandToolWithStore":   "not contract: the model's run_command",
}

// sandboxExecCallees are the calls that hand a command to a sandbox. The
// executor-internal names forward to the engine or API server; every other
// package reaches the sandbox through Executor.Exec and ExecStream.
var (
	executorInternalExecCalls = map[string]bool{
		"streamExec": true, "execInContainer": true, "execInContainerStream": true, "execInContainerInput": true,
	}
	executorMethodExecCalls = map[string]bool{"Exec": true, "ExecStream": true}
)

const executorDir = "harness/internal/executor"

// literalSandboxCommands are the commands the sandbox exec path runs from a
// literal at the call site; the contract document lists each.
var literalSandboxCommands = map[string]bool{
	"ls": true, "tar": true, "pwd": true, "test": true, "rg": true,
}

func TestSandboxExecSites_ArePinnedToTheImageContract(t *testing.T) {
	check := assert.New(t)
	fset := token.NewFileSet()
	files := parseProductionTree(t, fset, "harness/internal")
	sites := newAllowList(sandboxExecSites)

	eachCall(files, func(sf *sourceFile, encl funcDecl, call *ast.CallExpr) {
		name := calleeName(call)
		inExecutor := strings.HasPrefix(sf.path, executorDir+"/")
		switch {
		case inExecutor && !executorInternalExecCalls[name]:
			return
		case !inExecutor && !executorMethodExecCalls[name]:
			return
		}
		if len(call.Args) < 2 {
			return
		}
		if encl.decl == nil {
			check.Fail("package-level sandbox exec call", position(fset, call.Pos()))
			return
		}
		if inExecutor && executorInternalExecCalls[encl.decl.Name.Name] {
			return
		}
		check.True(sites.allows(encl.site()),
			"%s runs a command in the sandbox from %s, which is not pinned: if the command is something the image must supply, update docs/executors/image-contract.md, bump its version and update harness/internal/executor/imagecontract_test.go, then add the site; if the operator or model supplies it, add the site with that reason",
			position(fset, call.Pos()), encl.site())
		if cmd, ok := literalCommand(call.Args[1]); ok {
			check.True(literalSandboxCommands[cmd],
				"%s runs %q in the sandbox, which the image contract does not list", position(fset, call.Pos()), cmd)
		}
	})
	sites.assertAllUsed(t)
	for site, reason := range sandboxExecSites {
		check.NotEmpty(reason, "%s needs a reason", site)
	}
}

// literalCommand returns the command a call site spells out: the first
// element of an inline argv, or the first word of a string literal.
func literalCommand(arg ast.Expr) (string, bool) {
	var lit *ast.BasicLit
	switch a := arg.(type) {
	case *ast.CompositeLit:
		if len(a.Elts) == 0 {
			return "", false
		}
		lit, _ = a.Elts[0].(*ast.BasicLit)
	case *ast.BasicLit:
		lit = a
	}
	if lit == nil || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return "", false
	}
	return fields[0], true
}
