package codescanner

import (
	"context"
	"errors"
	"testing"
)

// fakeScanner returns a fixed result regardless of input. Used so
// composite tests don't depend on the regex pack's exact rule set.
type fakeScanner struct {
	findings []Finding
	err      error
}

func (f fakeScanner) Scan(ctx context.Context, path string, content []byte) (*ScanResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &ScanResult{Findings: append([]Finding(nil), f.findings...)}, nil
}

func TestCompositeScanner_UnionsFindings(t *testing.T) {
	a := fakeScanner{findings: []Finding{{Severity: SeverityBlock, Rule: "a/1", Line: 1, Message: "from-a"}}}
	b := fakeScanner{findings: []Finding{{Severity: SeverityWarn, Rule: "b/2", Line: 5, Message: "from-b"}}}

	c := NewCompositeScanner(a, b)
	res, err := c.Scan(context.Background(), "x.go", []byte("content"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Findings) != 2 {
		t.Fatalf("expected 2 findings (union), got %d", len(res.Findings))
	}
	if res.Findings[0].Rule != "a/1" || res.Findings[1].Rule != "b/2" {
		t.Errorf("unexpected ordering: %+v", res.Findings)
	}
}

func TestCompositeScanner_PropagatesErrors(t *testing.T) {
	want := errors.New("boom")
	c := NewCompositeScanner(fakeScanner{err: want})
	_, err := c.Scan(context.Background(), "x.go", []byte("y"))
	if !errors.Is(err, want) {
		t.Errorf("expected error to wrap boom, got %v", err)
	}
}

func TestNewComposite_RejectsEmpty(t *testing.T) {
	_, err := newComposite(nil, "")
	if err == nil {
		t.Fatal("expected error for empty scanners list")
	}
}

func TestNewComposite_RejectsUnknownChild(t *testing.T) {
	_, err := newComposite([]string{"patterns", "nope"}, "")
	if err == nil {
		t.Fatal("expected error for unknown child name")
	}
}

func TestNewComposite_BuildsKnownChildren(t *testing.T) {
	c, err := newComposite([]string{"patterns", "none"}, "")
	if err != nil {
		t.Fatalf("newComposite: %v", err)
	}
	cc, ok := c.(*CompositeScanner)
	if !ok {
		t.Fatalf("expected *CompositeScanner, got %T", c)
	}
	if len(cc.scanners) != 2 {
		t.Errorf("expected 2 children, got %d", len(cc.scanners))
	}
}
