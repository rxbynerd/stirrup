package provider

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"syscall"

	"github.com/rxbynerd/stirrup/types"
)

// classifyStatusError marks err, built from a terminal non-2xx pre-stream
// response, with its HTTP status, and as retryable for the agentic loop's
// turn retry when the status is one DoWithRetry would retry and the body
// names no billing or quota limit. Authentication failures and other 4xx
// request errors stay non-retryable.
func classifyStatusError(err error, status int, body []byte) error {
	err = types.MarkHTTPStatus(err, status)
	if !retryableStatus(status) {
		return err
	}
	if status == http.StatusTooManyRequests {
		if _, exhausted := quotaExhaustedBody(body); exhausted {
			return err
		}
	}
	return types.MarkRetryable(err)
}

// IsRequestRejected reports whether err is a terminal 4xx response other
// than 429: the provider refused the request as sent, so re-sending the
// same body cannot succeed, while a request without an optional field
// such as an output schema might.
func IsRequestRejected(err error) bool {
	status, ok := types.HTTPStatus(err)
	return ok && status >= 400 && status < 500 && status != http.StatusTooManyRequests
}

// classifyTransportError marks err as retryable when it is a transport
// fault a fresh request may not hit: an idle stream, a reset or broken
// connection, a truncated body, or a network timeout. Context
// cancellation is never retryable.
func classifyTransportError(err error) error {
	if retryableTransportErr(err) {
		return types.MarkRetryable(err)
	}
	return err
}

func retryableTransportErr(err error) bool {
	switch {
	case err == nil,
		errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return false
	case errors.Is(err, errStreamIdle),
		errors.Is(err, syscall.ECONNRESET),
		errors.Is(err, syscall.EPIPE),
		errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, io.EOF):
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
