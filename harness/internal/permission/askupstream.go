package permission

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// DefaultAskUpstreamTimeout is the default duration to wait for a permission
// response from the control plane before timing out.
const DefaultAskUpstreamTimeout = 60 * time.Second

// Transport is a minimal interface matching the subset of transport.Transport
// needed by AskUpstreamPolicy. Defined locally to avoid a circular import
// between the permission and transport packages.
type Transport interface {
	Emit(event types.HarnessEvent) error
	OnControl(handler func(event types.ControlEvent))
}

// AskUpstreamPolicy is a PermissionPolicy that auto-allows read-only tools
// and sends side-effecting tool calls to the control plane for approval via
// the Transport. It blocks until a permission_response control event arrives
// or the context is cancelled.
type AskUpstreamPolicy struct {
	transport Transport

	// Timeout is the maximum duration to wait for a permission response from
	// the control plane. If the control plane does not respond within this
	// window, Check returns an error.
	Timeout time.Duration

	// sideEffectingTools maps tool names that have side effects. Tools in
	// this set require upstream approval; all others are auto-allowed.
	sideEffectingTools map[string]bool

	mu       sync.Mutex
	nextID   int
	pending  map[string]chan permissionResponse
	attached bool
}

type permissionResponse struct {
	allowed bool
	reason  string
}

// NewAskUpstreamPolicy creates a new AskUpstreamPolicy. The sideEffectingTools
// map keys are tool names considered to have side effects; calls to those tools
// are forwarded to the control plane for approval. If timeout is zero,
// DefaultAskUpstreamTimeout (60s) is used.
func NewAskUpstreamPolicy(transport Transport, sideEffectingTools map[string]bool, timeout time.Duration) *AskUpstreamPolicy {
	if timeout <= 0 {
		timeout = DefaultAskUpstreamTimeout
	}
	p := &AskUpstreamPolicy{
		transport:          transport,
		Timeout:            timeout,
		sideEffectingTools: sideEffectingTools,
		pending:            make(map[string]chan permissionResponse),
	}
	p.attachHandler()
	return p
}

// attachHandler registers a control event handler on the transport to route
// permission_response events to their waiting Check calls.
func (p *AskUpstreamPolicy) attachHandler() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.attached {
		return
	}
	p.attached = true

	p.transport.OnControl(func(event types.ControlEvent) {
		if event.Type != "permission_response" {
			return
		}
		p.mu.Lock()
		ch, ok := p.pending[event.RequestID]
		if ok {
			delete(p.pending, event.RequestID)
		}
		p.mu.Unlock()

		if ok {
			allowed := event.Allowed != nil && *event.Allowed
			ch <- permissionResponse{allowed: allowed, reason: event.Reason}
		}
	})
}

// Check implements PermissionPolicy. Read-only tools are auto-allowed.
// Side-effecting tools emit a permission_request event via Transport and
// block until the control plane responds or the context is cancelled.
func (p *AskUpstreamPolicy) Check(ctx context.Context, tool types.ToolDefinition, input json.RawMessage) (*PermissionResult, error) {
	if !p.sideEffectingTools[tool.Name] {
		return &PermissionResult{Allowed: true}, nil
	}

	requestID := p.nextRequestID()
	ch := make(chan permissionResponse, 1)

	p.mu.Lock()
	p.pending[requestID] = ch
	p.mu.Unlock()

	err := p.transport.Emit(types.HarnessEvent{
		Type:      "permission_request",
		RequestID: requestID,
		ToolName:  tool.Name,
		Input:     input,
	})
	if err != nil {
		p.mu.Lock()
		delete(p.pending, requestID)
		p.mu.Unlock()
		return nil, fmt.Errorf("emit permission request: %w", err)
	}

	timer := time.NewTimer(p.Timeout)
	defer timer.Stop()

	select {
	case resp := <-ch:
		return &PermissionResult{
			Allowed: resp.allowed,
			Reason:  resp.reason,
		}, nil
	case <-timer.C:
		p.mu.Lock()
		delete(p.pending, requestID)
		p.mu.Unlock()
		return nil, fmt.Errorf("permission check timed out after %s waiting for upstream response", p.Timeout)
	case <-ctx.Done():
		p.mu.Lock()
		delete(p.pending, requestID)
		p.mu.Unlock()
		return nil, fmt.Errorf("permission check cancelled: %w", ctx.Err())
	}
}

func (p *AskUpstreamPolicy) nextRequestID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	return fmt.Sprintf("perm-%d", p.nextID)
}
