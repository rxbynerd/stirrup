// Package builtins provides the built-in tools available to the coding agent.
package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
)

// readFileSchema is the JSON Schema for the read_file tool input.
var readFileSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"path": {
			"type": "string",
			"description": "Path to the file to read, relative to the workspace root."
		}
	},
	"required": ["path"],
	"additionalProperties": false
}`)

// writeFileSchema is the JSON Schema for the write_file tool input.
var writeFileSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"path": {
			"type": "string",
			"description": "Path to the file to write, relative to the workspace root. Parent directories are created automatically."
		},
		"content": {
			"type": "string",
			"description": "The content to write to the file."
		}
	},
	"required": ["path", "content"],
	"additionalProperties": false
}`)

// listDirectorySchema is the JSON Schema for the list_directory tool input.
var listDirectorySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"path": {
			"type": "string",
			"description": "Path to the directory to list, relative to the workspace root."
		}
	},
	"required": ["path"],
	"additionalProperties": false
}`)

// ReadFileTool returns a tool that reads a file from the workspace.
func ReadFileTool(exec executor.Executor) *tool.Tool {
	return &tool.Tool{
		Name:        "read_file",
		Description: "Read the contents of a file from the workspace.",
		InputSchema: readFileSchema,
		SideEffects: false,
		Handler: func(ctx context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("parse input: %w", err)
			}
			if params.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			return exec.ReadFile(ctx, params.Path)
		},
	}
}

// WriteFileTool returns a tool that writes content to a file in the workspace.
func WriteFileTool(exec executor.Executor) *tool.Tool {
	return &tool.Tool{
		Name:        "write_file",
		Description: "Write content to a file in the workspace. Creates parent directories as needed.",
		InputSchema: writeFileSchema,
		SideEffects: true,
		Handler: func(ctx context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("parse input: %w", err)
			}
			if params.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			if err := exec.WriteFile(ctx, params.Path, params.Content); err != nil {
				return "", err
			}
			return fmt.Sprintf("Successfully wrote %d bytes to %s", len(params.Content), params.Path), nil
		},
	}
}

// ListDirectoryTool returns a tool that lists the contents of a directory.
func ListDirectoryTool(exec executor.Executor) *tool.Tool {
	return &tool.Tool{
		Name:        "list_directory",
		Description: "List the contents of a directory in the workspace. Directories have a trailing slash.",
		InputSchema: listDirectorySchema,
		SideEffects: false,
		Handler: func(ctx context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("parse input: %w", err)
			}
			if params.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			entries, err := exec.ListDirectory(ctx, params.Path)
			if err != nil {
				return "", err
			}
			return strings.Join(entries, "\n"), nil
		},
	}
}
