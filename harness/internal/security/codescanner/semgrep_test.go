package codescanner

import (
	"context"
	"testing"
)

func TestNewSemgrepScanner_NoopWhenBinaryMissing(t *testing.T) {
	// Force the lookup to a name that cannot exist on PATH so we
	// exercise the no-op fallback regardless of the test host.
	prev := semgrepBinary
	semgrepBinary = "stirrup-semgrep-does-not-exist-xyz"
	t.Cleanup(func() { semgrepBinary = prev })

	s := NewSemgrepScanner("")
	if _, ok := s.(NoopSemgrepScanner); !ok {
		t.Fatalf("expected NoopSemgrepScanner, got %T", s)
	}

	res, err := s.Scan(context.Background(), "x.py", []byte("eval('boom')"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Errorf("noop must yield no findings, got: %+v", res.Findings)
	}
}

func TestParseSemgrepOutput_MapsSeverities(t *testing.T) {
	raw := []byte(`{
		"results": [
			{
				"check_id": "rule/error",
				"start": {"line": 7},
				"extra": {"severity": "ERROR", "message": "blocker"}
			},
			{
				"check_id": "rule/warning",
				"start": {"line": 10},
				"extra": {"severity": "WARNING", "message": "advisory"}
			},
			{
				"check_id": "rule/info",
				"start": {"line": 11},
				"extra": {"severity": "INFO", "message": "fyi"}
			},
			{
				"check_id": "rule/unknown",
				"start": {"line": 12},
				"extra": {"severity": "MYSTERY", "message": "unmapped"}
			}
		]
	}`)
	res, err := parseSemgrepOutput(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Findings) != 4 {
		t.Fatalf("expected 4 findings, got %d", len(res.Findings))
	}
	expected := []struct {
		rule, severity string
	}{
		{"rule/error", SeverityBlock},
		{"rule/warning", SeverityWarn},
		{"rule/info", SeverityWarn},
		{"rule/unknown", SeverityWarn},
	}
	for i, exp := range expected {
		if res.Findings[i].Rule != exp.rule {
			t.Errorf("[%d] rule: got %q want %q", i, res.Findings[i].Rule, exp.rule)
		}
		if res.Findings[i].Severity != exp.severity {
			t.Errorf("[%d] severity: got %q want %q", i, res.Findings[i].Severity, exp.severity)
		}
	}
}

func TestParseSemgrepOutput_EmptyResults(t *testing.T) {
	res, err := parseSemgrepOutput([]byte(`{"results": []}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(res.Findings))
	}
}

func TestParseSemgrepOutput_EmptyBytes(t *testing.T) {
	res, err := parseSemgrepOutput(nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(res.Findings))
	}
}

func TestParseSemgrepOutput_BadJSON(t *testing.T) {
	_, err := parseSemgrepOutput([]byte(`not json`))
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

func TestSemgrepScanner_RunFnHookIsHonoured(t *testing.T) {
	// Verify the runFn hook contract: a fake exec returning canned JSON
	// flows through Scan unchanged. This lets the constructor wire
	// SemgrepScanner.path without us having to shell out to a real
	// binary in tests.
	canned := []byte(`{"results":[{"check_id":"fake/rule","start":{"line":3},"extra":{"severity":"ERROR","message":"hi"}}]}`)
	s := &SemgrepScanner{
		path: "/fake/semgrep",
		runFn: func(ctx context.Context, p, lang, configArg string, stdin []byte) ([]byte, error) {
			return canned, nil
		},
	}
	res, err := s.Scan(context.Background(), "x.py", []byte("anything"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Findings) != 1 || res.Findings[0].Rule != "fake/rule" {
		t.Fatalf("unexpected findings: %+v", res.Findings)
	}
	if res.Findings[0].Severity != SeverityBlock {
		t.Errorf("expected block severity, got %q", res.Findings[0].Severity)
	}
}

// TestSemgrepScanner_ConfigPathUsed covers M7: a non-empty
// ConfigPath flows into the runFn argv as the value of --config.
// This is the supply-chain pin: operators set a local rules bundle
// (e.g. /etc/stirrup/semgrep-rules) so semgrep stops fetching from
// semgrep.dev at scan time.
func TestSemgrepScanner_ConfigPathUsed(t *testing.T) {
	var captured string
	s := &SemgrepScanner{
		path:       "/fake/semgrep",
		ConfigPath: "/etc/stirrup/semgrep-rules",
		runFn: func(ctx context.Context, p, lang, configArg string, stdin []byte) ([]byte, error) {
			captured = configArg
			return []byte(`{"results":[]}`), nil
		},
	}
	if _, err := s.Scan(context.Background(), "x.py", []byte("ok")); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if want := "/etc/stirrup/semgrep-rules"; captured != want {
		t.Errorf("configArg: got %q, want %q", captured, want)
	}
}

// TestSemgrepScanner_DefaultConfigIsAuto ensures the historical
// behaviour is preserved when ConfigPath is left empty.
func TestSemgrepScanner_DefaultConfigIsAuto(t *testing.T) {
	var captured string
	s := &SemgrepScanner{
		path: "/fake/semgrep",
		runFn: func(ctx context.Context, p, lang, configArg string, stdin []byte) ([]byte, error) {
			captured = configArg
			return []byte(`{"results":[]}`), nil
		},
	}
	if _, err := s.Scan(context.Background(), "x.py", []byte("ok")); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if captured != "auto" {
		t.Errorf("configArg: got %q, want auto", captured)
	}
}

func TestLanguageFromPath(t *testing.T) {
	cases := map[string]string{
		"x.py":      "python",
		"x.js":      "javascript",
		"x.ts":      "typescript",
		"x.tsx":     "tsx",
		"x.go":      "go",
		"x.sh":      "bash",
		"unknown":   "",
		"noext.":    "",
		"a.unknown": "",
	}
	for in, want := range cases {
		if got := languageFromPath(in); got != want {
			t.Errorf("languageFromPath(%q) = %q, want %q", in, got, want)
		}
	}
}
