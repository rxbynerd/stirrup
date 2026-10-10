// Package postedit runs the operator-authored tools.postEdit checks after
// an edit tool succeeds and renders their failures for the model. See
// docs/configuration.md for the configuration surface.
package postedit

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

// MaxDiagnosticsBytes caps the scrubbed check output appended to a tool
// result. The head is kept: compilers and linters report the first
// problem first.
const MaxDiagnosticsBytes = 16 * 1024

// fenceLabel names the data fence around check output in a tool result.
const fenceLabel = "POST_EDIT_DIAGNOSTICS"

// maxCommandLabelBytes bounds the command template quoted back to the
// model when a check fails.
const maxCommandLabelBytes = 200

// Recorder receives one record per check execution. Trace emitters
// implementing trace.PostEditCheckRecorder satisfy it.
type Recorder interface {
	RecordPostEditCheck(types.PostEditCheckRecord)
}

// Handler is the tool-handler shape Wrap decorates.
type Handler func(ctx context.Context, input json.RawMessage) (string, error)

// Checker runs a run's configured post-edit checks through its Executor.
type Checker struct {
	checks  []types.PostEditCheckConfig
	exec    executor.Executor
	logger  *slog.Logger
	entropy io.Reader

	mu       sync.Mutex
	recorder Recorder
}

// New returns a Checker for checks, or nil when checks is empty so the
// caller can skip wrapping entirely. A nil logger falls back to
// slog.Default().
func New(checks []types.PostEditCheckConfig, exec executor.Executor, logger *slog.Logger) *Checker {
	if len(checks) == 0 || exec == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Checker{
		checks:  append([]types.PostEditCheckConfig(nil), checks...),
		exec:    exec,
		logger:  logger,
		entropy: rand.Reader,
	}
}

// SetRecorder installs the trace recorder. The trace emitter is built
// after the tool registry, so it is attached once both exist.
func (c *Checker) SetRecorder(r Recorder) {
	c.mu.Lock()
	c.recorder = r
	c.mu.Unlock()
}

// Wrap returns a handler that runs inner and, only when inner succeeds,
// the checks matching the "path" field of the tool input. Check feedback
// is appended to inner's output; it never turns a successful edit into an
// error.
func (c *Checker) Wrap(toolName string, inner Handler) Handler {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		out, err := inner(ctx, input)
		if err != nil {
			return out, err
		}
		feedback := c.Run(ctx, toolName, pathFromInput(input))
		if feedback == "" {
			return out, nil
		}
		return out + "\n\n" + feedback, nil
	}
}

// Run executes every check whose glob matches editedPath, in
// configuration order, and returns the text to append to the tool
// result: empty when every check exited zero or none matched.
func (c *Checker) Run(ctx context.Context, toolName, editedPath string) string {
	if editedPath == "" {
		return ""
	}
	cleaned := path.Clean(editedPath)
	var matched []int
	for i, check := range c.checks {
		if globMatches(check.Glob, cleaned) {
			matched = append(matched, i)
		}
	}
	if len(matched) == 0 {
		return ""
	}

	meta := tool.CallContextFrom(ctx)
	if hasControlChar(editedPath) {
		for _, i := range matched {
			c.record(types.PostEditCheckRecord{
				ToolUseID: meta.ToolUseID,
				Turn:      meta.Turn,
				Tool:      toolName,
				Path:      security.Scrub(editedPath),
				Index:     i,
				Command:   security.Scrub(c.checks[i].Command),
				Error:     "path rejected: contains a control character",
			})
		}
		return "Warning: post-edit checks were skipped because the edited path contains a control character. The edit was applied."
	}

	var blocks []string
	for _, i := range matched {
		if ctx.Err() != nil {
			break
		}
		if block := c.runOne(ctx, meta, toolName, cleaned, i); block != "" {
			blocks = append(blocks, block)
		}
	}
	return strings.Join(blocks, "\n\n")
}

func (c *Checker) runOne(ctx context.Context, meta tool.CallContext, toolName, editedPath string, index int) string {
	check := c.checks[index]
	command := strings.ReplaceAll(check.Command, types.PostEditPathPlaceholder, ShellQuote(commandPath(editedPath)))

	timeout := time.Duration(types.EffectivePostEditTimeout(check)) * time.Second
	if caps := c.exec.Capabilities(); caps.MaxTimeout > 0 && timeout > caps.MaxTimeout {
		timeout = caps.MaxTimeout
	}

	rec := types.PostEditCheckRecord{
		ToolUseID: meta.ToolUseID,
		Turn:      meta.Turn,
		Tool:      toolName,
		Path:      security.Scrub(editedPath),
		Index:     index,
		Command:   security.Scrub(check.Command),
	}
	label := commandLabel(check.Command)
	quotedPath := security.NeutraliseFenceMarkers(strconv.Quote(security.Scrub(editedPath)))

	start := time.Now()
	res, err := c.exec.Exec(ctx, command, timeout)
	rec.DurationMs = time.Since(start).Milliseconds()

	var feedback string
	switch {
	case err != nil && errors.Is(err, executor.ErrTimeout):
		rec.TimedOut = true
		rec.Error = security.Scrub(err.Error())
		feedback = fmt.Sprintf("Warning: post-edit check %s timed out after %s for %s. The edit was applied; no diagnostics are available.", label, timeout, quotedPath)
		c.logger.Warn("post-edit check timed out", "index", index, "tool", toolName, "timeout", timeout)
	case err != nil:
		rec.Error = security.Scrub(err.Error())
		feedback = fmt.Sprintf("Warning: post-edit check %s could not run for %s: %s. The edit was applied.", label, quotedPath, rec.Error)
		c.logger.Warn("post-edit check failed to run", "index", index, "tool", toolName, "error", rec.Error)
	case res == nil:
		rec.Error = "executor returned no result"
		feedback = fmt.Sprintf("Warning: post-edit check %s could not run for %s. The edit was applied.", label, quotedPath)
	case res.ExitCode != 0:
		rec.ExitCode = res.ExitCode
		diagnostics, total, truncated := boundedDiagnostics(res.Stdout, res.Stderr)
		rec.OutputBytes = total
		rec.Truncated = truncated || res.OutputTruncated
		feedback = c.renderFailure(label, quotedPath, res.ExitCode, diagnostics)
	}
	c.record(rec)
	return feedback
}

func (c *Checker) renderFailure(label, quotedPath string, exitCode int, diagnostics string) string {
	header := fmt.Sprintf("Post-edit check %s exited with code %d for %s. The edit was applied; fix the reported problems with a further edit.", label, exitCode, quotedPath)
	fence, err := security.NewDataFence(c.entropy)
	if err != nil {
		return header + " Diagnostics are withheld because no data fence could be created."
	}
	return header + "\n" + fence.Notice(fenceLabel) + "\n" + fence.Wrap(fenceLabel, diagnostics)
}

func (c *Checker) record(rec types.PostEditCheckRecord) {
	c.mu.Lock()
	r := c.recorder
	c.mu.Unlock()
	if r != nil {
		r.RecordPostEditCheck(rec)
	}
}

// ShellQuote wraps s in POSIX single quotes so sh -c reads it as one
// literal word.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// commandPath keeps a path that starts with "-" from being read as an
// option by the check command.
func commandPath(p string) string {
	if strings.HasPrefix(p, "-") {
		return "./" + p
	}
	return p
}

// globMatches applies a slash-free glob to the base name and any other
// glob to the whole cleaned path.
func globMatches(glob, cleaned string) bool {
	target := cleaned
	if !strings.Contains(glob, "/") {
		target = path.Base(cleaned)
	}
	ok, err := path.Match(glob, target)
	return err == nil && ok
}

func hasControlChar(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

func commandLabel(command string) string {
	label := strings.Join(strings.Fields(security.Scrub(command)), " ")
	if len(label) > maxCommandLabelBytes {
		label = truncateToRuneBoundary(label, maxCommandLabelBytes) + "..."
	}
	return security.NeutraliseFenceMarkers(strconv.Quote(label))
}

// boundedDiagnostics returns the scrubbed combined output, cut to
// MaxDiagnosticsBytes, with the scrubbed byte count and whether a cut
// happened. Scrubbing runs before the cut so a secret straddling the
// boundary is still matched.
func boundedDiagnostics(stdout, stderr string) (string, int, bool) {
	combined := stdout
	if stderr != "" {
		if combined != "" && !strings.HasSuffix(combined, "\n") {
			combined += "\n"
		}
		combined += stderr
	}
	scrubbed := strings.TrimRight(security.Scrub(combined), " \t\r\n")
	total := len(scrubbed)
	if total == 0 {
		return "(the check produced no output)", 0, false
	}
	if total <= MaxDiagnosticsBytes {
		return scrubbed, total, false
	}
	head := truncateToRuneBoundary(scrubbed, MaxDiagnosticsBytes)
	return head + fmt.Sprintf("\n[diagnostics truncated: showing the first %d of %d bytes]", len(head), total), total, true
}

func truncateToRuneBoundary(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// pathFromInput reads the "path" field every edit-strategy input shape
// carries; "" on malformed input.
func pathFromInput(input json.RawMessage) string {
	var probe struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(input, &probe)
	return probe.Path
}
