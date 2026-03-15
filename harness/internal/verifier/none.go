package verifier

import (
	"context"

	"github.com/rubynerd/stirrup/types"
)

// NoneVerifier is a Verifier that always reports success. Use it when no
// verification step is needed (e.g. research or planning modes).
type NoneVerifier struct{}

// NewNoneVerifier returns a new NoneVerifier.
func NewNoneVerifier() *NoneVerifier {
	return &NoneVerifier{}
}

// Verify always returns a passing result.
func (n *NoneVerifier) Verify(_ context.Context, _ VerifyContext) (*types.VerificationResult, error) {
	return &types.VerificationResult{Passed: true}, nil
}
