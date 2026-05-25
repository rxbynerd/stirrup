package tool

import (
	"fmt"
	"sort"

	"github.com/rxbynerd/stirrup/harness/internal/tool/toolname"
	"github.com/rxbynerd/stirrup/types"
)

// Profile is a model-facing presentation for a set of registered tools
// (issue #234). It remaps internal tool IDs to provider-friendly aliases
// and optional alternate descriptions without touching dispatch
// identities: a tool aliased to "grep" still resolves to grep_files and
// runs the same handler. Tools absent from the table are presented under
// their internal name unchanged.
//
// A Profile carries presentation data only. It cannot add, remove, or
// re-gate tools — the presenter applies a Profile over whatever the
// underlying registry already holds, so a tool a read-only mode declined
// to register has no alias and cannot be smuggled in via a profile.
type Profile struct {
	// Name is the closed-set RunConfig.Tools.Profile value this table
	// implements ("default", "coding-classic", …). Stored for diagnostics
	// and so a presenter can report which profile produced an alias.
	Name string
	// aliases maps internal tool ID → model-facing alias. An entry whose
	// alias equals the key is allowed (an explicit identity) but pointless;
	// omit it instead. An internal ID with no entry is presented under its
	// own name.
	aliases map[string]string
	// descriptions maps internal tool ID → an alternate model-facing
	// description. Empty for a tool means "present the registered
	// description unchanged". Kept separate from aliases so a profile can
	// re-describe a tool without renaming it (and vice versa).
	descriptions map[string]string
}

// defaultProfile is the identity presentation: no aliases, no description
// overrides. ProfileFor("") and ProfileFor("default") both return it, so
// a bare run presents tools exactly as registered.
var defaultProfile = &Profile{Name: "default"}

// codingClassicProfile presents the terse coding-CLI names that models
// with strong shell/coding priors call by reflex. The targets are the
// canonical Unix tool names (grep, find) plus "bash" for the shell, which
// several model families bias toward over "run_command". read_file,
// list_directory, and write_file/edit_file keep their internal names —
// they are already the idiomatic descriptive forms and renaming them
// would lose more recognition than it gains.
//
// Only the built-in coding primitives are remapped; MCP tools (namespaced
// mcp_*) and any tool absent from this table present unchanged.
var codingClassicProfile = &Profile{
	Name: "coding-classic",
	aliases: map[string]string{
		"grep_files":  "grep",
		"find_files":  "find",
		"run_command": "bash",
	},
}

// NewProfile constructs a Profile from an internal-ID→alias map and an
// internal-ID→description map. Either map may be nil (no aliases / no
// description overrides). The maps are used directly, not copied, so the
// caller must not mutate them after construction.
//
// The built-in profiles are package-private values; this constructor is
// the supported way to build a custom presentation (e.g. an embedder
// wiring a provider-native profile, or a test exercising a collision the
// built-in profiles do not produce). It does not register the profile
// with ProfileFor — a custom profile is passed straight to NewPresenter.
func NewProfile(name string, aliases, descriptions map[string]string) *Profile {
	return &Profile{Name: name, aliases: aliases, descriptions: descriptions}
}

// ProfileFor returns the Profile table for a RunConfig.Tools.Profile
// value. The empty string and "default" both select the identity
// presentation. An unknown name returns the default profile and false;
// callers that have already passed ValidateRunConfig will never see
// false, but the factory checks it defensively so a name added to the
// validator's closed set without a matching table here fails loudly
// rather than silently presenting no aliases.
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
// the provider adapters see model-facing aliases while dispatch keeps
// resolving to internal tool IDs (issue #234).
//
// Construction builds a single toolname.Mapping over the *alias targets*
// of the wrapped registry's current tool set. Reusing toolname.Build is
// deliberate: alias collisions (two internal tools whose profile aliases
// land on the same string) are resolved by exactly the same deterministic
// hash-suffix disambiguation that provider function-name normalization
// uses (#223), so there is no second collision algorithm to keep in sync.
// The mapping's external side is the presented name; its internal side is
// recovered for Resolve.
//
// The presented name set is computed once at construction. The registry a
// Presenter wraps is the post-MCP-discovery registry the factory builds,
// which is not mutated after the loop starts, so a snapshot is correct.
type Presenter struct {
	inner   ToolRegistry
	profile *Profile

	// presentedFor maps internal name → presented (alias, collision-
	// resolved) name. Used by List to rename definitions and by callers
	// that need the model-facing name for a known internal tool (trace).
	presentedFor map[string]string
	// internalFor maps presented name → internal name. The Resolve
	// reverse lookup. Internal names are also inserted as identity keys so
	// a config or model that calls a tool by its internal name still
	// resolves — this is the config-compatibility guarantee.
	internalFor map[string]string
}

// Compile-time assertion that Presenter satisfies ToolRegistry. Kept in
// the (*Impl)(nil) form rather than the concrete-value form the linter
// suggests so the guard cannot be silently weakened — see CLAUDE.md
// "Known false positives".
var _ ToolRegistry = (*Presenter)(nil)

// NewPresenter wraps inner with the named profile's presentation. The
// default profile (and an empty name) produces an identity presenter:
// List and Resolve behave exactly as the wrapped registry, so the
// presenter is safe to install unconditionally.
//
// Returns an error only when alias-target collision resolution fails
// inside toolname.Build (e.g. a pathological alias that cannot be made
// unique under the policy). The policy used is the permissive ASCII set
// (hyphen and leading digit allowed) because alias targets are author-
// controlled clean names defined in this package, not arbitrary MCP
// strings; per-provider wire normalization still runs downstream in the
// NormalizingAdapter, so this build's only job is collision resolution.
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

	// Resolve alias collisions through toolname.BuildFromCandidates, the
	// shared collision core (#223). The keys are the internal tool IDs
	// (unique, the registry contract), and the candidate for each key is
	// its profile alias target. Keying disambiguation off the internal ID
	// — not the alias — is what lets two tools alias to the same target:
	// the IDs are distinct, so Build sees a candidate collision (not a
	// duplicate-key error) and disambiguates one alias with the same
	// deterministic SHA-suffix a provider name collision would get. No
	// second collision algorithm exists.
	// Sort by internal ID before resolving so the collision disambiguation
	// is independent of registration order: two runs of the same tool set
	// pin the same alias to the same internal ID regardless of the order
	// the registry happened to list them in. Mirrors the BuildSorted
	// rationale in the provider normalizer.
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
		// Insert the internal name as an identity key too so a model or
		// config that calls the tool by its internal ID still resolves.
		// Skip when it would clobber a distinct mapping (a presented alias
		// equal to some *other* tool's internal name): the alias mapping
		// wins, and the colliding internal-name call falls through to the
		// renamed-tool / unknown-tool path, which is the safe outcome.
		if _, taken := p.internalFor[internal]; !taken {
			p.internalFor[internal] = internal
		}
	}

	return p, nil
}

// aliasPolicy is the toolname.Policy used to resolve alias-target
// collisions. Alias targets are clean author-controlled ASCII, so the
// permissive set (hyphen + leading digit allowed, 64-char cap) suffices;
// the strict per-provider normalization that turns a presented name into
// a wire-safe function name runs later in the NormalizingAdapter.
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

// Resolve looks up a tool by its model-facing presented name or by its
// internal ID, returning the underlying *Tool from the wrapped registry.
// Returns nil when neither resolves. The returned tool's Name is the
// internal identity — dispatch, permission checks, and the security guard
// all key on it, so aliasing changes only the name the model uses, never
// the tool that runs.
func (p *Presenter) Resolve(name string) *Tool {
	if internal, ok := p.internalFor[name]; ok {
		return p.inner.Resolve(internal)
	}
	return p.inner.Resolve(name)
}

// Unwrap returns the ToolRegistry the presenter wraps. It exists so a
// caller that holds the loop's Tools (a *Presenter) can reach the
// underlying registry for operations the presentation layer does not
// expose (e.g. a test swapping a tool's handler via *Registry.Register).
// Production dispatch should go through Resolve so the alias→internal
// binding is honoured; Unwrap is an escape hatch, not the common path.
func (p *Presenter) Unwrap() ToolRegistry {
	return p.inner
}

// InternalName returns the internal tool ID for a model-facing name,
// falling back to the input when the name is not a known alias. Used by
// the dispatch trace path to record the internal identity alongside the
// model-facing alias (issue #234 acceptance criterion). The fallback
// keeps the call site terse: a name that is already an internal ID, or an
// unknown tool name the model invented, round-trips unchanged.
func (p *Presenter) InternalName(name string) string {
	if internal, ok := p.internalFor[name]; ok {
		return internal
	}
	return name
}
