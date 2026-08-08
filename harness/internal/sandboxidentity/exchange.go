// Package sandboxidentity requests a control-plane-issued sandbox identity
// token over the gRPC control stream and composes the non-secret sandbox
// environment that wires it into a git-credential proxy. The exchange
// mirrors permission/askupstream.go's correlation template: emit a request,
// block with a bounded fail-closed timeout on the matching response.
// Operator doc: docs/configuration.md#sandbox-identity-and-git-proxy-wiring.
package sandboxidentity

import (
	"context"
	"fmt"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// DefaultTimeout matches permission.DefaultAskUpstreamTimeout: both wait on
// a control-plane response before proceeding, and both abort the run rather
// than fall back on a timeout or a decline.
const DefaultTimeout = 60 * time.Second

// MaxTokenBytes caps the control-plane-supplied token. The control plane is
// only partially trusted (cf. core/types.go's maxAsyncToolResultBytes), and
// an oversized token fails hard rather than truncating: a truncated JWT is
// not a usable credential, so silently trimming would hide a misbehaving
// control plane behind an auth error.
const MaxTokenBytes = 16 * 1024

// Transport is the minimal surface Exchange needs, declared locally rather
// than depending on transport.Transport (which also requires Close) to keep
// the dependency surface narrow, mirroring permission.Transport.
type Transport interface {
	Emit(event types.HarnessEvent) error
	OnControl(handler func(event types.ControlEvent))
}

// Result carries the outcome of a successful token exchange.
type Result struct {
	// Token is the signed JWT sandbox identity token. SENSITIVE: callers
	// must never log, trace, transcribe, or persist it to RunConfig.
	Token string
	// ExpiresAt is the token's optional Unix-seconds expiry as reported by
	// the control plane.
	ExpiresAt *int64
}

// tokenResponse exists so Exchange never handles a types.ControlEvent
// directly beyond extractTokenResponse's single destructuring point: the raw
// event must never reach a log call or a %v/%+v verb, which would echo Token.
type tokenResponse struct {
	token     string
	expiresAt *int64
	isError   bool
	reason    string
}

// extractTokenResponse turns a control event into a tokenResponse payload,
// or returns an empty id to ignore unrelated events. This is the ONLY place
// permitted to read event.Token.
func extractTokenResponse(event types.ControlEvent) (string, any) {
	if event.Type != "sandbox_token_response" {
		return "", nil
	}
	return event.RequestID, tokenResponse{
		token:     event.Token,
		expiresAt: event.ExpiresAt,
		isError:   event.IsError != nil && *event.IsError,
		reason:    event.Reason,
	}
}

// Exchange requests a sandbox identity token and blocks until the matching
// response arrives, timeout elapses (DefaultTimeout when non-positive), or
// ctx is cancelled. Every non-success outcome — timeout, decline, empty or
// oversized token — returns an error and a zero Result, never a partial
// credential; callers must abort sandbox creation on any error.
//
// A fresh Correlator is minted per call, so at most one Exchange may be in
// flight per t (the sole call site invokes it once per Transport).
func Exchange(ctx context.Context, t Transport, audience string, timeout time.Duration) (Result, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	correlator := transport.NewCorrelator("sbid")
	correlator.AttachTo(t, extractTokenResponse)

	payload, err := correlator.Await(ctx, timeout, func(requestID string) error {
		return t.Emit(types.HarnessEvent{
			Type:      "sandbox_token_request",
			RequestID: requestID,
			Audience:  audience,
		})
	})
	if err != nil {
		return Result{}, fmt.Errorf("sandbox identity token exchange: %w", err)
	}

	resp, ok := payload.(tokenResponse)
	if !ok {
		// Unreachable unless the correlator was wired with an extractor
		// other than the one installed above.
		return Result{}, fmt.Errorf("sandbox identity token exchange: unexpected payload type %T", payload)
	}

	if resp.isError {
		return Result{}, fmt.Errorf("sandbox identity token exchange declined by control plane: %s", resp.reason)
	}
	if resp.token == "" {
		return Result{}, fmt.Errorf("sandbox identity token exchange: control plane returned an empty token")
	}
	if len(resp.token) > MaxTokenBytes {
		// Length only, never content.
		return Result{}, fmt.Errorf("sandbox identity token exchange: token exceeds %d byte cap (got %d bytes)", MaxTokenBytes, len(resp.token))
	}

	return Result{Token: resp.token, ExpiresAt: resp.expiresAt}, nil
}
