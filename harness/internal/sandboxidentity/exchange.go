// Package sandboxidentity requests a control-plane-issued sandbox identity
// token over the gRPC control stream (issue #516 Part A) and composes the
// non-secret sandbox environment that wires it into a git-credential proxy
// such as haybale (issue #516 Part B). The token-exchange helper mirrors
// harness/internal/permission/askupstream.go's correlation template: emit a
// request, block with a bounded fail-closed timeout on the matching
// response.
package sandboxidentity

import (
	"context"
	"fmt"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// DefaultTimeout is the default duration to wait for a sandbox_token_response
// from the control plane before timing out. Matches
// permission.DefaultAskUpstreamTimeout for consistency: both are "wait for a
// control-plane response before proceeding" timeouts, and the run must abort
// before sandbox creation on either the timeout or an explicit decline —
// there is no partial-credential fallback.
const DefaultTimeout = 60 * time.Second

// MaxTokenBytes caps the length of the control-plane-supplied token. The
// control plane is partially trusted (see harness/internal/core/types.go's
// maxAsyncToolResultBytes precedent for control-plane-supplied content); an
// oversized token is treated as a hard failure rather than truncated, since a
// truncated JWT is not a usable credential and silently swallowing the
// excess would hide a misbehaving or compromised control plane.
const MaxTokenBytes = 16 * 1024

// Transport is the minimal transport surface Exchange needs. Declared
// locally (rather than depending on transport.Transport's full interface,
// which also requires Close) to keep the package's dependency surface
// narrow and testable, mirroring permission.Transport.
type Transport interface {
	Emit(event types.HarnessEvent) error
	OnControl(handler func(event types.ControlEvent))
}

// Result carries the outcome of a successful token exchange.
type Result struct {
	// Token is the signed JWT sandbox identity token. SENSITIVE: callers
	// must never log, trace, transcribe, or persist it to RunConfig.
	Token string
	// ExpiresAt is the token's optional Unix-seconds expiry, as reported by
	// the control plane. Nil when the control plane did not report one.
	ExpiresAt *int64
}

// tokenResponse is the payload extractTokenResponse delivers through the
// correlator. It exists so Exchange never handles a *types.ControlEvent
// directly beyond the single destructuring point in extractTokenResponse —
// CF-1 (issue #516 carry-forward): the raw ControlEvent must never reach a
// log call or a %v/%+v format verb, since that would echo Token.
type tokenResponse struct {
	token     string
	expiresAt *int64
	isError   bool
	reason    string
}

// extractTokenResponse turns a control event into a tokenResponse payload,
// or returns an empty id to ignore unrelated events. This is the ONLY place
// permitted to read event.Token — every other function in this package
// receives the already-destructured tokenResponse or Result.
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

// Exchange requests a sandbox identity token from the control plane
// (HarnessEvent{Type: "sandbox_token_request"}) and blocks until the
// matching sandbox_token_response ControlEvent arrives, timeout elapses, or
// ctx is cancelled. Fail-closed on every non-success outcome: a timeout, a
// control-plane decline (ControlEvent.IsError), an oversized token, or an
// empty token all return an error and a zero Result, with no partial
// credential ever surfaced to the caller. Callers (the factory) must abort
// sandbox creation on any returned error.
//
// If timeout is non-positive, DefaultTimeout (60s) is used.
//
// audience is the intended JWT "aud" claim (RunConfig
// executor.sandboxIdentity.audience); it is informational to the control
// plane, which may override it.
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
		// Defensive: extractTokenResponse only ever delivers tokenResponse,
		// so reaching this branch means the correlator was wired with a
		// different extractor than installed above.
		return Result{}, fmt.Errorf("sandbox identity token exchange: unexpected payload type %T", payload)
	}

	if resp.isError {
		return Result{}, fmt.Errorf("sandbox identity token exchange declined by control plane: %s", resp.reason)
	}
	if resp.token == "" {
		return Result{}, fmt.Errorf("sandbox identity token exchange: control plane returned an empty token")
	}
	if len(resp.token) > MaxTokenBytes {
		// Never include the token's content, only its length — a truncated
		// JWT is not a usable credential, so this fails closed rather than
		// truncating-and-using.
		return Result{}, fmt.Errorf("sandbox identity token exchange: token exceeds %d byte cap (got %d bytes)", MaxTokenBytes, len(resp.token))
	}

	return Result{Token: resp.token, ExpiresAt: resp.expiresAt}, nil
}
