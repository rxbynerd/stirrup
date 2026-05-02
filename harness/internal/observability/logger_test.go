package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// notifierCall records a single SecretRedactedInOutput invocation made
// against the spy notifier.
type notifierCall struct {
	pattern  string
	location string
}

// spyNotifier captures every SecretRedactedInOutput call so tests can
// assert that the logger fired the security event when a secret was
// redacted from an attribute. It satisfies the SecurityNotifier interface.
type spyNotifier struct {
	mu    sync.Mutex
	calls []notifierCall
}

func (s *spyNotifier) SecretRedactedInOutput(pattern, location string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, notifierCall{pattern: pattern, location: location})
}

func (s *spyNotifier) snapshot() []notifierCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]notifierCall, len(s.calls))
	copy(out, s.calls)
	return out
}

// anthropicKeyFixture is a syntactically-valid Anthropic API key fixture
// with the prefix that secretPatterns matches. The body is 46
// alphanumeric characters; ScrubWithStats must redact it.
const anthropicKeyFixture = "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// TestLogger_WithSessionNameAttribute verifies that a logger built via
// NewLoggerWithSecurity preserves caller-attached default attributes
// (e.g. sessionName) on every record. This is the property the harness
// factory relies on when it does logger = logger.With("sessionName", ...).
// If a future change makes the returned logger not chain .With() correctly,
// this test will catch it before sessionName silently disappears from
// production logs.
func TestLogger_WithSessionNameAttribute(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("test-run", slog.LevelInfo, &buf)
	logger = logger.With("sessionName", "nightly-eval")

	logger.Info("hello")

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("unmarshal log record: %v\n%s", err, buf.String())
	}
	if got, ok := record["sessionName"].(string); !ok || got != "nightly-eval" {
		t.Errorf("sessionName attribute missing or wrong: %v", record)
	}
	if got, ok := record["runId"].(string); !ok || got != "test-run" {
		t.Errorf("runId attribute missing or wrong: %v", record)
	}
}

func TestScrubHandler_RedactsAnthropicKey(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("test-run", slog.LevelInfo, &buf)

	logger.Info("api call", "apiKey", "sk-ant-api03-abcdef1234567890")

	output := buf.String()
	if strings.Contains(output, "sk-ant-api03") {
		t.Errorf("Anthropic key not scrubbed from output: %s", output)
	}
	if !strings.Contains(output, "[REDACTED]") {
		t.Errorf("expected [REDACTED] placeholder in output: %s", output)
	}
}

func TestScrubHandler_RedactsOpenAIKey(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("test-run", slog.LevelInfo, &buf)

	logger.Info("api call", "key", "sk-proj-abcdef1234567890abcdef")

	output := buf.String()
	if strings.Contains(output, "sk-proj-abcdef") {
		t.Errorf("OpenAI key not scrubbed from output: %s", output)
	}
	if !strings.Contains(output, "[REDACTED]") {
		t.Errorf("expected [REDACTED] placeholder in output: %s", output)
	}
}

func TestScrubHandler_RedactsGitHubPAT(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("test-run", slog.LevelInfo, &buf)

	logger.Info("auth", "token", "ghp_abc123def456ghi789")

	output := buf.String()
	if strings.Contains(output, "ghp_") {
		t.Errorf("GitHub PAT not scrubbed from output: %s", output)
	}
}

func TestScrubHandler_RedactsAWSAccessKey(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("test-run", slog.LevelInfo, &buf)

	logger.Info("aws", "accessKeyId", "AKIAIOSFODNN7EXAMPLE")

	output := buf.String()
	if strings.Contains(output, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("AWS access key not scrubbed from output: %s", output)
	}
}

func TestScrubHandler_RedactsBearerToken(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("test-run", slog.LevelInfo, &buf)

	logger.Info("request", "auth", "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abc123")

	output := buf.String()
	if strings.Contains(output, "eyJhbGci") {
		t.Errorf("Bearer token not scrubbed from output: %s", output)
	}
}

func TestScrubHandler_PreservesNonSecretValues(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("test-run", slog.LevelInfo, &buf)

	logger.Info("normal log", "workspace", "/tmp/workspace", "turn", 5)

	output := buf.String()
	if !strings.Contains(output, "/tmp/workspace") {
		t.Errorf("non-secret string value was modified: %s", output)
	}
	if !strings.Contains(output, "normal log") {
		t.Errorf("message was modified: %s", output)
	}
}

func TestScrubHandler_HandlesGroupAttrs(t *testing.T) {
	var buf bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	scrubHandler := NewScrubHandler(jsonHandler)
	logger := slog.New(scrubHandler)

	logger.Info("nested",
		slog.Group("config",
			slog.String("apiKey", "sk-ant-api03-secret123"),
			slog.String("name", "test"),
		),
	)

	output := buf.String()
	if strings.Contains(output, "sk-ant-api03") {
		t.Errorf("secret in group attr not scrubbed: %s", output)
	}
	if !strings.Contains(output, "test") {
		t.Errorf("non-secret value in group was modified: %s", output)
	}
}

func TestScrubHandler_WithAttrs(t *testing.T) {
	var buf bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	scrubHandler := NewScrubHandler(jsonHandler)

	// WithAttrs should scrub the default attrs too
	childHandler := scrubHandler.WithAttrs([]slog.Attr{
		slog.String("token", "ghp_abc123def456"),
	})
	logger := slog.New(childHandler)

	logger.Info("test message")

	output := buf.String()
	if strings.Contains(output, "ghp_") {
		t.Errorf("secret in WithAttrs not scrubbed: %s", output)
	}
}

func TestScrubHandler_WithGroup(t *testing.T) {
	var buf bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	scrubHandler := NewScrubHandler(jsonHandler)

	childHandler := scrubHandler.WithGroup("details")
	logger := slog.New(childHandler)

	logger.Info("test", "secret", "sk-ant-api03-xyz")

	output := buf.String()
	if strings.Contains(output, "sk-ant-api03") {
		t.Errorf("secret in group not scrubbed: %s", output)
	}
	if !strings.Contains(output, "details") {
		t.Errorf("group name not present in output: %s", output)
	}
}

func TestNewLogger_ProducesValidJSON(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("run-123", slog.LevelInfo, &buf)

	logger.Info("test event", "key", "value")

	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, buf.String())
	}

	runID, ok := parsed["runId"]
	if !ok {
		t.Errorf("runId field missing from JSON output: %s", buf.String())
	}
	if runID != "run-123" {
		t.Errorf("runId = %q, want %q", runID, "run-123")
	}

	if _, ok := parsed["time"]; !ok {
		t.Errorf("time field missing from JSON output")
	}
	if _, ok := parsed["level"]; !ok {
		t.Errorf("level field missing from JSON output")
	}
	if _, ok := parsed["msg"]; !ok {
		t.Errorf("msg field missing from JSON output")
	}
}

func TestNewLogger_RespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("run-123", slog.LevelWarn, &buf)

	logger.Info("should not appear")
	logger.Warn("should appear")

	output := buf.String()
	if strings.Contains(output, "should not appear") {
		t.Errorf("info message logged despite warn level: %s", output)
	}
	if !strings.Contains(output, "should appear") {
		t.Errorf("warn message not logged: %s", output)
	}
}

// TestScrubHandlerWithSecurity_FiresSecretRedactedInOutput asserts that the
// logger emits a SecretRedactedInOutput event (via the wired notifier) when
// the scrubber redacts a secret from an attribute value.
func TestScrubHandlerWithSecurity_FiresSecretRedactedInOutput(t *testing.T) {
	var buf bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	spy := &spyNotifier{}
	scrub := NewScrubHandlerWithSecurity(jsonHandler, spy)
	logger := slog.New(scrub)

	logger.Info("api call", slog.String("token", anthropicKeyFixture))

	calls := spy.snapshot()
	if len(calls) != 1 {
		t.Fatalf("spy received %d calls, want 1: %+v", len(calls), calls)
	}
	if calls[0].pattern != "anthropic_api_key" {
		t.Errorf("calls[0].pattern = %q, want anthropic_api_key", calls[0].pattern)
	}
	if !strings.Contains(calls[0].location, "token") {
		t.Errorf("calls[0].location = %q, want it to contain 'token'", calls[0].location)
	}

	// The redacted value must not appear in the JSON output.
	if strings.Contains(buf.String(), anthropicKeyFixture) {
		t.Errorf("secret leaked into log output: %s", buf.String())
	}
}

// TestScrubHandlerWithSecurity_NoEventOnCleanAttr asserts that emitting an
// attribute with no secret content does not produce a notifier call.
func TestScrubHandlerWithSecurity_NoEventOnCleanAttr(t *testing.T) {
	var buf bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	spy := &spyNotifier{}
	scrub := NewScrubHandlerWithSecurity(jsonHandler, spy)
	logger := slog.New(scrub)

	logger.Info("plain log", slog.String("ok", "hello"))

	if calls := spy.snapshot(); len(calls) != 0 {
		t.Errorf("spy received %d calls, want 0: %+v", len(calls), calls)
	}
}

// TestScrubHandlerWithSecurity_WithGroupPropagatesNotifier asserts that
// WithGroup returns a handler that still notifies on redactions, i.e. the
// security field survives the group-binding code path.
func TestScrubHandlerWithSecurity_WithGroupPropagatesNotifier(t *testing.T) {
	var buf bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	spy := &spyNotifier{}
	scrub := NewScrubHandlerWithSecurity(jsonHandler, spy)
	grouped := scrub.WithGroup("g")
	logger := slog.New(grouped)

	logger.Info("with group", slog.String("token", anthropicKeyFixture))

	calls := spy.snapshot()
	if len(calls) == 0 {
		t.Fatal("expected at least one notifier call after WithGroup, got 0")
	}
	if calls[0].pattern != "anthropic_api_key" {
		t.Errorf("calls[0].pattern = %q, want anthropic_api_key", calls[0].pattern)
	}
}

// TestScrubHandlerWithSecurity_WithAttrsPropagatesNotifier asserts that
// WithAttrs scrubs and notifies at bind time (and that the resulting
// child handler retains the notifier so subsequent emissions also fire).
func TestScrubHandlerWithSecurity_WithAttrsPropagatesNotifier(t *testing.T) {
	var buf bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	spy := &spyNotifier{}
	scrub := NewScrubHandlerWithSecurity(jsonHandler, spy)

	child := scrub.WithAttrs([]slog.Attr{
		slog.String("k", anthropicKeyFixture),
	})
	logger := slog.New(child)

	// WithAttrs scrubs at bind time → notifier fired during the call.
	preEmitCalls := spy.snapshot()
	if len(preEmitCalls) == 0 {
		t.Fatal("expected notifier to fire during WithAttrs bind, got 0 calls")
	}
	if preEmitCalls[0].pattern != "anthropic_api_key" {
		t.Errorf("preEmitCalls[0].pattern = %q, want anthropic_api_key", preEmitCalls[0].pattern)
	}

	// Sanity: emitting through the child handler still works.
	logger.Info("child emit")
}
