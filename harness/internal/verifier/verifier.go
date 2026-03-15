// Package verifier defines the Verifier interface and implementations for
// checking the outcome of a harness run.
package verifier

import (
	"context"

	"github.com/rubynerd/stirrup/types"
)

// VerifyContext provides the inputs a verifier needs to assess a run's result.
type VerifyContext struct {
	Mode      string
	Executor  any // use any to avoid circular dependencies
	Messages  []types.Message
	Artifacts []types.Artifact
}

// Verifier checks whether a run's output meets the task requirements.
type Verifier interface {
	Verify(ctx context.Context, vc VerifyContext) (*types.VerificationResult, error)
}
