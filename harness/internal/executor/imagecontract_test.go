package executor

import (
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"mvdan.cc/sh/v3/syntax"

	"github.com/rxbynerd/stirrup/harness/internal/sandboxidentity"
	"github.com/rxbynerd/stirrup/types"
)

// imageContractVersion is the version docs/executors/image-contract.md
// states. Bump it, and the document, when an executor starts needing
// something new from the sandbox image.
const imageContractVersion = 1

const imageContractDoc = "../../../docs/executors/image-contract.md"

// imageContractScriptCommands are the external commands the executors' own
// sandbox scripts and argv run. Adding a command to a script fails
// TestImageContract_ScriptsRunOnlyPinnedCommands until it is listed here,
// in the contract document, and the version is bumped.
var imageContractScriptCommands = []string{
	"cat", "ls", "mkdir", "mv", "readlink", "rm", "setsid", "sleep", "tar", "tr", "wc",
}

// imageContractOtherCommands are the commands run from outside the
// executor's scripts: the git tools and strategy, the search tools and the
// workspace-root probe. Their sites are pinned in
// harness/internal/invariants/imagecontract_test.go.
var imageContractOtherCommands = []string{
	"find", "git", "grep", "pwd", "rg", "test",
}

// shellBuiltins are commands every POSIX shell runs itself, so the image
// does not supply them. Anything else a script runs is an external command.
var shellBuiltins = map[string]bool{
	":": true, ".": true, "[": true, "break": true, "cd": true, "command": true,
	"continue": true, "echo": true, "eval": true, "exec": true, "exit": true,
	"export": true, "false": true, "kill": true, "local": true, "printf": true,
	"read": true, "return": true, "set": true, "shift": true, "true": true,
	"umask": true, "unset": true, "wait": true,
}

// scriptCommands returns the external command names a POSIX sh script runs:
// the first word of each simple command, the operand of `command -v` and of
// `exec`, and the commands inside a `-c '<script>'` argument. Words that are
// not literal (such as "$s") name no command, and shell functions the
// script defines are not external.
func scriptCommands(t *testing.T, script string) []string {
	t.Helper()
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(script), "")
	require.NoError(t, err)

	functions := map[string]bool{}
	syntax.Walk(file, func(node syntax.Node) bool {
		if fn, ok := node.(*syntax.FuncDecl); ok {
			functions[fn.Name.Value] = true
		}
		return true
	})

	found := map[string]bool{}
	var collect func(file *syntax.File)
	collect = func(file *syntax.File) {
		syntax.Walk(file, func(node syntax.Node) bool {
			call, ok := node.(*syntax.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			words := make([]string, len(call.Args))
			for i, arg := range call.Args {
				words[i] = staticWord(arg)
			}
			name := words[0]
			switch {
			case name == "command" && slices.Contains(words, "-v") && len(words) > 2:
				name = words[len(words)-1]
			case name == "exec" && len(words) > 1:
				name = words[1]
			}
			if name != "" && !shellBuiltins[name] && !functions[name] {
				found[name] = true
			}
			for i, w := range words {
				if w != "-c" || i+1 >= len(call.Args) {
					continue
				}
				if quoted, ok := call.Args[i+1].Parts[0].(*syntax.SglQuoted); ok && len(call.Args[i+1].Parts) == 1 {
					inner, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(quoted.Value), "")
					require.NoError(t, err)
					collect(inner)
				}
			}
			return true
		})
	}
	collect(file)

	var names []string
	for name := range found {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// staticWord returns the text of a word made only of literals and quoted
// literals, and "" for a word that expands anything.
func staticWord(word *syntax.Word) string {
	var b strings.Builder
	for _, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					return ""
				}
				b.WriteString(lit.Value)
			}
		default:
			return ""
		}
	}
	return b.String()
}

func TestScriptCommands_FindsCommandPositions(t *testing.T) {
	cases := []struct {
		name   string
		script string
		want   []string
	}{
		{"builtins only", `read -r l && printf '%s' "$l" && [ -n "$l" ]`, nil},
		{"pipeline and substitution", `x=$(foo a | bar b); baz "$x"`, []string{"bar", "baz", "foo"}},
		{"control flow", `if probe; then run1; else run2; fi; while cond; do body; done`, []string{"body", "cond", "probe", "run1", "run2"}},
		{"case patterns are not commands", `case $x in a|b) act ;; *) other ;; esac`, []string{"act", "other"}},
		{"command -v names its operand", `command -v setsid >/dev/null 2>&1 && s=setsid`, []string{"setsid"}},
		{"exec names its operand", `exec tar -cf - .`, []string{"tar"}},
		{"dynamic words name nothing", `$s "$1" -c 'echo hi'`, nil},
		{"inner -c script is scanned", `sh -c 'echo hi; date'`, []string{"date", "sh"}},
		{"script's own functions are not external", `f() { echo hi; }; f; g`, []string{"g"}},
		{"redirection and arithmetic", `i=$((i+1)); cat < "$f" > /dev/null`, []string{"cat"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, scriptCommands(t, tc.script))
		})
	}
}

// sandboxScripts are the shell scripts the executors run inside the
// sandbox, with the argv entry that carries each.
func sandboxScripts(t *testing.T) map[string]string {
	t.Helper()
	gitProxyEnv, err := sandboxidentity.ComposeEnv("TOKEN", "t", SandboxIdentityTokenPath, &types.GitProxyConfig{
		URL:   "https://proxy.example",
		Hosts: []string{"example.com"},
	})
	require.NoError(t, err)
	helper := ""
	for _, env := range gitProxyEnv {
		if strings.HasPrefix(env.Value, "!") {
			helper = strings.TrimPrefix(env.Value, "!")
		}
	}
	require.NotEmpty(t, helper, "git proxy wiring carries no credential helper")

	grouped := groupedCommand{shell: "sh", pidFile: "/tmp/x.pid", command: "true"}
	return map[string]string{
		"group run":              grouped.runArgv()[2],
		"group kill":             grouped.killArgv()[2],
		"workspace read":         workspaceReadCommand("/workspace", "/workspace/f")[2],
		"workspace write guard":  workspaceWriteGuardCommand("/workspace", "/workspace/f")[2],
		"workspace archive":      workspaceArchiveArgv("sh", "/workspace")[2],
		"sandbox identity write": sandboxIdentityWriteCommand(1)[2],
		"guest file dump":        guestFileDumpCommand("/proc/net/dev")[2],
		"exec readiness":         execProbeCommand[2],
		"git proxy credential":   helper,
	}
}

func TestImageContract_ScriptsRunOnlyPinnedCommands(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	union := map[string]bool{}
	for name, script := range sandboxScripts(t) {
		for _, command := range scriptCommands(t, script) {
			union[command] = true
			check.Contains(imageContractScriptCommands, command,
				"the %s script runs %q, which the image contract does not list: add it to docs/executors/image-contract.md, bump the version and update imageContractScriptCommands", name, command)
		}
	}
	// ls is exec'd directly, and mkdir is also the scratch setup's argv.
	union["mkdir"] = true
	union["ls"] = true

	got := make([]string, 0, len(union))
	for command := range union {
		got = append(got, command)
	}
	slices.Sort(got)
	want := slices.Clone(imageContractScriptCommands)
	slices.Sort(want)
	must.Equal(want, got, "imageContractScriptCommands lists a command no script runs, or a script runs one it omits")
}

func TestImageContract_DocStatesVersion(t *testing.T) {
	must := require.New(t)
	doc, err := os.ReadFile(imageContractDoc)
	must.NoError(err)

	match := regexp.MustCompile(`\*\*Contract version: (\d+)\*\*`).FindSubmatch(doc)
	must.NotNil(match, "docs/executors/image-contract.md does not state its contract version")
	must.Equal(strconv.Itoa(imageContractVersion), string(match[1]))
}

func TestImageContract_DocNamesEveryCommand(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	doc, err := os.ReadFile(imageContractDoc)
	must.NoError(err)

	commands := append(slices.Clone(imageContractScriptCommands), imageContractOtherCommands...)
	commands = append(commands, "sh")
	for _, command := range commands {
		mention := regexp.MustCompile("`" + regexp.QuoteMeta(command) + "[` ]")
		check.Regexp(mention, string(doc), "docs/executors/image-contract.md does not mention %s", command)
	}
}
