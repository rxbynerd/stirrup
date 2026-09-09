// Package sandboxidentity requests a control-plane-issued sandbox identity
// token over the gRPC control stream and composes the non-secret sandbox
// environment that wires it into a git-credential proxy. The exchange
// mirrors permission/askupstream.go's correlation template: emit a request,
// block with a bounded fail-closed timeout on the matching response.
// Operator doc: docs/configuration.md#sandbox-identity-and-git-proxy-wiring.
package sandboxidentity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
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

// maxReasonRunes bounds the control-plane-supplied decline reason before it
// is interpolated into an error that reaches a warning event and the log.
const maxReasonRunes = 200

// Exchange outcomes. The transient pair means the control plane was not
// reached or did not answer in time; every other sentinel means it
// answered with something the harness must not use, so retrying would
// only repeat the answer.
var (
	ErrDeclined       = errors.New("sandbox identity token exchange declined by control plane")
	ErrTimeout        = errors.New("sandbox identity token exchange timed out")
	ErrTransport      = errors.New("sandbox identity token request could not be sent")
	ErrEmptyToken     = errors.New("sandbox identity token exchange: control plane returned an empty token")
	ErrTokenTooLarge  = errors.New("sandbox identity token exchange: token exceeds the byte cap")
	ErrMalformedToken = errors.New("sandbox identity token exchange: token contains characters outside printable ASCII")
)

// IsTransient reports whether err is an exchange failure worth retrying
// while the current token still has lifetime left.
func IsTransient(err error) bool {
	return errors.Is(err, ErrTimeout) || errors.Is(err, ErrTransport)
}

// validToken accepts printable ASCII only. The token is echoed verbatim
// into git's line-oriented credential-helper protocol, so a newline or
// carriage return would let a control plane append helper directives of
// its own; a JWT is base64url and dots, so the intended issuer loses
// nothing.
func validToken(token string) bool {
	for i := 0; i < len(token); i++ {
		if token[i] < 0x20 || token[i] > 0x7e {
			return false
		}
	}
	return true
}

// sanitizeReason drops control characters and caps the length of a
// control-plane-supplied reason so it cannot smuggle multi-line or
// oversized content into a warning event or a log line.
func sanitizeReason(reason string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, reason)
	if runes := []rune(cleaned); len(runes) > maxReasonRunes {
		return string(runes[:maxReasonRunes]) + "…"
	}
	return cleaned
}

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

// Exchanger issues sandbox_token_requests over one transport. One
// correlator is attached at construction, so the initial exchange and every
// refresh share a single control handler and draw request IDs from one
// monotonic sequence (sbid-1, sbid-2, ...). The factory performs the initial
// exchange and the Refresher the rest, strictly in sequence.
type Exchanger struct {
	t          Transport
	correlator *transport.Correlator
	requests   atomic.Int64
}

// NewExchanger attaches a correlator for sandbox_token_response events to t.
func NewExchanger(t Transport) *Exchanger {
	c := transport.NewCorrelator("sbid")
	c.AttachTo(t, extractTokenResponse)
	return &Exchanger{t: t, correlator: c}
}

// Requests reports how many sandbox_token_requests reached the transport.
func (e *Exchanger) Requests() int {
	return int(e.requests.Load())
}

// Exchange is the one-shot form of Exchanger.Exchange for callers that need
// a single token over t.
func Exchange(ctx context.Context, t Transport, audience string, timeout time.Duration) (Result, error) {
	return NewExchanger(t).Exchange(ctx, audience, timeout)
}

// Exchange requests a sandbox identity token and blocks until the matching
// response arrives, timeout elapses (DefaultTimeout when non-positive), or
// ctx is cancelled. Every non-success outcome — timeout, decline, empty or
// oversized token — returns an error and a zero Result, never a partial
// credential; callers must abort sandbox creation on any error.
func (e *Exchanger) Exchange(ctx context.Context, audience string, timeout time.Duration) (Result, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	payload, err := e.correlator.Await(ctx, timeout, func(requestID string) error {
		if err := e.t.Emit(types.HarnessEvent{
			Type:      "sandbox_token_request",
			RequestID: requestID,
			Audience:  audience,
		}); err != nil {
			return err
		}
		e.requests.Add(1)
		return nil
	})
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return Result{}, fmt.Errorf("sandbox identity token exchange: %w", err)
		case errors.Is(err, transport.ErrAwaitTimeout):
			return Result{}, fmt.Errorf("%w: %v", ErrTimeout, err)
		default:
			return Result{}, fmt.Errorf("%w: %v", ErrTransport, err)
		}
	}

	resp, ok := payload.(tokenResponse)
	if !ok {
		// Unreachable unless the correlator was wired with an extractor
		// other than the one installed above.
		return Result{}, fmt.Errorf("sandbox identity token exchange: unexpected payload type %T", payload)
	}

	if resp.isError {
		return Result{}, fmt.Errorf("%w: %s", ErrDeclined, sanitizeReason(resp.reason))
	}
	if resp.token == "" {
		return Result{}, ErrEmptyToken
	}
	if len(resp.token) > MaxTokenBytes {
		// Length only, never content.
		return Result{}, fmt.Errorf("%w: %d bytes over %d", ErrTokenTooLarge, len(resp.token), MaxTokenBytes)
	}
	if !validToken(resp.token) {
		return Result{}, ErrMalformedToken
	}

	return Result{Token: resp.token, ExpiresAt: resp.expiresAt}, nil
}
