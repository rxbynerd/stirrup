package edit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rubynerd/stirrup/harness/internal/executor"
	"github.com/rubynerd/stirrup/types"
)

// wholeFileSchema is the JSON Schema for the whole-file edit tool input.
var wholeFileSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"path": {
			"type": "string",
			"description": "Path to the file to write, relative to the workspace root."
		},
		"content": {
			"type": "string",
			"description": "The complete new content for the file."
		}
	},
	"required": ["path", "content"],
	"additionalProperties": false
}`)

// WholeFileStrategy implements EditStrategy by replacing the entire file
// content. This is the simplest strategy and serves as the Phase 1 default.
type WholeFileStrategy struct{}

// NewWholeFileStrategy creates a new WholeFileStrategy.
func NewWholeFileStrategy() *WholeFileStrategy {
	return &WholeFileStrategy{}
}

// ToolDefinition returns the tool definition for the whole-file edit strategy.
func (s *WholeFileStrategy) ToolDefinition() types.ToolDefinition {
	return types.ToolDefinition{
		Name:        "write_file",
		Description: "Write the complete content of a file. Creates parent directories as needed. Replaces the entire file.",
		InputSchema: wholeFileSchema,
	}
}

// Apply parses the input, writes the file via the executor, and returns an
// EditResult describing the outcome.
func (s *WholeFileStrategy) Apply(ctx context.Context, input json.RawMessage, exec executor.Executor) (*EditResult, error) {
	var params struct {
		Path    string `json:"path"`
		Content string `json:"content"`
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

	if err := exec.WriteFile(ctx, params.Path, params.Content); err != nil {
		return &EditResult{
			Path:    params.Path,
			Applied: false,
			Error:   err.Error(),
		}, nil
	}

	return &EditResult{
		Path:    params.Path,
		Applied: true,
	}, nil
}
