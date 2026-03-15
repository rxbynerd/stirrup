package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rubynerd/stirrup/harness/internal/executor"
	"github.com/rubynerd/stirrup/harness/internal/tool"
)

// searchFilesSchema is the JSON Schema for the search_files tool input.
var searchFilesSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"pattern": {
			"type": "string",
			"description": "The search pattern. For grep: a regular expression to match file contents. For glob: a file name pattern (e.g. '*.go')."
		},
		"path": {
			"type": "string",
			"description": "Directory to search in, relative to the workspace root. Defaults to the workspace root if omitted."
		},
		"type": {
			"type": "string",
			"enum": ["grep", "glob"],
			"description": "Search type: 'grep' searches file contents with a regex, 'glob' finds files matching a name pattern."
		}
	},
	"required": ["pattern", "type"],
	"additionalProperties": false
}`)

const searchTimeout = 30 * time.Second

// SearchFilesTool returns a tool that searches for files by content (grep) or
// name pattern (glob) within the workspace.
func SearchFilesTool(exec executor.Executor) *tool.Tool {
	return &tool.Tool{
		Name:        "search_files",
		Description: "Search for files in the workspace. Use type 'grep' to search file contents with a regex, or 'glob' to find files matching a name pattern.",
		InputSchema: searchFilesSchema,
		SideEffects: false,
		Handler: func(ctx context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
				Type    string `json:"type"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("parse input: %w", err)
			}
			if params.Pattern == "" {
				return "", fmt.Errorf("pattern is required")
			}
			if params.Type != "grep" && params.Type != "glob" {
				return "", fmt.Errorf("type must be 'grep' or 'glob', got %q", params.Type)
			}

			searchDir := "."
			if params.Path != "" {
				searchDir = params.Path
			}

			var cmd string
			switch params.Type {
			case "grep":
				cmd = fmt.Sprintf("grep -rn --include='*' %s %s",
					shellQuote(params.Pattern), shellQuote(searchDir))
			case "glob":
				cmd = fmt.Sprintf("find %s -name %s -type f",
					shellQuote(searchDir), shellQuote(params.Pattern))
			}

			result, err := exec.Exec(ctx, cmd, searchTimeout)
			if err != nil {
				return "", fmt.Errorf("search command failed: %w", err)
			}

			output := strings.TrimSpace(result.Stdout)

			// grep returns exit code 1 when no matches found — not an error.
			if result.ExitCode == 1 && output == "" {
				return "No matches found.", nil
			}
			if result.ExitCode > 1 {
				return "", fmt.Errorf("search failed (exit %d): %s", result.ExitCode, strings.TrimSpace(result.Stderr))
			}

			if output == "" {
				return "No matches found.", nil
			}
			return output, nil
		},
	}
}

// shellQuote wraps a string in single quotes, escaping any embedded single
// quotes. This is sufficient for passing arguments to sh -c.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
