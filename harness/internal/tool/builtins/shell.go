package builtins

import (
	"context"
	"encoding/json"
	"errors"
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
		// #222 structured example, pinned to the description by TestBuiltinInputExamples_MatchDescription.
		InputExamples:     []json.RawMessage{json.RawMessage(`{"command": "go test ./harness/internal/tool/...", "timeout": 120}`)},
		InputSchema:       runCommandSchema,
		WorkspaceMutating: true,
		RequiresApproval:  true,
		StructuredHandler: func(ctx context.Context, input json.RawMessage) (tool.StructuredResult, error) {
			var params struct {
				Command string `json:"command"`
				Timeout *int   `json:"timeout"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return tool.StructuredResult{}, fmt.Errorf("parse input: %w", err)
			}
			if params.Command == "" {
				return tool.StructuredResult{}, fmt.Errorf("command is required")
			}

			timeoutSeconds := 30
			if params.Timeout != nil {
				timeoutSeconds = *params.Timeout
				if timeoutSeconds <= 0 {
					timeoutSeconds = 30
				}
				if timeoutSeconds > 300 {
					timeoutSeconds = 300
				}
			}
			timeout := time.Duration(timeoutSeconds) * time.Second

			result, err := exec.Exec(ctx, params.Command, timeout)
			if err != nil {
				if !errors.Is(err, executor.ErrTimeout) {
					return tool.StructuredResult{}, err
				}
				// #489's executors return partial output alongside the
				// wrapped ErrTimeout, so a genuine timeout is a soft
				// outcome the model can act on (e.g. rerun with a longer
				// timeout or narrow the command) rather than a hard tool
				// error. result can in principle still be nil if the
				// executor was cut off before producing one at all
				// (e.g. a control-plane deadline before exec even
				// started); handle that defensively instead of panicking.
				if result == nil {
					result = &executor.ExecResult{}
				}
				return timedOutRunCommandResult(result, timeoutSeconds)
			}

			structured, marshalErr := json.Marshal(commandResult{
				Stdout:         result.Stdout,
				Stderr:         result.Stderr,
				ExitCode:       result.ExitCode,
				TimedOut:       false,
				TimeoutSeconds: timeoutSeconds,
			})
			if marshalErr != nil {
				return tool.StructuredResult{}, fmt.Errorf("marshal structured result: %w", marshalErr)
			}

			return tool.StructuredResult{
				Text:       formatRunCommand(result.Stdout, result.Stderr, result.ExitCode),
				Structured: structured,
				Kind:       kindCommandResult,
			}, nil
		},
	}
}

// timedOutRunCommandResult builds the soft-outcome StructuredResult for a
// run_command invocation killed by its timeout. result carries whatever
// partial output the executor captured before the kill (result.ExitCode is
// the executor's zero value, not a sentinel, since the process never ran to
// completion — callers must not read it as a real exit status). The Text
// fallback appends an unambiguous "[timed out after Ns]" marker after the
// normal formatRunCommand rendering so a model reading only Text (not
// Structured) can still distinguish a timeout from a clean exit; formatRunCommand
// itself stays byte-identical to its pre-#231/#489 contract.
func timedOutRunCommandResult(result *executor.ExecResult, timeoutSeconds int) (tool.StructuredResult, error) {
	structured, marshalErr := json.Marshal(commandResult{
		Stdout:         result.Stdout,
		Stderr:         result.Stderr,
		ExitCode:       result.ExitCode,
		TimedOut:       true,
		TimeoutSeconds: timeoutSeconds,
	})
	if marshalErr != nil {
		return tool.StructuredResult{}, fmt.Errorf("marshal structured result: %w", marshalErr)
	}

	text := formatRunCommand(result.Stdout, result.Stderr, result.ExitCode)
	text += fmt.Sprintf("\n[timed out after %ds]", timeoutSeconds)

	return tool.StructuredResult{
		Text:       text,
		Structured: structured,
		Kind:       kindCommandResult,
	}, nil
}

// formatRunCommand renders the canonical text output for run_command:
// stdout, then a "STDERR:" block when stderr is non-empty, then a
// "[exit code: N]" line when the command exited non-zero. This is the
// text fallback every provider can accept and must stay byte-identical to
// the pre-#231 rendering.
func formatRunCommand(stdout, stderr string, exitCode int) string {
	var out strings.Builder
	if stdout != "" {
		out.WriteString(stdout)
	}
	if stderr != "" {
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		out.WriteString("STDERR:\n")
		out.WriteString(stderr)
	}
	if exitCode != 0 {
		fmt.Fprintf(&out, "\n[exit code: %d]", exitCode)
	}
	return out.String()
}
