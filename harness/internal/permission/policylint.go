package permission

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/cedar-policy/cedar-go"
	cedartypes "github.com/cedar-policy/cedar-go/types"
	internalast "github.com/cedar-policy/cedar-go/x/exp/ast"
)

// x/exp/ast is the only package exposing cedar-go's parsed node types.
// The stable cedar-go/ast package is a thin defined-type wrapper over it
// (ast.Policy is `type Policy internalast.Policy`), so walking a parsed
// policy structurally requires the experimental import. cedar-go is
// pinned in go.mod, so an upstream shape change surfaces at upgrade time
// as a compile error rather than silently degrading the lint.

// Lint rule identifiers. These are the values an operator names in the
// @stirrupLintIgnore annotation, and the "rule" field of every emitted
// policy_lint security event.
const (
	// LintRuleUnknownContextAttribute fires on a context attribute the
	// harness never populates (context.inputs, context.tool, ...).
	LintRuleUnknownContextAttribute = "unknown-context-attribute"

	// LintRuleUnknownPrincipalAttribute fires on a principal attribute the
	// harness never populates (principal.runID, principal.role, ...).
	LintRuleUnknownPrincipalAttribute = "unknown-principal-attribute"

	// LintRuleNoSuchAttribute fires on any attribute access against the
	// action or resource entity; neither carries attributes.
	LintRuleNoSuchAttribute = "no-such-attribute"

	// LintRuleUnknownScopeEntity fires when a scope clause names an entity
	// type or action ID the harness never mints.
	LintRuleUnknownScopeEntity = "unknown-scope-entity"

	// LintRuleWildcardHostPattern fires on a `like` URL pattern whose
	// wildcard sits at or before the end of the authority component.
	LintRuleWildcardHostPattern = "wildcard-host-pattern"

	// LintRuleUndeclaredInputAttribute fires on a context.input attribute
	// no in-scope tool schema declares.
	LintRuleUndeclaredInputAttribute = "undeclared-input-attribute"

	// LintRuleUnknownTool fires when a policy scopes to a tool name that is
	// not registered for this run. Advisory: the tool may belong to an MCP
	// server that failed to connect, or be disabled by tools.builtIn.
	LintRuleUnknownTool = "unknown-tool"

	// LintRuleUnverifiableInputAttribute fires when an in-scope tool schema
	// permits additional properties, so a context.input attribute cannot be
	// proven dead. Advisory.
	LintRuleUnverifiableInputAttribute = "unverifiable-input-attribute"
)

// lintIgnoreAnnotation is the Cedar annotation key an operator sets to
// accept a rule against a single policy:
//
//	@stirrupLintIgnore("wildcard-host-pattern")
//
// The value is a comma-separated list of rule identifiers. An honoured
// ignore is never silent — it downgrades to a warning-severity finding so
// it still lands in the run's audit trail.
const lintIgnoreAnnotation = "stirrupLintIgnore"

// LintSeverity classifies a LintFinding. Error-severity findings abort
// construction of the policy engine; warning-severity findings are emitted
// as policy_lint security events and the run proceeds.
type LintSeverity string

const (
	LintError   LintSeverity = "error"
	LintWarning LintSeverity = "warning"
)

// LintFinding is a single defect found in a Cedar policy set.
type LintFinding struct {
	// PolicyID is the cedar-go policy ID ("policy0" for the first
	// unannotated statement in a file, or the @id annotation when set).
	PolicyID string
	// Rule is one of the LintRule* constants.
	Rule string
	// Severity decides whether the finding aborts the run.
	Severity LintSeverity
	// Message states the defect and the remedy.
	Message string
	// Line is the 1-based source line of the offending policy statement,
	// or 0 when the parser recorded no position.
	Line int
}

// String renders a finding for an operator-facing error or log line.
func (f LintFinding) String() string {
	loc := f.PolicyID
	if f.Line > 0 {
		loc = fmt.Sprintf("%s (line %d)", f.PolicyID, f.Line)
	}
	return fmt.Sprintf("[%s] %s: %s", f.Rule, loc, f.Message)
}

// ToolSchema is the slice of a tool's JSON Schema the policy linter needs:
// the set of declared top-level input properties, and whether the schema
// closes the object against undeclared ones.
type ToolSchema struct {
	// Properties holds the declared top-level property names.
	Properties map[string]bool
	// Closed reports whether the schema sets additionalProperties:false.
	// Only a closed schema lets the linter prove that a policy clause
	// keyed on an undeclared attribute is dead: harness tool dispatch
	// validates input against the schema before consulting Cedar
	// (harness/internal/core/types.go), so an undeclared property on a
	// closed schema is rejected before the policy engine ever sees it.
	Closed bool
}

// NewToolSchema parses a raw JSON Schema document into the subset the
// linter needs. A schema that fails to parse yields an open schema with no
// declared properties, which makes the linter treat every attribute as
// unverifiable rather than dead.
func NewToolSchema(raw json.RawMessage) ToolSchema {
	out := ToolSchema{Properties: map[string]bool{}}
	if len(raw) == 0 {
		return out
	}
	var doc struct {
		Properties           map[string]json.RawMessage `json:"properties"`
		AdditionalProperties *json.RawMessage           `json:"additionalProperties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return out
	}
	for name := range doc.Properties {
		out.Properties[name] = true
	}
	if doc.AdditionalProperties != nil {
		out.Closed = strings.TrimSpace(string(*doc.AdditionalProperties)) == "false"
	}
	return out
}

// Request-shape vocabulary. These mirror buildRequest exactly; a change
// there without a change here silently narrows the linter.
var (
	knownContextKeys = map[string]bool{
		"input":          true,
		"workspace":      true,
		"dynamicContext": true,
	}
	knownPrincipalAttrs = map[string]bool{
		"runId":        true,
		"mode":         true,
		"parentRunId":  true,
		"capabilities": true,
	}
)

// inputRawAttribute is the synthetic property buildRequest wraps a
// non-object tool input in, so a policy may legitimately key on it
// regardless of what any tool schema declares.
const inputRawAttribute = "raw"

// actionIDPrefix is the prefix buildRequest gives every action UID.
const actionIDPrefix = "tool:"

// LintPolicySetStructure checks a parsed policy set against the fixed
// shape of the Cedar request the harness builds (see buildRequest). Every
// rule here is decidable from the policy alone — no tool registry, no
// run configuration — so it is safe to run on every load path, including
// the dry-run preflight where no tools are registered.
//
// Findings are returned in a stable order (policy ID, then rule, then
// message) so error text is deterministic across runs.
func LintPolicySetStructure(ps *cedar.PolicySet) []LintFinding {
	if ps == nil {
		return nil
	}
	var findings []LintFinding
	forEachPolicy(ps, func(pl policyUnderLint) {
		findings = append(findings, lintScopes(pl)...)
		findings = append(findings, lintConditionAttributes(pl)...)
		findings = append(findings, lintWildcardHosts(pl)...)
	})
	return normaliseFindings(findings)
}

// LintPolicySetTools cross-checks every context.input attribute a policy
// references against the input schemas of the tools registered for this
// run. It is the load-time generalisation of issue #524: a policy keyed on
// `context.input.cmd` while run_command declares `command` parses cleanly,
// loads cleanly, and never fires.
//
// schemas maps tool name to its parsed input schema. An empty map disables
// the whole tier and returns no findings — the dry-run preflight's
// component build registers no tools, and reporting every attribute as
// undeclared there would be noise, not signal.
//
// Scoping follows the policy's action scope: `action == Action::"tool:X"`
// (or an `in [...]` set) narrows the check to the named tools, and a
// policy with an unconstrained action scope is checked against the union
// of every registered schema.
func LintPolicySetTools(ps *cedar.PolicySet, schemas map[string]ToolSchema) []LintFinding {
	if ps == nil || len(schemas) == 0 {
		return nil
	}
	var findings []LintFinding
	forEachPolicy(ps, func(pl policyUnderLint) {
		findings = append(findings, lintInputAttributes(pl, schemas)...)
	})
	return normaliseFindings(findings)
}

// emitLintFindings forwards every finding to the security audit trail as
// a policy_lint event. Errors are emitted too, not only the warnings the
// run survives: a run aborted by the linter should leave the same
// evidence behind as one that continued. A nil emitter is a no-op.
func emitLintFindings(security SecurityEventEmitter, policyFile string, findings []LintFinding) {
	if security == nil {
		return
	}
	for _, f := range findings {
		level := "warn"
		if f.Severity == LintError {
			level = "error"
		}
		security.Emit(level, "policy_lint", map[string]any{
			"policyFile": policyFile,
			"policyId":   f.PolicyID,
			"rule":       f.Rule,
			"severity":   string(f.Severity),
			"line":       f.Line,
			"message":    f.Message,
		})
	}
}

// LintErrors folds error-severity findings into a single error suitable
// for aborting construction, and returns nil when none are present.
func LintErrors(source string, findings []LintFinding) error {
	var msgs []string
	for _, f := range findings {
		if f.Severity == LintError {
			msgs = append(msgs, f.String())
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return fmt.Errorf("policy-engine: %s failed policy lint:\n  %s", source, strings.Join(msgs, "\n  "))
}

// policyUnderLint bundles a policy with the identity every finding needs.
type policyUnderLint struct {
	id      string
	line    int
	ast     *internalast.Policy
	ignores map[string]bool
}

// finding builds a LintFinding for this policy, downgrading severity to a
// warning when the policy carries a matching @stirrupLintIgnore entry.
func (pl policyUnderLint) finding(rule string, sev LintSeverity, format string, args ...any) LintFinding {
	msg := fmt.Sprintf(format, args...)
	if sev == LintError && pl.ignores[rule] {
		sev = LintWarning
		msg = fmt.Sprintf("%s (downgraded to a warning by @%s on this policy)", msg, lintIgnoreAnnotation)
	}
	return LintFinding{PolicyID: pl.id, Rule: rule, Severity: sev, Message: msg, Line: pl.line}
}

// forEachPolicy walks a policy set in ID order, handing each statement to
// fn with its annotations already parsed.
func forEachPolicy(ps *cedar.PolicySet, fn func(policyUnderLint)) {
	var linted []policyUnderLint
	for id := range ps.All() {
		p := ps.Get(id)
		if p == nil {
			continue
		}
		pl := policyUnderLint{
			id:      string(id),
			line:    p.Position().Line,
			ast:     (*internalast.Policy)(p.AST()),
			ignores: parseLintIgnores(p.Annotations()),
		}
		linted = append(linted, pl)
	}
	// Source order, so a multi-statement file (the documented way to
	// compose policies is concatenation) reports top to bottom. IDs break
	// ties naturally: cedar-go mints "policy0".."policyN", and a plain
	// string sort puts "policy10" before "policy2".
	sort.SliceStable(linted, func(i, j int) bool {
		if linted[i].line != linted[j].line {
			return linted[i].line < linted[j].line
		}
		return naturalLess(linted[i].id, linted[j].id)
	})
	for _, pl := range linted {
		if pl.ast == nil {
			// Unreachable for a policy set parsed from source; a nil AST
			// would mean cedar-go handed back a statement it could not
			// represent. Report rather than skip: silently excluding a
			// statement from every rule is the one behaviour a
			// fail-closed linter must not have.
			fn2 := pl
			fn2.ast = &internalast.Policy{}
			fn(fn2)
			continue
		}
		fn(pl)
	}
}

// naturalLess orders "policy2" before "policy10" by comparing any
// trailing digit run numerically. Falls back to a plain string compare
// for @id-derived policy IDs, which have no such convention.
func naturalLess(a, b string) bool {
	ai, aok := trailingNumber(a)
	bi, bok := trailingNumber(b)
	if aok && bok && strings.TrimSuffix(a, ai) == strings.TrimSuffix(b, bi) {
		an, _ := strconv.Atoi(ai)
		bn, _ := strconv.Atoi(bi)
		return an < bn
	}
	return a < b
}

// trailingNumber returns the trailing digit run of s, if any.
func trailingNumber(s string) (string, bool) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	if i == len(s) {
		return "", false
	}
	return s[i:], true
}

// parseLintIgnores reads the @stirrupLintIgnore annotation into a rule set.
func parseLintIgnores(annotations cedar.Annotations) map[string]bool {
	raw, ok := annotations[cedartypes.Ident(lintIgnoreAnnotation)]
	if !ok {
		return nil
	}
	out := map[string]bool{}
	for part := range strings.SplitSeq(string(raw), ",") {
		if rule := strings.TrimSpace(part); rule != "" {
			out[rule] = true
		}
	}
	return out
}

// lintScopes checks the principal / action / resource scope clauses
// against the entity types buildRequest actually mints.
func lintScopes(pl policyUnderLint) []LintFinding {
	var findings []LintFinding
	check := func(uid cedartypes.EntityUID, want cedartypes.EntityType, slot string) {
		if uid.Type != want {
			findings = append(findings, pl.finding(LintRuleUnknownScopeEntity, LintError,
				"%s scope names %s::%q, but the harness only ever mints %s entities for %s — the policy can never match",
				slot, uid.Type, string(uid.ID), want, slot))
			return
		}
		if want == "Action" && !strings.HasPrefix(string(uid.ID), actionIDPrefix) {
			findings = append(findings, pl.finding(LintRuleUnknownScopeEntity, LintError,
				"action scope names Action::%q, but every harness action ID is %q-prefixed (e.g. Action::%q) — the policy can never match",
				string(uid.ID), actionIDPrefix, actionIDPrefix+string(uid.ID)))
		}
	}
	checkType := func(typ cedartypes.EntityType, want cedartypes.EntityType, slot string) {
		if typ != want {
			findings = append(findings, pl.finding(LintRuleUnknownScopeEntity, LintError,
				"%s scope tests for entity type %s, but the harness only ever mints %s entities for %s — the policy can never match",
				slot, typ, want, slot))
		}
	}

	switch s := pl.ast.Principal.(type) {
	case internalast.ScopeTypeEq:
		check(s.Entity, "User", "principal")
	case internalast.ScopeTypeIn:
		check(s.Entity, "User", "principal")
	case internalast.ScopeTypeIs:
		checkType(s.Type, "User", "principal")
	case internalast.ScopeTypeIsIn:
		checkType(s.Type, "User", "principal")
		check(s.Entity, "User", "principal")
	}

	switch s := pl.ast.Action.(type) {
	case internalast.ScopeTypeEq:
		check(s.Entity, "Action", "action")
	case internalast.ScopeTypeIn:
		check(s.Entity, "Action", "action")
	case internalast.ScopeTypeInSet:
		for _, e := range s.Entities {
			check(e, "Action", "action")
		}
	}

	switch s := pl.ast.Resource.(type) {
	case internalast.ScopeTypeEq:
		check(s.Entity, "Tool", "resource")
	case internalast.ScopeTypeIn:
		check(s.Entity, "Tool", "resource")
	case internalast.ScopeTypeIs:
		checkType(s.Type, "Tool", "resource")
	case internalast.ScopeTypeIsIn:
		checkType(s.Type, "Tool", "resource")
		check(s.Entity, "Tool", "resource")
	}

	return findings
}

// lintConditionAttributes checks every attribute reference in the policy's
// when/unless bodies against the request shape.
func lintConditionAttributes(pl policyUnderLint) []LintFinding {
	var findings []LintFinding
	forEachAttributeRef(pl.ast, func(path []string) {
		if len(path) < 2 {
			return
		}
		switch path[0] {
		case "context":
			if !knownContextKeys[path[1]] {
				findings = append(findings, pl.finding(LintRuleUnknownContextAttribute, LintError,
					"references context.%s, which the harness never populates — the clause can never fire; the Cedar request carries only %s",
					path[1], joinKeys(knownContextKeys, "context.")))
			}
		case "principal":
			if !knownPrincipalAttrs[path[1]] {
				findings = append(findings, pl.finding(LintRuleUnknownPrincipalAttribute, LintError,
					"references principal.%s, which the harness never populates — the clause can never fire; the principal carries only %s",
					path[1], joinKeys(knownPrincipalAttrs, "principal.")))
			}
		case "resource", "action":
			findings = append(findings, pl.finding(LintRuleNoSuchAttribute, LintError,
				"references %s.%s, but the harness attaches no attributes to the %s entity — the clause can never fire; match on the entity ID instead (e.g. resource == Tool::\"run_command\")",
				path[0], path[1], path[0]))
		}
	})
	return findings
}

// lintInputAttributes is the registry-aware tier: every context.input
// attribute must be declared by a tool the policy can actually apply to.
func lintInputAttributes(pl policyUnderLint, schemas map[string]ToolSchema) []LintFinding {
	attrs := map[string]bool{}
	forEachAttributeRef(pl.ast, func(path []string) {
		if len(path) >= 3 && path[0] == "context" && path[1] == "input" {
			attrs[path[2]] = true
		}
	})
	if len(attrs) == 0 {
		return nil
	}

	named := scopedToolNames(pl.ast)
	var findings []LintFinding

	inScope := map[string]ToolSchema{}
	if len(named) == 0 {
		inScope = schemas
	} else {
		for _, name := range named {
			schema, ok := schemas[name]
			if !ok {
				findings = append(findings, pl.finding(LintRuleUnknownTool, LintWarning,
					"scopes to tool %q, which is not registered for this run — its context.input attributes cannot be checked (an MCP server that failed to connect, or a tool disabled via tools.builtIn, looks exactly like this)",
					name))
				continue
			}
			inScope[name] = schema
		}
		if len(inScope) == 0 {
			return findings
		}
	}

	// A schema that permits additional properties cannot prove a clause
	// dead: an undeclared property survives input validation and reaches
	// Cedar. Name the offenders so the warning is actionable.
	var openTools []string
	for name, schema := range inScope {
		if !schema.Closed {
			openTools = append(openTools, name)
		}
	}
	sort.Strings(openTools)

	declared := map[string]bool{}
	for _, schema := range inScope {
		for prop := range schema.Properties {
			declared[prop] = true
		}
	}

	names := make([]string, 0, len(attrs))
	for attr := range attrs {
		names = append(names, attr)
	}
	sort.Strings(names)

	for _, attr := range names {
		if attr == inputRawAttribute || declared[attr] {
			continue
		}
		if len(openTools) > 0 {
			findings = append(findings, pl.finding(LintRuleUnverifiableInputAttribute, LintWarning,
				"references context.input.%s, which no in-scope tool schema declares; %s permit undeclared properties, so the clause may still fire — verify the spelling against the tool's schema",
				attr, describeTools(openTools)))
			continue
		}
		findings = append(findings, pl.finding(LintRuleUndeclaredInputAttribute, LintError,
			"references context.input.%s, which no in-scope tool schema declares — tool input is schema-validated before Cedar is consulted, so the clause can never fire; in-scope tools declare %s",
			attr, joinSorted(declared)))
	}
	return findings
}

// scopedToolNames extracts the tool names a policy's action scope pins it
// to. An unconstrained action scope returns nil, meaning "every tool".
// Only the action scope is consulted: the resource scope mirrors it in
// every request the harness builds, so reading both would double-count.
func scopedToolNames(p *internalast.Policy) []string {
	add := func(out []string, uid cedartypes.EntityUID) []string {
		if uid.Type != "Action" {
			return out
		}
		name := strings.TrimPrefix(string(uid.ID), actionIDPrefix)
		if name == "" || name == string(uid.ID) {
			// Missing the tool: prefix — lintScopes already flagged it.
			return out
		}
		return append(out, name)
	}
	var names []string
	switch s := p.Action.(type) {
	case internalast.ScopeTypeEq:
		names = add(names, s.Entity)
	case internalast.ScopeTypeIn:
		names = add(names, s.Entity)
	case internalast.ScopeTypeInSet:
		for _, e := range s.Entities {
			names = add(names, e)
		}
	}
	sort.Strings(names)
	return names
}

// forEachAttributeRef walks every when/unless body and reports each
// attribute reference as a dotted path rooted at a Cedar variable, e.g.
// `context.input.command` -> ["context", "input", "command"]. Both the
// `.` access form and the `has` form are reported, and nested accesses
// report every prefix, so a caller matching on path length sees each
// level exactly once.
func forEachAttributeRef(p *internalast.Policy, fn func(path []string)) {
	for _, cond := range p.Conditions {
		internalast.Inspect(internalast.NewNode(cond.Body), func(n internalast.IsNode) bool {
			switch t := n.(type) {
			case internalast.NodeTypeAccess:
				if path, ok := attributePath(t.Arg); ok {
					fn(append(path, string(t.Value)))
				}
			case internalast.NodeTypeHas:
				if path, ok := attributePath(t.Arg); ok {
					fn(append(path, string(t.Value)))
				}
			}
			return true
		})
	}
}

// attributePath resolves a chain of attribute accesses back to its root
// variable. It returns false for any expression that is not rooted at a
// bare variable (a record literal, a set element, an extension call), so
// the caller never treats an unrelated expression as a request attribute.
func attributePath(n internalast.IsNode) ([]string, bool) {
	switch t := n.(type) {
	case internalast.NodeTypeVariable:
		return []string{string(t.Name)}, true
	case internalast.NodeTypeAccess:
		base, ok := attributePath(t.Arg)
		if !ok {
			return nil, false
		}
		// Copied rather than appended in place: the caller appends its own
		// segment, and two sibling accesses sharing a backing array would
		// overwrite each other's leaf.
		out := make([]string, len(base), len(base)+1)
		copy(out, base)
		return append(out, string(t.Value)), true
	default:
		return nil, false
	}
}

// urlAttributeSuffixes decide which `like` operands the wildcard-host rule
// applies to. Restricting by attribute name keeps the rule off legitimate
// substring matches such as `context.input.command like "*curl https://*"`,
// where a wildcard before the host is the whole point.
var urlAttributeSuffixes = []string{"url", "uri"}

// lintWildcardHosts flags URL allow-list clauses that fail to anchor the
// host. Cedar's `like` wildcard matches every character, `/` and `@`
// included, so a pattern whose authority component is not fully literal
// is satisfied by an attacker URL that embeds the expected host in its
// path or userinfo: `https://*.github.com/*`, `*.github.com/*`, and
// `https:*/github.com/*` are all matched by
// `https://evil.example/x.github.com/y`.
//
// The rule fires only where over-matching *widens* access — a `when` on
// a permit, or an `unless` on a forbid. The mirror cases over-match in
// the safe direction, and flagging them would push operators to narrow a
// deny-list. Negation flips that polarity and is tracked, including
// negations expressed through nodes this walk does not model.
//
// Anchoring is judged per attribute, and a conjunction shares credit:
// `url like "https://github.com/*" && url like "*.json"` is anchored by
// its first conjunct, because the second constrains a value the first
// already pinned. Disjuncts get no such credit — either branch alone can
// satisfy the clause, so each must anchor on its own.
func lintWildcardHosts(pl policyUnderLint) []LintFinding {
	permits := pl.ast.Effect == internalast.EffectPermit
	var findings []LintFinding
	for _, cond := range pl.ast.Conditions {
		widens := permits == (cond.Condition == internalast.ConditionWhen)
		for _, path := range urlAttributePaths(cond.Body) {
			offenders := &patternSet{}
			if anchoring(cond.Body, path, widens, offenders) != anchoringUnanchored {
				continue
			}
			findings = append(findings, pl.finding(LintRuleWildcardHostPattern, LintError,
				"matches %s against %s, which does not anchor the URL authority; Cedar's `*` matches `/` and `@`, so an attacker URL that embeds the expected host in its path or userinfo satisfies the pattern — spell out the full scheme://host/ prefix as literal text and enumerate hosts one clause each",
				path, offenders.join()))
		}
	}
	return findings
}

// anchoringVerdict is the three-valued result of asking "does every way
// of satisfying this subtree pin the host of one attribute".
//
// The three values are what let conjunction and disjunction share credit
// correctly without materialising disjunctive normal form. An earlier
// implementation did build the DNF, which is exponential in the number of
// conjoined disjunctions and needed a cross-product budget; past the
// budget it degraded to treating conjuncts as disjuncts, which rejected
// correct policies. This formulation is linear and needs no budget.
type anchoringVerdict int

const (
	// anchoringVacuous: the subtree does not constrain this attribute at
	// all. It neither anchors nor fails to; a disjunct that ignores the
	// attribute is not a hole in the attribute's allow-list, and this
	// rule is about wildcards rather than about whether a permit has
	// other unconstrained paths.
	anchoringVacuous anchoringVerdict = iota
	// anchoringAnchored: every satisfying assignment pins the host.
	anchoringAnchored
	// anchoringUnanchored: some satisfying assignment leaves the host
	// reachable by a wildcard.
	anchoringUnanchored
)

// mergeConjunction combines two operands of `&&`. One anchoring conjunct
// pins the value for the whole conjunction, so ANCHORED wins.
func mergeConjunction(a, b anchoringVerdict) anchoringVerdict {
	if a == anchoringAnchored || b == anchoringAnchored {
		return anchoringAnchored
	}
	if a == anchoringUnanchored || b == anchoringUnanchored {
		return anchoringUnanchored
	}
	return anchoringVacuous
}

// mergeDisjunction combines two operands of `||`. Either branch alone can
// satisfy the clause, so a single unanchored branch is a hole and
// UNANCHORED wins.
func mergeDisjunction(a, b anchoringVerdict) anchoringVerdict {
	if a == anchoringUnanchored || b == anchoringUnanchored {
		return anchoringUnanchored
	}
	if a == anchoringAnchored || b == anchoringAnchored {
		return anchoringAnchored
	}
	return anchoringVacuous
}

// anchoring evaluates a condition body for one attribute path. widening
// tracks whether over-matching at this node widens the clause's effect;
// negation flips it and swaps the roles of `&&` and `||` (De Morgan), so
// a negated subtree is evaluated correctly rather than approximated.
func anchoring(n internalast.IsNode, path string, widening bool, offenders *patternSet) anchoringVerdict {
	if n == nil {
		return anchoringVacuous
	}
	switch t := n.(type) {
	case internalast.NodeTypeNot:
		return anchoring(t.Arg, path, !widening, offenders)

	case internalast.NodeTypeAnd:
		if widening {
			return mergeConjunction(anchoring(t.Left, path, true, offenders), anchoring(t.Right, path, true, offenders))
		}
		// !(A && B) == !A || !B
		return mergeDisjunction(anchoring(t.Left, path, false, offenders), anchoring(t.Right, path, false, offenders))

	case internalast.NodeTypeOr:
		if widening {
			return mergeDisjunction(anchoring(t.Left, path, true, offenders), anchoring(t.Right, path, true, offenders))
		}
		// !(A || B) == !A && !B
		return mergeConjunction(anchoring(t.Left, path, false, offenders), anchoring(t.Right, path, false, offenders))

	case internalast.NodeTypeLike:
		clause, ok := urlLikeClauseOf(t)
		if !ok || !widening || clause.path != path {
			return anchoringVacuous
		}
		if clause.pattern.anchorsAuthority() {
			return anchoringAnchored
		}
		offenders.add(clause.pattern.source)
		return anchoringUnanchored

	// A comparison against a boolean literal is a negation in disguise:
	// `X == false` and `X != true` both mean `!X`. Missing that let a
	// double negation cancel out in Cedar while this walk flipped
	// polarity only once, so the enclosed `like` was judged narrowing and
	// the policy linted clean while authorizing any host.
	case internalast.NodeTypeEquals:
		if other, literal, ok := boolComparisonOperand(t.Left, t.Right); ok {
			return anchoring(other, path, widening == literal, offenders)
		}
	case internalast.NodeTypeNotEquals:
		if other, literal, ok := boolComparisonOperand(t.Left, t.Right); ok {
			return anchoring(other, path, widening != literal, offenders)
		}

	// An if/then/else is a boolean mux. Both branches are the value of
	// the expression, so they inherit its polarity and combine as
	// alternatives; the condition selects between them, and which way
	// over-matching there cuts depends on the branches' runtime values,
	// so it is evaluated under both polarities.
	case internalast.NodeTypeIfThenElse:
		branches := mergeDisjunction(anchoring(t.Then, path, widening, offenders), anchoring(t.Else, path, widening, offenders))
		return mergeDisjunction(branches, bothPolarities(t.If, path, offenders))
	}

	// Any remaining node type — `in`, `contains`, extension calls, record
	// and set literals — has semantics this walk does not model, so its
	// children are evaluated under BOTH polarities and combined as
	// alternatives. Passing the parent's polarity through would assume
	// the node is transparent to negation, which is exactly the
	// assumption that made `!([X].contains(false))` lint clean.
	// Disjunctive merging is the conservative direction: it can only
	// produce more findings, never fewer.
	verdict := anchoringVacuous
	root := true
	internalast.Inspect(internalast.NewNode(n), func(child internalast.IsNode) bool {
		if root {
			root = false
			return true
		}
		verdict = mergeDisjunction(verdict, bothPolarities(child, path, offenders))
		return false
	})
	return verdict
}

// bothPolarities evaluates a subtree under each polarity and merges the
// results as alternatives, for nodes whose effect on polarity this walk
// cannot determine.
func bothPolarities(n internalast.IsNode, path string, offenders *patternSet) anchoringVerdict {
	return mergeDisjunction(anchoring(n, path, true, offenders), anchoring(n, path, false, offenders))
}

// patternSet accumulates the pattern literals that failed to anchor, in
// first-seen order, so a finding names exactly the clauses at fault
// rather than every pattern the condition happens to mention.
type patternSet struct {
	seen  map[string]bool
	order []string
}

func (p *patternSet) add(source string) {
	if p.seen == nil {
		p.seen = map[string]bool{}
	}
	if p.seen[source] {
		return
	}
	p.seen[source] = true
	p.order = append(p.order, source)
}

func (p *patternSet) join() string {
	quoted := make([]string, len(p.order))
	for i, s := range p.order {
		quoted[i] = strconv.Quote(s)
	}
	return strings.Join(quoted, " / ")
}

// urlAttributePaths lists the URL-valued attribute paths a condition
// mentions, in first-seen order, so anchoring runs once per attribute.
// Collection ignores polarity: an attribute mentioned anywhere is worth
// evaluating, and anchoring itself decides whether the clauses that
// mention it actually widen.
func urlAttributePaths(body internalast.IsNode) []string {
	if body == nil {
		return nil
	}
	seen := map[string]bool{}
	var order []string
	internalast.Inspect(internalast.NewNode(body), func(n internalast.IsNode) bool {
		like, ok := n.(internalast.NodeTypeLike)
		if !ok {
			return true
		}
		clause, ok := urlLikeClauseOf(like)
		if !ok || seen[clause.path] {
			return true
		}
		seen[clause.path] = true
		order = append(order, clause.path)
		return true
	})
	return order
}

// urlLikeClause is one `like` test against a URL-valued attribute.
type urlLikeClause struct {
	path    string
	pattern cedarPattern
}

// urlLikeClauseOf decodes a `like` node into its attribute path and
// pattern, reporting false when the operand is not a URL-valued
// attribute reference.
func urlLikeClauseOf(n internalast.NodeTypeLike) (urlLikeClause, bool) {
	path, ok := attributePath(n.Arg)
	if !ok || !isURLAttribute(path) {
		return urlLikeClause{}, false
	}
	pattern, err := decodePattern(n.Value)
	if err != nil {
		// Fail closed. decodePattern only fails on a component shape
		// cedar-go does not currently produce, but if an upstream
		// encoding change ever made it fail for every pattern, the
		// alternative — skipping — would turn this rule silently inert.
		// An undecodable pattern never anchors, so the failure is loud.
		pattern = cedarPattern{
			undecodable: true,
			source:      strings.TrimSuffix(strings.TrimPrefix(string(n.Value.MarshalCedar()), `"`), `"`),
		}
	}
	return urlLikeClause{path: strings.Join(path, "."), pattern: pattern}, true
}

// boolComparisonOperand splits a comparison into its non-literal operand
// and the boolean literal it is compared against. Returns ok=false when
// neither side is a boolean literal, in which case the comparison's
// polarity is genuinely indeterminate and the caller must fall through to
// the conservative path.
func boolComparisonOperand(left, right internalast.IsNode) (internalast.IsNode, bool, bool) {
	if v, ok := boolLiteral(right); ok {
		return left, v, true
	}
	if v, ok := boolLiteral(left); ok {
		return right, v, true
	}
	return nil, false, false
}

// boolLiteral reports whether n is a literal true/false.
func boolLiteral(n internalast.IsNode) (bool, bool) {
	value, ok := n.(internalast.NodeValue)
	if !ok {
		return false, false
	}
	b, ok := value.Value.(cedartypes.Boolean)
	return bool(b), ok
}

// isURLAttribute reports whether the final segment of an attribute path
// names a URL-valued field.
func isURLAttribute(path []string) bool {
	if len(path) == 0 {
		return false
	}
	name := strings.ToLower(path[len(path)-1])
	for _, suffix := range urlAttributeSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// cedarPattern is a decoded `like` pattern: the literal text with every
// wildcard elided, plus the byte offsets at which the wildcards sat.
type cedarPattern struct {
	flat      string
	wildcards []int
	source    string
	// undecodable marks a pattern whose component list could not be read.
	// It never anchors, so the rule degrades visibly instead of going
	// silently inert. See collectURLGroups.
	undecodable bool
}

// decodePattern reads a types.Pattern through its JSON encoding, which is
// the only exported view of its component list. The encoding is a list of
// either the string "Wildcard" or {"Literal": "..."}.
func decodePattern(p cedartypes.Pattern) (cedarPattern, error) {
	raw, err := p.MarshalJSON()
	if err != nil {
		return cedarPattern{}, err
	}
	var comps []json.RawMessage
	if err := json.Unmarshal(raw, &comps); err != nil {
		return cedarPattern{}, err
	}
	var sb strings.Builder
	out := cedarPattern{source: strings.TrimSuffix(strings.TrimPrefix(string(p.MarshalCedar()), `"`), `"`)}
	for _, comp := range comps {
		var literal struct {
			Literal *string `json:"Literal"`
		}
		if err := json.Unmarshal(comp, &literal); err == nil && literal.Literal != nil {
			sb.WriteString(*literal.Literal)
			continue
		}
		var name string
		if err := json.Unmarshal(comp, &name); err != nil {
			return cedarPattern{}, fmt.Errorf("unrecognised pattern component %s", string(comp))
		}
		if name != "Wildcard" {
			return cedarPattern{}, fmt.Errorf("unrecognised pattern component %q", name)
		}
		out.wildcards = append(out.wildcards, sb.Len())
	}
	out.flat = sb.String()
	return out, nil
}

// anchorsAuthority reports whether the pattern pins the URL authority
// with literal text, so no wildcard can extend or replace the host.
//
// The test is deliberately strict: everything before the first wildcard
// must literally contain a full "://" separator AND a subsequent "/",
// "?" or "#" that terminates the authority. Anything less leaves the
// host reachable by a wildcard, and Cedar's `*` matches `/` and `@`, so
// an attacker URL can embed the expected host in its path or userinfo.
//
// Two earlier formulations were weaker and both let real bypasses
// through, so the reasoning is worth keeping:
//
//   - "is there a wildcard after a literal `://`" missed patterns that
//     never produce the literal substring — `*.github.com/*` (no scheme
//     at all) and `https:*/github.com/*` (whose literal segments
//     concatenate to `https:/github.com/`, one slash).
//   - Treating a pattern with no literal `://` as schemeless, and taking
//     its first `/` as the authority terminator, then misread
//     `https:/*p`: the `/` at offset 6 belongs to the scheme separator,
//     not to the path, so the wildcard that follows it swallows the
//     whole host.
//
// A consequence of the strict form: a genuinely schemeless pattern such
// as `github.com/*` is reported too. That clause is dead rather than
// dangerous — a `web_fetch` URL always carries its scheme, so nothing
// matches it — and the remedy the finding prescribes (spell out the
// full scheme://host/ prefix) is the right fix either way.
func (p cedarPattern) anchorsAuthority() bool {
	if p.undecodable {
		return false
	}
	if len(p.wildcards) == 0 {
		return true
	}
	literalPrefix := p.flat[:p.wildcards[0]]
	sep := strings.Index(literalPrefix, "://")
	if sep < 0 {
		return false
	}
	return strings.ContainsAny(literalPrefix[sep+len("://"):], "/?#")
}

// normaliseFindings sorts findings deterministically and drops exact
// duplicates, which arise when the same defect appears in several
// disjuncts of one clause.
func normaliseFindings(findings []LintFinding) []LintFinding {
	if len(findings) == 0 {
		return nil
	}
	sort.SliceStable(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.PolicyID != b.PolicyID {
			return naturalLess(a.PolicyID, b.PolicyID)
		}
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		return a.Message < b.Message
	})
	out := findings[:0]
	var prev LintFinding
	for i, f := range findings {
		if i > 0 && f.PolicyID == prev.PolicyID && f.Rule == prev.Rule && f.Message == prev.Message {
			continue
		}
		out = append(out, f)
		prev = f
	}
	return out
}

// joinKeys renders a known-attribute set as a sorted, prefixed list for an
// error message.
func joinKeys(set map[string]bool, prefix string) string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, prefix+k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// joinSorted renders a name set as a sorted comma-separated list.
func joinSorted(set map[string]bool) string {
	if len(set) == 0 {
		return "no properties at all"
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// describeTools renders a tool-name list for a warning message.
func describeTools(names []string) string {
	if len(names) == 1 {
		return fmt.Sprintf("tool %q's schema", names[0])
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return fmt.Sprintf("the schemas of %s", strings.Join(quoted, ", "))
}
