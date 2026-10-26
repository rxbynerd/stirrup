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

// LazyGroupKind is the origin of a deferred tool.
type LazyGroupKind int

const (
	// LazyGroupBuiltIn is a tool built into the harness.
	LazyGroupBuiltIn LazyGroupKind = iota
	// LazyGroupControlPlane is a tool fulfilled by the control plane.
	LazyGroupControlPlane
	// LazyGroupMCPServer is a tool imported from a configured MCP server.
	LazyGroupMCPServer
	// LazyGroupMCPOther is an MCP-prefixed tool that no configured
	// server name claims.
	LazyGroupMCPOther
)

// LazyGroup is the origin a tool is grouped under: the unit a search
// caps and a result line names. Server is the configured server name for
// LazyGroupMCPServer and empty otherwise, so two groups are equal exactly
// when their tools share an origin.
type LazyGroup struct {
	Kind   LazyGroupKind
	Server string
}

// Label describes the group for a search result line. The server name
// comes from operator configuration, never from the server.
func (g LazyGroup) Label() string {
	switch g.Kind {
	case LazyGroupControlPlane:
		return "control plane"
	case LazyGroupMCPServer:
		return fmt.Sprintf("MCP server %q", g.Server)
	case LazyGroupMCPOther:
		return "MCP"
	default:
		return "built-in"
	}
}

// controlPlaneNamespace is the namespace of deferred control-plane tools.
const controlPlaneNamespace = "control_plane"

// Namespace returns the name and description a native tool search groups
// the group's deferred tools under, or empty strings for a group that
// stays ungrouped (built-in tools and MCP tools no configured server
// claims). Both are built from operator configuration, never from text a
// tool source supplies.
func (g LazyGroup) Namespace() (name, description string) {
	switch g.Kind {
	case LazyGroupControlPlane:
		return controlPlaneNamespace, "Tools provided by the control plane."
	case LazyGroupMCPServer:
		return "mcp_" + g.Server, fmt.Sprintf("Tools from MCP server %q.", g.Server)
	default:
		return "", ""
	}
}

// LazyMatch is one Search result: the internal tool ID, the definition
// under the model-facing name, and the group the tool belongs to.
type LazyMatch struct {
	ID         string
	Definition types.ToolDefinition
	Group      LazyGroup
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
	// servers are opts.MCPServers longest first, so a server whose name
	// extends another's claims its own tools. They attribute only tools
	// that name no owner.
	servers []string
	// owners maps an MCP tool's internal ID to the server that registered it.
	owners map[string]string

	entries      []lazyEntry
	byID         map[string]int
	presentedFor map[string]string
	internalFor  map[string]string
	deferred     map[string]bool
	controlPlane map[string]bool
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
		servers:      longestFirst(opts.MCPServers),
		owners:       map[string]string{},
		byID:         map[string]int{},
		presentedFor: map[string]string{},
		internalFor:  map[string]string{},
		deferred:     map[string]bool{},
		controlPlane: make(map[string]bool, len(opts.ControlPlaneTools)),
		loaded:       map[string]bool{},
	}
	for _, def := range reg.List() {
		id := def.Name
		if t := reg.Resolve(def.Name); t != nil {
			id = t.Name
			if t.MCPServer != "" {
				l.owners[id] = t.MCPServer
			}
		}
		l.byID[id] = len(l.entries)
		l.presentedFor[id] = def.Name
		l.internalFor[def.Name] = id
		l.entries = append(l.entries, lazyEntry{id: id, def: def, schemaText: schemaSearchText(def.InputSchema)})
	}

	for _, name := range opts.ControlPlaneTools {
		l.controlPlane[name] = true
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
			deferred = strings.HasPrefix(e.id, "mcp_") || l.controlPlane[e.id]
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
// reg, with nothing loaded. A sub-agent gets one over its own registry;
// it reports no unknown eager names, because the parent's loader already
// reported them and the sub-agent's mode may have filtered tools out.
func (l *LazyLoader) ForRegistry(reg ToolRegistry) *LazyLoader {
	child := NewLazyLoader(reg, l.cfg, l.opts)
	child.unknownEager = nil
	return child
}

// UnknownEager returns the configured eager names that match no
// registered tool, in configuration order.
func (l *LazyLoader) UnknownEager() []string {
	return slices.Clone(l.unknownEager)
}

// LazyDeferredCounts counts a loader's deferred tools by origin.
type LazyDeferredCounts struct {
	BuiltIn      int
	MCP          int
	ControlPlane int
}

// Total returns the number of deferred tools.
func (c LazyDeferredCounts) Total() int {
	return c.BuiltIn + c.MCP + c.ControlPlane
}

// DeferredCounts returns the loader's deferred tools counted by origin.
func (l *LazyLoader) DeferredCounts() LazyDeferredCounts {
	var c LazyDeferredCounts
	for _, e := range l.entries {
		if !l.deferred[e.id] {
			continue
		}
		switch l.GroupOf(e.id).Kind {
		case LazyGroupControlPlane:
			c.ControlPlane++
		case LazyGroupMCPServer, LazyGroupMCPOther:
			c.MCP++
		default:
			c.BuiltIn++
		}
	}
	return c
}

// LazyGroupCount counts the eager and deferred tools of one group, the
// search tool excluded.
type LazyGroupCount struct {
	Group    LazyGroup
	Eager    int
	Deferred int
}

// GroupCounts returns the loader's tools counted per group, built-in
// first, then control-plane, then MCP servers by name. Groups with no
// tool are omitted.
func (l *LazyLoader) GroupCounts() []LazyGroupCount {
	counts := map[LazyGroup]*LazyGroupCount{}
	for _, e := range l.entries {
		if e.id == types.ToolSearchToolName {
			continue
		}
		group := l.GroupOf(e.id)
		c := counts[group]
		if c == nil {
			c = &LazyGroupCount{Group: group}
			counts[group] = c
		}
		if l.deferred[e.id] {
			c.Deferred++
		} else {
			c.Eager++
		}
	}
	out := make([]LazyGroupCount, 0, len(counts))
	for _, c := range counts {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group.Kind != out[j].Group.Kind {
			return out[i].Group.Kind < out[j].Group.Kind
		}
		return out[i].Group.Server < out[j].Group.Server
	})
	return out
}

// GroupOf returns the group the tool with internal ID id belongs to:
// control-plane tools first, then MCP tools by the server that registered
// them or, for an MCP-prefixed tool with no recorded owner, by the longest
// configured server name that prefixes mcp_<server>_, then every other
// tool as built-in.
func (l *LazyLoader) GroupOf(id string) LazyGroup {
	if l.controlPlane[id] {
		return LazyGroup{Kind: LazyGroupControlPlane}
	}
	if server, ok := l.owners[id]; ok {
		return LazyGroup{Kind: LazyGroupMCPServer, Server: server}
	}
	if !strings.HasPrefix(id, "mcp_") {
		return LazyGroup{Kind: LazyGroupBuiltIn}
	}
	for _, s := range l.servers {
		if strings.HasPrefix(id, "mcp_"+s+"_") {
			return LazyGroup{Kind: LazyGroupMCPServer, Server: s}
		}
	}
	return LazyGroup{Kind: LazyGroupMCPOther}
}

// EagerCount returns the number of eager tools, the search tool excluded.
func (l *LazyLoader) EagerCount() int {
	return l.eagerTools
}

// Strategy returns the effective search strategy, "auto" or "harness".
func (l *LazyLoader) Strategy() string {
	return l.cfg.EffectiveStrategy()
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
// across turns; deferred MCP-server and control-plane tools also carry
// their group's Namespace. Otherwise the eager and loaded tools are sent in
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
			def.Namespace, def.NamespaceDescription = l.GroupOf(e.id).Namespace()
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

// Search returns the deferred tools not yet loaded that best match query,
// at most limit of them (clamped to 1..EffectiveMaxResults). Each query
// term scores the strongest field it appears in: name, then description,
// then input schema property names and descriptions. Ties keep
// registration order; tools matching no term are not returned.
//
// A tool whose name or ID equals the whole query ranks ahead of every
// scored hit and is always returned, but counts toward its group's cap,
// so a group takes max((limit+1)/2, its exact hits) results while other
// groups have matches. Slots a group cap leaves free are backfilled in
// rank order, so a lone matching group is not starved.
func (l *LazyLoader) Search(query string, limit int) []LazyMatch {
	maxResults := l.cfg.EffectiveMaxResults()
	if limit <= 0 || limit > maxResults {
		limit = maxResults
	}
	return l.search(query, limit, true, func(id string) bool { return !l.Loaded(id) })
}

// LoadedMatches returns the loaded deferred tools that match query,
// ranked as Search ranks them, at most MaxLazyToolSearchResults of them.
// Nothing is loaded by this call, so no group cap applies.
func (l *LazyLoader) LoadedMatches(query string) []LazyMatch {
	return l.search(query, types.MaxLazyToolSearchResults, false, l.Loaded)
}

// search ranks the deferred tools for which include reports true against
// query and returns at most limit of them, capping each group when
// capGroups is set.
func (l *LazyLoader) search(query string, limit int, capGroups bool, include func(id string) bool) []LazyMatch {
	terms := searchTerms(query)
	if len(terms) == 0 {
		return nil
	}
	type scored struct {
		index, score int
		exact        bool
		group        LazyGroup
	}
	query = strings.ToLower(strings.TrimSpace(query))
	var hits []scored
	for i, e := range l.entries {
		if !l.deferred[e.id] || !include(e.id) {
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
		group := l.GroupOf(e.id)
		exact := l.exactName(e, group, query)
		if score > 0 || exact {
			hits = append(hits, scored{index: i, score: score, exact: exact, group: group})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool {
		if hits[a].exact != hits[b].exact {
			return hits[a].exact
		}
		return hits[a].score > hits[b].score
	})

	selected := make([]bool, len(hits))
	groupCap := limit
	if capGroups {
		groupCap = (limit + 1) / 2
	}
	perGroup := map[LazyGroup]int{}
	taken := 0
	for i, h := range hits {
		if taken == limit {
			break
		}
		if !h.exact && perGroup[h.group] >= groupCap {
			continue
		}
		perGroup[h.group]++
		selected[i] = true
		taken++
	}
	for i := range hits {
		if taken == limit {
			break
		}
		if !selected[i] {
			selected[i] = true
			taken++
		}
	}

	out := make([]LazyMatch, 0, taken)
	for i, h := range hits {
		if selected[i] {
			e := l.entries[h.index]
			out = append(out, LazyMatch{ID: e.id, Definition: e.def, Group: h.group})
		}
	}
	return out
}

// exactName reports whether query, already trimmed and lowercased, is the
// whole of the tool's model-facing name, internal ID, or (for a tool of a
// configured MCP server) the server's own name for it.
func (l *LazyLoader) exactName(e lazyEntry, group LazyGroup, query string) bool {
	if query == "" {
		return false
	}
	if strings.ToLower(e.def.Name) == query || strings.ToLower(e.id) == query {
		return true
	}
	if group.Kind != LazyGroupMCPServer {
		return false
	}
	return strings.ToLower(strings.TrimPrefix(e.id, "mcp_"+group.Server+"_")) == query
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
	perServer := map[string]int{}
	var serverOrder []string
	var otherMCP, controlPlaneCount, builtIn int
	for _, e := range l.entries {
		if !l.deferred[e.id] {
			continue
		}
		g := l.GroupOf(e.id)
		switch g.Kind {
		case LazyGroupControlPlane:
			controlPlaneCount++
		case LazyGroupMCPOther:
			otherMCP++
		case LazyGroupMCPServer:
			if perServer[g.Server] == 0 {
				serverOrder = append(serverOrder, g.Server)
			}
			perServer[g.Server]++
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

// longestFirst returns a copy of names sorted longest first.
func longestFirst(names []string) []string {
	out := slices.Clone(names)
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
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
