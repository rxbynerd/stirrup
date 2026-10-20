package codescanner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewSemgrepScanner_NoopWhenBinaryMissing(t *testing.T) {
	must := require.New(t)
	// Force the lookup to a name that cannot exist on PATH so we
	// exercise the no-op fallback regardless of the test host.
	prev := semgrepBinary
	semgrepBinary = "stirrup-semgrep-does-not-exist-xyz"
	t.Cleanup(func() { semgrepBinary = prev })

	s := NewSemgrepScanner("")
	must.IsType(NoopSemgrepScanner{}, s)

	res, err := s.Scan(context.Background(), "x.py", []byte("eval('boom')"))
	must.NoError(err)
	must.Empty(res.Findings, "noop scanner must yield no findings")
}

func TestParseSemgrepOutput_MapsSeverities(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	must.Len(res.Findings, 4)

	expected := []struct {
		name, rule, severity string
	}{
		{"error maps to block", "rule/error", SeverityBlock},
		{"warning maps to warn", "rule/warning", SeverityWarn},
		{"info maps to warn", "rule/info", SeverityWarn},
		{"unknown severity maps to warn", "rule/unknown", SeverityWarn},
	}
	for i, exp := range expected {
		t.Run(exp.name, func(t *testing.T) {
			must := require.New(t)
			finding := res.Findings[i]
			must.Equal(exp.rule, finding.Rule)
			must.Equal(exp.severity, finding.Severity)
		})
	}
}

func TestParseSemgrepOutput_EmptyResults(t *testing.T) {
	must := require.New(t)
	res, err := parseSemgrepOutput([]byte(`{"results": []}`))
	must.NoError(err)
	must.Empty(res.Findings)
}

func TestParseSemgrepOutput_EmptyBytes(t *testing.T) {
	must := require.New(t)
	res, err := parseSemgrepOutput(nil)
	must.NoError(err)
	must.Empty(res.Findings)
}

func TestParseSemgrepOutput_BadJSON(t *testing.T) {
	must := require.New(t)
	_, err := parseSemgrepOutput([]byte(`not json`))
	must.Error(err)
}

func TestSemgrepScanner_RunFnHookIsHonoured(t *testing.T) {
	must := require.New(t)
	canned := []byte(`{"results":[{"check_id":"fake/rule","start":{"line":3},"extra":{"severity":"ERROR","message":"hi"}}]}`)
	s := &SemgrepScanner{
		path: "/fake/semgrep",
		runFn: func(ctx context.Context, p, lang, configArg string, stdin []byte) ([]byte, error) {
			return canned, nil
		},
	}
	res, err := s.Scan(context.Background(), "x.py", []byte("anything"))
	must.NoError(err)
	must.Len(res.Findings, 1)
	must.Equal("fake/rule", res.Findings[0].Rule)
	must.Equal(SeverityBlock, res.Findings[0].Severity)
}

// TestSemgrepScanner_ConfigPathUsed verifies a non-empty ConfigPath
// flows into the runFn argv as the value of --config.
func TestSemgrepScanner_ConfigPathUsed(t *testing.T) {
	must := require.New(t)
	var captured string
	s := &SemgrepScanner{
		path:       "/fake/semgrep",
		ConfigPath: "/etc/stirrup/semgrep-rules",
		runFn: func(ctx context.Context, p, lang, configArg string, stdin []byte) ([]byte, error) {
			captured = configArg
			return []byte(`{"results":[]}`), nil
		},
	}
	_, err := s.Scan(context.Background(), "x.py", []byte("ok"))
	must.NoError(err)
	must.Equal("/etc/stirrup/semgrep-rules", captured)
}

// TestSemgrepScanner_DefaultConfigIsAuto ensures ConfigPath defaults to
// "auto" when left empty.
func TestSemgrepScanner_DefaultConfigIsAuto(t *testing.T) {
	must := require.New(t)
	var captured string
	s := &SemgrepScanner{
		path: "/fake/semgrep",
		runFn: func(ctx context.Context, p, lang, configArg string, stdin []byte) ([]byte, error) {
			captured = configArg
			return []byte(`{"results":[]}`), nil
		},
	}
	_, err := s.Scan(context.Background(), "x.py", []byte("ok"))
	must.NoError(err)
	must.Equal("auto", captured)
}

func TestLanguageFromPath_MapsExtensions(t *testing.T) {
	cases := []struct {
		name, path, want string
	}{
		{"python source", "x.py", "python"},
		{"javascript source", "x.js", "javascript"},
		{"typescript source", "x.ts", "typescript"},
		{"tsx source", "x.tsx", "tsx"},
		{"go source", "x.go", "go"},
		{"shell script", "x.sh", "bash"},
		{"no extension", "unknown", ""},
		{"trailing dot", "noext.", ""},
		{"unknown extension", "a.unknown", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, languageFromPath(tc.path))
		})
	}
}
