package permission

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// findingRules collapses findings to "rule:severity" strings for compact
// assertions.
func findingRules(findings []LintFinding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, string(f.Rule)+":"+string(f.Severity))
	}
	return out
}

func assertRules(t *testing.T, findings []LintFinding, want ...string) {
	t.Helper()
	got := findingRules(findings)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("findings = %v, want %v\nfull: %v", got, want, findings)
	}
}

// harnessSchemas mirrors the real built-in tool schemas closely enough for
// the linter: run_command declares `command`, web_fetch declares `url`,
// write_file declares `path`/`content`, all closed.
func harnessSchemas() map[string]ToolSchema {
	return map[string]ToolSchema{
		"run_command": {Properties: map[string]bool{"command": true, "timeout": true}, Closed: true},
		"web_fetch":   {Properties: map[string]bool{"url": true}, Closed: true},
		"write_file":  {Properties: map[string]bool{"path": true, "content": true}, Closed: true},
	}
}

// --- structural tier -------------------------------------------------

func TestLintPolicySetStructure_UnknownRequestAttributes(t *testing.T) {
	cases := []struct {
		name string
		src  string
		rule string
	}{
		{
			name: "context key the harness never populates",
			src: `forbid (principal, action, resource) when {
				context.inputs.command like "*rm*"
			};`,
			rule: LintRuleUnknownContextAttribute,
		},
		{
			name: "principal attribute misspelled",
			src: `forbid (principal, action, resource) when {
				principal.runID == "abc"
			};`,
			rule: LintRuleUnknownPrincipalAttribute,
		},
		{
			name: "principal has on an unknown attribute",
			src: `forbid (principal, action, resource) when {
				principal has role
			};`,
			rule: LintRuleUnknownPrincipalAttribute,
		},
		{
			name: "resource carries no attributes",
			src: `forbid (principal, action, resource) when {
				resource.name == "run_command"
			};`,
			rule: LintRuleNoSuchAttribute,
		},
		{
			name: "action carries no attributes",
			src: `forbid (principal, action, resource) when {
				action.id == "tool:run_command"
			};`,
			rule: LintRuleNoSuchAttribute,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertRules(t, LintPolicySetStructure(mustParse(t, tc.src)), tc.rule+":error")
		})
	}
}

func TestLintPolicySetStructure_KnownAttributesPass(t *testing.T) {
	src := `forbid (
		principal,
		action == Action::"tool:run_command",
		resource == Tool::"run_command"
	) when {
		principal has parentRunId &&
		principal.parentRunId != "" &&
		principal.mode == "execution" &&
		principal.runId != "" &&
		principal.capabilities.contains("shell") &&
		context.workspace like "/work*" &&
		context.dynamicContext.ticket like "PROJ-*" &&
		context.input has command
	};`
	assertRules(t, LintPolicySetStructure(mustParse(t, src)))
}

// context.input.raw is the synthetic wrapper buildRequest applies to a
// non-object tool input, so it is legitimate at both tiers.
func TestLint_InputRawIsLegitimate(t *testing.T) {
	src := `forbid (principal, action, resource) when { context.input.raw like "*rm -rf*" };`
	ps := mustParse(t, src)
	assertRules(t, LintPolicySetStructure(ps))
	assertRules(t, LintPolicySetTools(ps, harnessSchemas()))
}

func TestLintPolicySetStructure_ScopeEntities(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "action ID missing the tool: prefix",
			src:  `forbid (principal, action == Action::"run_command", resource);`,
			want: []string{LintRuleUnknownScopeEntity + ":error"},
		},
		{
			name: "resource entity type miscased",
			src:  `forbid (principal, action, resource == tool::"run_command");`,
			want: []string{LintRuleUnknownScopeEntity + ":error"},
		},
		{
			name: "principal entity type the harness never mints",
			src:  `forbid (principal == Agent::"abc", action, resource);`,
			want: []string{LintRuleUnknownScopeEntity + ":error"},
		},
		{
			name: "action set with one bad member",
			src:  `forbid (principal, action in [Action::"tool:run_command", Action::"web_fetch"], resource);`,
			want: []string{LintRuleUnknownScopeEntity + ":error"},
		},
		{
			name: "well-formed scopes",
			src: `forbid (
				principal in User::"any",
				action in [Action::"tool:run_command", Action::"tool:web_fetch"],
				resource == Tool::"run_command"
			);`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertRules(t, LintPolicySetStructure(mustParse(t, tc.src)), tc.want...)
		})
	}
}

// --- wildcard-host rule ----------------------------------------------

func TestWildcardHostPattern_Anchoring(t *testing.T) {
	cases := []struct {
		pattern string
		flagged bool
	}{
		// Fully-anchored: every byte of scheme + host is literal.
		{`https://github.com/*`, false},
		{`https://api.github.com/*`, false},
		{`https://github.com/anthropics/*`, false},
		{`https://github.com`, false},
		{`https://github.com?*`, false},
		{`https://github.com/x?q=*`, false},
		{`https://github.com/#*`, false},

		// Wildcard inside the host.
		{`https://*.github.com/*`, true},
		{`https://*github.com*`, true},
		{`https://github.com*/x`, true},
		{`https://*`, true},

		// Wildcard in or before the scheme.
		{`*://github.com/*`, true},
		{`*https://github.com/*`, true},

		// Regressions for two bypasses an earlier formulation missed: it
		// gated on a literal "://" surviving in the pattern, so a
		// scheme-relative pattern and a pattern that splits the separator
		// both slipped through. Both are matched by
		// https://evil.example/x.github.com/y.
		{`*.github.com/*`, true},
		{`*github.com*`, true},
		{`github.com*`, true},
		{`https:*/github.com/*`, true},

		// A substring probe over a URL is not an anchored allow-list
		// either; in a widening position it grants everything.
		{`*sk-*`, true},
	}
	for _, tc := range cases {
		t.Run(tc.pattern, func(t *testing.T) {
			src := `permit (principal, action == Action::"tool:web_fetch", resource == Tool::"web_fetch")
				when { context.input.url like "` + tc.pattern + `" };`
			findings := LintPolicySetStructure(mustParse(t, src))
			got := len(findings) > 0
			if got != tc.flagged {
				t.Fatalf("pattern %q: flagged = %v, want %v (%v)", tc.pattern, got, tc.flagged, findings)
			}
			if got && findings[0].Rule != LintRuleWildcardHostPattern {
				t.Fatalf("pattern %q: rule = %s, want %s", tc.pattern, findings[0].Rule, LintRuleWildcardHostPattern)
			}
		})
	}
}

// Anchoring is judged per attribute per conjunctive group. A second
// constraint on an already-anchored attribute is not itself a host match,
// and demanding that it anchor too would make the fail-closed rule reject
// a correct policy.
func TestWildcardHostPattern_ConjunctionCredit(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "second conjunct rides the first's anchor",
			body: `context.input.url like "https://github.com/*" && context.input.url like "*.json"`,
			want: false,
		},
		{
			name: "neither conjunct anchors",
			body: `context.input.url like "*github.com*" && context.input.url like "*.json"`,
			want: true,
		},
		{
			name: "disjuncts get no credit from each other",
			body: `context.input.url like "https://github.com/*" || context.input.url like "*.github.com/*"`,
			want: true,
		},
		{
			name: "an anchor on one attribute does not cover another",
			body: `context.input.url like "https://github.com/*" && context.input.callbackUri like "*.github.com/*"`,
			want: true,
		},
		{
			name: "negated conjunct cannot anchor",
			body: `context.input.url like "*github.com*" && !(context.input.url like "*evil*")`,
			want: true,
		},
		{
			name: "every disjunct anchored",
			body: `context.input.url like "https://github.com/*" || context.input.url like "https://gitlab.com/*"`,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := `permit (principal, action == Action::"tool:web_fetch", resource == Tool::"web_fetch")
				when { ` + tc.body + ` };`
			findings := LintPolicySetStructure(mustParse(t, src))
			if got := len(findings) > 0; got != tc.want {
				t.Fatalf("flagged = %v, want %v (%v)", got, tc.want, findings)
			}
		})
	}
}

// De Morgan: negation swaps the roles of && and ||, so the DNF of a
// negated subtree must come out correct rather than approximated.
func TestWildcardHostPattern_DeMorgan(t *testing.T) {
	// forbid/unless widens, and !(A || B) == !A && !B, so the inner
	// disjuncts become conjuncts: one anchor covers the group.
	src := `forbid (principal, action, resource) unless {
		!(!(context.input.url like "https://github.com/*") || !(context.input.url like "*.json"))
	};`
	if findings := LintPolicySetStructure(mustParse(t, src)); len(findings) != 0 {
		t.Fatalf("expected the anchored conjunct to cover the group, got %v", findings)
	}
}

// The rule only fires where over-matching WIDENS access. Over-matching a
// deny-list is fail-safe, and flagging it would push operators to narrow
// a forbid.
func TestWildcardHostPattern_PolarityMatrix(t *testing.T) {
	const pattern = `https://*.github.com/*`
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{
			name: "permit/when widens",
			src:  `permit (principal, action, resource) when { context.input.url like "` + pattern + `" };`,
			want: true,
		},
		{
			name: "forbid/when narrows nothing an attacker controls",
			src:  `forbid (principal, action, resource) when { context.input.url like "` + pattern + `" };`,
			want: false,
		},
		{
			name: "forbid/unless widens",
			src:  `forbid (principal, action, resource) unless { context.input.url like "` + pattern + `" };`,
			want: true,
		},
		{
			name: "permit/unless narrows",
			src:  `permit (principal, action, resource) unless { context.input.url like "` + pattern + `" };`,
			want: false,
		},
		{
			name: "negation inside a permit/when flips polarity",
			src:  `permit (principal, action, resource) when { !(context.input.url like "` + pattern + `") };`,
			want: false,
		},
		{
			name: "double negation restores polarity",
			src:  `permit (principal, action, resource) when { !(!(context.input.url like "` + pattern + `")) };`,
			want: true,
		},
		{
			name: "disjunct inside a permit/when still widens",
			src: `permit (principal, action, resource) when {
				context.input.url like "https://github.com/*" ||
				context.input.url like "` + pattern + `"
			};`,
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings := LintPolicySetStructure(mustParse(t, tc.src))
			if got := len(findings) > 0; got != tc.want {
				t.Fatalf("flagged = %v, want %v (%v)", got, tc.want, findings)
			}
		})
	}
}

// The rule is scoped to URL-valued attributes so a substring probe over a
// shell command — where a wildcard before the host is the whole point —
// is not flagged.
func TestWildcardHostPattern_OnlyURLAttributes(t *testing.T) {
	src := `permit (principal, action == Action::"tool:run_command", resource == Tool::"run_command")
		when { context.input.command like "*curl https://github.com/*" };`
	assertRules(t, LintPolicySetStructure(mustParse(t, src)))
}

func TestWildcardHostPattern_SuffixedAttributeNames(t *testing.T) {
	// webhookUrl / callbackURI are as URL-valued as `url` itself.
	for _, attr := range []string{"webhookUrl", "callbackURI", "baseUri"} {
		t.Run(attr, func(t *testing.T) {
			src := `permit (principal, action, resource) when { context.input.` + attr + ` like "https://*.github.com/*" };`
			findings := LintPolicySetStructure(mustParse(t, src))
			if len(findings) != 1 || findings[0].Rule != LintRuleWildcardHostPattern {
				t.Fatalf("attr %s: findings = %v, want one wildcard-host-pattern", attr, findings)
			}
		})
	}
}

// --- @stirrupLintIgnore ----------------------------------------------

func TestLintIgnoreAnnotation_DowngradesToWarning(t *testing.T) {
	src := `@stirrupLintIgnore("wildcard-host-pattern")
	permit (principal, action == Action::"tool:web_fetch", resource == Tool::"web_fetch")
	when { context.input.url like "https://*.github.com/*" };`

	findings := LintPolicySetStructure(mustParse(t, src))
	assertRules(t, findings, LintRuleWildcardHostPattern+":warning")
	if !strings.Contains(findings[0].Message, "downgraded to a warning") {
		t.Fatalf("message does not record the downgrade: %q", findings[0].Message)
	}
	if err := LintErrors("test", findings); err != nil {
		t.Fatalf("warning-only findings must not abort: %v", err)
	}
}

func TestLintIgnoreAnnotation_IsRuleScoped(t *testing.T) {
	// Ignoring one rule must not suppress a different defect in the same
	// policy.
	src := `@stirrupLintIgnore("wildcard-host-pattern")
	permit (principal, action, resource) when {
		context.input.url like "https://*.github.com/*" && principal.runID == "x"
	};`
	assertRules(t, LintPolicySetStructure(mustParse(t, src)),
		LintRuleUnknownPrincipalAttribute+":error",
		LintRuleWildcardHostPattern+":warning",
	)
}

func TestLintIgnoreAnnotation_MultipleRules(t *testing.T) {
	src := `@stirrupLintIgnore("wildcard-host-pattern, unknown-principal-attribute")
	permit (principal, action, resource) when {
		context.input.url like "https://*.github.com/*" && principal.runID == "x"
	};`
	findings := LintPolicySetStructure(mustParse(t, src))
	if err := LintErrors("test", findings); err != nil {
		t.Fatalf("both rules ignored, expected no error: %v", err)
	}
}

// --- registry-aware tier ---------------------------------------------

// The #524 defect itself: destructive-shell.cedar keyed on `cmd` while
// run_command declares `command`, so `rm -rf /workspace` sailed past the
// policy that claimed to forbid it.
func TestLintPolicySetTools_Issue524Regression(t *testing.T) {
	src := `forbid (
		principal,
		action == Action::"tool:run_command",
		resource == Tool::"run_command"
	) when {
		context.input has cmd && context.input.cmd like "*rm -rf*"
	};`
	findings := LintPolicySetTools(mustParse(t, src), harnessSchemas())
	assertRules(t, findings, LintRuleUndeclaredInputAttribute+":error")
	if !strings.Contains(findings[0].Message, "context.input.cmd") {
		t.Fatalf("message does not name the attribute: %q", findings[0].Message)
	}
	if !strings.Contains(findings[0].Message, "command") {
		t.Fatalf("message does not list the declared properties: %q", findings[0].Message)
	}
}

func TestLintPolicySetTools_ScopingRules(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "scoped to a tool that declares the attribute",
			src: `forbid (principal, action == Action::"tool:run_command", resource == Tool::"run_command")
				when { context.input.command like "*rm*" };`,
			want: nil,
		},
		{
			name: "scoped to one tool, attribute belongs to another",
			src: `forbid (principal, action == Action::"tool:run_command", resource == Tool::"run_command")
				when { context.input.url like "*evil*" };`,
			want: []string{LintRuleUndeclaredInputAttribute + ":error"},
		},
		{
			name: "action set widens the in-scope declarations",
			src: `forbid (principal, action in [Action::"tool:run_command", Action::"tool:web_fetch"], resource)
				when { context.input.url like "*evil*" };`,
			want: nil,
		},
		{
			name: "unscoped policy checks against the union",
			src: `forbid (principal, action, resource) when {
				context.input.command like "*sk-*" ||
				context.input.content like "*sk-*" ||
				context.input.url like "*sk-*"
			};`,
			want: nil,
		},
		{
			name: "unscoped policy with an attribute no tool declares",
			src:  `forbid (principal, action, resource) when { context.input.payload like "*sk-*" };`,
			want: []string{LintRuleUndeclaredInputAttribute + ":error"},
		},
		{
			name: "unregistered tool cannot be checked",
			src: `forbid (principal, action == Action::"tool:mcp_jira_search", resource)
				when { context.input.jql like "*" };`,
			want: []string{LintRuleUnknownTool + ":warning"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertRules(t, LintPolicySetTools(mustParse(t, tc.src), harnessSchemas()), tc.want...)
		})
	}
}

// A schema that permits additional properties cannot prove a clause dead:
// an undeclared property survives input validation and reaches Cedar. The
// finding degrades to a warning rather than disappearing.
func TestLintPolicySetTools_OpenSchemaDowngradesToWarning(t *testing.T) {
	schemas := harnessSchemas()
	schemas["mcp_jira_search"] = ToolSchema{Properties: map[string]bool{"jql": true}, Closed: false}

	src := `forbid (principal, action == Action::"tool:mcp_jira_search", resource)
		when { context.input.project like "*" };`
	findings := LintPolicySetTools(mustParse(t, src), schemas)
	assertRules(t, findings, LintRuleUnverifiableInputAttribute+":warning")
	if !strings.Contains(findings[0].Message, "mcp_jira_search") {
		t.Fatalf("message does not name the open-schema tool: %q", findings[0].Message)
	}
}

// An empty schema map disables the tier: the dry-run preflight builds
// components against an empty registry, and reporting every attribute as
// undeclared there would be noise, not signal.
func TestLintPolicySetTools_EmptySchemasDisablesTier(t *testing.T) {
	src := `forbid (principal, action == Action::"tool:run_command", resource)
		when { context.input.cmd like "*rm*" };`
	if findings := LintPolicySetTools(mustParse(t, src), nil); findings != nil {
		t.Fatalf("expected no findings against an empty registry, got %v", findings)
	}
}

func TestLintPolicySetTools_IgnoreAnnotationApplies(t *testing.T) {
	src := `@stirrupLintIgnore("undeclared-input-attribute")
	forbid (principal, action == Action::"tool:run_command", resource == Tool::"run_command")
	when { context.input.cmd like "*rm -rf*" };`
	findings := LintPolicySetTools(mustParse(t, src), harnessSchemas())
	assertRules(t, findings, LintRuleUndeclaredInputAttribute+":warning")
}

// --- NewToolSchema ---------------------------------------------------

func TestNewToolSchema(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantProps  []string
		wantClosed bool
	}{
		{
			name:       "closed schema",
			raw:        `{"type":"object","properties":{"command":{"type":"string"}},"additionalProperties":false}`,
			wantProps:  []string{"command"},
			wantClosed: true,
		},
		{
			name:      "open schema by omission",
			raw:       `{"type":"object","properties":{"command":{"type":"string"}}}`,
			wantProps: []string{"command"},
		},
		{
			name:      "additionalProperties as a schema object is not closed",
			raw:       `{"type":"object","properties":{"a":{}},"additionalProperties":{"type":"string"}}`,
			wantProps: []string{"a"},
		},
		{
			name: "unparseable schema yields an open, empty schema",
			raw:  `not json`,
		},
		{
			name: "empty schema",
			raw:  ``,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NewToolSchema(json.RawMessage(tc.raw))
			if got.Closed != tc.wantClosed {
				t.Errorf("Closed = %v, want %v", got.Closed, tc.wantClosed)
			}
			if len(got.Properties) != len(tc.wantProps) {
				t.Fatalf("Properties = %v, want %v", got.Properties, tc.wantProps)
			}
			for _, p := range tc.wantProps {
				if !got.Properties[p] {
					t.Errorf("missing property %q", p)
				}
			}
		})
	}
}

// --- load / construction integration ---------------------------------

func TestLoadPolicySetFromFile_RejectsStructuralDefect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.cedar")
	src := `forbid (principal, action, resource) when { context.inputs.command like "*rm*" };`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadPolicySetFromFile(path)
	if err == nil {
		t.Fatal("expected LoadPolicySetFromFile to reject a dead clause")
	}
	if !strings.Contains(err.Error(), LintRuleUnknownContextAttribute) {
		t.Fatalf("error does not name the rule: %v", err)
	}
	if !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("error does not locate the policy: %v", err)
	}
}

// The dry-run preflight and every other load path must still accept a
// well-formed file.
func TestLoadPolicySetFromFile_AcceptsShippedStarters(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "examples", "policies")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	checked := 0
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".cedar" {
			continue
		}
		checked++
		t.Run(entry.Name(), func(t *testing.T) {
			ps, err := LoadPolicySetFromFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatalf("shipped starter fails the structural lint: %v", err)
			}
			if findings := LintPolicySetStructure(ps); len(findings) != 0 {
				t.Fatalf("shipped starter produced findings: %v", findings)
			}
		})
	}
	if checked == 0 {
		t.Fatalf("no .cedar files found in %s — directory moved?", dir)
	}
}

func TestNewPolicyEngine_LintAbortsConstruction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "typo.cedar")
	src := `forbid (
		principal,
		action == Action::"tool:run_command",
		resource == Tool::"run_command"
	) when {
		context.input.cmd like "*rm -rf*"
	};`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := &fakeEmitter{}
	_, err := New(
		types.PermissionPolicyConfig{Type: "policy-engine", PolicyFile: path, Fallback: "deny-all"},
		PolicyEngineEnv{ToolSchemas: harnessSchemas(), Security: rec},
		func(string) (PermissionPolicy, error) { return NewDenyAll(), nil },
	)
	if err == nil {
		t.Fatal("expected construction to abort on an undeclared input attribute")
	}
	if !strings.Contains(err.Error(), LintRuleUndeclaredInputAttribute) {
		t.Fatalf("error does not name the rule: %v", err)
	}
	if len(rec.snapshot()) != 1 || rec.snapshot()[0].event != "policy_lint" || rec.snapshot()[0].level != "error" {
		t.Fatalf("expected one error-level policy_lint event, got %+v", rec.snapshot())
	}
	if rec.snapshot()[0].data["rule"] != LintRuleUndeclaredInputAttribute {
		t.Fatalf("event does not carry the rule: %+v", rec.snapshot()[0].data)
	}
}

func TestNewPolicyEngine_WarningsAreEmittedAndSurvive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.cedar")
	src := `forbid (
		principal,
		action == Action::"tool:mcp_jira_search",
		resource == Tool::"mcp_jira_search"
	) when {
		context.input.jql like "*DROP*"
	};`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := &fakeEmitter{}
	pp, err := New(
		types.PermissionPolicyConfig{Type: "policy-engine", PolicyFile: path, Fallback: "deny-all"},
		PolicyEngineEnv{ToolSchemas: harnessSchemas(), Security: rec},
		func(string) (PermissionPolicy, error) { return NewDenyAll(), nil },
	)
	if err != nil {
		t.Fatalf("an unregistered tool must warn, not abort: %v", err)
	}
	if pp == nil {
		t.Fatal("expected a constructed policy")
	}
	if len(rec.snapshot()) != 1 || rec.snapshot()[0].level != "warn" || rec.snapshot()[0].data["rule"] != LintRuleUnknownTool {
		t.Fatalf("expected one unknown-tool warning, got %+v", rec.snapshot())
	}
}

// --- finding formatting ----------------------------------------------

func TestLintFindingString(t *testing.T) {
	f := LintFinding{PolicyID: "policy0", Rule: LintRuleUnknownTool, Message: "nope", Line: 7}
	if got, want := f.String(), `[unknown-tool] policy0 (line 7): nope`; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	f.Line = 0
	if got, want := f.String(), `[unknown-tool] policy0: nope`; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestLintErrors_IgnoresWarnings(t *testing.T) {
	findings := []LintFinding{
		{PolicyID: "p0", Rule: "a", Severity: LintWarning, Message: "w"},
	}
	if err := LintErrors("src", findings); err != nil {
		t.Fatalf("warnings must not produce an error: %v", err)
	}
	findings = append(findings, LintFinding{PolicyID: "p1", Rule: "b", Severity: LintError, Message: "e"})
	err := LintErrors("src", findings)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "[a]") {
		t.Fatalf("warning leaked into the error: %v", err)
	}
}

// Multiple statements in one file are all linted, and findings are
// reported against the statement that produced them.
func TestLintPolicySetStructure_MultiStatementFile(t *testing.T) {
	src := `permit (principal, action == Action::"tool:web_fetch", resource == Tool::"web_fetch")
	when { context.input.url like "https://github.com/*" };

	forbid (principal, action, resource) when { principal.role == "admin" };`
	findings := LintPolicySetStructure(mustParse(t, src))
	assertRules(t, findings, LintRuleUnknownPrincipalAttribute+":error")
	if findings[0].PolicyID != "policy1" {
		t.Fatalf("finding attributed to %q, want policy1", findings[0].PolicyID)
	}
	if findings[0].Line != 4 {
		t.Fatalf("finding at line %d, want 4", findings[0].Line)
	}
}

func TestLint_NilPolicySet(t *testing.T) {
	if got := LintPolicySetStructure(nil); got != nil {
		t.Errorf("LintPolicySetStructure(nil) = %v", got)
	}
	if got := LintPolicySetTools(nil, harnessSchemas()); got != nil {
		t.Errorf("LintPolicySetTools(nil) = %v", got)
	}
}

// The linter's vocabulary of request attributes is a hand-maintained
// mirror of buildRequest. Drift in either direction is a silent failure:
// a new context key the linter does not know about makes it reject valid
// policies, and a removed one makes it accept dead ones. Pin both
// directions against a request built by the real code path.
func TestLintVocabularyMatchesBuildRequest(t *testing.T) {
	p, err := NewPolicyEnginePolicy(PolicyEngineConfig{
		PolicySet: mustParse(t, `permit (principal, action, resource);`),
		Fallback:  NewDenyAll(),
		RunID:     "run-1",
		Mode:      "execution",
		Workspace: "/workspace",
		// Both optional principal attributes populated so the entity
		// carries its widest shape.
		ParentRunID:    "parent-1",
		Capabilities:   []string{"shell"},
		DynamicContext: map[string]string{"issue.title": "x"},
	})
	if err != nil {
		t.Fatalf("NewPolicyEnginePolicy: %v", err)
	}

	req, entities, err := p.buildRequest(types.ToolDefinition{Name: "run_command"}, []byte(`{"command":"ls"}`))
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	gotContext := map[string]bool{}
	for key := range req.Context.Keys() {
		gotContext[string(key)] = true
	}
	if !sameKeys(gotContext, knownContextKeys) {
		t.Errorf("context keys = %v, linter knows %v", gotContext, knownContextKeys)
	}

	gotPrincipal := map[string]bool{}
	for key := range entities[req.Principal].Attributes.Keys() {
		gotPrincipal[string(key)] = true
	}
	if !sameKeys(gotPrincipal, knownPrincipalAttrs) {
		t.Errorf("principal attrs = %v, linter knows %v", gotPrincipal, knownPrincipalAttrs)
	}

	// Entity types and the action-ID prefix are what lintScopes rejects
	// policies against, so pin them here too.
	if req.Principal.Type != "User" || req.Action.Type != "Action" || req.Resource.Type != "Tool" {
		t.Errorf("entity types = %s/%s/%s, linter expects User/Action/Tool",
			req.Principal.Type, req.Action.Type, req.Resource.Type)
	}
	if !strings.HasPrefix(string(req.Action.ID), actionIDPrefix) {
		t.Errorf("action ID %q does not carry the %q prefix the linter requires", req.Action.ID, actionIDPrefix)
	}

	// The action and resource entities must stay attribute-free, which is
	// what makes the no-such-attribute rule sound.
	if n := entities[req.Action].Attributes.Len(); n != 0 {
		t.Errorf("action entity carries %d attributes; the no-such-attribute rule assumes none", n)
	}
	if n := entities[req.Resource].Attributes.Len(); n != 0 {
		t.Errorf("resource entity carries %d attributes; the no-such-attribute rule assumes none", n)
	}
}

func sameKeys(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// A structural-tier abort must leave the same audit evidence as a
// registry-tier one. It previously did not: LoadPolicySetFromFile
// returned its error before newPolicyEngineFromConfig could emit
// anything, so the simpler and more common defect class — a misspelled
// attribute — aborted the run silently while docs promised otherwise.
func TestNewPolicyEngine_StructuralErrorIsAudited(t *testing.T) {
	path := filepath.Join(t.TempDir(), "structural.cedar")
	src := `forbid (principal, action, resource) when { context.inputs.command like "*rm*" };`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := &fakeEmitter{}
	_, err := New(
		types.PermissionPolicyConfig{Type: "policy-engine", PolicyFile: path, Fallback: "deny-all"},
		PolicyEngineEnv{ToolSchemas: harnessSchemas(), Security: rec},
		func(string) (PermissionPolicy, error) { return NewDenyAll(), nil },
	)
	if err == nil {
		t.Fatal("expected construction to abort")
	}
	events := rec.snapshot()
	if len(events) != 1 {
		t.Fatalf("expected exactly one policy_lint event, got %+v", events)
	}
	if events[0].event != "policy_lint" || events[0].level != "error" {
		t.Fatalf("event = %+v, want an error-level policy_lint", events[0])
	}
	if events[0].data["rule"] != LintRuleUnknownContextAttribute {
		t.Fatalf("event does not carry the rule: %+v", events[0].data)
	}
}

// The generic descent in collectURLGroups hands each child back to a
// fresh polarity-aware walk. Node types with more than one child are the
// ones that stress it, and neither `&&`/`||`/`!` (handled explicitly) nor
// a bare `like` reaches that branch at all.
func TestWildcardHostPattern_MultiChildNodes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "like nested in an if/then/else",
			body: `if principal.mode == "execution" then context.input.url like "*.github.com/*" else false`,
			want: true,
		},
		{
			name: "anchored like nested in an if/then/else",
			body: `if principal.mode == "execution" then context.input.url like "https://github.com/*" else false`,
			want: false,
		},
		{
			name: "like inside a set literal's element",
			body: `[context.input.url like "*.github.com/*"].contains(true)`,
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := `permit (principal, action, resource) when { ` + tc.body + ` };`
			findings := LintPolicySetStructure(mustParse(t, src))
			if got := len(findings) > 0; got != tc.want {
				t.Fatalf("flagged = %v, want %v (%v)", got, tc.want, findings)
			}
		})
	}
}

// normaliseFindings exists to collapse the same defect reported from
// several disjuncts of one clause.
func TestLint_DuplicateFindingsCollapse(t *testing.T) {
	src := `forbid (principal, action, resource) when {
		context.inputs.a like "x" || context.inputs.a like "y" || context.inputs.a like "z"
	};`
	assertRules(t, LintPolicySetStructure(mustParse(t, src)), LintRuleUnknownContextAttribute+":error")
}

// Findings are ordered by source line so a concatenated policy file —
// the documented way to compose policies — reports top to bottom. A
// plain string sort over cedar-go's "policy0".."policyN" IDs puts
// "policy10" before "policy2", which scrambles any file past ten
// statements.
func TestLint_FindingsFollowSourceOrder(t *testing.T) {
	var sb strings.Builder
	for i := range 12 {
		fmt.Fprintf(&sb, "forbid (principal, action, resource) when { context.bad%02d == \"x\" };\n", i)
	}
	findings := LintPolicySetStructure(mustParse(t, sb.String()))
	if len(findings) != 12 {
		t.Fatalf("expected 12 findings, got %d: %v", len(findings), findings)
	}
	for i, f := range findings {
		if f.Line != i+1 {
			t.Fatalf("finding %d is at line %d, want %d — order does not follow the file", i, f.Line, i+1)
		}
		if want := fmt.Sprintf("context.bad%02d", i); !strings.Contains(f.Message, want) {
			t.Fatalf("finding %d names %q, want %s", i, f.Message, want)
		}
	}
}
