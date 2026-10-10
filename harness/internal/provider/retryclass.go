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
// response, as retryable for the agentic loop's turn retry when the
// status is one DoWithRetry would retry and the body names no billing or
// quota limit. Authentication failures and other 4xx request errors stay
// non-retryable.
func classifyStatusError(err error, status int, body []byte) error {
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
