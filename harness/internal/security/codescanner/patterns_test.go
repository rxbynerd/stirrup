package codescanner

import (
	"context"
	"strings"
	"testing"
)

func TestPatternScanner_BlocksOnPlantedAnthropicKey(t *testing.T) {
	s := NewPatternScanner()
	content := []byte("config = {\n  apiKey: 'sk-ant-1234567890abcdef'\n}\n")

	res, err := s.Scan(context.Background(), "config.js", content)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !res.HasBlocking() {
		t.Fatalf("expected at least one block finding, got: %+v", res.Findings)
	}

	var found bool
	for _, f := range res.Findings {
		if strings.HasPrefix(f.Rule, "secret/anthropic_api_key") {
			if f.Severity != SeverityBlock {
				t.Errorf("expected severity %q, got %q", SeverityBlock, f.Severity)
			}
			if f.Line != 2 {
				t.Errorf("expected line 2, got %d", f.Line)
			}
			found = true
		}
	}
	if !found {
		t.Errorf("expected anthropic_api_key finding, got: %+v", res.Findings)
	}
}

func TestPatternScanner_BlocksOnAWSKey(t *testing.T) {
	s := NewPatternScanner()
	content := []byte("AWS_ACCESS_KEY_ID=AKIAABCDEFGHIJKLMNOP\n")

	res, err := s.Scan(context.Background(), ".env", content)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !res.HasBlocking() {
		t.Fatalf("expected blocking finding, got: %+v", res.Findings)
	}
}

func TestPatternScanner_WarnsOnPythonEvalSink(t *testing.T) {
	s := NewPatternScanner()
	content := []byte("def run(expr):\n    return eval(expr)\n")

	res, err := s.Scan(context.Background(), "run.py", content)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.HasBlocking() {
		t.Errorf("eval() alone should warn, not block: %+v", res.Findings)
	}

	var found bool
	for _, f := range res.Findings {
		if f.Rule == "sink/python_eval" {
			if f.Severity != SeverityWarn {
				t.Errorf("expected severity %q, got %q", SeverityWarn, f.Severity)
			}
			if f.Line != 2 {
				t.Errorf("expected line 2, got %d", f.Line)
			}
			found = true
		}
	}
	if !found {
		t.Errorf("expected python_eval finding, got: %+v", res.Findings)
	}
}

func TestPatternScanner_WarnsOnSubprocessShellTrue(t *testing.T) {
	s := NewPatternScanner()
	content := []byte("subprocess.run(cmd, shell=True)\n")

	res, err := s.Scan(context.Background(), "x.py", content)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var found bool
	for _, f := range res.Findings {
		if f.Rule == "sink/python_subprocess_shell_true" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected subprocess_shell_true finding, got: %+v", res.Findings)
	}
}

func TestPatternScanner_WarnsOnFunctionConstructor(t *testing.T) {
	s := NewPatternScanner()
	content := []byte("const fn = new Function('return 1');\n")

	res, err := s.Scan(context.Background(), "x.js", content)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var found bool
	for _, f := range res.Findings {
		if f.Rule == "sink/js_function_constructor" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected js_function_constructor finding, got: %+v", res.Findings)
	}
}

func TestPatternScanner_DoesNotMatchMethodCallNamedEval(t *testing.T) {
	// `obj.eval(...)` should not match — the pattern requires a token
	// boundary that is not `.` so member calls are excluded.
	s := NewPatternScanner()
	content := []byte("result = obj.eval(thing)\n")

	res, err := s.Scan(context.Background(), "x.py", content)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, f := range res.Findings {
		if f.Rule == "sink/python_eval" {
			t.Errorf("obj.eval should not match python_eval rule: %+v", f)
		}
	}
}

func TestPatternScanner_EmptyContent(t *testing.T) {
	s := NewPatternScanner()
	res, err := s.Scan(context.Background(), "empty.txt", nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Errorf("empty content must yield no findings, got: %+v", res.Findings)
	}
}

func TestPatternScanner_CleanContent(t *testing.T) {
	s := NewPatternScanner()
	content := []byte("package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n")
	res, err := s.Scan(context.Background(), "main.go", content)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Errorf("clean content must yield no findings, got: %+v", res.Findings)
	}
}
