package verifier

import (
	"context"
	"fmt"

	"github.com/rxbynerd/stirrup/types"
)

// CompositeVerifier chains multiple verifiers. All must pass for the composite
// to pass. It short-circuits on the first failure or error for efficiency,
// while collecting details from all passing verifiers into the result.
type CompositeVerifier struct {
	verifiers []Verifier
}

// NewCompositeVerifier creates a composite that runs verifiers in order.
// An empty list is valid and produces a vacuously passing result.
func NewCompositeVerifier(verifiers []Verifier) *CompositeVerifier {
	return &CompositeVerifier{verifiers: verifiers}
}

// Verify runs each sub-verifier in sequence. If any returns an error, that
// error is propagated immediately. If any fails verification, its feedback is
// returned along with details collected from preceding passes. If all pass,
// the composite passes with merged details.
func (c *CompositeVerifier) Verify(ctx context.Context, vc VerifyContext) (*types.VerificationResult, error) {
	merged := make(map[string]any)

	for i, v := range c.verifiers {
		// Respect context cancellation between sub-verifiers.
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("composite verifier: %w", err)
		}

		result, err := v.Verify(ctx, vc)
		if err != nil {
			return nil, fmt.Errorf("composite verifier [%d]: %w", i, err)
		}

		// Merge this verifier's details under a namespaced key.
		key := fmt.Sprintf("verifier_%d", i)
		if result.Details != nil {
			merged[key] = result.Details
		}

		if !result.Passed {
			return &types.VerificationResult{
				Passed:   false,
				Feedback: result.Feedback,
				Details:  merged,
			}, nil
		}
	}

	return &types.VerificationResult{
		Passed:  true,
		Details: merged,
	}, nil
}
