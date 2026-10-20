package core

import (
	"context"
	"fmt"

	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/types"
)

// visibleTools returns the tool definitions a turn routed to selection
// sends, and whether the turn uses the provider's native tool search.
// Without lazy loading every registered tool is sent.
func (l *AgenticLoop) visibleTools(selection router.ModelSelection) ([]types.ToolDefinition, bool) {
	if l.LazyTools == nil {
		return l.Tools.List(), false
	}
	supported := l.ToolSearch != nil && l.ToolSearch(selection.Provider, selection.Model).Supported
	native := l.LazyTools.UsesNative(supported)
	return l.LazyTools.Visible(native), native
}

// presentToolReferences returns messages with tool_search_result
// references under the names the provider knows the tools by.
func (l *AgenticLoop) presentToolReferences(messages []types.Message) []types.Message {
	if l.LazyTools == nil {
		return messages
	}
	return l.LazyTools.PresentReferences(messages)
}

// loadSearchedTools maps the references of every tool_search_result block
// in a response to internal IDs, in place, and loads them. Each search
// that loaded a tool not already loaded is reported as a tools_loaded
// event; the tools it loaded are returned in order.
func (l *AgenticLoop) loadSearchedTools(blocks []types.ContentBlock) []string {
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
			for j, name := range b.ToolReferences {
				b.ToolReferences[j] = l.LazyTools.InternalID(name)
			}
			if b.IsError {
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
// continue carried history starts with nothing loaded, and every eager
// name that matches no registered tool is reported as a setup warning.
func (l *AgenticLoop) startLazyTools(ctx context.Context, continuesHistory bool) {
	if l.LazyTools == nil {
		return
	}
	if !continuesHistory {
		l.LazyTools.Reset()
	}
	for _, name := range l.LazyTools.UnknownEager() {
		l.Logger.WarnContext(ctx, "tools.lazyLoading.eager names a tool this run does not have; it is ignored", "tool", name)
		l.emitRunWarning(types.HarnessEvent{
			Message: fmt.Sprintf("tools.lazyLoading.eager names %q, which is not a registered tool; it is ignored", name),
		})
	}
}
