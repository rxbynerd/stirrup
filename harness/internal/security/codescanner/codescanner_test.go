package codescanner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func TestNew_NilConfigReturnsNoneScanner(t *testing.T) {
	s, err := New(nil)
	require.NoError(t, err)
	require.IsType(t, NoneScanner{}, s)
}

func TestNew_EmptyTypeReturnsNoneScanner(t *testing.T) {
	s, err := New(&types.CodeScannerConfig{})
	require.NoError(t, err)
	require.IsType(t, NoneScanner{}, s)
}

func TestNew_PatternsTypeReturnsPatternScanner(t *testing.T) {
	s, err := New(&types.CodeScannerConfig{Type: "patterns"})
	require.NoError(t, err)
	require.IsType(t, &PatternScanner{}, s)
}

func TestNew_CompositeRequiresChildren(t *testing.T) {
	_, err := New(&types.CodeScannerConfig{Type: "composite"})
	require.Error(t, err)
}

func TestNew_CompositeRejectsNestedComposite(t *testing.T) {
	_, err := New(&types.CodeScannerConfig{
		Type:     "composite",
		Scanners: []string{"patterns", "composite"},
	})
	require.Error(t, err)
}

func TestNew_UnknownTypeRejected(t *testing.T) {
	_, err := New(&types.CodeScannerConfig{Type: "bogus"})
	require.Error(t, err)
}

func TestNoneScanner_AlwaysEmpty(t *testing.T) {
	s := NoneScanner{}
	res, err := s.Scan(context.Background(), "x.py", []byte("eval('boom')"))
	require.NoError(t, err)
	require.Empty(t, res.Findings)
}

func TestScanResult_HasBlocking(t *testing.T) {
	t.Run("warn findings are not blocking", func(t *testing.T) {
		r := &ScanResult{Findings: []Finding{{Severity: SeverityWarn}}}
		require.False(t, r.HasBlocking())
	})

	t.Run("block finding is blocking", func(t *testing.T) {
		r := &ScanResult{Findings: []Finding{
			{Severity: SeverityWarn},
			{Severity: SeverityBlock},
		}}
		require.True(t, r.HasBlocking())
	})

	t.Run("nil result is not blocking", func(t *testing.T) {
		var r *ScanResult
		require.False(t, r.HasBlocking())
	})
}
