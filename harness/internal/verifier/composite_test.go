package verifier

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

type stubVerifier struct {
	result *types.VerificationResult
	err    error
	called bool
}

func (s *stubVerifier) Verify(_ context.Context, _ VerifyContext) (*types.VerificationResult, error) {
	s.called = true
	return s.result, s.err
}

func passingStub(details map[string]any) *stubVerifier {
	return &stubVerifier{
		result: &types.VerificationResult{
			Passed:  true,
			Details: details,
		},
	}
}

func failingStub(feedback string, details map[string]any) *stubVerifier {
	return &stubVerifier{
		result: &types.VerificationResult{
			Passed:   false,
			Feedback: feedback,
			Details:  details,
		},
	}
}

func errorStub(err error) *stubVerifier {
	return &stubVerifier{err: err}
}

var _ Verifier = (*CompositeVerifier)(nil)

func TestCompositeVerifier_PassesWhenAllVerifiersPass(t *testing.T) {
	must := require.New(t)
	v1 := passingStub(map[string]any{"tool": "lint"})
	v2 := passingStub(map[string]any{"tool": "test"})
	v3 := passingStub(map[string]any{"tool": "typecheck"})

	c := NewCompositeVerifier([]Verifier{v1, v2, v3})
	result, err := c.Verify(context.Background(), VerifyContext{})

	must.NoError(err)
	must.True(result.Passed)
	must.Empty(result.Feedback)
	// Details from all three verifiers are kept, keyed by index.
	must.Contains(result.Details, "verifier_0")
	must.Contains(result.Details, "verifier_1")
	must.Contains(result.Details, "verifier_2")
	for i, v := range []*stubVerifier{v1, v2, v3} {
		must.True(v.called, "sub-verifier %d should be called", i)
	}
}

func TestCompositeVerifier_StopsAtFirstFailure(t *testing.T) {
	must := require.New(t)
	v1 := failingStub("lint errors found", map[string]any{"tool": "lint"})
	v2 := passingStub(map[string]any{"tool": "test"})

	c := NewCompositeVerifier([]Verifier{v1, v2})
	result, err := c.Verify(context.Background(), VerifyContext{})

	must.NoError(err)
	must.False(result.Passed)
	must.Equal("lint errors found", result.Feedback)
	must.False(v2.called, "second verifier should be skipped after a failure")
}

func TestCompositeVerifier_MiddleFailureKeepsDetailsAndStops(t *testing.T) {
	must := require.New(t)
	v1 := passingStub(map[string]any{"tool": "lint"})
	v2 := failingStub("test failures", map[string]any{"tool": "test", "failures": 3})
	v3 := passingStub(map[string]any{"tool": "typecheck"})

	c := NewCompositeVerifier([]Verifier{v1, v2, v3})
	result, err := c.Verify(context.Background(), VerifyContext{})

	must.NoError(err)
	must.False(result.Passed)
	must.Equal("test failures", result.Feedback)
	// Details from the passing first verifier and the failing second one are both kept.
	must.Contains(result.Details, "verifier_0")
	must.Contains(result.Details, "verifier_1")
	must.False(v3.called, "third verifier should be skipped after a failure")
}

func TestCompositeVerifier_EmptyListPasses(t *testing.T) {
	must := require.New(t)
	c := NewCompositeVerifier([]Verifier{})
	result, err := c.Verify(context.Background(), VerifyContext{})

	must.NoError(err)
	must.True(result.Passed, "an empty composite passes vacuously")
}

func TestCompositeVerifier_ErrorStopsAndPropagates(t *testing.T) {
	must := require.New(t)
	v1 := passingStub(map[string]any{"tool": "lint"})
	v2 := errorStub(fmt.Errorf("network timeout"))
	v3 := passingStub(nil)

	c := NewCompositeVerifier([]Verifier{v1, v2, v3})
	_, err := c.Verify(context.Background(), VerifyContext{})

	must.EqualError(err, "composite verifier [1]: network timeout")
	must.False(v3.called, "third verifier should be skipped after an error")
}

func TestCompositeVerifier_SinglePassingVerifierPasses(t *testing.T) {
	must := require.New(t)
	// A composite with one verifier should behave identically to that verifier.
	inner := passingStub(map[string]any{"exitCode": 0})

	c := NewCompositeVerifier([]Verifier{inner})
	result, err := c.Verify(context.Background(), VerifyContext{})

	must.NoError(err)
	must.True(result.Passed)
	must.Contains(result.Details, "verifier_0")
}

func TestCompositeVerifier_SingleFailingVerifierFails(t *testing.T) {
	must := require.New(t)
	inner := failingStub("compilation failed", map[string]any{"exitCode": 1})

	c := NewCompositeVerifier([]Verifier{inner})
	result, err := c.Verify(context.Background(), VerifyContext{})

	must.NoError(err)
	must.False(result.Passed)
	must.Equal("compilation failed", result.Feedback)
}

func TestCompositeVerifier_CancelledContextReturnsError(t *testing.T) {
	must := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	v1 := &stubVerifier{
		result: &types.VerificationResult{Passed: true},
	}
	v2 := passingStub(nil)

	c := NewCompositeVerifier([]Verifier{v1, v2})
	_, err := c.Verify(ctx, VerifyContext{})

	must.Error(err)
	must.Error(ctx.Err(), "the context should be cancelled")
}

func TestCompositeVerifier_NilDetailsAddNoKey(t *testing.T) {
	must := require.New(t)
	// A verifier with nil details adds no key to the merged details.
	v1 := passingStub(nil)
	v2 := passingStub(map[string]any{"tool": "test"})

	c := NewCompositeVerifier([]Verifier{v1, v2})
	result, err := c.Verify(context.Background(), VerifyContext{})

	must.NoError(err)
	must.True(result.Passed)
	must.NotContains(result.Details, "verifier_0")
	must.Contains(result.Details, "verifier_1")
}
