package verifier

import (
	"context"
	"fmt"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// stubVerifier is a configurable Verifier for testing the composite.
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

// Verify CompositeVerifier satisfies the Verifier interface at compile time.
var _ Verifier = (*CompositeVerifier)(nil)

func TestCompositeVerifier_AllPass(t *testing.T) {
	v1 := passingStub(map[string]any{"tool": "lint"})
	v2 := passingStub(map[string]any{"tool": "test"})
	v3 := passingStub(map[string]any{"tool": "typecheck"})

	c := NewCompositeVerifier([]Verifier{v1, v2, v3})
	result, err := c.Verify(context.Background(), VerifyContext{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Passed {
		t.Fatal("expected composite to pass when all sub-verifiers pass")
	}
	if result.Feedback != "" {
		t.Errorf("expected no feedback, got %q", result.Feedback)
	}
	// Details from all three should be present, keyed by index.
	for _, key := range []string{"verifier_0", "verifier_1", "verifier_2"} {
		if _, ok := result.Details[key]; !ok {
			t.Errorf("expected details key %q to be present", key)
		}
	}
	if !v1.called || !v2.called || !v3.called {
		t.Error("expected all sub-verifiers to be called")
	}
}

func TestCompositeVerifier_FirstFails(t *testing.T) {
	v1 := failingStub("lint errors found", map[string]any{"tool": "lint"})
	v2 := passingStub(map[string]any{"tool": "test"})

	c := NewCompositeVerifier([]Verifier{v1, v2})
	result, err := c.Verify(context.Background(), VerifyContext{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Passed {
		t.Fatal("expected composite to fail")
	}
	if result.Feedback != "lint errors found" {
		t.Errorf("expected failure feedback, got %q", result.Feedback)
	}
	// Second verifier should not have been called (short-circuit).
	if v2.called {
		t.Error("expected short-circuit: second verifier should not have been called")
	}
}

func TestCompositeVerifier_MiddleFails(t *testing.T) {
	v1 := passingStub(map[string]any{"tool": "lint"})
	v2 := failingStub("test failures", map[string]any{"tool": "test", "failures": 3})
	v3 := passingStub(map[string]any{"tool": "typecheck"})

	c := NewCompositeVerifier([]Verifier{v1, v2, v3})
	result, err := c.Verify(context.Background(), VerifyContext{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Passed {
		t.Fatal("expected composite to fail")
	}
	if result.Feedback != "test failures" {
		t.Errorf("expected failure feedback, got %q", result.Feedback)
	}
	// First verifier's details should be preserved.
	if _, ok := result.Details["verifier_0"]; !ok {
		t.Error("expected details from first (passing) verifier to be preserved")
	}
	// Failing verifier's details should also be present.
	if _, ok := result.Details["verifier_1"]; !ok {
		t.Error("expected details from failing verifier to be present")
	}
	// Third verifier should not have been called.
	if v3.called {
		t.Error("expected short-circuit: third verifier should not have been called")
	}
}

func TestCompositeVerifier_EmptyList(t *testing.T) {
	c := NewCompositeVerifier([]Verifier{})
	result, err := c.Verify(context.Background(), VerifyContext{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Passed {
		t.Fatal("expected empty composite to pass (vacuous truth)")
	}
}

func TestCompositeVerifier_ErrorPropagated(t *testing.T) {
	v1 := passingStub(map[string]any{"tool": "lint"})
	v2 := errorStub(fmt.Errorf("network timeout"))
	v3 := passingStub(nil)

	c := NewCompositeVerifier([]Verifier{v1, v2, v3})
	_, err := c.Verify(context.Background(), VerifyContext{})

	if err == nil {
		t.Fatal("expected error to be propagated")
	}
	if got := err.Error(); got != "composite verifier [1]: network timeout" {
		t.Errorf("unexpected error message: %q", got)
	}
	if v3.called {
		t.Error("expected short-circuit on error: third verifier should not have been called")
	}
}

func TestCompositeVerifier_SingleVerifier(t *testing.T) {
	// A composite with one verifier should behave identically to that verifier.
	inner := passingStub(map[string]any{"exitCode": 0})

	c := NewCompositeVerifier([]Verifier{inner})
	result, err := c.Verify(context.Background(), VerifyContext{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Passed {
		t.Fatal("expected single-verifier composite to pass")
	}
	if _, ok := result.Details["verifier_0"]; !ok {
		t.Error("expected details from the single verifier")
	}
}

func TestCompositeVerifier_SingleVerifierFailing(t *testing.T) {
	inner := failingStub("compilation failed", map[string]any{"exitCode": 1})

	c := NewCompositeVerifier([]Verifier{inner})
	result, err := c.Verify(context.Background(), VerifyContext{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Passed {
		t.Fatal("expected single-verifier composite to fail")
	}
	if result.Feedback != "compilation failed" {
		t.Errorf("expected feedback from inner verifier, got %q", result.Feedback)
	}
}

func TestCompositeVerifier_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// First verifier passes but cancels the context as a side effect.
	v1 := &stubVerifier{
		result: &types.VerificationResult{Passed: true},
	}
	v2 := passingStub(nil)

	c := NewCompositeVerifier([]Verifier{v1, v2})

	// Cancel before the second verifier gets a chance to run.
	cancel()

	_, err := c.Verify(ctx, VerifyContext{})

	// The context was already cancelled before the first verifier ran,
	// so the composite should detect it before invoking the first verifier.
	if err == nil {
		t.Fatal("expected error due to context cancellation")
	}
	if ctx.Err() == nil {
		t.Fatal("expected context to be cancelled")
	}
}

func TestCompositeVerifier_NilDetails(t *testing.T) {
	// Verifier that passes with nil details should not add a key to merged.
	v1 := passingStub(nil)
	v2 := passingStub(map[string]any{"tool": "test"})

	c := NewCompositeVerifier([]Verifier{v1, v2})
	result, err := c.Verify(context.Background(), VerifyContext{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Passed {
		t.Fatal("expected composite to pass")
	}
	// v1 had nil details, so verifier_0 should not appear.
	if _, ok := result.Details["verifier_0"]; ok {
		t.Error("expected nil-details verifier to not add a key")
	}
	if _, ok := result.Details["verifier_1"]; !ok {
		t.Error("expected verifier_1 details to be present")
	}
}
