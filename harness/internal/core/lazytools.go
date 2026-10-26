package core

import (
	"context"
	"fmt"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

// maxSearchReferences bounds the tool references kept from one native
// tool_search_result block.
const maxSearchReferences = 256

// maxLoggedSearchReferences bounds the dropped references named in one
// debug log line.
const maxLoggedSearchReferences = 16

// lazySearch is the tool search a turn offers the model under lazy tool
// loading: the provider's native search, or the harness tool_search.
type lazySearch struct {
	native bool
	// tool is the native search tool's name; empty for the harness search.
	tool string
}

// notLoadedMessage is the rejection text for a call to the deferred tool
// name before a search has loaded it.
func (s lazySearch) notLoadedMessage(name string) string {
	if s.native {
		return fmt.Sprintf("Tool %q is not loaded; search for it with %s first", name, s.tool)
	}
	return fmt.Sprintf("Tool %q is not loaded; call %s to load it first", name, types.ToolSearchToolName)
}

// visibleTools returns the tool definitions a turn routed to selection
// sends, and the tool search it offers. Without lazy loading every
// registered tool is sent.
func (l *AgenticLoop) visibleTools(selection router.ModelSelection) ([]types.ToolDefinition, lazySearch) {
	if l.LazyTools == nil {
		return l.Tools.List(), lazySearch{}
	}
	var capability quirks.ToolSearchCapability
	if l.ToolSearch != nil {
		capability = l.ToolSearch(selection.Provider, selection.Model)
	}
	search := lazySearchFor(l.LazyTools, capability)
	return l.LazyTools.Visible(search.native), search
}

// lazySearchFor returns the tool search a turn offers when the routed
// (provider, model) pair has capability.
func lazySearchFor(loader *tool.LazyLoader, capability quirks.ToolSearchCapability) lazySearch {
	if loader.UsesNative(capability.Supported) {
		return lazySearch{native: true, tool: capability.NativeToolName()}
	}
	return lazySearch{}
}

// deferredToolLoaded reports whether a search has loaded the deferred tool
// presented as name.
func (l *AgenticLoop) deferredToolLoaded(name string) bool {
	return l.LazyTools != nil && l.LazyTools.Loaded(l.LazyTools.InternalID(name))
}

// presentToolReferences returns messages with tool_search_result
// references under the names the provider knows the tools by.
func (l *AgenticLoop) presentToolReferences(messages []types.Message) []types.Message {
	if l.LazyTools == nil {
		return messages
	}
	return l.LazyTools.PresentReferences(messages)
}

// loadSearchedTools rewrites the references of every tool_search_result
// block in a response, in place, to the internal IDs of the registered
// deferred tools they name, and loads them. native is whether the turn
// offered the provider's native search; without it a result is
// unsolicited, keeps no references and loads nothing. Each search that
// loaded a tool not already loaded is reported as a tools_loaded event;
// the tools it loaded are returned in order.
func (l *AgenticLoop) loadSearchedTools(ctx context.Context, blocks []types.ContentBlock, native bool) []string {
	if l.LazyTools == nil {
		return nil
	}
	searches := map[string]types.ContentBlock{}
	var loaded []string
	for i := range blocks {
		b := &blocks[i]
		switch b.Type {
		case "tool_search":
			searches[b.ID] = *b
		case "tool_search_result":
			b.ToolReferences = l.searchReferences(ctx, b.ToolReferences, native)
			if b.IsError || len(b.ToolReferences) == 0 {
				continue
			}
			added := l.LazyTools.Load(b.ToolReferences)
			if len(added) == 0 {
				continue
			}
			search := searches[b.ToolUseID]
			l.emitToolsLoaded(types.ToolsLoadedSourceNative, search.Name, search.Input, added)
			loaded = append(loaded, added...)
		}
	}
	return loaded
}

// searchReferences returns the internal IDs of the registered deferred
// tools among refs, at most maxSearchReferences of them, or nil when
// native is false. Every other reference is dropped and logged at debug.
func (l *AgenticLoop) searchReferences(ctx context.Context, refs []string, native bool) []string {
	if len(refs) == 0 {
		return nil
	}
	var kept, dropped []string
	droppedCount := 0
	for _, name := range refs {
		id := l.LazyTools.InternalID(name)
		if native && l.LazyTools.Deferred(id) && len(kept) < maxSearchReferences {
			kept = append(kept, id)
			continue
		}
		droppedCount++
		if len(dropped) < maxLoggedSearchReferences {
			dropped = append(dropped, name)
		}
	}
	if droppedCount > 0 {
		l.Logger.DebugContext(ctx, "tool search result references dropped",
			"nativeSearch", native, "dropped", droppedCount, "tools", dropped)
	}
	return kept
}

// lazyLoadedCount returns how many tools the run has loaded; zero without
// lazy loading.
func (l *AgenticLoop) lazyLoadedCount() int {
	if l.LazyTools == nil {
		return 0
	}
	return l.LazyTools.LoadedCount()
}

// lazyLoadedSince returns the tools loaded after lazyLoadedCount returned
// n.
func (l *AgenticLoop) lazyLoadedSince(n int) []string {
	if l.LazyTools == nil {
		return nil
	}
	return l.LazyTools.LoadedSince(n)
}

// startLazyTools prepares lazy loading for a run: a run that does not
// continue carried history starts with nothing loaded. The loop's first
// run also logs what lazy loading defers and reports every eager name
// that matches no registered tool as a setup warning.
func (l *AgenticLoop) startLazyTools(ctx context.Context, continuesHistory bool) {
	if l.LazyTools == nil {
		return
	}
	if !continuesHistory {
		l.LazyTools.Reset()
	}
	if l.lazyStartReported {
		return
	}
	l.lazyStartReported = true

	counts := l.LazyTools.DeferredCounts()
	msg := "lazy tool loading enabled"
	if counts.Total() == 0 {
		msg = "lazy tool loading enabled with nothing to defer; every tool is sent"
	}
	l.Logger.InfoContext(ctx, msg,
		"strategy", l.LazyTools.Strategy(),
		"eager", l.LazyTools.EagerCount(),
		"deferred", counts.Total(),
		"deferred.builtIn", counts.BuiltIn,
		"deferred.mcp", counts.MCP,
		"deferred.controlPlane", counts.ControlPlane,
	)
	for _, name := range l.LazyTools.UnknownEager() {
		l.Logger.WarnContext(ctx, "tools.lazyLoading.eager names a tool this run does not have; it is ignored", "tool", name)
		l.emitRunWarning(types.HarnessEvent{
			Message: fmt.Sprintf("tools.lazyLoading.eager names %q, which is not a registered tool; it is ignored", name),
		})
	}
}
