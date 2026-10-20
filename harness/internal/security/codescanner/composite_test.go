package codescanner

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
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
	must := require.New(t)
	a := fakeScanner{findings: []Finding{{Severity: SeverityBlock, Rule: "a/1", Line: 1, Message: "from-a"}}}
	b := fakeScanner{findings: []Finding{{Severity: SeverityWarn, Rule: "b/2", Line: 5, Message: "from-b"}}}

	c := NewCompositeScanner(a, b)
	res, err := c.Scan(context.Background(), "x.go", []byte("content"))
	must.NoError(err)
	must.Len(res.Findings, 2)
	must.Equal("a/1", res.Findings[0].Rule)
	must.Equal("b/2", res.Findings[1].Rule)
}

func TestCompositeScanner_PropagatesErrors(t *testing.T) {
	must := require.New(t)
	want := errors.New("boom")
	c := NewCompositeScanner(fakeScanner{err: want})
	_, err := c.Scan(context.Background(), "x.go", []byte("y"))
	must.ErrorIs(err, want)
}

func TestNewComposite_RejectsEmpty(t *testing.T) {
	must := require.New(t)
	_, err := newComposite(nil, "")
	must.Error(err)
}

func TestNewComposite_RejectsUnknownChild(t *testing.T) {
	must := require.New(t)
	_, err := newComposite([]string{"patterns", "nope"}, "")
	must.Error(err)
}

func TestNewComposite_BuildsKnownChildren(t *testing.T) {
	must := require.New(t)
	c, err := newComposite([]string{"patterns", "none"}, "")
	must.NoError(err)
	cc, ok := c.(*CompositeScanner)
	must.True(ok, "expected *CompositeScanner, got %T", c)
	must.Len(cc.scanners, 2)
}
