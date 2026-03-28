package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

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
