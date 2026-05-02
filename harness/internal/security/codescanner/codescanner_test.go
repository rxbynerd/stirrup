package codescanner

import (
	"context"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

func TestNew_NilConfigReturnsNoneScanner(t *testing.T) {
	s, err := New(nil)
	if err != nil {
		t.Fatalf("New(nil): %v", err)
	}
	if _, ok := s.(NoneScanner); !ok {
		t.Errorf("expected NoneScanner, got %T", s)
	}
}

func TestNew_EmptyTypeReturnsNoneScanner(t *testing.T) {
	s, err := New(&types.CodeScannerConfig{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := s.(NoneScanner); !ok {
		t.Errorf("expected NoneScanner, got %T", s)
	}
}

func TestNew_PatternsType(t *testing.T) {
	s, err := New(&types.CodeScannerConfig{Type: "patterns"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := s.(*PatternScanner); !ok {
		t.Errorf("expected *PatternScanner, got %T", s)
	}
}

func TestNew_CompositeRequiresChildren(t *testing.T) {
	_, err := New(&types.CodeScannerConfig{Type: "composite"})
	if err == nil {
		t.Fatal("expected error for empty composite, got nil")
	}
}

func TestNew_CompositeRejectsNestedComposite(t *testing.T) {
	_, err := New(&types.CodeScannerConfig{
		Type:     "composite",
		Scanners: []string{"patterns", "composite"},
	})
	if err == nil {
		t.Fatal("expected error for nested composite, got nil")
	}
}

func TestNew_UnknownTypeRejected(t *testing.T) {
	_, err := New(&types.CodeScannerConfig{Type: "bogus"})
	if err == nil {
		t.Fatal("expected error for unknown type, got nil")
	}
}

func TestNoneScanner_AlwaysEmpty(t *testing.T) {
	s := NoneScanner{}
	res, err := s.Scan(context.Background(), "x.py", []byte("eval('boom')"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Errorf("expected no findings, got %d", len(res.Findings))
	}
}

func TestScanResult_HasBlocking(t *testing.T) {
	r := &ScanResult{Findings: []Finding{
		{Severity: SeverityWarn},
	}}
	if r.HasBlocking() {
		t.Error("HasBlocking should be false with only warn findings")
	}
	r.Findings = append(r.Findings, Finding{Severity: SeverityBlock})
	if !r.HasBlocking() {
		t.Error("HasBlocking should be true with a block finding")
	}
	var nilR *ScanResult
	if nilR.HasBlocking() {
		t.Error("HasBlocking on nil result must be false")
	}
}
