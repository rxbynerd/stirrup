package codescanner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func TestNew_NilConfigReturnsNoneScanner(t *testing.T) {
	must := require.New(t)
	s, err := New(nil)
	must.NoError(err)
	must.IsType(NoneScanner{}, s)
}

func TestNew_EmptyTypeReturnsNoneScanner(t *testing.T) {
	must := require.New(t)
	s, err := New(&types.CodeScannerConfig{})
	must.NoError(err)
	must.IsType(NoneScanner{}, s)
}

func TestNew_PatternsTypeReturnsPatternScanner(t *testing.T) {
	must := require.New(t)
	s, err := New(&types.CodeScannerConfig{Type: "patterns"})
	must.NoError(err)
	must.IsType(&PatternScanner{}, s)
}

func TestNew_CompositeRequiresChildren(t *testing.T) {
	must := require.New(t)
	_, err := New(&types.CodeScannerConfig{Type: "composite"})
	must.Error(err)
}

func TestNew_CompositeRejectsNestedComposite(t *testing.T) {
	must := require.New(t)
	_, err := New(&types.CodeScannerConfig{
		Type:     "composite",
		Scanners: []string{"patterns", "composite"},
	})
	must.Error(err)
}

func TestNew_UnknownTypeRejected(t *testing.T) {
	must := require.New(t)
	_, err := New(&types.CodeScannerConfig{Type: "bogus"})
	must.Error(err)
}

func TestNoneScanner_AlwaysEmpty(t *testing.T) {
	must := require.New(t)
	s := NoneScanner{}
	res, err := s.Scan(context.Background(), "x.py", []byte("eval('boom')"))
	must.NoError(err)
	must.Empty(res.Findings)
}

func TestScanResult_HasBlocking(t *testing.T) {
	t.Run("warn findings are not blocking", func(t *testing.T) {
		must := require.New(t)
		r := &ScanResult{Findings: []Finding{{Severity: SeverityWarn}}}
		must.False(r.HasBlocking())
	})

	t.Run("block finding is blocking", func(t *testing.T) {
		must := require.New(t)
		r := &ScanResult{Findings: []Finding{
			{Severity: SeverityWarn},
			{Severity: SeverityBlock},
		}}
		must.True(r.HasBlocking())
	})

	t.Run("nil result is not blocking", func(t *testing.T) {
		must := require.New(t)
		var r *ScanResult
		must.False(r.HasBlocking())
	})
}
