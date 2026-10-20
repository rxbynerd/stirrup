package tool

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/rxbynerd/stirrup/types"
)

// maxSearchTerms bounds how many distinct query terms one Search scores,
// so a long query cannot make a search quadratic in the registry size.
const maxSearchTerms = 32

// Search weights per matched query term: the strongest field wins.
const (
	searchWeightName        = 3
	searchWeightDescription = 2
	searchWeightSchema      = 1
)

// LazyLoaderOptions names the tools the default eager rule defers beyond
// the mcp_ prefix, and the MCP servers the search tool's description
// groups deferred tools under.
type LazyLoaderOptions struct {
	// ControlPlaneTools are the internal IDs of tools.controlPlane entries.
	ControlPlaneTools []string
	// MCPServers are the configured MCP server names; an MCP tool is
	// registered as mcp_<server>_<tool>.
	MCPServers []string
}

// lazyEntry is one registered tool as the model would see it.
type lazyEntry struct {
	id  string
	def types.ToolDefinition
	// schemaText is the lowercased property names and descriptions of
	// the tool's top-level input schema, matched by Search.
	schemaText string
}

// LazyMatch is one Search result: the internal tool ID and the
// definition under the model-facing name.
type LazyMatch struct {
	ID         string
	Definition types.ToolDefinition
}

// LazyLoader decides which registered tools the model sees on a turn
// under lazy tool loading. Tools outside the eager set are deferred: they
// stay registered and dispatchable, but reach the model only once a
// search loads them. It is built over the loop's ToolRegistry (normally
// the profile Presenter) after every tool is registered; all IDs it takes
// and returns are internal tool IDs, while definitions carry the
// model-facing names. Safe for concurrent use.
type LazyLoader struct {
	cfg  types.LazyToolLoadingConfig
	opts LazyLoaderOptions

	entries      []lazyEntry
	byID         map[string]int
	presentedFor map[string]string
	internalFor  map[string]string
	deferred     map[string]bool
	// eagerTools counts eager tools other than the search tool.
	eagerTools   int
	unknownEager []string

	mu        sync.Mutex
	loaded    map[string]bool
	loadOrder []string
}

// NewLazyLoader snapshots reg and resolves the eager and deferred sets.
// A non-empty cfg.Eager makes exactly those tools (and the search tool)
// eager; an empty one defers only MCP-imported and control-plane tools.
func NewLazyLoader(reg ToolRegistry, cfg types.LazyToolLoadingConfig, opts LazyLoaderOptions) *LazyLoader {
	l := &LazyLoader{
		cfg:          cfg,
		opts:         opts,
		byID:         map[string]int{},
		presentedFor: map[string]string{},
		internalFor:  map[string]string{},
		deferred:     map[string]bool{},
		loaded:       map[string]bool{},
	}
	for _, def := range reg.List() {
		id := def.Name
		if t := reg.Resolve(def.Name); t != nil {
			id = t.Name
		}
		l.byID[id] = len(l.entries)
		l.presentedFor[id] = def.Name
		l.internalFor[def.Name] = id
		l.entries = append(l.entries, lazyEntry{id: id, def: def, schemaText: schemaSearchText(def.InputSchema)})
	}

	controlPlane := make(map[string]bool, len(opts.ControlPlaneTools))
	for _, name := range opts.ControlPlaneTools {
		controlPlane[name] = true
	}
	eager := make(map[string]bool, len(cfg.Eager))
	for _, name := range cfg.Eager {
		eager[name] = true
		if _, ok := l.byID[name]; !ok {
			l.unknownEager = append(l.unknownEager, name)
		}
	}
	for _, e := range l.entries {
		var deferred bool
		switch {
		case e.id == types.ToolSearchToolName:
			deferred = false
		case len(cfg.Eager) > 0:
			deferred = !eager[e.id]
		default:
			deferred = strings.HasPrefix(e.id, "mcp_") || controlPlane[e.id]
		}
		if deferred {
			l.deferred[e.id] = true
		} else if e.id != types.ToolSearchToolName {
			l.eagerTools++
		}
	}
	return l
}

// ForRegistry returns a fresh loader with the same configuration over
// reg, with nothing loaded. A sub-agent gets one over its own registry.
func (l *LazyLoader) ForRegistry(reg ToolRegistry) *LazyLoader {
	return NewLazyLoader(reg, l.cfg, l.opts)
}

// UnknownEager returns the configured eager names that match no
// registered tool, in configuration order.
func (l *LazyLoader) UnknownEager() []string {
	return slices.Clone(l.unknownEager)
}

// HasDeferred reports whether any tool is deferred. Without one, lazy
// loading changes nothing: Visible returns the plain tool list.
func (l *LazyLoader) HasDeferred() bool {
	return len(l.deferred) > 0
}

// UsesNative reports whether a turn should use the provider's native tool
// search, given whether the routed (provider, model) pair has one. It
// needs strategy "auto", a deferred tool, and an eager tool besides the
// search tool, because a provider rejects a request whose every tool is
// deferred.
func (l *LazyLoader) UsesNative(supported bool) bool {
	return supported &&
		l.cfg.EffectiveStrategy() == types.LazyToolStrategyAuto &&
		l.HasDeferred() &&
		l.eagerTools > 0
}

// Deferred reports whether the tool with internal ID id is deferred.
func (l *LazyLoader) Deferred(id string) bool {
	return l.deferred[id]
}

// Loaded reports whether a search has loaded the deferred tool id.
func (l *LazyLoader) Loaded(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.loaded[id]
}

// Unloaded reports whether id is deferred and not yet loaded, so a call
// to it must be rejected.
func (l *LazyLoader) Unloaded(id string) bool {
	return l.Deferred(id) && !l.Loaded(id)
}

// Load marks the deferred tools among ids as loaded and returns those
// that were not loaded before, in the order given. Unknown and eager IDs
// are ignored.
func (l *LazyLoader) Load(ids []string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var added []string
	for _, id := range ids {
		if !l.deferred[id] || l.loaded[id] {
			continue
		}
		l.loaded[id] = true
		l.loadOrder = append(l.loadOrder, id)
		added = append(added, id)
	}
	return added
}

// LoadedCount returns how many tools have been loaded; LoadedSince with
// the result returns the tools loaded after this call.
func (l *LazyLoader) LoadedCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.loadOrder)
}

// LoadedSince returns the tools loaded after LoadedCount returned n, in
// load order.
func (l *LazyLoader) LoadedSince(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n < 0 || n >= len(l.loadOrder) {
		return nil
	}
	return slices.Clone(l.loadOrder[n:])
}

// Reset unloads every tool, for a run that does not continue the history
// the tools were loaded in.
func (l *LazyLoader) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loaded = map[string]bool{}
	l.loadOrder = nil
}

// InternalID returns the internal ID for a model-facing tool name, or the
// name itself when it names no registered tool.
func (l *LazyLoader) InternalID(name string) string {
	if id, ok := l.internalFor[name]; ok {
		return id
	}
	return name
}

// Visible returns the tool definitions a turn sends. With nothing
// deferred it is the plain list, without the search tool. With native
// set, every tool except the harness search tool is sent and deferred
// ones carry Deferred, whether loaded or not, so the request stays stable
// across turns. Otherwise the eager and loaded tools are sent in
// registration order, followed by the search tool.
func (l *LazyLoader) Visible(native bool) []types.ToolDefinition {
	out := make([]types.ToolDefinition, 0, len(l.entries))
	var search *types.ToolDefinition
	for _, e := range l.entries {
		def := e.def
		switch {
		case e.id == types.ToolSearchToolName:
			if l.HasDeferred() && !native {
				def.Description = l.searchDescription(def.Description)
				search = &def
			}
			continue
		case !l.deferred[e.id]:
		case native:
			def.Deferred = true
		case !l.Loaded(e.id):
			continue
		}
		out = append(out, def)
	}
	if search != nil {
		out = append(out, *search)
	}
	return out
}

// Search returns the deferred tools that best match query, at most limit
// of them (clamped to 1..EffectiveMaxResults). Each query term scores the
// strongest field it appears in: name, then description, then input
// schema property names and descriptions. Ties keep registration order;
// tools matching no term are not returned.
func (l *LazyLoader) Search(query string, limit int) []LazyMatch {
	maxResults := l.cfg.EffectiveMaxResults()
	if limit <= 0 || limit > maxResults {
		limit = maxResults
	}
	terms := searchTerms(query)
	if len(terms) == 0 {
		return nil
	}
	type scored struct {
		index, score int
	}
	var hits []scored
	for i, e := range l.entries {
		if !l.deferred[e.id] {
			continue
		}
		name := strings.ToLower(e.def.Name)
		description := strings.ToLower(e.def.Description)
		score := 0
		for _, term := range terms {
			switch {
			case strings.Contains(name, term):
				score += searchWeightName
			case strings.Contains(description, term):
				score += searchWeightDescription
			case strings.Contains(e.schemaText, term):
				score += searchWeightSchema
			}
		}
		if score > 0 {
			hits = append(hits, scored{index: i, score: score})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].score > hits[b].score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]LazyMatch, len(hits))
	for i, h := range hits {
		e := l.entries[h.index]
		out[i] = LazyMatch{ID: e.id, Definition: e.def}
	}
	return out
}

// PresentReferences returns messages with the internal IDs in every
// tool_search_result block's ToolReferences replaced by model-facing
// names, which is what the provider knows the tools as. Messages without
// such a block are shared, not copied.
func (l *LazyLoader) PresentReferences(messages []types.Message) []types.Message {
	var out []types.Message
	for i, m := range messages {
		var blocks []types.ContentBlock
		for j, b := range m.Content {
			if b.Type != "tool_search_result" || !l.renames(b.ToolReferences) {
				continue
			}
			if blocks == nil {
				blocks = slices.Clone(m.Content)
			}
			refs := make([]string, len(b.ToolReferences))
			for k, id := range b.ToolReferences {
				refs[k] = l.presentedName(id)
			}
			blocks[j].ToolReferences = refs
		}
		if blocks == nil {
			continue
		}
		if out == nil {
			out = slices.Clone(messages)
		}
		out[i].Content = blocks
	}
	if out == nil {
		return messages
	}
	return out
}

// renames reports whether any of ids presents under another name.
func (l *LazyLoader) renames(ids []string) bool {
	for _, id := range ids {
		if l.presentedName(id) != id {
			return true
		}
	}
	return false
}

func (l *LazyLoader) presentedName(id string) string {
	if name, ok := l.presentedFor[id]; ok {
		return name
	}
	return id
}

// searchDescription appends a summary of the deferred tool groups to the
// search tool's registered description, so the model knows what a search
// can find without seeing every tool.
func (l *LazyLoader) searchDescription(base string) string {
	servers := slices.Clone(l.opts.MCPServers)
	// Longest first, so a server whose name extends another's claims its
	// own tools.
	sort.SliceStable(servers, func(i, j int) bool { return len(servers[i]) > len(servers[j]) })
	controlPlane := make(map[string]bool, len(l.opts.ControlPlaneTools))
	for _, name := range l.opts.ControlPlaneTools {
		controlPlane[name] = true
	}

	perServer := map[string]int{}
	var serverOrder []string
	var otherMCP, controlPlaneCount, builtIn int
	for _, e := range l.entries {
		if !l.deferred[e.id] {
			continue
		}
		switch {
		case controlPlane[e.id]:
			controlPlaneCount++
		case strings.HasPrefix(e.id, "mcp_"):
			server := ""
			for _, s := range servers {
				if strings.HasPrefix(e.id, "mcp_"+s+"_") {
					server = s
					break
				}
			}
			if server == "" {
				otherMCP++
				continue
			}
			if perServer[server] == 0 {
				serverOrder = append(serverOrder, server)
			}
			perServer[server]++
		default:
			builtIn++
		}
	}

	var groups []string
	for _, s := range serverOrder {
		groups = append(groups, fmt.Sprintf("%s from MCP server %q", countTools(perServer[s]), s))
	}
	if otherMCP > 0 {
		groups = append(groups, countTools(otherMCP)+" from other MCP servers")
	}
	if controlPlaneCount > 0 {
		groups = append(groups, countTools(controlPlaneCount)+" provided by the control plane")
	}
	if builtIn > 0 {
		groups = append(groups, countTools(builtIn)+" built into the harness")
	}
	return base + " Tools available to load: " + strings.Join(groups, "; ") + "."
}

func countTools(n int) string {
	if n == 1 {
		return "1 tool"
	}
	return fmt.Sprintf("%d tools", n)
}

// searchTerms lowercases query and splits it into distinct terms on any
// character that is not a letter or digit.
func searchTerms(query string) []string {
	fields := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	var terms []string
	for _, f := range fields {
		if slices.Contains(terms, f) {
			continue
		}
		terms = append(terms, f)
		if len(terms) == maxSearchTerms {
			break
		}
	}
	return terms
}

// schemaSearchText returns the lowercased top-level property names and
// descriptions of a JSON Schema, or "" when it has none or does not parse.
func schemaSearchText(schema json.RawMessage) string {
	var parsed struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if len(schema) == 0 || json.Unmarshal(schema, &parsed) != nil {
		return ""
	}
	names := make([]string, 0, len(parsed.Properties))
	for name := range parsed.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		b.WriteString(name)
		b.WriteByte('\n')
		if d := parsed.Properties[name].Description; d != "" {
			b.WriteString(d)
			b.WriteByte('\n')
		}
	}
	return strings.ToLower(b.String())
}
