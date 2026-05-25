package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
)

// runCommandSchema is the JSON Schema for the run_command tool input.
var runCommandSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"command": {
			"type": "string",
			"description": "The shell command to execute in the workspace directory."
		},
		"timeout": {
			"type": "integer",
			"description": "Optional timeout in seconds. Defaults to 30, maximum 300."
		}
	},
	"required": ["command"],
	"additionalProperties": false
}`)

// RunCommandTool returns a tool that executes a shell command in the workspace.
func RunCommandTool(exec executor.Executor) *tool.Tool {
	return &tool.Tool{
		Name: "run_command",
		Description: "Execute a shell command in the workspace directory. Returns stdout, then a 'STDERR:' block when stderr is non-empty, then '[exit code: N]' when the command exited non-zero. " +
			"Use this for build, test, format, lint, and other tooling invocations that have to actually run. " +
			"Do not use for filesystem inspection that a dedicated tool covers — prefer read_file, list_directory, grep_files, find_files because they return structured, bounded output and do not need a shell. " +
			"timeout is in seconds (default 30, max 300); the command is killed when the timeout elapses. " +
			"Example: {\"command\": \"go test ./harness/internal/tool/...\", \"timeout\": 120}",
		InputSchema:       runCommandSchema,
		WorkspaceMutating: true,
		RequiresApproval:  true,
		Handler: func(ctx context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Command string `json:"command"`
				Timeout *int   `json:"timeout"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("parse input: %w", err)
			}
			if params.Command == "" {
				return "", fmt.Errorf("command is required")
			}

			timeout := 30 * time.Second
			if params.Timeout != nil {
				t := *params.Timeout
				if t <= 0 {
					t = 30
				}
				if t > 300 {
					t = 300
				}
				timeout = time.Duration(t) * time.Second
			}

			result, err := exec.Exec(ctx, params.Command, timeout)
			if err != nil {
				return "", err
			}

			var out strings.Builder
			if result.Stdout != "" {
				out.WriteString(result.Stdout)
			}
			if result.Stderr != "" {
				if out.Len() > 0 {
					out.WriteString("\n")
				}
				out.WriteString("STDERR:\n")
				out.WriteString(result.Stderr)
			}
			if result.ExitCode != 0 {
				fmt.Fprintf(&out, "\n[exit code: %d]", result.ExitCode)
			}

			return out.String(), nil
		},
	}
}
