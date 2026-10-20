package builtins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

// toolSearchDescriptionBytes bounds each matched tool's description in a
// tool_search result; the full definition reaches the model once loaded.
const toolSearchDescriptionBytes = 200

// toolSearchSchema returns the tool_search input schema for a search that
// loads at most maxResults tools, which is also the default limit.
func toolSearchSchema(maxResults int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
	"type": "object",
	"properties": {
		"query": {
			"type": "string",
			"description": "Keywords describing the capability needed, e.g. \"create github issue\". Matched against tool names, descriptions and parameter names.",
			"minLength": 1,
			"maxLength": 500
		},
		"limit": {
			"type": "integer",
			"description": "Maximum number of tools to load (default and maximum %d).",
			"minimum": 1,
			"maximum": %d
		}
	},
	"required": ["query"],
	"additionalProperties": false
}`, maxResults, maxResults))
}

// ToolSearchTool returns the harness's tool search for lazy tool loading,
// with its limit bounded by cfg's effective maximum. It searches the
// deferred tools of the loader the loop attaches to the call context and
// loads the matches, so they are sent from the next turn on. The loader
// appends the deferred tool groups to the description each turn.
func ToolSearchTool(cfg types.LazyToolLoadingConfig) *tool.Tool {
	return &tool.Tool{
		Name: types.ToolSearchToolName,
		Description: "Find and load tools that are hidden until needed. Only some of the available tools are listed up front; the rest must be loaded with this tool before they can be called. " +
			"Search with keywords for the capability needed; the best-matching tools are loaded and can be called from the next step on. " +
			"Search again with different words if nothing suitable is found.",
		InputSchema:       toolSearchSchema(cfg.EffectiveMaxResults()),
		WorkspaceMutating: false,
		RequiresApproval:  false,
		SelfBoundedResult: true,
		Handler:           toolSearchHandler,
	}
}

func toolSearchHandler(ctx context.Context, input json.RawMessage) (string, error) {
	loader := tool.CallContextFrom(ctx).Lazy
	if loader == nil {
		return "", errors.New("lazy tool loading is not active")
	}
	var params struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", fmt.Errorf("parse tool_search input: %w", err)
	}
	matches := loader.Search(params.Query, params.Limit)
	alreadyLoaded := loader.LoadedMatches(params.Query)
	if len(matches) == 0 && len(alreadyLoaded) == 0 {
		return fmt.Sprintf("No tools matched %q; try different terms.", params.Query), nil
	}
	ids := make([]string, len(matches))
	for i, m := range matches {
		ids[i] = m.ID
	}
	loader.Load(ids)

	var b strings.Builder
	switch len(matches) {
	case 0:
		fmt.Fprintf(&b, "No new tools matched %q.", params.Query)
	case 1:
		b.WriteString("Loaded 1 tool:")
	default:
		fmt.Fprintf(&b, "Loaded %d tools:", len(matches))
	}
	for _, m := range matches {
		fmt.Fprintf(&b, "\n- %s: %s", m.Definition.Name, truncateDescription(m.Definition.Description))
	}
	if len(alreadyLoaded) > 0 {
		names := make([]string, len(alreadyLoaded))
		for i, m := range alreadyLoaded {
			names[i] = m.Definition.Name
		}
		b.WriteString("\nAlready loaded: " + strings.Join(names, ", "))
	}
	return b.String(), nil
}

// truncateDescription returns the first line of d, cut on a rune boundary
// to toolSearchDescriptionBytes with an ellipsis when longer.
func truncateDescription(d string) string {
	d, _, cut := strings.Cut(strings.TrimSpace(d), "\n")
	if len(d) <= toolSearchDescriptionBytes {
		if cut {
			return d + "…"
		}
		return d
	}
	n := toolSearchDescriptionBytes
	for n > 0 && !utf8.RuneStart(d[n]) {
		n--
	}
	return d[:n] + "…"
}
