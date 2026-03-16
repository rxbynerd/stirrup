// Package provider defines the ProviderAdapter interface and implementations
// for streaming model responses from LLM providers.
package provider

import (
	"context"

	"github.com/rxbynerd/stirrup/types"
)

// ProviderAdapter streams model responses for a given set of parameters.
type ProviderAdapter interface {
	Stream(ctx context.Context, params types.StreamParams) (<-chan types.StreamEvent, error)
}
