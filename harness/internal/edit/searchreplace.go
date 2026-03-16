package edit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/types"
)

// searchReplaceSchema is the JSON Schema for the search-replace edit tool input.
var searchReplaceSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"path": {
			"type": "string",
			"description": "Path to the file to edit, relative to the workspace root."
		},
		"old_string": {
			"type": "string",
			"description": "The exact text to find in the file. Must match exactly one location. If empty and the file does not exist, the file is created with new_string as content."
		},
		"new_string": {
			"type": "string",
			"description": "The replacement text."
		}
	},
	"required": ["path", "old_string", "new_string"],
	"additionalProperties": false
}`)

// SearchReplaceStrategy implements EditStrategy by replacing a single exact
// occurrence of old_string with new_string. It rejects ambiguous edits where
// the search string matches zero or more than one location.
type SearchReplaceStrategy struct{}

// NewSearchReplaceStrategy creates a new SearchReplaceStrategy.
func NewSearchReplaceStrategy() *SearchReplaceStrategy {
	return &SearchReplaceStrategy{}
}

// ToolDefinition returns the tool definition for the search-replace edit strategy.
func (s *SearchReplaceStrategy) ToolDefinition() types.ToolDefinition {
	return types.ToolDefinition{
		Name:        "search_replace",
		Description: "Replace an exact occurrence of old_string with new_string in a file. The old_string must match exactly one location. If old_string is empty and the file does not exist, the file is created with new_string as content.",
		InputSchema: searchReplaceSchema,
	}
}

// Apply parses the input, performs the search-replace via the executor, and
// returns an EditResult with a unified diff showing what changed.
func (s *SearchReplaceStrategy) Apply(ctx context.Context, input json.RawMessage, exec executor.Executor) (*EditResult, error) {
	var params struct {
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}
	if params.Path == "" {
		return &EditResult{
			Applied: false,
			Error:   "path is required",
		}, nil
	}

	// Handle file creation: empty old_string on a nonexistent file.
	if params.OldString == "" {
		return s.handleCreate(ctx, params.Path, params.NewString, exec)
	}

	// Read existing file content.
	content, err := exec.ReadFile(ctx, params.Path)
	if err != nil {
		return &EditResult{
			Path:    params.Path,
			Applied: false,
			Error:   fmt.Sprintf("read file: %s", err),
		}, nil
	}

	// Count occurrences to enforce exactly-one semantics.
	count := strings.Count(content, params.OldString)
	if count == 0 {
		return &EditResult{
			Path:    params.Path,
			Applied: false,
			Error:   "old_string not found in file",
		}, nil
	}
	if count > 1 {
		return &EditResult{
			Path:    params.Path,
			Applied: false,
			Error:   fmt.Sprintf("old_string matches %d locations; must match exactly 1", count),
		}, nil
	}

	// Perform the replacement.
	newContent := strings.Replace(content, params.OldString, params.NewString, 1)

	if err := exec.WriteFile(ctx, params.Path, newContent); err != nil {
		return &EditResult{
			Path:    params.Path,
			Applied: false,
			Error:   err.Error(),
		}, nil
	}

	diff := unifiedDiff(params.Path, content, newContent)

	return &EditResult{
		Path:    params.Path,
		Applied: true,
		Diff:    diff,
	}, nil
}

// handleCreate creates a new file when old_string is empty. If the file
// already exists, it returns an error — empty old_string is only valid for
// creation.
func (s *SearchReplaceStrategy) handleCreate(ctx context.Context, path, newString string, exec executor.Executor) (*EditResult, error) {
	// Check if the file already exists.
	_, err := exec.ReadFile(ctx, path)
	if err == nil {
		return &EditResult{
			Path:    path,
			Applied: false,
			Error:   "old_string is empty but file already exists; provide old_string to edit an existing file",
		}, nil
	}

	if err := exec.WriteFile(ctx, path, newString); err != nil {
		return &EditResult{
			Path:    path,
			Applied: false,
			Error:   err.Error(),
		}, nil
	}

	diff := unifiedDiff(path, "", newString)

	return &EditResult{
		Path:    path,
		Applied: true,
		Diff:    diff,
	}, nil
}

// unifiedDiff produces a minimal unified-diff-style output showing what changed
// between old and new content. This is a simple line-based implementation
// sufficient for edit result display.
func unifiedDiff(path, oldContent, newContent string) string {
	oldLines := splitLines(oldContent)
	newLines := splitLines(newContent)

	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n", path)
	fmt.Fprintf(&b, "+++ b/%s\n", path)

	// Walk through both line slices and emit hunks for changed regions.
	// This is a simple O(n) scan that works well for search-replace edits
	// which typically affect a small contiguous region.
	i, j := 0, 0
	for i < len(oldLines) || j < len(newLines) {
		// Find next difference.
		if i < len(oldLines) && j < len(newLines) && oldLines[i] == newLines[j] {
			i++
			j++
			continue
		}

		// Found a difference — determine the hunk boundaries.
		// Include up to 3 lines of context before the change.
		contextBefore := 3
		hunkStartOld := i - contextBefore
		if hunkStartOld < 0 {
			hunkStartOld = 0
		}
		hunkStartNew := j - (i - hunkStartOld)

		// Scan to find the end of the changed region.
		oi, nj := i, j
		for oi < len(oldLines) || nj < len(newLines) {
			if oi < len(oldLines) && nj < len(newLines) && oldLines[oi] == newLines[nj] {
				// Check if we have enough matching lines to end the hunk.
				match := 0
				for oi+match < len(oldLines) && nj+match < len(newLines) && oldLines[oi+match] == newLines[nj+match] {
					match++
					if match >= 6 {
						break
					}
				}
				if match >= 6 {
					break
				}
				oi++
				nj++
				continue
			}
			if oi < len(oldLines) {
				oi++
			}
			if nj < len(newLines) {
				nj++
			}
		}

		// Include up to 3 lines of context after the change.
		contextAfter := 3
		hunkEndOld := oi + contextAfter
		if hunkEndOld > len(oldLines) {
			hunkEndOld = len(oldLines)
		}
		hunkEndNew := nj + contextAfter
		if hunkEndNew > len(newLines) {
			hunkEndNew = len(newLines)
		}

		// Emit hunk header.
		oldLen := hunkEndOld - hunkStartOld
		newLen := hunkEndNew - hunkStartNew
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", hunkStartOld+1, oldLen, hunkStartNew+1, newLen)

		// Emit context before.
		for k := hunkStartOld; k < i; k++ {
			fmt.Fprintf(&b, " %s\n", oldLines[k])
		}

		// Emit removed lines.
		for k := i; k < oi; k++ {
			fmt.Fprintf(&b, "-%s\n", oldLines[k])
		}

		// Emit added lines.
		for k := j; k < nj; k++ {
			fmt.Fprintf(&b, "+%s\n", newLines[k])
		}

		// Emit context after.
		afterCount := 0
		for oi+afterCount < hunkEndOld {
			fmt.Fprintf(&b, " %s\n", oldLines[oi+afterCount])
			afterCount++
		}

		// Advance past the hunk.
		i = hunkEndOld
		j = hunkEndNew
	}

	return b.String()
}

// splitLines splits a string into lines, handling the edge case of an empty
// string returning no lines rather than a single empty line.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	// Remove trailing newline to avoid a spurious empty line at the end.
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}
