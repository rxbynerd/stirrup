package tool

import (
	"fmt"
	"sort"

	"github.com/rxbynerd/stirrup/harness/internal/tool/toolname"
	"github.com/rxbynerd/stirrup/types"
)

// Profile is a model-facing presentation for a set of registered tools:
// it remaps internal tool IDs to provider-friendly aliases and optional
// alternate descriptions without changing dispatch identity. A tool
// absent from the table presents under its internal name. Profile data
// is presentation only — it cannot add, remove, or re-gate tools.
type Profile struct {
	// Name is the RunConfig.Tools.Profile value this table implements.
	Name string
	// aliases maps internal tool ID → model-facing alias. An internal ID
	// with no entry is presented under its own name.
	aliases map[string]string
	// descriptions maps internal tool ID → an alternate model-facing
	// description. Empty means present the registered description unchanged.
	descriptions map[string]string
}

// defaultProfile is the identity presentation: no aliases, no description
// overrides.
var defaultProfile = &Profile{Name: "default"}

// codingClassicProfile presents terse coding-CLI names some models call by
// reflex (grep, find, bash). Other built-ins keep their internal names;
// MCP tools and anything absent from the table present unchanged.
var codingClassicProfile = &Profile{
	Name: "coding-classic",
	aliases: map[string]string{
		"grep_files":  "grep",
		"find_files":  "find",
		"run_command": "bash",
	},
	descriptions: map[string]string{},
}

// NewProfile constructs a Profile from an internal-ID→alias map and an
// internal-ID→description map; either may be nil. The maps are used
// directly, not copied — callers must not mutate them after construction.
// It does not register the profile with ProfileFor.
func NewProfile(name string, aliases, descriptions map[string]string) *Profile {
	return &Profile{Name: name, aliases: aliases, descriptions: descriptions}
}

// ProfileFor returns the Profile table for a RunConfig.Tools.Profile
// value. "" and "default" both select the identity presentation. An
// unknown name returns the default profile and false.
func ProfileFor(name string) (*Profile, bool) {
	switch name {
	case "", "default":
		return defaultProfile, true
	case "coding-classic":
		return codingClassicProfile, true
	default:
		return defaultProfile, false
	}
}

// aliasTarget returns the model-facing alias for an internal tool ID
// before collision resolution, falling back to the internal ID when the
// profile does not remap it.
func (p *Profile) aliasTarget(internal string) string {
	if p == nil {
		return internal
	}
	if alias, ok := p.aliases[internal]; ok && alias != "" {
		return alias
	}
	return internal
}

// describe returns the model-facing description for an internal tool ID,
// falling back to the registered description when the profile does not
// override it.
func (p *Profile) describe(internal, registered string) string {
	if p == nil {
		return registered
	}
	if d, ok := p.descriptions[internal]; ok && d != "" {
		return d
	}
	return registered
}

// Presenter wraps a ToolRegistry and applies a Profile so the loop and
// provider adapters see model-facing aliases while dispatch keeps
// resolving to internal tool IDs. Alias collisions are resolved with the
// same toolname.Build disambiguation the provider function-name
// normalizer uses.
//
// The presented name set is computed once at construction, over the
// registry snapshot at that time — safe because the wrapped registry is
// not mutated after the loop starts.
type Presenter struct {
	inner   ToolRegistry
	profile *Profile

	// presentedFor maps internal name → presented (alias, collision-
	// resolved) name.
	presentedFor map[string]string
	// internalFor maps presented name → internal name (the Resolve reverse
	// lookup). Internal names are also inserted as identity keys so a
	// config or model that calls a tool by its internal name still
	// resolves.
	internalFor map[string]string
}

// Compile-time assertion that Presenter satisfies ToolRegistry. Kept as
// (*Impl)(nil), not the concrete-value form golangci-lint suggests.
var _ ToolRegistry = (*Presenter)(nil)

// NewPresenter wraps inner with the named profile's presentation. A nil
// profile is treated as the default (identity) profile, safe to install
// unconditionally.
//
// Returns an error only when alias-target collision resolution fails in
// toolname.Build. Per-provider wire normalization still runs downstream
// in the NormalizingAdapter; this build's only job is collision
// resolution.
func NewPresenter(inner ToolRegistry, profile *Profile) (*Presenter, error) {
	if profile == nil {
		profile = defaultProfile
	}
	p := &Presenter{
		inner:        inner,
		profile:      profile,
		presentedFor: map[string]string{},
		internalFor:  map[string]string{},
	}

	defs := inner.List()

	// Keys are internal tool IDs (unique); the alias is the candidate, so
	// two tools aliasing to the same target hit BuildFromCandidates'
	// collision path rather than a duplicate-key error. Sorted by internal
	// ID first so disambiguation is independent of registration order.
	sortedDefs := make([]types.ToolDefinition, len(defs))
	copy(sortedDefs, defs)
	sort.Slice(sortedDefs, func(i, j int) bool { return sortedDefs[i].Name < sortedDefs[j].Name })

	keys := make([]string, len(sortedDefs))
	candidates := make([]string, len(sortedDefs))
	for i, d := range sortedDefs {
		keys[i] = d.Name
		candidates[i] = profile.aliasTarget(d.Name)
	}

	mapping, err := toolname.BuildFromCandidates(keys, candidates, aliasPolicy)
	if err != nil {
		return nil, fmt.Errorf("tool profile %q alias collision: %w", profile.Name, err)
	}

	for _, d := range defs {
		internal := d.Name
		presented := mapping.Translate(internal) // internal ID → collision-resolved alias
		p.presentedFor[internal] = presented
		p.internalFor[presented] = internal
		// Also insert the internal name as an identity key, skipping when
		// it would clobber a distinct alias mapping — the alias wins.
		if _, taken := p.internalFor[internal]; !taken {
			p.internalFor[internal] = internal
		}
	}

	return p, nil
}

// aliasPolicy is the permissive policy for alias-target collisions; alias
// targets are author-controlled clean ASCII, not arbitrary MCP strings, so
// strict per-provider wire normalization runs later in NormalizingAdapter,
// not here.
var aliasPolicy = toolname.Policy{MaxLen: 64, AllowHyphen: true, AllowLeadingDigit: true}

// List returns the wrapped registry's definitions with names (and
// descriptions, when the profile overrides them) rewritten to the
// model-facing presentation. Order matches the wrapped registry's List.
func (p *Presenter) List() []types.ToolDefinition {
	defs := p.inner.List()
	out := make([]types.ToolDefinition, len(defs))
	for i, d := range defs {
		out[i] = d
		if presented, ok := p.presentedFor[d.Name]; ok {
			out[i].Name = presented
		}
		out[i].Description = p.profile.describe(d.Name, d.Description)
	}
	return out
}

// Resolve looks up a tool by its model-facing presented name or its
// internal ID, returning nil if neither resolves. The returned tool's
// Name is the internal identity — permission checks and the security
// guard key on it, so aliasing changes only the name the model uses.
func (p *Presenter) Resolve(name string) *Tool {
	if internal, ok := p.internalFor[name]; ok {
		return p.inner.Resolve(internal)
	}
	return p.inner.Resolve(name)
}

// Unwrap returns the wrapped ToolRegistry. It is an escape hatch (e.g. a
// test swapping a handler via *Registry.Register) — production dispatch
// should go through Resolve so aliasing is honoured.
func (p *Presenter) Unwrap() ToolRegistry {
	return p.inner
}

// Profile returns the Profile this presenter applies. Never nil —
// NewPresenter substitutes the default profile for a nil argument.
func (p *Presenter) Profile() *Profile {
	return p.profile
}

// InternalName returns the internal tool ID for a model-facing name,
// falling back to the input when the name is not a known alias.
func (p *Presenter) InternalName(name string) string {
	if internal, ok := p.internalFor[name]; ok {
		return internal
	}
	return name
}
