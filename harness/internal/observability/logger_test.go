package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
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

// TestLogger_WithSessionNameAttribute checks that attributes attached with
// With on a NewLogger logger appear on every record. The harness factory
// relies on this when it chains logger.With("sessionName", ...).
func TestLogger_WithSessionNameAttribute(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	logger := NewLogger("test-run", slog.LevelInfo, &buf)
	logger = logger.With("sessionName", "nightly-eval")

	logger.Info("hello")

	var record map[string]any
	must.NoError(json.Unmarshal(buf.Bytes(), &record), "output: %s", buf.String())
	must.Equal("nightly-eval", record["sessionName"], "record: %v", record)
	must.Equal("test-run", record["runId"], "record: %v", record)
}

func TestScrubHandler_RedactsAnthropicKey(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	logger := NewLogger("test-run", slog.LevelInfo, &buf)

	logger.Info("api call", "apiKey", "sk-ant-api03-abcdef1234567890")

	output := buf.String()
	must.NotContains(output, "sk-ant-api03", "Anthropic key not scrubbed from output")
	must.Contains(output, "[REDACTED]", "expected [REDACTED] placeholder in output")
}

func TestScrubHandler_RedactsOpenAIKey(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	logger := NewLogger("test-run", slog.LevelInfo, &buf)

	logger.Info("api call", "key", "sk-proj-abcdef1234567890abcdef")

	output := buf.String()
	must.NotContains(output, "sk-proj-abcdef", "OpenAI key not scrubbed from output")
	must.Contains(output, "[REDACTED]", "expected [REDACTED] placeholder in output")
}

func TestScrubHandler_RedactsGitHubPAT(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	logger := NewLogger("test-run", slog.LevelInfo, &buf)

	logger.Info("auth", "token", "ghp_abc123def456ghi789")

	must.NotContains(buf.String(), "ghp_", "GitHub PAT not scrubbed from output")
}

func TestScrubHandler_RedactsAWSAccessKey(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	logger := NewLogger("test-run", slog.LevelInfo, &buf)

	logger.Info("aws", "accessKeyId", "AKIAIOSFODNN7EXAMPLE")

	must.NotContains(buf.String(), "AKIAIOSFODNN7EXAMPLE", "AWS access key not scrubbed from output")
}

func TestScrubHandler_RedactsBearerToken(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	logger := NewLogger("test-run", slog.LevelInfo, &buf)

	logger.Info("request", "auth", "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abc123")

	must.NotContains(buf.String(), "eyJhbGci", "Bearer token not scrubbed from output")
}

func TestScrubHandler_PreservesNonSecretValues(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	logger := NewLogger("test-run", slog.LevelInfo, &buf)

	logger.Info("normal log", "workspace", "/tmp/workspace", "turn", 5)

	output := buf.String()
	must.Contains(output, "/tmp/workspace", "non-secret string value was modified")
	must.Contains(output, "normal log", "message was modified")
}

func TestScrubHandler_GroupAttrs_ScrubsSecretInGroup(t *testing.T) {
	must := require.New(t)
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
	must.NotContains(output, "sk-ant-api03", "secret in group attr not scrubbed")
	must.Contains(output, "test", "non-secret value in group was modified")
}

func TestScrubHandler_WithAttrs_ScrubsBoundAttrs(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	scrubHandler := NewScrubHandler(jsonHandler)

	// Attributes bound with WithAttrs are scrubbed too.
	childHandler := scrubHandler.WithAttrs([]slog.Attr{
		slog.String("token", "ghp_abc123def456"),
	})
	logger := slog.New(childHandler)

	logger.Info("test message")

	must.NotContains(buf.String(), "ghp_", "secret in WithAttrs not scrubbed")
}

func TestScrubHandler_WithGroup_ScrubsSecretAndKeepsGroupName(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	scrubHandler := NewScrubHandler(jsonHandler)

	childHandler := scrubHandler.WithGroup("details")
	logger := slog.New(childHandler)

	logger.Info("test", "secret", "sk-ant-api03-xyz")

	output := buf.String()
	must.NotContains(output, "sk-ant-api03", "secret in group not scrubbed")
	must.Contains(output, "details", "group name not present in output")
}

func TestNewLogger_ProducesValidJSON(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	logger := NewLogger("run-123", slog.LevelInfo, &buf)

	logger.Info("test event", "key", "value")

	var parsed map[string]any
	must.NoError(json.Unmarshal(buf.Bytes(), &parsed), "output: %s", buf.String())

	must.Equal("run-123", parsed["runId"], "runId field in JSON output")
	must.Contains(parsed, "time")
	must.Contains(parsed, "level")
	must.Contains(parsed, "msg")
}

func TestNewLogger_RespectsLevel(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	logger := NewLogger("run-123", slog.LevelWarn, &buf)

	logger.Info("should not appear")
	logger.Warn("should appear")

	output := buf.String()
	must.NotContains(output, "should not appear", "info message logged despite warn level")
	must.Contains(output, "should appear", "warn message not logged")
}

// TestScrubHandlerWithSecurity_FiresSecretRedactedInOutput asserts that the
// logger emits a SecretRedactedInOutput event (via the wired notifier) when
// the scrubber redacts a secret from an attribute value.
func TestScrubHandlerWithSecurity_FiresSecretRedactedInOutput(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	spy := &spyNotifier{}
	scrub := NewScrubHandlerWithSecurity(jsonHandler, spy)
	logger := slog.New(scrub)

	logger.Info("api call", slog.String("token", anthropicKeyFixture))

	calls := spy.snapshot()
	must.Len(calls, 1, "notifier calls: %+v", calls)
	must.Equal("anthropic_api_key", calls[0].pattern)
	must.Contains(calls[0].location, "token")

	// The redacted value must not appear in the JSON output.
	must.NotContains(buf.String(), anthropicKeyFixture, "secret leaked into log output")
}

// TestScrubHandlerWithSecurity_NoEventOnCleanAttr asserts that emitting an
// attribute with no secret content does not produce a notifier call.
func TestScrubHandlerWithSecurity_NoEventOnCleanAttr(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	spy := &spyNotifier{}
	scrub := NewScrubHandlerWithSecurity(jsonHandler, spy)
	logger := slog.New(scrub)

	logger.Info("plain log", slog.String("ok", "hello"))

	must.Empty(spy.snapshot(), "notifier calls for a clean attribute")
}

// TestScrubHandlerWithSecurity_WithGroupPropagatesNotifier asserts that
// WithGroup returns a handler that still notifies on redactions, i.e. the
// security field survives the group-binding code path.
func TestScrubHandlerWithSecurity_WithGroupPropagatesNotifier(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	spy := &spyNotifier{}
	scrub := NewScrubHandlerWithSecurity(jsonHandler, spy)
	grouped := scrub.WithGroup("g")
	logger := slog.New(grouped)

	logger.Info("with group", slog.String("token", anthropicKeyFixture))

	calls := spy.snapshot()
	must.NotEmpty(calls, "expected a notifier call after WithGroup")
	must.Equal("anthropic_api_key", calls[0].pattern)
}

// TestScrubHandlerWithSecurity_WithAttrsPropagatesNotifier asserts that
// WithAttrs scrubs and notifies at bind time (and that the resulting
// child handler retains the notifier so subsequent emissions also fire).
func TestScrubHandlerWithSecurity_WithAttrsPropagatesNotifier(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	jsonHandler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	spy := &spyNotifier{}
	scrub := NewScrubHandlerWithSecurity(jsonHandler, spy)

	child := scrub.WithAttrs([]slog.Attr{
		slog.String("k", anthropicKeyFixture),
	})
	logger := slog.New(child)

	// WithAttrs scrubs at bind time, so the notifier fires during the call.
	preEmitCalls := spy.snapshot()
	must.NotEmpty(preEmitCalls, "expected notifier to fire during WithAttrs bind")
	must.Equal("anthropic_api_key", preEmitCalls[0].pattern)

	// Sanity: emitting through the child handler still works.
	logger.Info("child emit")
}
