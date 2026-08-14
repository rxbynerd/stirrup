package permission

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cedar-policy/cedar-go"

	"github.com/rxbynerd/stirrup/harness/internal/security"
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

// A differential probe over the wildcard-host rule: mechanically insert
// wildcards at every position of a canonical allow-list URL, and for
// every pattern the linter passes, assert the REAL Cedar authorizer
// denies a corpus of URLs on a DIFFERENT host. The rule's whole job is
// to make "the linter said nothing" mean "the host is pinned", so that
// implication is what gets tested rather than the rule's internals.
//
// Two formulations shipped inside this branch failed exactly here:
// gating on a literal "://" let `*.github.com/*` and
// `https:*/github.com/*` through, and treating a pattern with no
// literal "://" as schemeless let `https:/*p` through — its `/` at
// offset 6 belongs to the scheme separator, not the path.
//
// Scope note: this pins HOST anchoring only. A pattern that permits a
// wider path on the same host (`https://github.com/*` reaching
// /a and /b alike) is the allow-list working as written, not a defect,
// so every probe URL below sits on a host the pattern never names.
func TestWildcardHostPattern_NoAuthorizedBypass(t *testing.T) {
	bases := []string{
		"https://github.com/p",
		"https://a.b-c.example.co.uk:8443/x/y?q=1#f",
		"http://h/p",
		"git+ssh://git@github.com/o/r",
		"ftp://ftp.example.com/pub",
		"https://1.2.3.4/p",
	}
	otherHost := []string{
		"https://evil.example/x.github.com/p",
		"https://evil.example/github.com/p",
		"https://evil.example//github.com/p",
		"https://evil.example/p/https://github.com/p",
		"https://evil.example/https://github.com/p",
		"https://evil.example/?u=https://github.com/p",
		"https://evil.example/p?https://github.com/p",
		"https://evil.example/p#https://github.com/p",
		"https://github.com@evil.example/p",
		"https://github.com:pw@evil.example/p",
		"https://user:pw@evil.example/p",
		"https://github.com.evil.example/p",
		"https://evil.example/x.b-c.example.co.uk/y",
		"https://h@evil.example/p",
		"https://evil.example/h/p",
		"https://evil.example//h/p",
		"https://notgithub.com/p",
		"https://evil.example/p",
		"http://evil.example/p",
		"HTTPS://evil.example/p",
		"ftp://evil.example/pub",
		"git+ssh://git@evil.example/o/r",
	}

	seen := map[string]bool{}
	var clean int
	check := func(t *testing.T, pattern string) {
		t.Helper()
		if seen[pattern] {
			return
		}
		seen[pattern] = true

		src := fmt.Sprintf(`permit (principal, action == Action::"tool:web_fetch", resource == Tool::"web_fetch")
			when { context.input.url like "%s" };`, pattern)
		ps, err := cedar.NewPolicySetFromBytes("probe.cedar", []byte(src))
		if err != nil {
			return // not a legal Cedar pattern literal
		}
		if len(LintPolicySetStructure(ps)) > 0 {
			return
		}
		clean++

		policy, err := NewPolicyEnginePolicy(PolicyEngineConfig{PolicySet: ps, Fallback: NewDenyAll()})
		if err != nil {
			t.Fatalf("build policy for %q: %v", pattern, err)
		}
		for _, url := range otherHost {
			input, err := json.Marshal(map[string]string{"url": url})
			if err != nil {
				t.Fatal(err)
			}
			res, err := policy.Check(t.Context(), types.ToolDefinition{Name: "web_fetch"}, input)
			if err != nil {
				t.Fatalf("check %q against %q: %v", url, pattern, err)
			}
			if res.Allowed {
				t.Errorf("pattern %q passes the lint but authorizes %q", pattern, url)
			}
		}
	}

	for _, base := range bases {
		for i := 0; i <= len(base); i++ {
			check(t, base[:i]+"*"+base[i:])
			for j := i; j <= len(base); j++ {
				check(t, base[:i]+"*"+base[j:])
				check(t, base[:i]+"*"+base[i:j]+"*"+base[j:])
			}
		}
	}
	// Shapes the mechanical insertion cannot produce.
	for _, pattern := range []string{
		"*", "*.github.com/*", "github.com*", "https:*/github.com/*",
		"*://github.com/*", "https://*", "https://github.com*",
		"HTTPS://github.com/*", "https://github.com:443/*",
		"https://user@github.com/*", "https://[::1]/*",
		`https://git\*hub.com/*`,
	} {
		check(t, pattern)
	}

	// Guards against the probe passing vacuously: if a future change
	// flagged every pattern, no authorization would be exercised at all.
	if clean == 0 {
		t.Fatal("no pattern survived the lint — the probe authorized nothing and proves nothing")
	}
	t.Logf("%d of %d patterns passed the lint; none authorized another host", clean, len(seen))
}

// A URL attribute that is not a string must not slip past a policy that
// constrains it: Cedar raises a type error on `like` against a Long or a
// Record, the policy abstains, and the fallback decides. buildRequest
// also wraps a non-object tool input as context.input.raw, which no
// url clause names.
func TestWildcardHostPattern_NonStringURLDoesNotAuthorize(t *testing.T) {
	src := `permit (principal, action, resource) when { context.input.url like "https://github.com/*" };`
	ps := mustParse(t, src)
	if findings := LintPolicySetStructure(ps); len(findings) != 0 {
		t.Fatalf("unexpected findings: %v", findings)
	}
	policy, err := NewPolicyEnginePolicy(PolicyEngineConfig{PolicySet: ps, Fallback: NewDenyAll()})
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{
		`{"url":123}`, `{"url":{"a":1}}`, `{"url":["x"]}`, `{"url":null}`,
		`{}`, `"a raw string"`, `null`, `123`,
	} {
		res, err := policy.Check(t.Context(), types.ToolDefinition{Name: "web_fetch"}, json.RawMessage(input))
		if err != nil {
			t.Errorf("input %s: %v", input, err)
			continue
		}
		if res.Allowed {
			t.Errorf("input %s was authorized by a url-constrained permit", input)
		}
	}
}

// The mirror of the probe above: legitimate allow-list spellings must
// survive a rule strict enough to reject everything else. Ports,
// userinfo, IPv6 literals, an uppercase scheme, and an escaped literal
// asterisk are all fully-anchored and must not be flagged.
func TestWildcardHostPattern_AnchoredSpellingsSurvive(t *testing.T) {
	for _, pattern := range []string{
		`https://github.com/*`,
		`https://github.com/p*`,
		`https://github.com/*p*`,
		`https://github.com:443/*`,
		`https://user@github.com/*`,
		`https://[::1]/*`,
		`HTTPS://github.com/*`,
		`https://git\*hub.com/*`,
		`https://github.com?*`,
		`https://github.com/#*`,
	} {
		t.Run(pattern, func(t *testing.T) {
			src := `permit (principal, action, resource) when { context.input.url like "` + pattern + `" };`
			if findings := LintPolicySetStructure(mustParse(t, src)); len(findings) != 0 {
				t.Fatalf("anchored pattern rejected: %v", findings)
			}
		})
	}
}

// Polarity must survive a negation expressed through a node the DNF walk
// does not model. `X == false`, `X != true`, `if X then false else true`
// and `[X].contains(false)` all mean `!X`; wrapped in an outer `!`, the
// two negations cancel and the enclosed `like` is genuinely in a
// widening position. An earlier walk passed the parent's polarity through
// unrecognised nodes unchanged, so it flipped once instead of twice,
// dropped the clause from the DNF, and reported a policy clean that the
// real authorizer used to grant any host.
//
// Assertions run against cedar.Authorize, not just the lint result: this
// class of bug is invisible to a test that only inspects findings for a
// pattern it already expects to be flagged.
func TestWildcardHostPattern_NegationThroughUnmodelledNodes(t *testing.T) {
	const unanchored = `context.input.url like "*.github.com/*"`
	cases := []struct {
		name string
		body string
		want bool // want a wildcard-host finding
	}{
		{"double negation via == false", `!((` + unanchored + `) == false)`, true},
		{"double negation via literal on the left", `!(false == (` + unanchored + `))`, true},
		{"single negation via != false", `(` + unanchored + `) != false`, true},
		{"double negation via if/then/else", `!(if (` + unanchored + `) then false else true)`, true},
		{"double negation via contains", `!([` + unanchored + `].contains(false))`, true},
		{"identity via == true is still widening", `(` + unanchored + `) == true`, true},

		// The mirror: these genuinely narrow, and a fail-closed rule must
		// not reject them.
		{
			name: "negation via == true narrows, alongside an anchor",
			body: `!((context.input.url like "*evil*") == true) && context.input.url like "https://github.com/*"`,
			want: false,
		},
		{
			name: "plain negation narrows, alongside an anchor",
			body: `!(context.input.url like "*evil*") && context.input.url like "https://github.com/*"`,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := `permit (principal, action == Action::"tool:web_fetch", resource == Tool::"web_fetch")
				when { ` + tc.body + ` };`
			ps := mustParse(t, src)
			findings := LintPolicySetStructure(ps)
			if got := len(findings) > 0; got != tc.want {
				t.Fatalf("flagged = %v, want %v (%v)", got, tc.want, findings)
			}

			// Cross-check the claim the lint result stands for: a clean
			// verdict must mean the authorizer denies another host.
			if tc.want {
				return
			}
			policy, err := NewPolicyEnginePolicy(PolicyEngineConfig{PolicySet: ps, Fallback: NewDenyAll()})
			if err != nil {
				t.Fatal(err)
			}
			input, err := json.Marshal(map[string]string{"url": "https://evil.example/x.github.com/y"})
			if err != nil {
				t.Fatal(err)
			}
			res, err := policy.Check(t.Context(), types.ToolDefinition{Name: "web_fetch"}, input)
			if err != nil {
				t.Fatal(err)
			}
			if res.Allowed {
				t.Fatalf("policy lints clean but authorizes an attacker host: %s", tc.body)
			}
		})
	}
}

// Metamorphic guard over the DNF walk. Rewriting a clause into a
// logically equivalent form must not change the linter's verdict — if
// `url like P` is flagged, so must every spelling that means the same
// thing, however it is dressed up in comparisons, muxes, or paired
// negations.
//
// This is the generalisation of the bypass that shipped in bd53dd82: a
// probe that varies PATTERNS cannot find a bug in how policy BODIES are
// walked, and three rounds of hand-written body cases each covered only
// the shapes their author had thought of. Equivalence is a property the
// walk must have for shapes nobody enumerated.
func TestWildcardHostPattern_EquivalentBodiesAgree(t *testing.T) {
	// Each entry rewrites %s — a clause — into an equivalent one.
	equivalent := []string{
		`%s`,
		`(%s)`,
		`!(!(%s))`,
		`(%s) == true`,
		`(%s) != false`,
		`true == (%s)`,
		`!((%s) == false)`,
		`!((%s) != true)`,
		`(%s) && true`,
		`(%s) || false`,
		`true && (%s)`,
		`if (%s) then true else false`,
		`!(if (%s) then false else true)`,
		`[(%s)].contains(true)`,
		`![(%s)].contains(false)`,
		`(if (%s) then true else false) == true`,
	}
	// Both an unanchored clause (must be flagged) and an anchored one
	// (must not be), so an implementation that flagged everything or
	// nothing cannot satisfy the test.
	clauses := []struct {
		name string
		expr string
		want bool
	}{
		{"unanchored", `context.input.url like "*.github.com/*"`, true},
		{"anchored", `context.input.url like "https://github.com/*"`, false},
	}

	for _, clause := range clauses {
		t.Run(clause.name, func(t *testing.T) {
			for _, shape := range equivalent {
				body := fmt.Sprintf(shape, clause.expr)
				src := `permit (principal, action == Action::"tool:web_fetch", resource == Tool::"web_fetch")
					when { ` + body + ` };`
				ps, err := cedar.NewPolicySetFromBytes("equiv.cedar", []byte(src))
				if err != nil {
					t.Fatalf("parse %q: %v", body, err)
				}
				findings := LintPolicySetStructure(ps)
				if got := len(findings) > 0; got != clause.want {
					t.Errorf("body %q: flagged = %v, want %v (%v)", body, got, clause.want, findings)
					continue
				}
				if clause.want {
					continue
				}
				// A clean verdict must still mean the host is pinned.
				policy, err := NewPolicyEnginePolicy(PolicyEngineConfig{PolicySet: ps, Fallback: NewDenyAll()})
				if err != nil {
					t.Fatal(err)
				}
				input, err := json.Marshal(map[string]string{"url": "https://evil.example/x.github.com/y"})
				if err != nil {
					t.Fatal(err)
				}
				res, err := policy.Check(t.Context(), types.ToolDefinition{Name: "web_fetch"}, input)
				if err != nil {
					t.Fatal(err)
				}
				if res.Allowed {
					t.Errorf("body %q lints clean but authorizes an attacker host", body)
				}
			}
		})
	}
}

// The rule is fail-closed, so a false positive bricks a run. These are
// shapes an operator would plausibly write, all genuinely safe.
func TestWildcardHostPattern_NoFalsePositives(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"forbid deny-list", `forbid (principal, action, resource) when { context.input.url like "*evil*" };`},
		{"forbid deny-list via == true", `forbid (principal, action, resource) when { (context.input.url like "*evil*") == true };`},
		{"forbid deny-list inside an if", `forbid (principal, action, resource) when { if principal.mode == "execution" then (context.input.url like "*evil*") else false };`},
		{"permit with an unless deny-list", `permit (principal, action, resource) unless { context.input.url like "*evil*" };`},
		{"has-guarded anchored allow-list", `permit (principal, action == Action::"tool:web_fetch", resource == Tool::"web_fetch") when { context.input has url && (context.input.url like "https://github.com/*" || context.input.url like "https://api.github.com/*") };`},
		{"anchor in one condition, deny-list in another", `permit (principal, action, resource) when { context.input has url } unless { context.input.url like "*evil*" } when { context.input.url like "https://github.com/*" };`},
		{"wildcard confined to the query", `permit (principal, action, resource) when { context.input.url like "https://api.github.com/search?q=*" };`},
		{"three conjuncts, anchor last", `permit (principal, action, resource) when { context.input.url like "*.json" && context.input.url like "*v2*" && context.input.url like "https://github.com/*" };`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if findings := LintPolicySetStructure(mustParse(t, tc.src)); len(findings) != 0 {
				t.Fatalf("safe policy rejected: %v", findings)
			}
		})
	}
}

// Anchoring is evaluated bottom-up over a three-valued lattice rather
// than by materialising disjunctive normal form, which is exponential in
// the number of conjoined disjunctions. The DNF version needed a
// cross-product budget and, past it, degraded to treating conjuncts as
// disjuncts — so an anchored policy with enough additional constraints
// was rejected. Scale is therefore a correctness property, not just a
// performance one.
func TestWildcardHostPattern_WideConjunctionsStayLinear(t *testing.T) {
	for _, n := range []int{4, 8, 16, 32, 64} {
		t.Run(fmt.Sprintf("conjuncts=%d", n), func(t *testing.T) {
			parts := []string{`context.input.url like "https://github.com/*"`}
			for range n {
				parts = append(parts, `(context.input.url like "*a*" || context.input.url like "*b*")`)
			}
			src := `permit (principal, action, resource) when { ` + strings.Join(parts, " && ") + ` };`
			if findings := LintPolicySetStructure(mustParse(t, src)); len(findings) != 0 {
				t.Fatalf("anchored policy rejected at %d conjuncts: %v", n, findings)
			}

			// The mirror: drop the anchor and it must still be caught.
			unanchored := `permit (principal, action, resource) when { ` + strings.Join(parts[1:], " && ") + ` };`
			if findings := LintPolicySetStructure(mustParse(t, unanchored)); len(findings) == 0 {
				t.Fatalf("unanchored policy accepted at %d conjuncts", n)
			}
		})
	}
}

// The operand of `like` need not be a bare attribute chain. An
// if/then/else selecting between fields, or a record literal accessed by
// member, both evaluate to the attribute at runtime — but neither
// resolves through attributePath, and a clause the rule cannot resolve
// used to be skipped rather than judged. That failed OPEN: both policies
// below linted clean while the real authorizer granted any host.
//
// The mirror matters as much: an operand with no URL attribute anywhere
// inside is still not this rule's business.
func TestWildcardHostPattern_UnresolvableOperands(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "if/then/else selecting the same url field",
			body: `(if principal.mode == "execution" then context.input.url else context.input.url) like "*.github.com/*"`,
			want: true,
		},
		{
			name: "if/then/else selecting between two fields",
			body: `(if principal.mode == "execution" then context.input.url else context.input.callbackUri) like "*.github.com/*"`,
			want: true,
		},
		{
			name: "record literal accessed by member",
			body: `{u: context.input.url}.u like "*.github.com/*"`,
			want: true,
		},
		{
			name: "anchored pattern through an unresolvable operand is fine",
			body: `{u: context.input.url}.u like "https://github.com/*"`,
			want: false,
		},
		{
			name: "no URL attribute in the operand is not this rule's business",
			body: `(if principal.mode == "execution" then context.input.command else context.input.command) like "*rm -rf*"`,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := `permit (principal, action == Action::"tool:web_fetch", resource == Tool::"web_fetch")
				when { ` + tc.body + ` };`
			ps := mustParse(t, src)
			findings := LintPolicySetStructure(ps)
			if got := len(findings) > 0; got != tc.want {
				t.Fatalf("flagged = %v, want %v (%v)", got, tc.want, findings)
			}
		})
	}
}

// Metamorphic guard on the OPERAND axis, mirroring
// TestWildcardHostPattern_EquivalentBodiesAgree on the body axis.
// Wrapping the attribute reference in an expression that evaluates to
// the same value must not change the verdict — a clause the walk cannot
// resolve has to be judged, not skipped.
func TestWildcardHostPattern_EquivalentOperandsAgree(t *testing.T) {
	// Each entry rewrites %s — an attribute reference — into an
	// expression with the same value.
	equivalent := []string{
		`%s`,
		`(%s)`,
		`(if true then %s else %s)`,
		`(if principal.mode == "execution" then %s else %s)`,
		`{u: %s}.u`,
		`{a: {b: %s}}.a.b`,
		`(if true then {u: %s}.u else %s)`,
	}
	clauses := []struct {
		name    string
		pattern string
		want    bool
	}{
		{"unanchored", `*.github.com/*`, true},
		{"anchored", `https://github.com/*`, false},
	}

	for _, clause := range clauses {
		t.Run(clause.name, func(t *testing.T) {
			for _, shape := range equivalent {
				operand := strings.ReplaceAll(shape, "%s", "context.input.url")
				body := fmt.Sprintf(`%s like "%s"`, operand, clause.pattern)
				src := `permit (principal, action == Action::"tool:web_fetch", resource == Tool::"web_fetch")
					when { ` + body + ` };`
				ps, err := cedar.NewPolicySetFromBytes("operand.cedar", []byte(src))
				if err != nil {
					t.Fatalf("parse %q: %v", body, err)
				}
				findings := LintPolicySetStructure(ps)
				if got := len(findings) > 0; got != clause.want {
					t.Errorf("operand %q: flagged = %v, want %v (%v)", operand, got, clause.want, findings)
					continue
				}
				if clause.want {
					continue
				}
				policy, err := NewPolicyEnginePolicy(PolicyEngineConfig{PolicySet: ps, Fallback: NewDenyAll()})
				if err != nil {
					t.Fatal(err)
				}
				input, err := json.Marshal(map[string]string{"url": "https://evil.example/x.github.com/y"})
				if err != nil {
					t.Fatal(err)
				}
				res, err := policy.Check(t.Context(), types.ToolDefinition{Name: "web_fetch"}, input)
				if err != nil {
					t.Fatal(err)
				}
				if res.Allowed {
					t.Errorf("operand %q lints clean but authorizes an attacker host", operand)
				}
			}
		})
	}
}

// Cross-product oracle. Every wildcard-host defect found on this branch
// was the same shape: some Cedar construct the walk does not model made
// a clause vanish or be misjudged, and the policy linted clean while the
// authorizer granted an attacker host. The single-axis tests above each
// caught one such construct after it was known. This one generates the
// combinations — pattern x operand wrapper x body wrapper x polarity —
// and uses the real Cedar evaluator as the oracle, so a construct nobody
// enumerated is still covered as long as it is built from these pieces.
//
// The property is one-directional on purpose: a lint-clean policy must
// not authorize another host. A flagged policy is not asserted about,
// because over-flagging is the safe direction and false positives are
// pinned separately by TestWildcardHostPattern_NoFalsePositives.
func TestWildcardHostPattern_CrossProductOracle(t *testing.T) {
	operands := []string{
		`context.input.url`,
		`(context.input.url)`,
		`(if true then context.input.url else context.input.url)`,
		`(if principal.mode == "execution" then context.input.url else context.input.url)`,
		`{u: context.input.url}.u`,
		`{a: {b: context.input.url}}.a.b`,
	}
	// Each wraps a clause into a logically equivalent one.
	bodies := []string{
		`%s`,
		`!(!(%s))`,
		`(%s) == true`,
		`(%s) != false`,
		`!((%s) == false)`,
		`!((%s) != true)`,
		`(%s) && true`,
		`true && (%s)`,
		`(%s) || false`,
		`if (%s) then true else false`,
		`!(if (%s) then false else true)`,
		`[(%s)].contains(true)`,
		`![(%s)].contains(false)`,
		`(%s) && context.input has url`,
	}
	patterns := []string{
		`*.github.com/*`, `https://*.github.com/*`, `https:*/github.com/*`,
		`*github.com*`, `github.com/*`, `https://*`, `*`,
		`https://github.com/*`, `https://api.github.com/*`,
	}
	// Both widening arrangements: a `when` on a permit and an `unless` on
	// a forbid. Only these can widen access, so only these must hold.
	scopes := []struct{ effect, condition string }{
		{"permit", "when"},
		{"forbid", "unless"},
	}
	attacker := []string{
		"https://evil.example/x.github.com/y",
		"https://evil.example/github.com/y",
		"https://github.com@evil.example/y",
		"https://evil.example/?u=https://github.com/y",
		"https://notgithub.com/y",
	}

	var clean, total int
	for _, scope := range scopes {
		for _, operand := range operands {
			for _, bodyShape := range bodies {
				for _, pattern := range patterns {
					clause := fmt.Sprintf(`%s like "%s"`, operand, pattern)
					body := strings.ReplaceAll(bodyShape, "%s", clause)
					src := fmt.Sprintf(`%s (principal, action == Action::"tool:web_fetch", resource == Tool::"web_fetch") %s { %s };`,
						scope.effect, scope.condition, body)

					ps, err := cedar.NewPolicySetFromBytes("oracle.cedar", []byte(src))
					if err != nil {
						continue // not a legal Cedar body
					}
					total++
					if len(LintPolicySetStructure(ps)) > 0 {
						continue
					}
					clean++

					// A forbid/unless that lints clean is only meaningful
					// paired with something that permits; the fallback
					// stands in for that.
					fallback := PermissionPolicy(NewDenyAll())
					if scope.effect == "forbid" {
						fallback = NewAllowAll()
					}
					policy, err := NewPolicyEnginePolicy(PolicyEngineConfig{PolicySet: ps, Fallback: fallback})
					if err != nil {
						t.Fatalf("build policy for %q: %v", src, err)
					}
					for _, url := range attacker {
						input, err := json.Marshal(map[string]string{"url": url})
						if err != nil {
							t.Fatal(err)
						}
						res, err := policy.Check(t.Context(), types.ToolDefinition{Name: "web_fetch"}, input)
						if err != nil {
							t.Fatalf("check %q: %v", src, err)
						}
						if res.Allowed {
							t.Errorf("lints clean but authorizes %q:\n%s", url, src)
						}
					}
				}
			}
		}
	}
	if clean == 0 {
		t.Fatal("no policy survived the lint — the oracle proves nothing")
	}
	t.Logf("%d of %d generated policies lint clean; none authorized another host", clean, total)
}

// The undeclared-input-attribute rule asserts a clause is DEAD, and
// aborts the run on that basis. The assertion rests on tool dispatch
// validating input against the JSON Schema before consulting Cedar, so
// it is only sound while NewToolSchema's cheap flat read of the schema
// agrees with the real validator about which property names can reach
// Cedar.
//
// MCP servers supply their own schemas, so the shapes below are ones a
// remote server could plausibly send — including the composition forms
// ($ref, allOf, anyOf) where a hand-rolled reader is most likely to
// diverge. The failure that matters is one-directional: the linter must
// never call an attribute dead that the validator would actually let
// through, because that rejects a working policy.
func TestNewToolSchema_AgreesWithTheRealValidator(t *testing.T) {
	schemas := map[string]string{
		"closed":                  `{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":false}`,
		"open by omission":        `{"type":"object","properties":{"a":{"type":"string"}}}`,
		"additionalProperties {}": `{"type":"object","properties":{"a":{}},"additionalProperties":{"type":"string"}}`,
		"additionalProperties tr": `{"type":"object","properties":{"a":{}},"additionalProperties":true}`,
		"properties via $ref":     `{"type":"object","$ref":"#/$defs/P","additionalProperties":false,"$defs":{"P":{"properties":{"a":{"type":"string"}}}}}`,
		"properties via allOf":    `{"type":"object","allOf":[{"properties":{"a":{"type":"string"}}}],"additionalProperties":false}`,
		"properties via anyOf":    `{"type":"object","anyOf":[{"properties":{"a":{}}},{"properties":{"b":{}}}],"additionalProperties":false}`,
		"empty schema object":     `{}`,
		"closed, no properties":   `{"type":"object","additionalProperties":false}`,
		"non-object root":         `{"type":"string"}`,
		"nested properties":       `{"type":"object","properties":{"a":{"type":"object","properties":{"b":{}}}},"additionalProperties":false}`,
	}

	for name, raw := range schemas {
		t.Run(name, func(t *testing.T) {
			schema := NewToolSchema(json.RawMessage(raw))
			// Would the linter call `context.input.a` dead for this tool?
			callsItDead := schema.Closed && !schema.Properties["a"]
			// Does the real validator let a call carrying `a` through?
			validatorAccepts := security.ValidateJSONSchema(
				json.RawMessage(`{"a":"x"}`), json.RawMessage(raw)) == nil

			if callsItDead && validatorAccepts {
				t.Fatalf("linter would reject a policy keyed on context.input.a as dead, but the validator accepts that input — a working policy would fail to load")
			}
		})
	}
}
