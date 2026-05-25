package quirks

import (
	"path"
	"sort"
	"sync"
	"time"
)

// Rule is one entry in the quirks registry.
//
// ProviderType is matched exactly against the RunConfig provider.type.
// ModelMatch is a path.Match glob compared against StreamParams.Model;
// an empty ModelMatch matches every model for the named provider.
//
// Description is required and surfaces in trace attributes and the
// `stirrup providers quirks` CLI output, so operators can recognise
// which rule fired without reading the source.
//
// LastVerified is the date the rule was last validated against the
// upstream provider's wire shape. Set via Date("2026-05-24"). Resolve
// treats rules older than the staleness window as a signal, not an
// error — see TestRuleStaleness in quirks_test.go.
//
// Apply mutates the ProviderQuirks the registry constructed for a
// resolution. The registry pre-initialises every map and slice field
// on the value, so Apply can read-modify-write maps without nil
// guards.
//
// BaseURLMatch is intentionally absent in v1 (design D11). A predicate
// against the provider's BaseURL is the obvious extension point; it is
// not added until a concrete divergence requires URL keying.
type Rule struct {
	ProviderType string    // exact RunConfig provider.type; "" reserved as wildcard (not used in v1)
	ModelMatch   string    // path.Match glob against StreamParams.Model; "" matches all models
	Description  string    // required; used in trace attributes and CLI introspection
	LastVerified time.Time // set via Date("2026-05-24"); staleness signal at 180 days
	Apply        func(*ProviderQuirks)
}

// Registry is the ordered rule list. BuiltinRules() returns the default
// registry. Tests may inject a different registry via the adapters'
// constructors.
type Registry struct {
	rules []Rule
}

// NewRegistry returns a Registry that wraps the supplied rules in
// declaration order. The caller retains ownership of the slice; the
// registry stores its own copy so later mutations of the input do not
// affect resolutions.
func NewRegistry(rules []Rule) *Registry {
	r := &Registry{rules: make([]Rule, len(rules))}
	copy(r.rules, rules)
	return r
}

// defaultRegistry is the process-wide default registry built once from
// BuiltinRules. Per design risk 8 the singleton is read-only and must
// not be mutated after construction; Resolve returns by value so a
// caller cannot reach back through the result.
var (
	defaultRegistryOnce sync.Once
	defaultRegistry     *Registry
)

// DefaultRegistry returns the process-wide registry built from
// BuiltinRules. The registry is constructed exactly once and shared
// across every caller; callers that need a different rule set (tests,
// compat profile injection) should use NewRegistry instead.
func DefaultRegistry() *Registry {
	defaultRegistryOnce.Do(func() {
		defaultRegistry = NewRegistry(BuiltinRules())
	})
	return defaultRegistry
}

// Resolve walks all matching rules in specificity-then-declaration-order
// and applies each one to a fresh ProviderQuirks. The returned value has
// every map field initialised to a non-nil empty map and every slice
// field to a non-nil empty slice, so callers can read fields without
// nil-guarding.
//
// Specificity ordering: rules with a longer ModelMatch glob run later
// so their writes override earlier rules. Ties (identical glob length)
// break on declaration order. An empty ModelMatch is treated as length
// 0 and matches every model — so a "default for this provider" rule
// can be declared once and overridden by a specific glob.
//
// Resolve is safe to call concurrently; the registry's rule slice is
// read-only after NewRegistry, and the returned ProviderQuirks is a
// value type with freshly allocated maps and slices.
//
// Resolve delegates to ResolveWithRules and discards the contributing
// rule list. Call ResolveWithRules directly when the caller also needs
// to log or surface which rules fired (e.g. the per-Stream debug line
// in the OpenAI adapters that pins which rule caused a suppression).
func (r *Registry) Resolve(providerType, model string) ProviderQuirks {
	q, _ := r.ResolveWithRules(providerType, model)
	return q
}

// ResolveWithRules is Resolve plus the ordered list of rules that
// actually contributed to the result. The returned []Rule is sorted
// in the same specificity-then-declaration order that Apply was
// invoked in, so the last entry is the rule whose writes won on
// overlapping fields. Rules with a nil Apply are filtered out — they
// did not run, so reporting them as "applied" would be a lie that
// undermines the trace-attribute use case.
//
// The slice is freshly allocated on every call and safe for the
// caller to mutate; the registry's internal rule slice is not
// shared.
//
// Use this when the caller needs to surface contributing rules in
// trace attributes, debug logs, or CLI introspection. Use Resolve
// when the rule list is not needed.
func (r *Registry) ResolveWithRules(providerType, model string) (ProviderQuirks, []Rule) {
	q := ProviderQuirks{
		FieldRenames:   map[string]string{},
		OmitFields:     []string{},
		ValueOverrides: map[string]Value{},
		EnumCoercions:  map[string]map[string]string{},
		ReplayFields:   []string{},
		BehaviourFlags: ProviderBehaviourFlags{
			OpenAI: OpenAIBehaviourFlags{
				ExtraBodyFields: map[string]any{},
			},
		},
	}
	if r == nil {
		return q, nil
	}

	// Collect (originalIndex, rule) pairs for every matching rule, then
	// sort by glob length ascending with declaration order as the
	// tiebreaker. Sorting ascending and applying in order means longer
	// (more specific) rules write last and win on overlapping keys.
	type indexed struct {
		idx  int
		rule Rule
	}
	matches := make([]indexed, 0, len(r.rules))
	for i, rule := range r.rules {
		if !RuleMatches(rule, providerType, model) {
			continue
		}
		matches = append(matches, indexed{idx: i, rule: rule})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		li := len(matches[i].rule.ModelMatch)
		lj := len(matches[j].rule.ModelMatch)
		if li != lj {
			return li < lj
		}
		return matches[i].idx < matches[j].idx
	})
	applied := make([]Rule, 0, len(matches))
	for _, m := range matches {
		if m.rule.Apply == nil {
			continue
		}
		m.rule.Apply(&q)
		applied = append(applied, m.rule)
	}
	return q, applied
}

// RuleMatches reports whether the rule fires for the given (provider,
// model) pair. An empty ModelMatch matches every model. A glob that
// fails to compile (e.g. an unmatched `[`) is treated as a non-match
// rather than panicking, on the same principle as the existing
// validators: bad input rejects loudly elsewhere, runtime resolution
// stays safe.
//
// Exported so the CLI introspection surface can reuse the exact same
// predicate the agentic loop's Resolve walks, eliminating the silent
// divergence risk if the predicate ever gains a second dimension
// (e.g. design D11's BaseURLMatch seam).
func RuleMatches(rule Rule, providerType, model string) bool {
	if rule.ProviderType != providerType {
		return false
	}
	if rule.ModelMatch == "" {
		return true
	}
	ok, err := path.Match(rule.ModelMatch, model)
	if err != nil {
		return false
	}
	return ok
}

// Date parses an ISO-8601 calendar date (YYYY-MM-DD) into a time.Time
// pinned to UTC midnight. Rule authors use it so LastVerified is
// declarative and grep-able rather than a `time.Date(...)` expression.
// Panics on a malformed input — rule definitions are first-party Go
// code, not operator input, so a typo is a build-time bug worth
// failing fast on.
func Date(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic("quirks.Date: invalid date " + s + ": " + err.Error())
	}
	return t
}
