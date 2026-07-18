package quirks

import (
	"path"
	"sort"
	"sync"
	"time"
)

// Rule is one entry in the quirks registry. ModelMatch is a path.Match
// glob against StreamParams.Model; an empty ModelMatch matches every
// model for the named provider. Apply mutates the ProviderQuirks the
// registry constructed for a resolution; the registry pre-initialises
// every map and slice field, so Apply can read-modify-write without nil
// guards.
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

// NewRegistry returns a Registry wrapping the supplied rules in
// declaration order; it copies the slice, so later mutation of the
// input does not affect resolutions.
func NewRegistry(rules []Rule) *Registry {
	r := &Registry{rules: make([]Rule, len(rules))}
	copy(r.rules, rules)
	return r
}

// defaultRegistry is the process-wide default registry built once from
// BuiltinRules. It is read-only after construction; Resolve returns by
// value so a caller cannot reach back through the result.
var (
	defaultRegistryOnce sync.Once
	defaultRegistry     *Registry
)

// DefaultRegistry returns the process-wide registry built from
// BuiltinRules, constructed once and shared across callers. Callers
// needing a different rule set should use NewRegistry instead.
func DefaultRegistry() *Registry {
	defaultRegistryOnce.Do(func() {
		defaultRegistry = NewRegistry(BuiltinRules())
	})
	return defaultRegistry
}

// Resolve walks all matching rules in specificity-then-declaration order
// and applies each one to a fresh ProviderQuirks. The returned value has
// every map and slice field initialised (non-nil), so callers can read
// them without nil-guarding. Safe to call concurrently.
//
// Resolve delegates to ResolveWithRules and discards the contributing
// rule list; call ResolveWithRules directly when the caller also needs
// to know which rules fired.
func (r *Registry) Resolve(providerType, model string) ProviderQuirks {
	q, _ := r.ResolveWithRules(providerType, model)
	return q
}

// ResolveWithRules is Resolve plus the ordered list of rules that
// actually contributed to the result, in the same specificity-then-
// declaration order Apply was invoked in — the last entry is the rule
// whose writes won on overlapping fields. Rules with a nil Apply are
// filtered out since they did not run.
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
			Gemini: GeminiBehaviourFlags{
				SchemaUnsupportedFeatures: []string{},
			},
		},
	}
	if r == nil {
		return q, nil
	}

	// Sort by glob length ascending, declaration order as tiebreaker, so
	// longer (more specific) rules apply last and win on overlapping keys.
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
// fails to compile is treated as a non-match rather than panicking.
// Exported so CLI introspection reuses the same predicate Resolve uses.
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
// pinned to UTC midnight. Panics on a malformed input, since rule
// definitions are first-party Go code and a typo is a build-time bug.
func Date(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic("quirks.Date: invalid date " + s + ": " + err.Error())
	}
	return t
}
