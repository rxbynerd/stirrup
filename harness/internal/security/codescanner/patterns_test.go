package codescanner

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPatternScanner_BlocksOnPlantedAnthropicKey(t *testing.T) {
	s := NewPatternScanner()
	content := []byte("config = {\n  apiKey: 'sk-ant-1234567890abcdef'\n}\n")

	res, err := s.Scan(context.Background(), "config.js", content)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !res.HasBlocking() {
		t.Fatalf("expected at least one block finding, got: %+v", res.Findings)
	}

	var found bool
	for _, f := range res.Findings {
		if strings.HasPrefix(f.Rule, "secret/anthropic_api_key") {
			if f.Severity != SeverityBlock {
				t.Errorf("expected severity %q, got %q", SeverityBlock, f.Severity)
			}
			if f.Line != 2 {
				t.Errorf("expected line 2, got %d", f.Line)
			}
			found = true
		}
	}
	if !found {
		t.Errorf("expected anthropic_api_key finding, got: %+v", res.Findings)
	}
}

func TestPatternScanner_BlocksOnAWSKey(t *testing.T) {
	s := NewPatternScanner()
	content := []byte("AWS_ACCESS_KEY_ID=AKIAABCDEFGHIJKLMNOP\n")

	res, err := s.Scan(context.Background(), ".env", content)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !res.HasBlocking() {
		t.Fatalf("expected blocking finding, got: %+v", res.Findings)
	}
}

func TestPatternScanner_WarnsOnDynamicEvalSink(t *testing.T) {
	s := NewPatternScanner()
	content := []byte("def run(expr):\n    return eval(expr)\n")

	res, err := s.Scan(context.Background(), "run.py", content)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.HasBlocking() {
		t.Errorf("eval() alone should warn, not block: %+v", res.Findings)
	}

	var found bool
	for _, f := range res.Findings {
		if f.Rule == "sink/dynamic_eval" {
			if f.Severity != SeverityWarn {
				t.Errorf("expected severity %q, got %q", SeverityWarn, f.Severity)
			}
			if f.Line != 2 {
				t.Errorf("expected line 2, got %d", f.Line)
			}
			found = true
		}
	}
	if !found {
		t.Errorf("expected dynamic_eval finding, got: %+v", res.Findings)
	}
}

func TestPatternScanner_WarnsOnSubprocessShellTrue(t *testing.T) {
	s := NewPatternScanner()
	content := []byte("subprocess.run(cmd, shell=True)\n")

	res, err := s.Scan(context.Background(), "x.py", content)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var found bool
	for _, f := range res.Findings {
		if f.Rule == "sink/python_subprocess_shell_true" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected subprocess_shell_true finding, got: %+v", res.Findings)
	}
}

func TestPatternScanner_WarnsOnFunctionConstructor(t *testing.T) {
	s := NewPatternScanner()
	content := []byte("const fn = new Function('return 1');\n")

	res, err := s.Scan(context.Background(), "x.js", content)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var found bool
	for _, f := range res.Findings {
		if f.Rule == "sink/js_function_constructor" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected js_function_constructor finding, got: %+v", res.Findings)
	}
}

func TestPatternScanner_DoesNotMatchMethodCallNamedEval(t *testing.T) {
	// `obj.eval(...)` should not match — the pattern requires a token
	// boundary that is not `.` so member calls are excluded.
	s := NewPatternScanner()
	content := []byte("result = obj.eval(thing)\n")

	res, err := s.Scan(context.Background(), "x.py", content)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, f := range res.Findings {
		if f.Rule == "sink/dynamic_eval" {
			t.Errorf("obj.eval should not match dynamic_eval rule: %+v", f)
		}
	}
}

func TestPatternScanner_EmptyContent(t *testing.T) {
	s := NewPatternScanner()
	res, err := s.Scan(context.Background(), "empty.txt", nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Errorf("empty content must yield no findings, got: %+v", res.Findings)
	}
}

func TestPatternScanner_CleanContent(t *testing.T) {
	s := NewPatternScanner()
	content := []byte("package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n")
	res, err := s.Scan(context.Background(), "main.go", content)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Errorf("clean content must yield no findings, got: %+v", res.Findings)
	}
}

// countRule returns how many findings the scan produced for rule.
func countRule(t *testing.T, path string, content string, rule string) int {
	t.Helper()
	res, err := NewPatternScanner().Scan(context.Background(), path, []byte(content))
	if err != nil {
		t.Fatalf("Scan(%q): %v", path, err)
	}
	var n int
	for _, f := range res.Findings {
		if f.Rule == rule {
			n++
		}
	}
	return n
}

func TestPatternScanner_ShellBacktickScope(t *testing.T) {
	const templateLiteral = "debug(`springboard.js loaded: ${chrome.runtime.id}`);\n"
	const backtickSubstitution = "USER=`whoami`\n"

	cases := []struct {
		name    string
		path    string
		content string
		want    int
	}{
		{"js template literal", "springboard.js", templateLiteral, 0},
		{"ts template literal", "src/app.ts", templateLiteral, 0},
		{"markdown code span", "README.md", "Run `make test` first.\n", 0},
		{"shell script", "scripts/deploy.sh", backtickSubstitution, 1},
		{"workflow yaml", ".github/workflows/ci.yml", "    run: " + backtickSubstitution, 1},
		{"dockerfile", "Dockerfile", "RUN " + backtickSubstitution, 1},
		{"dockerfile variant", "Dockerfile.dev", "RUN " + backtickSubstitution, 1},
		{"makefile", "Makefile", "\t" + backtickSubstitution, 1},
		{"extensionless bash shebang", "bin/deploy", "#!/usr/bin/env bash\n" + backtickSubstitution, 1},
		{"extensionless sh shebang", "bin/deploy", "#!/bin/sh\n" + backtickSubstitution, 1},
		{"extensionless node shebang", "bin/tool", "#!/usr/bin/env node\n" + templateLiteral, 0},
		{"extensionless without shebang", "NOTES", backtickSubstitution, 0},
		{"bash shebang under a text extension", "deploy.txt", "#!/bin/bash\n" + backtickSubstitution, 1},
		{"bash shebang under a js extension", "deploy.js", "#!/bin/bash\n" + backtickSubstitution, 1},
		{"node shebang under a js extension", "app.js", "#!/usr/bin/env node\n" + templateLiteral, 0},
		{"trailing dot with shebang", "deploy.", "#!/bin/bash\n" + backtickSubstitution, 1},
		{"trailing dot without shebang", "deploy.", backtickSubstitution, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countRule(t, tc.path, tc.content, "sink/shell_backtick"); got != tc.want {
				t.Errorf("sink/shell_backtick findings = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPatternScanner_PythonSinkScope(t *testing.T) {
	const osSystem = "os.system(cmd)\n"

	cases := []struct {
		name    string
		path    string
		content string
		want    int
	}{
		{"python module", "tasks.py", osSystem, 1},
		{"python stub", "tasks.pyi", osSystem, 1},
		{"extensionless python shebang", "bin/task", "#!/usr/bin/env python3.12\n" + osSystem, 1},
		{"markdown prose", "docs/runbook.md", "Legacy code called " + osSystem, 0},
		{"plain text", "notes.txt", osSystem, 0},
		{"json fixture", "fixture.json", "{\"snippet\": \"os.system(cmd)\"}\n", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countRule(t, tc.path, tc.content, "sink/python_os_system"); got != tc.want {
				t.Errorf("sink/python_os_system findings = %d, want %d", got, tc.want)
			}
		})
	}
}

// eval() and exec() are spelled the same way in several languages, so
// the rules covering them span all of those file types rather than
// Python alone.
func TestPatternScanner_DynamicEvalScope(t *testing.T) {
	const evalCall = "const r = eval(userInput);\n"

	fires := []string{
		"x.py", "x.pyi", "app.js", "app.jsx", "app.mjs", "app.cjs",
		"app.ts", "app.tsx", "widget.vue", "widget.svelte",
		"index.html", "index.htm", "legacy.php", "legacy.phtml",
		"job.rb", "view.erb", "bin/task",
	}
	for _, path := range fires {
		t.Run("fires/"+path, func(t *testing.T) {
			content := evalCall
			if filepath.Ext(path) == "" {
				content = "#!/usr/bin/env ruby\n" + content
			}
			if got := countRule(t, path, content, "sink/dynamic_eval"); got != 1 {
				t.Errorf("sink/dynamic_eval findings = %d, want 1", got)
			}
		})
	}

	quiet := []string{"notes.md", "notes.txt", "main.go", "data.json", "deploy.sh"}
	for _, path := range quiet {
		t.Run("quiet/"+path, func(t *testing.T) {
			if got := countRule(t, path, evalCall, "sink/dynamic_eval"); got != 0 {
				t.Errorf("sink/dynamic_eval findings = %d, want 0", got)
			}
		})
	}

	if got := countRule(t, "legacy.php", "exec($cmd);\n", "sink/dynamic_exec"); got != 1 {
		t.Errorf("sink/dynamic_exec findings on .php = %d, want 1", got)
	}
}

func TestPatternScanner_FunctionConstructorScope(t *testing.T) {
	const fnCtor = "const fn = new Function('return 1');\n"

	if got := countRule(t, "app.mjs", fnCtor, "sink/js_function_constructor"); got != 1 {
		t.Errorf(".mjs findings = %d, want 1", got)
	}
	if got := countRule(t, "notes.md", fnCtor, "sink/js_function_constructor"); got != 0 {
		t.Errorf(".md findings = %d, want 0", got)
	}
}

// Secret rules carry no scope: a hardcoded credential is a finding in
// any file, including one whose extension the scanner does not know.
func TestPatternScanner_SecretRulesIgnoreFileType(t *testing.T) {
	const key = "key = 'sk-ant-1234567890abcdef'\n"

	for _, path := range []string{"vault.unknownext", "LICENSE", "config.js", ".env", "a.b.c.qqq"} {
		t.Run(path, func(t *testing.T) {
			res, err := NewPatternScanner().Scan(context.Background(), path, []byte(key))
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if !res.HasBlocking() {
				t.Errorf("expected a blocking secret finding for %q, got: %+v", path, res.Findings)
			}
		})
	}
}

// Scope entries are compared against a lower-cased, dot-prefixed
// extension and a lower-cased basename, so an entry written any other
// way silently stops its rule from ever firing. This pins the shape of
// every entry, and pins secret rules as unscoped.
func TestRuleScopeInvariants(t *testing.T) {
	for _, r := range NewPatternScanner().rules {
		scoped := len(r.scope.exts)+len(r.scope.basenames)+len(r.scope.interpreters) > 0
		if strings.HasPrefix(r.id, "secret/") && scoped {
			t.Errorf("%s: secret rules must stay unscoped", r.id)
		}
		if strings.HasPrefix(r.id, "sink/") && !scoped {
			t.Errorf("%s: sink rules must declare a scope", r.id)
		}
		for _, e := range r.scope.exts {
			if !strings.HasPrefix(e, ".") || e != strings.ToLower(e) {
				t.Errorf("%s: ext %q must be lower-case and dot-prefixed", r.id, e)
			}
		}
		for _, n := range slices.Concat(r.scope.basenames, r.scope.interpreters) {
			if n != strings.ToLower(n) {
				t.Errorf("%s: %q must be lower-case", r.id, n)
			}
		}
	}
}

func TestShebangInterpreter(t *testing.T) {
	cases := []struct {
		content string
		want    string
	}{
		{"#!/bin/bash\n", "bash"},
		{"#!/usr/bin/env bash\n", "bash"},
		{"#!/usr/bin/env -S python3 -u\n", "python3"},
		{"#!/usr/bin/env --split-string=bash -e\n", "bash"},
		{"#!/usr/bin/env FOO=1 node\n", "node"},
		{"#!/bin/sh -e\n", "sh"},
		{"#!/usr/bin/env\n", ""},
		{"#!", ""},
		{"echo hi\n", ""},
		{"", ""},
	}

	for _, tc := range cases {
		if got := shebangInterpreter([]byte(tc.content)); got != tc.want {
			t.Errorf("shebangInterpreter(%q) = %q, want %q", tc.content, got, tc.want)
		}
	}
}

func TestMatchesInterpreter(t *testing.T) {
	names := []string{"sh", "bash", "python"}
	cases := []struct {
		interp string
		want   bool
	}{
		{"sh", true},
		{"bash", true},
		{"python3", true},
		{"python3.12", true},
		{"shellcheck", false},
		{"pythonista", false},
		{"node", false},
	}

	for _, tc := range cases {
		if got := matchesInterpreter(tc.interp, names); got != tc.want {
			t.Errorf("matchesInterpreter(%q) = %v, want %v", tc.interp, got, tc.want)
		}
	}
}
