package permission

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// TestAskUpstream_InputRefusedByTransport_DeniesWithoutError: when the
// transport refuses a permission_request because its input is over the
// bound, the call is denied outright rather than failing with an error or
// being approved on a partial view of its input.
func TestAskUpstream_InputRefusedByTransport_DeniesWithoutError(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	mt := &mockTransport{emitErr: fmt.Errorf("%w: permission_request input is 300000 bytes", transport.ErrRequestInputTooLarge)}
	policy := NewAskUpstreamPolicy(mt, sideEffectingSet(), 0)

	result, err := policy.Check(context.Background(), types.ToolDefinition{Name: "write_file"}, []byte(`{}`))
	must.NoError(err)
	must.NotNil(result)
	check.False(result.Allowed)
	check.Equal(inputTooLargeForApprovalReason, result.Reason)
	check.Contains(result.Reason, fmt.Sprintf("%d-byte limit", transport.MaxRequestInputBytes))
	check.Zero(policy.correlator.PendingCount())
}

// TestAskUpstream_InputWithheldByTransport_DeniesWithoutError: when the
// secret scrub leaves the control plane only a placeholder for the input,
// the call is denied rather than sent for a blind approval.
func TestAskUpstream_InputWithheldByTransport_DeniesWithoutError(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	mt := &mockTransport{emitErr: fmt.Errorf("%w: permission_request", transport.ErrRequestInputWithheld)}
	policy := NewAskUpstreamPolicy(mt, sideEffectingSet(), 0)

	result, err := policy.Check(context.Background(), types.ToolDefinition{Name: "write_file"}, []byte(`{}`))
	must.NoError(err)
	must.NotNil(result)
	check.False(result.Allowed)
	check.Equal(inputWithheldFromApprovalReason, result.Reason)
	check.Zero(policy.correlator.PendingCount())
}
