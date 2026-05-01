package permission

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// DefaultAskUpstreamTimeout is the default duration to wait for a permission
// response from the control plane before timing out.
const DefaultAskUpstreamTimeout = 60 * time.Second

// Transport is a minimal interface matching the subset of transport.Transport
// needed by AskUpstreamPolicy. Defined locally to avoid a circular import
// between the permission and transport packages.
//
// (The transport package itself does not import permission, so this is
// strictly a hygiene measure: it keeps callers free to substitute a fake
// in tests without depending on the full transport.Transport interface.)
type Transport interface {
	Emit(event types.HarnessEvent) error
	OnControl(handler func(event types.ControlEvent))
}

// AskUpstreamPolicy is a PermissionPolicy that auto-allows tool calls which
// do not require operator approval and forwards approval-required tool
// calls to the control plane via the Transport. It blocks until a
// permission_response control event arrives or the context is cancelled.
//
// The set of approval-required tools is built from the registry by
// collecting tools whose Tool.RequiresApproval flag is true (see
// core.factory.approvalRequiredToolSet).
type AskUpstreamPolicy struct {
	transport Transport

	// Timeout is the maximum duration to wait for a permission response from
	// the control plane. If the control plane does not respond within this
	// window, Check returns an error.
	Timeout time.Duration

	// approvalTools maps tool names that require upstream approval. Tools
	// in this set are forwarded to the control plane; all others are
	// auto-allowed.
	approvalTools map[string]bool

	mu sync.Mutex

	// correlator pairs outbound permission_request events with their
	// corresponding permission_response control events. It owns the
	// pending-channel map and the request-ID counter.
	correlator *transport.Correlator
}

type permissionResponse struct {
	allowed bool
	reason  string
}

// NewAskUpstreamPolicy creates a new AskUpstreamPolicy. The approvalTools
// map keys are tool names whose calls must be approved by the control
// plane; calls to those tools are forwarded as permission_request events
// and block until a permission_response arrives. If timeout is zero,
// DefaultAskUpstreamTimeout (60s) is used.
func NewAskUpstreamPolicy(t Transport, approvalTools map[string]bool, timeout time.Duration) *AskUpstreamPolicy {
	if timeout <= 0 {
		timeout = DefaultAskUpstreamTimeout
	}
	p := &AskUpstreamPolicy{
		transport:     t,
		Timeout:       timeout,
		approvalTools: approvalTools,
		correlator:    transport.NewCorrelator("perm"),
	}
	p.correlator.AttachTo(t, extractPermissionResponse)
	return p
}

// extractPermissionResponse turns a control event into a permissionResponse
// payload, or returns an empty id to ignore unrelated events.
func extractPermissionResponse(event types.ControlEvent) (string, any) {
	if event.Type != "permission_response" {
		return "", nil
	}
	allowed := event.Allowed != nil && *event.Allowed
	return event.RequestID, permissionResponse{
		allowed: allowed,
		reason:  event.Reason,
	}
}

// ApprovalToolNames returns a snapshot of tool names that require upstream
// approval. The returned slice is owned by the caller; modifications do
// not affect the policy. Order is not guaranteed.
func (p *AskUpstreamPolicy) ApprovalToolNames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	names := make([]string, 0, len(p.approvalTools))
	for name := range p.approvalTools {
		names = append(names, name)
	}
	return names
}

// AddApprovalTool registers an additional tool name that must be forwarded
// to the control plane for approval. This is used by the factory to add
// tools that are registered post-loop-construction (e.g. spawn_agent),
// which would otherwise miss the snapshot taken at policy creation time.
// Safe to call concurrently with Check.
func (p *AskUpstreamPolicy) AddApprovalTool(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.approvalTools[name] = true
}

// Check implements PermissionPolicy. Tools that do not require upstream
// approval are auto-allowed. Approval-required tools emit a
// permission_request event via Transport and block until the control plane
// responds or the context is cancelled.
func (p *AskUpstreamPolicy) Check(ctx context.Context, tool types.ToolDefinition, input json.RawMessage) (*PermissionResult, error) {
	p.mu.Lock()
	required := p.approvalTools[tool.Name]
	p.mu.Unlock()
	if !required {
		return &PermissionResult{Allowed: true}, nil
	}

	payload, err := p.correlator.Await(ctx, p.Timeout, func(requestID string) error {
		return p.transport.Emit(types.HarnessEvent{
			Type:      "permission_request",
			RequestID: requestID,
			ToolName:  tool.Name,
			Input:     input,
		})
	})
	if err != nil {
		// Surface a domain-specific error message while preserving the
		// underlying cause (timeout, ctx, emit failure) for callers that
		// inspect the chain via errors.Is.
		return nil, fmt.Errorf("permission check: %w", err)
	}

	resp, ok := payload.(permissionResponse)
	if !ok {
		// Defensive: extractPermissionResponse only ever delivers
		// permissionResponse, so reaching this branch means the
		// correlator was wired with a different extractor than we
		// installed. Treat as a hard error rather than panicking.
		return nil, fmt.Errorf("permission check: unexpected payload type %T", payload)
	}
	return &PermissionResult{
		Allowed: resp.allowed,
		Reason:  resp.reason,
	}, nil
}
