package permission

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cedar-policy/cedar-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	must := require.New(t)
	must.Equal(strings.Join(want, ","), strings.Join(findingRules(findings), ","), "full: %v", findings)
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
			name: "principal is an entity type the harness never mints",
			src:  `forbid (principal is Agent, action, resource);`,
			want: []string{LintRuleUnknownScopeEntity + ":error"},
		},
		{
			name: "resource is-in names a type the harness never mints",
			src:  `forbid (principal, action, resource is Agent in Tool::"run_command");`,
			want: []string{LintRuleUnknownScopeEntity + ":error"},
		},
		{
			name: "well-formed is / is-in scopes",
			src:  `forbid (principal is User, action, resource is Tool in Tool::"run_command");`,
			want: nil,
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

		// Patterns that leave the scheme or host open are flagged, including
		// forms that split the "://" separator.
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
			must := require.New(t)
			src := `permit (principal, action == Action::"tool:web_fetch", resource == Tool::"web_fetch")
				when { context.input.url like "` + tc.pattern + `" };`
			findings := LintPolicySetStructure(mustParse(t, src))
			must.Equal(tc.flagged, len(findings) > 0, "findings: %v", findings)
			if tc.flagged {
				must.Equal(LintRuleWildcardHostPattern, findings[0].Rule)
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
			must := require.New(t)
			src := `permit (principal, action == Action::"tool:web_fetch", resource == Tool::"web_fetch")
				when { ` + tc.body + ` };`
			findings := LintPolicySetStructure(mustParse(t, src))
			must.Equal(tc.want, len(findings) > 0, "findings: %v", findings)
		})
	}
}

// De Morgan: negation swaps the roles of && and ||, so the DNF of a
// negated subtree must come out correct rather than approximated.
func TestWildcardHostPattern_NegatedDisjunctionIsCoveredByAnchor(t *testing.T) {
	must := require.New(t)
	// forbid/unless widens, and !(A || B) == !A && !B, so the inner
	// disjuncts become conjuncts: one anchor covers the group.
	src := `forbid (principal, action, resource) unless {
		!(!(context.input.url like "https://github.com/*") || !(context.input.url like "*.json"))
	};`
	must.Empty(LintPolicySetStructure(mustParse(t, src)), "expected the anchored conjunct to cover the group")
}

// The rule only fires where over-matching WIDENS access. Over-matching a
// deny-list is fail-safe, and flagging it would push operators to narrow
// a forbid.
func TestWildcardHostPattern_FlagsOnlyWideningPolarity(t *testing.T) {
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
			must := require.New(t)
			findings := LintPolicySetStructure(mustParse(t, tc.src))
			must.Equal(tc.want, len(findings) > 0, "findings: %v", findings)
		})
	}
}

// The rule is scoped to URL-valued attributes so a substring probe over a
// shell command — where a wildcard before the host is the whole point —
// is not flagged.
func TestWildcardHostPattern_IgnoresNonURLAttributes(t *testing.T) {
	src := `permit (principal, action == Action::"tool:run_command", resource == Tool::"run_command")
		when { context.input.command like "*curl https://github.com/*" };`
	assertRules(t, LintPolicySetStructure(mustParse(t, src)))
}

func TestWildcardHostPattern_FlagsURLSuffixedAttributes(t *testing.T) {
	// webhookUrl / callbackURI are as URL-valued as `url` itself.
	for _, attr := range []string{"webhookUrl", "callbackURI", "baseUri"} {
		t.Run(attr, func(t *testing.T) {
			must := require.New(t)
			src := `permit (principal, action, resource) when { context.input.` + attr + ` like "https://*.github.com/*" };`
			findings := LintPolicySetStructure(mustParse(t, src))
			must.Len(findings, 1)
			must.Equal(LintRuleWildcardHostPattern, findings[0].Rule)
		})
	}
}

// --- @stirrupLintIgnore ----------------------------------------------

func TestLintIgnoreAnnotation_DowngradesToWarning(t *testing.T) {
	must := require.New(t)
	src := `@stirrupLintIgnore("wildcard-host-pattern")
	permit (principal, action == Action::"tool:web_fetch", resource == Tool::"web_fetch")
	when { context.input.url like "https://*.github.com/*" };`

	findings := LintPolicySetStructure(mustParse(t, src))
	assertRules(t, findings, LintRuleWildcardHostPattern+":warning")
	must.Contains(findings[0].Message, "downgraded to a warning", "message does not record the downgrade")
	must.NoError(LintErrors("test", findings), "warning-only findings must not abort")
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

func TestLintIgnoreAnnotation_SuppressesAllListedRules(t *testing.T) {
	must := require.New(t)
	src := `@stirrupLintIgnore("wildcard-host-pattern, unknown-principal-attribute")
	permit (principal, action, resource) when {
		context.input.url like "https://*.github.com/*" && principal.runID == "x"
	};`
	findings := LintPolicySetStructure(mustParse(t, src))
	must.NoError(LintErrors("test", findings), "both rules ignored, expected no error")
}

// --- registry-aware tier ---------------------------------------------

// A forbid keyed on `cmd` never fires, because run_command declares only
// `command`; the lint must report it.
func TestLintPolicySetTools_FlagsCmdKeyForCommandProperty(t *testing.T) {
	must := require.New(t)
	src := `forbid (
		principal,
		action == Action::"tool:run_command",
		resource == Tool::"run_command"
	) when {
		context.input has cmd && context.input.cmd like "*rm -rf*"
	};`
	findings := LintPolicySetTools(mustParse(t, src), harnessSchemas())
	assertRules(t, findings, LintRuleUndeclaredInputAttribute+":error")
	must.Contains(findings[0].Message, "context.input.cmd", "message does not name the attribute")
	must.Contains(findings[0].Message, "command", "message does not list the declared properties")
}

func TestLintPolicySetTools_ChecksAttributesAgainstScopedTools(t *testing.T) {
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
	must := require.New(t)
	schemas := harnessSchemas()
	schemas["mcp_jira_search"] = ToolSchema{Properties: map[string]bool{"jql": true}, Closed: false}

	src := `forbid (principal, action == Action::"tool:mcp_jira_search", resource)
		when { context.input.project like "*" };`
	findings := LintPolicySetTools(mustParse(t, src), schemas)
	assertRules(t, findings, LintRuleUnverifiableInputAttribute+":warning")
	must.Contains(findings[0].Message, "mcp_jira_search", "message does not name the open-schema tool")
}

// An empty schema map disables the tier: the dry-run preflight builds
// components against an empty registry, and reporting every attribute as
// undeclared there would be noise, not signal.
func TestLintPolicySetTools_EmptySchemasDisablesTier(t *testing.T) {
	must := require.New(t)
	src := `forbid (principal, action == Action::"tool:run_command", resource)
		when { context.input.cmd like "*rm*" };`
	findings := LintPolicySetTools(mustParse(t, src), nil)
	must.Nil(findings, "expected no findings against an empty registry")
}

func TestLintPolicySetTools_IgnoreAnnotationApplies(t *testing.T) {
	src := `@stirrupLintIgnore("undeclared-input-attribute")
	forbid (principal, action == Action::"tool:run_command", resource == Tool::"run_command")
	when { context.input.cmd like "*rm -rf*" };`
	findings := LintPolicySetTools(mustParse(t, src), harnessSchemas())
	assertRules(t, findings, LintRuleUndeclaredInputAttribute+":warning")
}

// --- NewToolSchema ---------------------------------------------------

func TestNewToolSchema_ReadsPropertiesAndClosedness(t *testing.T) {
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
			must := require.New(t)
			got := NewToolSchema(json.RawMessage(tc.raw))
			must.Equal(tc.wantClosed, got.Closed)
			must.Len(got.Properties, len(tc.wantProps), "properties: %v", got.Properties)
			for _, p := range tc.wantProps {
				must.True(got.Properties[p], "missing property %q", p)
			}
		})
	}
}

// --- load / construction integration ---------------------------------

func TestLoadPolicySetFromFile_RejectsStructuralDefect(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "bad.cedar")
	src := `forbid (principal, action, resource) when { context.inputs.command like "*rm*" };`
	must.NoError(os.WriteFile(path, []byte(src), 0o600))
	_, err := LoadPolicySetFromFile(path)
	must.Error(err, "expected LoadPolicySetFromFile to reject a dead clause")
	must.ErrorContains(err, LintRuleUnknownContextAttribute)
	must.ErrorContains(err, "line 1", "error does not locate the policy")
}

// The dry-run preflight and every other load path must still accept a
// well-formed file.
func TestLoadPolicySetFromFile_AcceptsShippedStarters(t *testing.T) {
	must := require.New(t)
	dir := filepath.Join("..", "..", "..", "examples", "policies")
	entries, err := os.ReadDir(dir)
	must.NoError(err, "read %s", dir)
	checked := 0
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".cedar" {
			continue
		}
		checked++
		t.Run(entry.Name(), func(t *testing.T) {
			must := require.New(t)
			ps, err := LoadPolicySetFromFile(filepath.Join(dir, entry.Name()))
			must.NoError(err, "shipped starter fails the structural lint")
			must.Empty(LintPolicySetStructure(ps), "shipped starter produced findings")
		})
	}
	must.NotZero(checked, "no .cedar files found in %s; directory moved?", dir)
}

func TestNewPolicyEngine_LintAbortsConstruction(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "typo.cedar")
	src := `forbid (
		principal,
		action == Action::"tool:run_command",
		resource == Tool::"run_command"
	) when {
		context.input.cmd like "*rm -rf*"
	};`
	must.NoError(os.WriteFile(path, []byte(src), 0o600))

	rec := &fakeEmitter{}
	_, err := New(
		types.PermissionPolicyConfig{Type: "policy-engine", PolicyFile: path, Fallback: "deny-all"},
		PolicyEngineEnv{ToolSchemas: harnessSchemas(), Security: rec},
		func(string) (PermissionPolicy, error) { return NewDenyAll(), nil },
	)
	must.Error(err, "expected construction to abort on an undeclared input attribute")
	must.ErrorContains(err, LintRuleUndeclaredInputAttribute)
	events := rec.snapshot()
	must.Len(events, 1)
	must.Equal("error", events[0].level)
	must.Equal("policy_lint", events[0].event)
	must.Equal(LintRuleUndeclaredInputAttribute, events[0].data["rule"], "event does not carry the rule")
}

func TestNewPolicyEngine_WarningsAreEmittedAndSurvive(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "mcp.cedar")
	src := `forbid (
		principal,
		action == Action::"tool:mcp_jira_search",
		resource == Tool::"mcp_jira_search"
	) when {
		context.input.jql like "*DROP*"
	};`
	must.NoError(os.WriteFile(path, []byte(src), 0o600))

	rec := &fakeEmitter{}
	pp, err := New(
		types.PermissionPolicyConfig{Type: "policy-engine", PolicyFile: path, Fallback: "deny-all"},
		PolicyEngineEnv{ToolSchemas: harnessSchemas(), Security: rec},
		func(string) (PermissionPolicy, error) { return NewDenyAll(), nil },
	)
	must.NoError(err, "an unregistered tool must warn, not abort")
	must.NotNil(pp, "expected a constructed policy")

	events := rec.snapshot()
	must.Len(events, 1, "expected one unknown-tool warning")
	must.Equal("warn", events[0].level)
	must.Equal(LintRuleUnknownTool, events[0].data["rule"])
}

// --- finding formatting ----------------------------------------------

func TestLintFinding_String_FormatsWithAndWithoutLine(t *testing.T) {
	must := require.New(t)
	f := LintFinding{PolicyID: "policy0", Rule: LintRuleUnknownTool, Message: "nope", Line: 7}
	must.Equal(`[unknown-tool] policy0 (line 7): nope`, f.String())

	f.Line = 0
	must.Equal(`[unknown-tool] policy0: nope`, f.String())
}

func TestLintErrors_IgnoresWarnings(t *testing.T) {
	must := require.New(t)
	findings := []LintFinding{
		{PolicyID: "p0", Rule: "a", Severity: LintWarning, Message: "w"},
	}
	must.NoError(LintErrors("src", findings), "warnings must not produce an error")

	findings = append(findings, LintFinding{PolicyID: "p1", Rule: "b", Severity: LintError, Message: "e"})
	err := LintErrors("src", findings)
	must.Error(err)
	must.NotContains(err.Error(), "[a]", "warning leaked into the error")
}

// Multiple statements in one file are all linted, and findings are
// reported against the statement that produced them.
func TestLintPolicySetStructure_AttributesFindingsPerStatement(t *testing.T) {
	must := require.New(t)
	src := `permit (principal, action == Action::"tool:web_fetch", resource == Tool::"web_fetch")
	when { context.input.url like "https://github.com/*" };

	forbid (principal, action, resource) when { principal.role == "admin" };`
	findings := LintPolicySetStructure(mustParse(t, src))
	assertRules(t, findings, LintRuleUnknownPrincipalAttribute+":error")
	must.Equal("policy1", findings[0].PolicyID)
	must.Equal(4, findings[0].Line)
}

func TestLint_NilPolicySetReturnsNoFindings(t *testing.T) {
	must := require.New(t)
	must.Nil(LintPolicySetStructure(nil))
	must.Nil(LintPolicySetTools(nil, harnessSchemas()))
}

// The linter's vocabulary of request attributes is a hand-maintained
// mirror of buildRequest. Drift in either direction is a silent failure:
// a new context key the linter does not know about makes it reject valid
// policies, and a removed one makes it accept dead ones. Pin both
// directions against a request built by the real code path.
func TestLintVocabularyMatchesBuildRequest(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err, "NewPolicyEnginePolicy")

	req, entities, err := p.buildRequest(types.ToolDefinition{Name: "run_command"}, []byte(`{"command":"ls"}`))
	must.NoError(err, "buildRequest")

	t.Run("context keys match the linter vocabulary", func(t *testing.T) {
		must := require.New(t)
		gotContext := map[string]bool{}
		for key := range req.Context.Keys() {
			gotContext[string(key)] = true
		}
		must.True(sameKeys(gotContext, knownContextKeys), "context keys = %v, linter knows %v", gotContext, knownContextKeys)
	})

	t.Run("principal attributes match the linter vocabulary", func(t *testing.T) {
		must := require.New(t)
		gotPrincipal := map[string]bool{}
		for key := range entities[req.Principal].Attributes.Keys() {
			gotPrincipal[string(key)] = true
		}
		must.True(sameKeys(gotPrincipal, knownPrincipalAttrs), "principal attrs = %v, linter knows %v", gotPrincipal, knownPrincipalAttrs)
	})

	// Entity types and the action-ID prefix are what lintScopes rejects
	// policies against, so pin them here too.
	t.Run("entity types match the scopes the linter expects", func(t *testing.T) {
		must := require.New(t)
		must.EqualValues("User", req.Principal.Type)
		must.EqualValues("Action", req.Action.Type)
		must.EqualValues("Tool", req.Resource.Type)
	})

	t.Run("action ID carries the linter prefix", func(t *testing.T) {
		must := require.New(t)
		must.True(strings.HasPrefix(string(req.Action.ID), actionIDPrefix), "action ID %q does not carry the %q prefix the linter requires", req.Action.ID, actionIDPrefix)
	})

	// The action and resource entities must stay attribute-free, which is
	// what makes the no-such-attribute rule sound.
	t.Run("action and resource entities carry no attributes", func(t *testing.T) {
		must := require.New(t)
		must.Zero(entities[req.Action].Attributes.Len(), "the no-such-attribute rule assumes none")
		must.Zero(entities[req.Resource].Attributes.Len(), "the no-such-attribute rule assumes none")
	})
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
// registry-tier one, so a misspelled attribute is never rejected silently.
func TestNewPolicyEngine_StructuralErrorIsAudited(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "structural.cedar")
	src := `forbid (principal, action, resource) when { context.inputs.command like "*rm*" };`
	must.NoError(os.WriteFile(path, []byte(src), 0o600))

	rec := &fakeEmitter{}
	_, err := New(
		types.PermissionPolicyConfig{Type: "policy-engine", PolicyFile: path, Fallback: "deny-all"},
		PolicyEngineEnv{ToolSchemas: harnessSchemas(), Security: rec},
		func(string) (PermissionPolicy, error) { return NewDenyAll(), nil },
	)
	must.Error(err, "expected construction to abort")

	events := rec.snapshot()
	must.Len(events, 1, "expected exactly one policy_lint event")
	must.Equal("policy_lint", events[0].event)
	must.Equal("error", events[0].level)
	must.Equal(LintRuleUnknownContextAttribute, events[0].data["rule"], "event does not carry the rule")
}

// The generic descent in collectURLGroups hands each child back to a
// fresh polarity-aware walk. Node types with more than one child are the
// ones that stress it, and neither `&&`/`||`/`!` (handled explicitly) nor
// a bare `like` reaches that branch at all.
func TestWildcardHostPattern_DescendsIntoMultiChildNodes(t *testing.T) {
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
			must := require.New(t)
			src := `permit (principal, action, resource) when { ` + tc.body + ` };`
			findings := LintPolicySetStructure(mustParse(t, src))
			must.Equal(tc.want, len(findings) > 0, "findings: %v", findings)
		})
	}
}

// normaliseFindings exists to collapse the same defect reported from
// several disjuncts of one clause.
func TestLint_CollapsesDuplicateFindings(t *testing.T) {
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
	must := require.New(t)
	var sb strings.Builder
	for i := range 12 {
		fmt.Fprintf(&sb, "forbid (principal, action, resource) when { context.bad%02d == \"x\" };\n", i)
	}
	findings := LintPolicySetStructure(mustParse(t, sb.String()))
	must.Len(findings, 12)
	for i, f := range findings {
		must.Equal(i+1, f.Line, "finding %d is at a line that does not follow the file", i)
		must.Contains(f.Message, fmt.Sprintf("context.bad%02d", i), "finding %d", i)
	}
}

// Differential probe over the wildcard-host rule. Wildcards are inserted at
// every position of allow-list URLs. Any pattern the linter passes must be
// denied by the real Cedar authorizer for URLs on a different host, so a
// clean lint result means the host is pinned.
//
// Scope note: this pins HOST anchoring only. A pattern that permits a
// wider path on the same host (`https://github.com/*` reaching /a and /b
// alike) is the allow-list working as written, so every probe URL below
// sits on a host the pattern never names.
func TestWildcardHostPattern_NoAuthorizedBypass(t *testing.T) {
	must := require.New(t)
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
	lintPattern := func(t *testing.T, pattern string) {
		t.Helper()
		must := require.New(t)
		check := assert.New(t)
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
		must.NoError(err, "build policy for %q", pattern)
		for _, url := range otherHost {
			input, err := json.Marshal(map[string]string{"url": url})
			must.NoError(err)
			res, err := policy.Check(t.Context(), types.ToolDefinition{Name: "web_fetch"}, input)
			must.NoError(err, "check %q against %q", url, pattern)
			check.False(res.Allowed, "pattern %q passes the lint but authorizes %q", pattern, url)
		}
	}

	for _, base := range bases {
		for i := 0; i <= len(base); i++ {
			lintPattern(t, base[:i]+"*"+base[i:])
			for j := i; j <= len(base); j++ {
				lintPattern(t, base[:i]+"*"+base[j:])
				lintPattern(t, base[:i]+"*"+base[i:j]+"*"+base[j:])
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
		lintPattern(t, pattern)
	}

	// Guards against the probe passing vacuously: if a future change
	// flagged every pattern, no authorization would be exercised at all.
	must.NotZero(clean, "no pattern survived the lint, so the probe authorized nothing")
	t.Logf("%d of %d patterns passed the lint; none authorized another host", clean, len(seen))
}

// A URL attribute that is not a string must not slip past a policy that
// constrains it: Cedar raises a type error on `like` against a Long or a
// Record, the policy abstains, and the fallback decides. buildRequest
// also wraps a non-object tool input as context.input.raw, which no
// url clause names.
func TestWildcardHostPattern_NonStringURLDoesNotAuthorize(t *testing.T) {
	must := require.New(t)
	src := `permit (principal, action, resource) when { context.input.url like "https://github.com/*" };`
	ps := mustParse(t, src)
	must.Empty(LintPolicySetStructure(ps), "the url-constrained permit must lint clean")

	policy, err := NewPolicyEnginePolicy(PolicyEngineConfig{PolicySet: ps, Fallback: NewDenyAll()})
	must.NoError(err)

	cases := []struct {
		name  string
		input string
	}{
		{"url is a number", `{"url":123}`},
		{"url is an object", `{"url":{"a":1}}`},
		{"url is an array", `{"url":["x"]}`},
		{"url is null", `{"url":null}`},
		{"url is absent", `{}`},
		{"raw string input", `"a raw string"`},
		{"null input", `null`},
		{"number input", `123`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			res, err := policy.Check(t.Context(), types.ToolDefinition{Name: "web_fetch"}, json.RawMessage(tc.input))
			must.NoError(err)
			must.False(res.Allowed, "input %s was authorized by a url-constrained permit", tc.input)
		})
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
			must := require.New(t)
			src := `permit (principal, action, resource) when { context.input.url like "` + pattern + `" };`
			must.Empty(LintPolicySetStructure(mustParse(t, src)), "anchored pattern rejected")
		})
	}
}

// Polarity must survive a negation expressed through a node the DNF walk
// does not model. `X == false`, `X != true`, `if X then false else true`
// and `[X].contains(false)` all mean `!X`. Wrapped in an outer `!`, the
// two negations cancel and the enclosed `like` is in a widening position.
//
// Clean verdicts are cross-checked against the real authorizer, because a
// clean verdict must mean the authorizer denies another host.
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
			must := require.New(t)
			src := `permit (principal, action == Action::"tool:web_fetch", resource == Tool::"web_fetch")
				when { ` + tc.body + ` };`
			ps := mustParse(t, src)
			findings := LintPolicySetStructure(ps)
			must.Equal(tc.want, len(findings) > 0, "findings: %v", findings)

			if tc.want {
				return
			}
			policy, err := NewPolicyEnginePolicy(PolicyEngineConfig{PolicySet: ps, Fallback: NewDenyAll()})
			must.NoError(err)
			input, err := json.Marshal(map[string]string{"url": "https://evil.example/x.github.com/y"})
			must.NoError(err)
			res, err := policy.Check(t.Context(), types.ToolDefinition{Name: "web_fetch"}, input)
			must.NoError(err)
			must.False(res.Allowed, "policy lints clean but authorizes an attacker host: %s", tc.body)
		})
	}
}

// Metamorphic check over the DNF walk. Rewriting a clause into a logically
// equivalent form must not change the linter's verdict, however the clause
// is dressed up in comparisons, muxes or paired negations. Probes that vary
// only the pattern cannot exercise how policy bodies are walked, so this
// test varies the body.
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
			check := assert.New(t)
			must := require.New(t)
			for _, shape := range equivalent {
				body := fmt.Sprintf(shape, clause.expr)
				src := `permit (principal, action == Action::"tool:web_fetch", resource == Tool::"web_fetch")
					when { ` + body + ` };`
				ps, err := cedar.NewPolicySetFromBytes("equiv.cedar", []byte(src))
				must.NoError(err, "parse %q", body)

				findings := LintPolicySetStructure(ps)
				if !check.Equal(clause.want, len(findings) > 0, "body %q: findings %v", body, findings) {
					continue
				}
				if clause.want {
					continue
				}

				// A clean verdict must still mean the host is pinned.
				policy, err := NewPolicyEnginePolicy(PolicyEngineConfig{PolicySet: ps, Fallback: NewDenyAll()})
				must.NoError(err, "body %q", body)
				input, err := json.Marshal(map[string]string{"url": "https://evil.example/x.github.com/y"})
				must.NoError(err)
				res, err := policy.Check(t.Context(), types.ToolDefinition{Name: "web_fetch"}, input)
				must.NoError(err, "body %q", body)
				check.False(res.Allowed, "body %q lints clean but authorizes an attacker host", body)
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
			must := require.New(t)
			must.Empty(LintPolicySetStructure(mustParse(t, tc.src)), "safe policy rejected")
		})
	}
}

// Anchoring is evaluated bottom-up over a three-valued lattice, not by
// materialising disjunctive normal form, which is exponential in the number
// of conjoined disjunctions. The verdict must not depend on how many
// constraints surround the anchor, so scale is a correctness property.
func TestWildcardHostPattern_WideConjunctionsStayLinear(t *testing.T) {
	for _, n := range []int{4, 8, 16, 32, 64} {
		t.Run(fmt.Sprintf("conjuncts=%d", n), func(t *testing.T) {
			must := require.New(t)
			parts := []string{`context.input.url like "https://github.com/*"`}
			for range n {
				parts = append(parts, `(context.input.url like "*a*" || context.input.url like "*b*")`)
			}
			src := `permit (principal, action, resource) when { ` + strings.Join(parts, " && ") + ` };`
			must.Empty(LintPolicySetStructure(mustParse(t, src)), "anchored policy rejected at %d conjuncts", n)

			// Dropping the anchor must still be caught.
			unanchored := `permit (principal, action, resource) when { ` + strings.Join(parts[1:], " && ") + ` };`
			must.NotEmpty(LintPolicySetStructure(mustParse(t, unanchored)), "unanchored policy accepted at %d conjuncts", n)
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
func TestWildcardHostPattern_JudgesUnresolvableOperands(t *testing.T) {
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
			must := require.New(t)
			src := `permit (principal, action == Action::"tool:web_fetch", resource == Tool::"web_fetch")
				when { ` + tc.body + ` };`
			ps := mustParse(t, src)
			findings := LintPolicySetStructure(ps)
			must.Equal(tc.want, len(findings) > 0, "findings: %v", findings)
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
				t.Run(operand, func(t *testing.T) {
					must := require.New(t)
					body := fmt.Sprintf(`%s like "%s"`, operand, clause.pattern)
					src := `permit (principal, action == Action::"tool:web_fetch", resource == Tool::"web_fetch")
						when { ` + body + ` };`
					ps, err := cedar.NewPolicySetFromBytes("operand.cedar", []byte(src))
					must.NoError(err, "parse %q", body)
					findings := LintPolicySetStructure(ps)
					must.Equal(clause.want, len(findings) > 0, "findings: %v", findings)
					if clause.want {
						return
					}
					policy, err := NewPolicyEnginePolicy(PolicyEngineConfig{PolicySet: ps, Fallback: NewDenyAll()})
					must.NoError(err)
					input, err := json.Marshal(map[string]string{"url": "https://evil.example/x.github.com/y"})
					must.NoError(err)
					res, err := policy.Check(t.Context(), types.ToolDefinition{Name: "web_fetch"}, input)
					must.NoError(err)
					must.False(res.Allowed, "lints clean but authorizes an attacker host")
				})
			}
		})
	}
}

// Cross-product oracle for the wildcard-host rule. It generates policies from
// the product of pattern, operand wrapper, body wrapper and polarity, then
// uses the real Cedar evaluator to check that no lint-clean policy authorizes
// an attacker URL. Constructs nobody listed are still covered when they are
// built from these pieces.
//
// The property is one-directional: a lint-clean policy must not authorize
// another host. Flagged policies are not asserted about, because over-flagging
// is the safe direction. TestWildcardHostPattern_NoFalsePositives pins false
// positives separately.
func TestWildcardHostPattern_CrossProductOracle(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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
					must.NoError(err, "build policy for %q", src)
					for _, url := range attacker {
						input, err := json.Marshal(map[string]string{"url": url})
						must.NoError(err)
						res, err := policy.Check(t.Context(), types.ToolDefinition{Name: "web_fetch"}, input)
						must.NoError(err, "check %q", src)
						check.False(res.Allowed, "lints clean but authorizes %q:\n%s", url, src)
					}
				}
			}
		}
	}
	must.NotZero(clean, "no policy survived the lint; the oracle proves nothing")
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
		"closed":                    `{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":false}`,
		"open by omission":          `{"type":"object","properties":{"a":{"type":"string"}}}`,
		"additionalProperties {}":   `{"type":"object","properties":{"a":{}},"additionalProperties":{"type":"string"}}`,
		"additionalProperties true": `{"type":"object","properties":{"a":{}},"additionalProperties":true}`,
		"properties via $ref":       `{"type":"object","$ref":"#/$defs/P","additionalProperties":false,"$defs":{"P":{"properties":{"a":{"type":"string"}}}}}`,
		"properties via allOf":      `{"type":"object","allOf":[{"properties":{"a":{"type":"string"}}}],"additionalProperties":false}`,
		"properties via anyOf":      `{"type":"object","anyOf":[{"properties":{"a":{}}},{"properties":{"b":{}}}],"additionalProperties":false}`,
		"empty schema object":       `{}`,
		"closed, no properties":     `{"type":"object","additionalProperties":false}`,
		"non-object root":           `{"type":"string"}`,
		"nested properties":         `{"type":"object","properties":{"a":{"type":"object","properties":{"b":{}}}},"additionalProperties":false}`,
	}

	for name, raw := range schemas {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			schema := NewToolSchema(json.RawMessage(raw))
			// Would the linter call `context.input.a` dead for this tool?
			callsItDead := schema.Closed && !schema.Properties["a"]
			// Does the real validator let a call carrying `a` through?
			validatorAccepts := security.ValidateJSONSchema(
				json.RawMessage(`{"a":"x"}`), json.RawMessage(raw)) == nil

			must.False(callsItDead && validatorAccepts,
				"linter would reject a policy keyed on context.input.a as dead, but the validator accepts that input; a working policy would fail to load")
		})
	}
}

// Cedar's standard RBAC idiom `principal in Group::"..."` is dead here:
// buildRequest gives the principal one parent (User::"any") and mints no
// Group entity. The linter flags membership tests against entity types the
// harness never mints, in the body as well as in the scope.
func TestLintConditionEntities_DeadMembershipTests(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"principal in a Group", `principal in Group::"malicious-actor"`, true},
		{"principal is a Group", `principal is Group`, true},
		{"principal is Group in a Group", `principal is Group in Group::"x"`, true},
		{"membership in a set with one bad type", `principal in [User::"any", Group::"x"]`, true},

		// The harness does mint these, so they must keep working.
		{"principal in the root alias", `principal in User::"any"`, false},
		{"principal is a User", `principal is User`, false},
		{"resource in a Tool", `resource in Tool::"run_command"`, false},
		{"action in an Action set", `action in [Action::"tool:run_command"]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			src := `forbid (principal, action, resource) when { ` + tc.body + ` };`
			findings := LintPolicySetStructure(mustParse(t, src))
			must.Equal(tc.want, len(findings) > 0, "findings: %v", findings)
			if tc.want {
				must.Equal(LintRuleUnknownScopeEntity, findings[0].Rule)
			}
		})
	}
}

// A dead forbid is the defect this lint exists to catch. The test checks the
// authorizer as well as the finding: the policy really does fail to block
// what it names.
func TestLintConditionEntities_DeadForbidIsProvenDead(t *testing.T) {
	must := require.New(t)
	src := `forbid (principal, action == Action::"tool:run_command", resource == Tool::"run_command")
		when { principal in Group::"malicious-actor" };`
	ps := mustParse(t, src)
	must.NotEmpty(LintPolicySetStructure(ps), "expected the dead membership test to be flagged")
	policy, err := NewPolicyEnginePolicy(PolicyEngineConfig{PolicySet: ps, Fallback: NewAllowAll()})
	must.NoError(err)
	res, err := policy.Check(t.Context(), types.ToolDefinition{Name: "run_command"}, json.RawMessage(`{"command":"rm -rf /"}`))
	must.NoError(err)
	must.True(res.Allowed, "the forbid fired, so the finding would be a false positive; re-check knownEntityTypes against buildRequest")
}

// patternProperties admits names that no `properties` entry lists, so
// additionalProperties:false alongside it does not close the object. Such a
// schema must not be read as closed, or a policy on a pattern-matched
// attribute would be rejected as dead.
func TestNewToolSchema_PatternPropertiesIsNotClosed(t *testing.T) {
	must := require.New(t)
	raw := `{"type":"object","properties":{"url":{"type":"string"}},"patternProperties":{"^x-":{"type":"string"}},"additionalProperties":false}`

	// The real validator accepts a pattern-matched property.
	must.NoError(security.ValidateJSONSchema(
		json.RawMessage(`{"url":"https://example.com","x-custom":"hello"}`),
		json.RawMessage(raw),
	), "the validator must accept a pattern-matched property, or this test's premise no longer holds")

	must.False(NewToolSchema(json.RawMessage(raw)).Closed, "a schema with patternProperties must not read as closed")

	src := `forbid (principal, action == Action::"tool:mytool", resource) when { context.input["x-custom"] like "*" };`
	findings := LintPolicySetTools(mustParse(t, src), map[string]ToolSchema{
		"mytool": NewToolSchema(json.RawMessage(raw)),
	})
	assertRules(t, findings, LintRuleUnverifiableInputAttribute+":warning")
}

// Every condition block on a statement must hold for the policy to apply, so
// the blocks form one conjunction and share anchoring credit: a later block
// that pins the host anchors an earlier wildcard block.
func TestWildcardHostPattern_CreditSharedAcrossConditionBlocks(t *testing.T) {
	must := require.New(t)
	src := `permit (principal, action == Action::"tool:web_fetch", resource == Tool::"web_fetch")
		when { context.input.url like "*.github.com/*" }
		when { context.input.url like "https://github.com/*" };`
	ps := mustParse(t, src)
	must.Empty(LintPolicySetStructure(ps), "anchored-by-a-later-block policy rejected")

	// The clean verdict is honest: the authorizer really does deny.
	policy, err := NewPolicyEnginePolicy(PolicyEngineConfig{PolicySet: ps, Fallback: NewDenyAll()})
	must.NoError(err)
	input, err := json.Marshal(map[string]string{"url": "https://evil.example/x.github.com/y"})
	must.NoError(err)
	res, err := policy.Check(t.Context(), types.ToolDefinition{Name: "web_fetch"}, input)
	must.NoError(err)
	must.False(res.Allowed, "lints clean but authorizes an attacker host")

	// Drop the anchoring block and it must be caught again.
	unanchored := `permit (principal, action == Action::"tool:web_fetch", resource == Tool::"web_fetch")
		when { context.input.url like "*.github.com/*" }
		when { context.input has url };`
	must.NotEmpty(LintPolicySetStructure(mustParse(t, unanchored)),
		"unanchored policy accepted once split across condition blocks")
}

// Pins the scoping decision that a permit with an unconstrained path is not
// flagged, for example `anchored || unrelated`. The rule judges a pattern's
// anchoring, not whether a permit has an unconstrained path, so every spelling
// of one policy gets the same verdict. The last case is the boundary: an
// unanchored disjunct is still flagged.
func TestWildcardHostPattern_UnconstrainedPathIsOutOfScope(t *testing.T) {
	const anchored = `context.input.url like "https://github.com/*"`
	scope := `permit (principal, action == Action::"tool:web_fetch", resource == Tool::"web_fetch")`

	equivalentSpellings := []struct{ name, src string }{
		{"inline disjunction", fmt.Sprintf(`%s when { %s || context.input.method == "GET" };`, scope, anchored)},
		{"two statements", fmt.Sprintf("%s when { %s };\n%s when { context.input.method == \"GET\" };", scope, anchored, scope)},
		{"no url clause", scope + ";"},
		{"mux with open else", fmt.Sprintf(`%s when { if principal.mode == "execution" then %s else true };`, scope, anchored)},
	}
	for _, tc := range equivalentSpellings {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			ps := mustParse(t, tc.src)
			findings := LintPolicySetStructure(ps)
			must.Empty(findings, "expected no findings")
			policy, err := NewPolicyEnginePolicy(PolicyEngineConfig{PolicySet: ps, Fallback: NewDenyAll(), Mode: "planning"})
			must.NoError(err)
			input, err := json.Marshal(map[string]string{"url": "https://evil.example/steal", "method": "GET"})
			must.NoError(err)
			res, err := policy.Check(t.Context(), types.ToolDefinition{Name: "web_fetch"}, input)
			must.NoError(err)
			// Asserting the permissive outcome documents what the clean verdict
			// costs rather than implying the policy is safe. If this fails, the
			// scoping argument above no longer holds.
			must.True(res.Allowed, "the unconstrained path must authorize")
		})
	}

	t.Run("unanchored disjunct is still flagged", func(t *testing.T) {
		must := require.New(t)
		src := fmt.Sprintf(`%s when { %s || context.input.url like "*.github.com/*" };`, scope, anchored)
		findings := LintPolicySetStructure(mustParse(t, src))
		must.Len(findings, 1, "findings: %v", findings)
		must.Equal(LintRuleWildcardHostPattern, findings[0].Rule)
	})
}
