package codescanner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// defaultSemgrepTimeout bounds a single Scan invocation. Semgrep with
// `--config auto` over a single small file should complete in well under
// 30s; longer runs almost always indicate the registry fetch is timing
// out and we'd rather fail fast than hang the agent loop.
const defaultSemgrepTimeout = 30 * time.Second

// semgrepBinary is the executable name to look for on PATH. Kept as a
// package-level var so tests can substitute a fake (a small wrapper script
// that emits canned JSON) without rewriting the constructor contract.
var semgrepBinary = "semgrep"

// SemgrepScanner shells out to a local `semgrep` binary, piping the file
// content on stdin and parsing the JSON result. If the binary is not on
// PATH at construction time we return a no-op so the harness keeps
// working — Semgrep is intentionally optional.
type SemgrepScanner struct {
	// Timeout is the per-Scan wall-clock cap. Defaults to
	// defaultSemgrepTimeout (30s).
	Timeout time.Duration

	// path is the resolved absolute path to the semgrep binary.
	path string

	// runFn is the exec hook. Tests substitute it; production passes nil
	// and the default exec.CommandContext is used.
	runFn func(ctx context.Context, path, language string, stdin []byte) ([]byte, error)
}

// NoopSemgrepScanner is returned by NewSemgrepScanner when the binary is
// missing. It logs a single warning at construction time and then always
// returns an empty result, so silent absence is a feature: operators who
// want hard enforcement should configure a composite scanner with the
// pattern pack alongside.
type NoopSemgrepScanner struct{}

// Scan returns an empty result.
func (NoopSemgrepScanner) Scan(ctx context.Context, path string, content []byte) (*ScanResult, error) {
	return &ScanResult{}, nil
}

// noopWarnOnce ensures we only log the "semgrep not found" warning a
// single time per process: a busy run that builds many scanners (e.g.
// per-task in eval) would otherwise spam the log.
var noopWarnOnce sync.Once

// NewSemgrepScanner returns a CodeScanner backed by the semgrep binary if
// present on PATH. Returns NoopSemgrepScanner otherwise (with a single
// startup warning routed through slog).
func NewSemgrepScanner() CodeScanner {
	resolved, err := exec.LookPath(semgrepBinary)
	if err != nil {
		noopWarnOnce.Do(func() {
			slog.Warn("codescanner: semgrep binary not found on PATH; semgrep scanner disabled",
				"binary", semgrepBinary,
				"error", err.Error(),
			)
		})
		return NoopSemgrepScanner{}
	}
	return &SemgrepScanner{
		Timeout: defaultSemgrepTimeout,
		path:    resolved,
	}
}

// Scan invokes semgrep on the provided content. The content is passed via
// stdin (with `-` as the input filename) so we never write a temp file
// containing potentially sensitive content to disk.
func (s *SemgrepScanner) Scan(ctx context.Context, path string, content []byte) (*ScanResult, error) {
	if len(content) == 0 {
		return &ScanResult{}, nil
	}

	timeout := s.Timeout
	if timeout <= 0 {
		timeout = defaultSemgrepTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	language := languageFromPath(path)

	run := s.runFn
	if run == nil {
		run = defaultSemgrepRun
	}
	out, err := run(ctx, s.path, language, content)
	if err != nil {
		return nil, fmt.Errorf("codescanner: semgrep run failed: %w", err)
	}
	return parseSemgrepOutput(out)
}

// defaultSemgrepRun executes semgrep against stdin with `--config auto`.
// The `--lang` flag is set when we can infer it from the path; otherwise
// semgrep does its own detection on the buffer.
func defaultSemgrepRun(ctx context.Context, binPath, language string, stdin []byte) ([]byte, error) {
	args := []string{"--config", "auto", "--json", "--quiet", "-"}
	if language != "" {
		args = append([]string{"--lang", language}, args...)
	}
	cmd := exec.CommandContext(ctx, binPath, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Semgrep returns non-zero on findings as well as on errors;
		// distinguish via stderr content and exit code. Exit code 1
		// (findings) is acceptable; everything else is a hard error.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return stdout.Bytes(), nil
		}
		return nil, fmt.Errorf("%w: stderr=%s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// semgrepResultsEnvelope is the subset of semgrep --json output we
// consume. The schema is stable across Semgrep 1.x.
type semgrepResultsEnvelope struct {
	Results []struct {
		CheckID string `json:"check_id"`
		Start   struct {
			Line int `json:"line"`
		} `json:"start"`
		Extra struct {
			Severity string `json:"severity"`
			Message  string `json:"message"`
		} `json:"extra"`
	} `json:"results"`
}

// parseSemgrepOutput maps the semgrep envelope to ScanResult. Severities
// are normalised: ERROR → block, WARNING/INFO → warn, anything else →
// warn (defensive: an unknown severity is treated as advisory).
func parseSemgrepOutput(raw []byte) (*ScanResult, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return &ScanResult{}, nil
	}
	var env semgrepResultsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("codescanner: parse semgrep output: %w", err)
	}
	findings := make([]Finding, 0, len(env.Results))
	for _, r := range env.Results {
		findings = append(findings, Finding{
			Severity: mapSemgrepSeverity(r.Extra.Severity),
			Rule:     r.CheckID,
			Line:     r.Start.Line,
			Message:  r.Extra.Message,
		})
	}
	return &ScanResult{Findings: findings}, nil
}

func mapSemgrepSeverity(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "ERROR":
		return SeverityBlock
	case "WARNING", "INFO":
		return SeverityWarn
	default:
		return SeverityWarn
	}
}

// languageFromPath returns a Semgrep --lang argument inferred from the
// file extension, or "" if we can't tell. Semgrep's auto-detection is
// usually correct, but passing an explicit language avoids the case where
// reading from stdin defeats extension sniffing.
func languageFromPath(path string) string {
	idx := strings.LastIndex(path, ".")
	if idx < 0 || idx == len(path)-1 {
		return ""
	}
	switch strings.ToLower(path[idx+1:]) {
	case "py":
		return "python"
	case "js", "mjs", "cjs":
		return "javascript"
	case "ts":
		return "typescript"
	case "tsx":
		return "tsx"
	case "go":
		return "go"
	case "rb":
		return "ruby"
	case "java":
		return "java"
	case "sh", "bash":
		return "bash"
	case "yaml", "yml":
		return "yaml"
	default:
		return ""
	}
}
