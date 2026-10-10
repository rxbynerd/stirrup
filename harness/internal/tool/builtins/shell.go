package builtins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rxbynerd/stirrup/harness/internal/commandoutput"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
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
	return RunCommandToolWithStore(exec, nil, types.CommandOutputConfig{}, types.DefaultToolMaxResultBytes)
}

// RunCommandToolWithStore returns the compliance-capturing run_command tool.
// A nil store selects the uncaptured path for embedders and executors that
// do not expose streaming execution; that path bounds its text to
// maxResultBytes (tools.maxResultBytes), keeping the head and tail of each
// stream and the exit-code and timeout markers.
func RunCommandToolWithStore(exec executor.Executor, store *commandoutput.Store, cfg types.CommandOutputConfig, maxResultBytes int) *tool.Tool {
	cfg = (types.ToolsConfig{CommandOutput: cfg}).EffectiveCommandOutput()
	return &tool.Tool{
		Name: "run_command",
		Description: "Execute a shell command in the workspace directory. Returns stdout, then a 'STDERR:' block when stderr is non-empty, then '[exit code: N]' when the command exited non-zero. " +
			"Use this for build, test, format, lint, and other tooling invocations that have to actually run. " +
			"Do not use for filesystem inspection that a dedicated tool covers — prefer read_file, list_directory, grep_files, find_files because they return structured, bounded output and do not need a shell. " +
			"timeout is in seconds (default 30, max 300); when it elapses the command and the processes it started are killed (SIGTERM, then SIGKILL after a short grace), except processes that detached into their own session or process group. " +
			"On timeout this returns the partial stdout/stderr captured before the deadline plus a '[timed out after Ns]' marker, not a hard error, so act on the partial output or rerun with a longer timeout. " +
			"Example: {\"command\": \"go test ./harness/internal/tool/...\", \"timeout\": 120}",
		// Pinned to the description by TestBuiltinInputExamples_MatchDescription.
		InputExamples:     []json.RawMessage{json.RawMessage(`{"command": "go test ./harness/internal/tool/...", "timeout": 120}`)},
		InputSchema:       runCommandSchema,
		WorkspaceMutating: true,
		RequiresApproval:  true,
		SelfBoundedResult: true,
		StructuredHandler: func(ctx context.Context, input json.RawMessage) (tool.StructuredResult, error) {
			if res, found := replayResult(exec, "run_command", input); found {
				return res, nil
			}
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

			if store == nil {
				result, err := exec.Exec(ctx, params.Command, timeout)
				if err != nil {
					if !errors.Is(err, executor.ErrTimeout) {
						return tool.StructuredResult{}, err
					}
					// Executors return partial output alongside the wrapped
					// ErrTimeout, so a timeout is a soft outcome the model
					// can act on rather than a hard tool error. result can
					// still be nil if the executor was cut off before
					// producing one (e.g. a control-plane deadline before
					// exec even started).
					if result == nil {
						result = &executor.ExecResult{}
					}
					return buildCommandResult(result, timeoutSeconds, true, maxResultBytes)
				}
				return buildCommandResult(result, timeoutSeconds, false, maxResultBytes)
			}

			streaming, ok := exec.(executor.StreamingExecutor)
			if !ok {
				return tool.StructuredResult{}, fmt.Errorf("executor does not support compliant command output streaming")
			}
			execCtx, cancel := context.WithCancelCause(ctx)
			defer cancel(nil)
			capture, err := store.Begin(ctx, cancel)
			if err != nil {
				return tool.StructuredResult{}, err
			}
			result, execErr := streaming.ExecStream(execCtx, params.Command, timeout, capture.Stdout(), capture.Stderr())
			if result == nil {
				result = &executor.ExecResult{ExitCode: -1}
			}
			timedOut := errors.Is(execErr, executor.ErrTimeout)
			cause := context.Cause(execCtx)
			cancelled := execErr != nil && !timedOut && execCtx.Err() != nil &&
				!errors.Is(cause, commandoutput.ErrCaptureLimit) && !errors.Is(cause, commandoutput.ErrCaptureIO)
			captured, captureErr := capture.Complete(commandoutput.Completion{ExitCode: result.ExitCode, TimedOut: timedOut, Cancelled: cancelled})
			isError := execErr != nil || captureErr != nil
			text, spilled := formatCapturedCommand(captured, cfg, timeoutSeconds)
			if execErr != nil && !timedOut && cause != nil && !cancelled {
				text += "\n[capture/command error: " + security.Scrub(execErr.Error()) + "]"
			}
			if err := store.RecordInitial(&captured.Record, text); err != nil {
				return tool.StructuredResult{}, fmt.Errorf("record command model result: %w", err)
			}

			structured, marshalErr := json.Marshal(commandResult{
				Stdout:         previewForStructured(captured.Stdout, cfg, spilled),
				Stderr:         previewForStructured(captured.Stderr, cfg, spilled),
				ExitCode:       result.ExitCode,
				TimedOut:       timedOut,
				TimeoutSeconds: timeoutSeconds,
				Spilled:        spilled, CaptureComplete: captured.Record.CaptureComplete,
				ArchiveID: captured.Record.ArchiveID,
				StdoutRef: captured.Record.Stdout.Reference, StderrRef: captured.Record.Stderr.Reference,
				StdoutRawBytes: captured.Record.Stdout.RawBytes, StderrRawBytes: captured.Record.Stderr.RawBytes,
				StdoutScrubbedBytes: captured.Record.Stdout.ScrubbedBytes, StderrScrubbedBytes: captured.Record.Stderr.ScrubbedBytes,
				StdoutSHA256: captured.Record.Stdout.ScrubbedSHA256, StderrSHA256: captured.Record.Stderr.ScrubbedSHA256,
			})
			if marshalErr != nil {
				return tool.StructuredResult{}, fmt.Errorf("marshal structured result: %w", marshalErr)
			}

			return tool.StructuredResult{
				Text:       text,
				Structured: structured,
				Kind:       kindCommandResult,
				IsError:    isError,
			}, nil
		},
	}
}

// buildCommandResult builds the StructuredResult for an uncaptured
// run_command invocation, covering both the clean-exit and timed-out cases.
// Text longer than maxBytes is elided in the middle of each stream; the
// structured envelope is dropped then because it would repeat the elided
// output. See docs/architecture.md for the timeout/ExitCode contract.
func buildCommandResult(result *executor.ExecResult, timeoutSeconds int, timedOut bool, maxBytes int) (tool.StructuredResult, error) {
	stdout, stderr := security.Scrub(result.Stdout), security.Scrub(result.Stderr)
	suffix := ""
	if timedOut {
		suffix = fmt.Sprintf("\n[timed out after %ds]", timeoutSeconds)
	}
	text, truncated := boundCommandText(stdout, stderr, result.ExitCode, suffix, maxBytes)
	if truncated {
		return tool.StructuredResult{Text: text}, nil
	}

	structured, marshalErr := json.Marshal(commandResult{
		Stdout:         stdout,
		Stderr:         stderr,
		ExitCode:       result.ExitCode,
		TimedOut:       timedOut,
		TimeoutSeconds: timeoutSeconds,
	})
	if marshalErr != nil {
		return tool.StructuredResult{}, fmt.Errorf("marshal structured result: %w", marshalErr)
	}

	return tool.StructuredResult{
		Text:       text,
		Structured: structured,
		Kind:       kindCommandResult,
	}, nil
}

// boundCommandText renders formatRunCommand plus suffix within maxBytes. The
// STDERR header, exit-code line and suffix are always kept; stderr is
// guaranteed at least half of the remaining space when it needs it, and any
// space one stream leaves unused passes to the other.
func boundCommandText(stdout, stderr string, exitCode int, suffix string, maxBytes int) (string, bool) {
	text := formatRunCommand(stdout, stderr, exitCode) + suffix
	if len(text) <= maxBytes {
		return text, false
	}
	avail := max(maxBytes-(len(text)-len(stdout)-len(stderr)), 0)
	stderrShare := min(len(stderr), avail/2)
	stdoutShare := min(len(stdout), avail-stderrShare)
	stderrShare = min(len(stderr), avail-stdoutShare)
	return formatRunCommand(elideMiddle(stdout, stdoutShare, "stdout"), elideMiddle(stderr, stderrShare, "stderr"), exitCode) + suffix, true
}

const elisionMarkerFmt = "\n[run_command %s truncated: %d of %d bytes omitted here]\n"

// elideMiddle returns s within limit bytes, replacing its middle with a
// marker and cutting on UTF-8 boundaries. When limit cannot hold the marker
// it keeps only the tail, where failures are usually reported.
func elideMiddle(s string, limit int, stream string) string {
	if len(s) <= limit {
		return s
	}
	keep := limit - len(fmt.Sprintf(elisionMarkerFmt, stream, len(s), len(s)))
	if keep <= 0 {
		start := len(s) - limit
		for start < len(s) && !utf8.RuneStart(s[start]) {
			start++
		}
		return s[start:]
	}
	headEnd := keep / 2
	for headEnd > 0 && !utf8.RuneStart(s[headEnd]) {
		headEnd--
	}
	tailStart := len(s) - (keep - keep/2)
	for tailStart < len(s) && !utf8.RuneStart(s[tailStart]) {
		tailStart++
	}
	return s[:headEnd] + fmt.Sprintf(elisionMarkerFmt, stream, tailStart-headEnd, len(s)) + s[tailStart:]
}

func formatCapturedCommand(c commandoutput.Captured, cfg types.CommandOutputConfig, timeoutSeconds int) (string, bool) {
	combined := c.Record.Stdout.ScrubbedBytes + c.Record.Stderr.ScrubbedBytes
	if combined <= cfg.InlineMaxBytes && c.Record.CaptureComplete {
		text := formatRunCommand(c.Stdout, c.Stderr, c.Record.ExitCode)
		// A timeout is a soft outcome, so the inline rendering must carry
		// the same unambiguous marker buildCommandResult emits; the spilled
		// rendering below states timed_out explicitly instead.
		if c.Record.TimedOut {
			text += fmt.Sprintf("\n[timed out after %ds]", timeoutSeconds)
		}
		return text, false
	}
	var out strings.Builder
	out.WriteString("[command output spilled to compliance archive]\n")
	fmt.Fprintf(&out, "exit_code=%d timed_out=%t timeout_seconds=%d capture_complete=%t archive_id=%s\n",
		c.Record.ExitCode, c.Record.TimedOut, timeoutSeconds, c.Record.CaptureComplete, c.Record.ArchiveID)
	writeStreamPreview(&out, "stdout", c.Stdout, c.Record.Stdout, cfg.PreviewBytesPerStream)
	writeStreamPreview(&out, "stderr", c.Stderr, c.Record.Stderr, cfg.PreviewBytesPerStream)
	if c.Record.CaptureError != "" {
		out.WriteString("capture_error=" + c.Record.CaptureError + "\n")
	}
	return strings.TrimSuffix(out.String(), "\n"), true
}

func writeStreamPreview(out *strings.Builder, name, content string, meta types.CommandOutputStreamRecord, preview int64) {
	fmt.Fprintf(out, "%s raw_bytes=%d scrubbed_bytes=%d raw_sha256=%s scrubbed_sha256=%s\n", name, meta.RawBytes, meta.ScrubbedBytes, meta.RawSHA256, meta.ScrubbedSHA256)
	fmt.Fprintf(out, "%s_ref=%s\n", name, meta.Reference)
	tail := content
	if int64(len(tail)) > preview {
		tail = tail[len(tail)-int(preview):]
	}
	fmt.Fprintf(out, "%s_tail:\n%s\n", name, tail)
}

func previewForStructured(content string, cfg types.CommandOutputConfig, spilled bool) string {
	if !spilled {
		return content
	}
	if int64(len(content)) <= cfg.PreviewBytesPerStream {
		return content
	}
	return content[len(content)-int(cfg.PreviewBytesPerStream):]
}

var readCommandOutputSchema = json.RawMessage(fmt.Sprintf(`{
  "type":"object",
  "properties":{
    "ref":{"type":"string","pattern":"^stirrup://command-output/"},
    "offset":{"type":"integer","minimum":0},
    "limit":{"type":"integer","minimum":1,"maximum":%d}
  },
  "required":["ref"],
  "additionalProperties":false
}`, commandoutput.ReadMaxBytes))

// replayResult short-circuits a tool handler to the exact recorded
// model-visible result when the executor is a replay executor. Both
// run_command and read_command_output use it so replayed runs stay
// byte-identical to the recording.
func replayResult(exec executor.Executor, name string, input json.RawMessage) (tool.StructuredResult, bool) {
	replay, ok := exec.(interface {
		ReplayToolCall(string, json.RawMessage) (types.ToolCallRecord, bool)
	})
	if !ok {
		return tool.StructuredResult{}, false
	}
	rec, found := replay.ReplayToolCall(name, input)
	if !found {
		return tool.StructuredResult{}, false
	}
	return tool.StructuredResult{Text: rec.Output, Structured: rec.Structured, Kind: rec.Kind, IsError: rec.IsError || !rec.Success}, true
}

// ReadCommandOutputTool returns a read-only, approval-free paginator over
// scrubbed command streams. Each response is shortened to fit within
// maxResultBytes (tools.maxResultBytes); end_offset reports where it stopped.
func ReadCommandOutputTool(store *commandoutput.Store, exec executor.Executor, maxResultBytes int) *tool.Tool {
	return &tool.Tool{
		Name: "read_command_output",
		Description: fmt.Sprintf(
			"Read a scrubbed byte range from an opaque stirrup://command-output reference returned by run_command. Defaults to %d KiB and permits at most %d KiB per call. "+
				"A response may stop short of the requested limit to fit the tool result size limit; continue from its end_offset.",
			commandoutput.ReadDefaultBytes>>10, commandoutput.ReadMaxBytes>>10),
		InputSchema:       readCommandOutputSchema,
		SelfBoundedResult: true,
		StructuredHandler: func(ctx context.Context, input json.RawMessage) (tool.StructuredResult, error) {
			if res, found := replayResult(exec, "read_command_output", input); found {
				return res, nil
			}
			var params struct {
				Ref    string `json:"ref"`
				Offset int64  `json:"offset"`
				Limit  int64  `json:"limit"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return tool.StructuredResult{}, fmt.Errorf("parse input: %w", err)
			}
			read, err := store.Read(params.Ref, params.Offset, params.Limit)
			if err != nil {
				return tool.StructuredResult{}, err
			}
			read, structured, err := fitCommandOutputChunk(params.Ref, read, maxResultBytes)
			if err != nil {
				return tool.StructuredResult{}, err
			}
			text := string(structured)
			meta := tool.CallContextFrom(ctx)
			if err := store.RecordRead(meta, params.Ref, read, text); err != nil {
				return tool.StructuredResult{}, fmt.Errorf("record command output read: %w", err)
			}
			return tool.StructuredResult{Text: text, Structured: structured, Kind: kindCommandOutputChunk}, nil
		},
	}
}

// fitCommandOutputChunk marshals the read_command_output payload for read,
// dropping trailing content bytes, on a UTF-8 boundary where one exists,
// until the JSON fits within maxBytes. The returned ReadResult carries the
// shortened range so the access ledger records exactly what the model
// received.
func fitCommandOutputChunk(ref string, read commandoutput.ReadResult, maxBytes int) (commandoutput.ReadResult, []byte, error) {
	for {
		payload := commandOutputChunkResult{Reference: ref, Stream: read.Stream, Offset: read.Offset, EndOffset: read.End, EOF: read.EOF, Content: string(read.Bytes), RawBytes: read.Content.RawBytes, RawSHA256: read.Content.RawSHA256, ScrubbedBytes: read.Content.ScrubbedBytes, ScrubbedSHA256: read.Content.ScrubbedSHA256, RedactionCount: read.Content.RedactionCount}
		structured, err := json.Marshal(payload)
		if err != nil {
			return read, nil, err
		}
		over := len(structured) - maxBytes
		if over <= 0 || len(read.Bytes) == 0 {
			return read, structured, nil
		}
		// JSON escaping expands some bytes up to six-fold, so shrink in
		// proportion to the encoded content length rather than by the raw
		// overshoot.
		encoded, err := json.Marshal(payload.Content)
		if err != nil {
			return read, nil, err
		}
		keep := len(read.Bytes) * max(len(encoded)-over, 0) / len(encoded)
		keep = min(keep, len(read.Bytes)-1)
		for cut := keep; cut > 0; cut-- {
			if utf8.RuneStart(read.Bytes[cut]) {
				keep = cut
				break
			}
		}
		read.Bytes = read.Bytes[:keep]
		read.End = read.Offset + int64(keep)
		read.EOF = read.End >= read.Content.ScrubbedBytes
	}
}

// formatRunCommand renders the canonical text output for run_command:
// stdout, then a "STDERR:" block when stderr is non-empty, then a
// "[exit code: N]" line when the command exited non-zero.
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
