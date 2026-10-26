package codescanner

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scanContent runs the pattern scanner over content as if it were the file at path.
func scanContent(t *testing.T, path string, content []byte) *ScanResult {
	t.Helper()
	must := require.New(t)
	res, err := NewPatternScanner().Scan(context.Background(), path, content)
	must.NoError(err, "Scan(%q)", path)
	return res
}

// findingsWithRule returns the findings whose rule is exactly rule.
func findingsWithRule(findings []Finding, rule string) []Finding {
	var matched []Finding
	for _, f := range findings {
		if f.Rule == rule {
			matched = append(matched, f)
		}
	}
	return matched
}

// countRule returns how many findings the scan produced for rule.
func countRule(t *testing.T, path string, content string, rule string) int {
	t.Helper()
	return len(findingsWithRule(scanContent(t, path, []byte(content)).Findings, rule))
}

func TestPatternScanner_BlocksOnPlantedAnthropicKey(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	content := []byte("config = {\n  apiKey: 'sk-ant-1234567890abcdef'\n}\n")

	res := scanContent(t, "config.js", content)
	must.True(res.HasBlocking(), "expected at least one block finding, got: %+v", res.Findings)

	var found bool
	for _, f := range res.Findings {
		if strings.HasPrefix(f.Rule, "secret/anthropic_api_key") {
			check.Equal(SeverityBlock, f.Severity)
			check.Equal(2, f.Line)
			found = true
		}
	}
	must.True(found, "expected anthropic_api_key finding, got: %+v", res.Findings)
}

func TestPatternScanner_BlocksOnAWSKey(t *testing.T) {
	must := require.New(t)
	res := scanContent(t, ".env", []byte("AWS_ACCESS_KEY_ID=AKIAABCDEFGHIJKLMNOP\n"))
	must.True(res.HasBlocking(), "expected blocking finding, got: %+v", res.Findings)
}

// TestPatternScanner_BlocksOnNextLineYAMLValue pins that a key/value
// secret whose value sits on the next, indented YAML line blocks the write.
func TestPatternScanner_BlocksOnNextLineYAMLValue(t *testing.T) {
	must := require.New(t)
	content := []byte("aws:\n  aws_secret_access_key:\n    " + strings.Repeat("Ab1/", 10) + "\n")

	res := scanContent(t, "values.yaml", content)
	must.True(res.HasBlocking(), "expected blocking finding, got: %+v", res.Findings)
}

func TestPatternScanner_WarnsOnDynamicEvalSink(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	res := scanContent(t, "run.py", []byte("def run(expr):\n    return eval(expr)\n"))
	check.False(res.HasBlocking(), "eval() alone should warn, not block: %+v", res.Findings)

	var found bool
	for _, f := range res.Findings {
		if f.Rule == "sink/dynamic_eval" {
			check.Equal(SeverityWarn, f.Severity)
			check.Equal(2, f.Line)
			found = true
		}
	}
	must.True(found, "expected dynamic_eval finding, got: %+v", res.Findings)
}

func TestPatternScanner_WarnsOnSubprocessShellTrue(t *testing.T) {
	must := require.New(t)
	res := scanContent(t, "x.py", []byte("subprocess.run(cmd, shell=True)\n"))
	must.NotEmpty(findingsWithRule(res.Findings, "sink/python_subprocess_shell_true"),
		"expected subprocess_shell_true finding, got: %+v", res.Findings)
}

func TestPatternScanner_WarnsOnFunctionConstructor(t *testing.T) {
	must := require.New(t)
	res := scanContent(t, "x.js", []byte("const fn = new Function('return 1');\n"))
	must.NotEmpty(findingsWithRule(res.Findings, "sink/js_function_constructor"),
		"expected js_function_constructor finding, got: %+v", res.Findings)
}

func TestPatternScanner_DoesNotMatchMethodCallNamedEval(t *testing.T) {
	must := require.New(t)
	// `obj.eval(...)` should not match — the pattern requires a token
	// boundary that is not `.` so member calls are excluded.
	res := scanContent(t, "x.py", []byte("result = obj.eval(thing)\n"))
	must.Empty(findingsWithRule(res.Findings, "sink/dynamic_eval"),
		"obj.eval should not match dynamic_eval rule")
}

func TestPatternScanner_EmptyContentYieldsNoFindings(t *testing.T) {
	must := require.New(t)
	res := scanContent(t, "empty.txt", nil)
	must.Empty(res.Findings)
}

func TestPatternScanner_CleanContentYieldsNoFindings(t *testing.T) {
	must := require.New(t)
	res := scanContent(t, "main.go", []byte("package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n"))
	must.Empty(res.Findings)
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
			must := require.New(t)
			must.Equal(tc.want, countRule(t, tc.path, tc.content, "sink/shell_backtick"))
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
			must := require.New(t)
			must.Equal(tc.want, countRule(t, tc.path, tc.content, "sink/python_os_system"))
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
			must := require.New(t)
			content := evalCall
			if filepath.Ext(path) == "" {
				content = "#!/usr/bin/env ruby\n" + content
			}
			must.Equal(1, countRule(t, path, content, "sink/dynamic_eval"))
		})
	}

	quiet := []string{"notes.md", "notes.txt", "main.go", "data.json", "deploy.sh"}
	for _, path := range quiet {
		t.Run("quiet/"+path, func(t *testing.T) {
			must := require.New(t)
			must.Equal(0, countRule(t, path, evalCall, "sink/dynamic_eval"))
		})
	}

	t.Run("exec fires on php", func(t *testing.T) {
		must := require.New(t)
		must.Equal(1, countRule(t, "legacy.php", "exec($cmd);\n", "sink/dynamic_exec"))
	})
}

func TestPatternScanner_FunctionConstructorScope(t *testing.T) {
	const fnCtor = "const fn = new Function('return 1');\n"

	t.Run("fires on mjs", func(t *testing.T) {
		must := require.New(t)
		must.Equal(1, countRule(t, "app.mjs", fnCtor, "sink/js_function_constructor"))
	})
	t.Run("quiet on md", func(t *testing.T) {
		must := require.New(t)
		must.Equal(0, countRule(t, "notes.md", fnCtor, "sink/js_function_constructor"))
	})
}

// Secret rules carry no scope: a hardcoded credential is a finding in
// any file, including one whose extension the scanner does not know.
func TestPatternScanner_SecretRulesIgnoreFileType(t *testing.T) {
	const key = "key = 'sk-ant-1234567890abcdef'\n"

	for _, path := range []string{"vault.unknownext", "LICENSE", "config.js", ".env", "a.b.c.qqq"} {
		t.Run(path, func(t *testing.T) {
			must := require.New(t)
			res := scanContent(t, path, []byte(key))
			must.True(res.HasBlocking(), "expected a blocking secret finding, got: %+v", res.Findings)
		})
	}
}

// Scope entries are compared against a lower-cased, dot-prefixed
// extension and a lower-cased basename, so an entry written any other
// way silently stops its rule from ever firing. This pins the shape of
// every entry, and pins secret rules as unscoped.
func TestRuleScope_SinksScopedSecretsUnscoped(t *testing.T) {
	check := assert.New(t)
	for _, r := range NewPatternScanner().rules {
		scoped := len(r.scope.exts)+len(r.scope.basenames)+len(r.scope.interpreters) > 0
		if strings.HasPrefix(r.id, "secret/") {
			check.False(scoped, "%s: secret rules must stay unscoped", r.id)
		}
		if strings.HasPrefix(r.id, "sink/") {
			check.True(scoped, "%s: sink rules must declare a scope", r.id)
		}
		for _, e := range r.scope.exts {
			check.True(strings.HasPrefix(e, ".") && e == strings.ToLower(e),
				"%s: ext %q must be lower-case and dot-prefixed", r.id, e)
		}
		for _, n := range slices.Concat(r.scope.basenames, r.scope.interpreters) {
			check.Equal(strings.ToLower(n), n, "%s: %q must be lower-case", r.id, n)
		}
	}
}

func TestShebangInterpreter_ReadsProgramName(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"bin path", "#!/bin/bash\n", "bash"},
		{"env lookup", "#!/usr/bin/env bash\n", "bash"},
		{"env with split flag", "#!/usr/bin/env -S python3 -u\n", "python3"},
		{"env with split-string option", "#!/usr/bin/env --split-string=bash -e\n", "bash"},
		{"env with assignment before program", "#!/usr/bin/env FOO=1 node\n", "node"},
		{"bin path with flag", "#!/bin/sh -e\n", "sh"},
		{"env without program", "#!/usr/bin/env\n", ""},
		{"bare hashbang", "#!", ""},
		{"no hashbang", "echo hi\n", ""},
		{"empty content", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, shebangInterpreter([]byte(tc.content)))
		})
	}
}

func TestMatchesInterpreter_NameAndVersionSuffixOnly(t *testing.T) {
	names := []string{"sh", "bash", "python"}
	cases := []struct {
		name   string
		interp string
		want   bool
	}{
		{"exact name", "sh", true},
		{"bash name", "bash", true},
		{"python with major version", "python3", true},
		{"python with minor version", "python3.12", true},
		{"listed name only as a prefix of another word", "shellcheck", false},
		{"python only as a prefix of another word", "pythonista", false},
		{"unlisted program", "node", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, matchesInterpreter(tc.interp, names))
		})
	}
}

// TestPatternScanner_PEMKeyIsOneFindingOnItsBeginLine pins that the
// multi-line PEM pattern reports a key once, at its BEGIN line.
func TestPatternScanner_PEMKeyIsOneFindingOnItsBeginLine(t *testing.T) {
	must := require.New(t)
	key := "-----BEGIN EC PRIVATE KEY-----\n" +
		"MHcCAQEEIBkg4LVWM9nuwNSk3yByxZpYRTBnVJk5oWp1Mzs8Kjp2oAoGCCqGSM49\n" +
		"AwEHoUQDQgAEjzN8ZWbE0Z2Lm1hH3u6h5n6f3QK8m1o8w6m7u9S2Hn2b6eL0kV0b\n" +
		"-----END EC PRIVATE KEY-----\n"
	res := scanContent(t, "deploy/key.pem", []byte("# test key\n"+key))

	var lines []int
	for _, f := range findingsWithRule(res.Findings, "secret/pem_private_key") {
		lines = append(lines, f.Line)
	}
	must.Equal([]int{2}, lines, "all findings: %+v", res.Findings)
}

func TestSecretRules_HTTPAuthProseIsNotAFinding(t *testing.T) {
	for _, tc := range []struct {
		name, rule, content string
	}{
		{"basic_documentation", "secret/basic_auth", "Read the basic documentation first.\n"},
		{"basic_auth_prose", "secret/basic_auth", "Basic auth is supported.\n"},
		{"bearer_tokens_prose", "secret/bearer_token", "Bearer tokens are sent in the header.\n"},
		{"bearer_token_prose", "secret/bearer_token", "Use a bearer token to authenticate.\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := assert.New(t)
			res := scanContent(t, "README.md", []byte(tc.content))
			check.Empty(findingsWithRule(res.Findings, tc.rule), "prose must not be a finding: %+v", res.Findings)
			check.False(res.HasBlocking(), "prose must not block: %+v", res.Findings)
		})
	}
}

func TestSecretRules_HTTPAuthCredentialIsAFinding(t *testing.T) {
	for _, tc := range []struct {
		name, rule, content string
	}{
		{"basic_credential", "secret/basic_auth", "Authorization: Basic dXNlcjpwYXNzd29yZA==\n"},
		{"basic_credential_after_prose", "secret/basic_auth", "Basic auth uses Basic dXNlcjpwYXNzd29yZA==\n"},
		{"basic_credential_after_rejected_basic", "secret/basic_auth", "basic basic dXNlcjpwYXNzd29yZA==\n"},
		{"basic_credential_unpadded", "secret/basic_auth", "Authorization: Basic dXNlcjpwYXNzd29yZA\n"},
		{"bearer_credential", "secret/bearer_token", "Authorization: Bearer abc123def456ghi789jkl\n"},
		{"bearer_jwt_credential", "secret/bearer_token", "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJl\n"},
		{"bearer_credential_after_prose", "secret/bearer_token", "Bearer token Bearer abc123def456ghi789jkl\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			res := scanContent(t, "README.md", []byte(tc.content))
			found := findingsWithRule(res.Findings, tc.rule)
			must.Len(found, 1, "findings: %+v", res.Findings)
			check.Equal(SeverityBlock, found[0].Severity)
			check.Equal(1, found[0].Line)
		})
	}
}
