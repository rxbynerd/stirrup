package resultsink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/harness/internal/gcs"
	"github.com/rxbynerd/stirrup/types"
)

// remoteRequestTimeout bounds one HTTP round-trip of a remote sink. The
// payload is a single small RunResult, so the bound only has to cover a
// slow TLS handshake or a backend stall.
const remoteRequestTimeout = 30 * time.Second

// defaultRetryDelays are the waits between the remote sinks' delivery
// attempts: three attempts in all. The caller's context bounds the
// whole sequence, so a stalled attempt can use up the budget for the
// ones after it.
var defaultRetryDelays = []time.Duration{500 * time.Millisecond, time.Second}

// newRemoteHTTPClient returns the bounded client the remote sinks use
// when the caller supplies none.
func newRemoteHTTPClient() *http.Client {
	return &http.Client{
		Timeout: remoteRequestTimeout,
		Transport: &http.Transport{
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: remoteRequestTimeout,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

// permanentError marks a delivery failure that another attempt cannot fix.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// statusError is a non-2xx response from the Pub/Sub publish endpoint.
type statusError struct {
	StatusCode int
	Body       string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("pubsub publish returned HTTP %d: %s", e.StatusCode, e.Body)
}

// retryable reports whether another delivery attempt could succeed
// after err. Transport and credential errors are retried; an HTTP status
// is retried only when it signals a transient condition.
func retryable(err error) bool {
	var perm *permanentError
	if errors.As(err, &perm) {
		return false
	}
	var gcsStatus *gcs.UploadStatusError
	if errors.As(err, &gcsStatus) {
		return transientStatus(gcsStatus.StatusCode)
	}
	var status *statusError
	if errors.As(err, &status) {
		// Pub/Sub also retries publish on ABORTED (409) and CANCELLED (499).
		return transientStatus(status.StatusCode) || status.StatusCode == http.StatusConflict || status.StatusCode == 499
	}
	return true
}

func transientStatus(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500
}

// deliver runs attempt until it succeeds, fails permanently, ctx ends,
// or every delay in delays has been waited out. Each failed attempt
// that will be retried is logged; the final error is returned for the
// caller to report.
func deliver(ctx context.Context, sink string, delays []time.Duration, attempt func(context.Context) error) error {
	for i := 0; ; i++ {
		err := attempt(ctx)
		if err == nil {
			return nil
		}
		if i >= len(delays) || !retryable(err) || ctx.Err() != nil {
			return fmt.Errorf("after %d attempt(s): %w", i+1, err)
		}
		slog.WarnContext(ctx, "resultSink: delivery attempt failed; retrying",
			"sink", sink, "attempt", i+1, "err", err)
		timer := time.NewTimer(delays[i])
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("after %d attempt(s): %w", i+1, err)
		case <-timer.C:
		}
	}
}

// resolveBearer resolves source into the bearer closure a request
// authenticates with.
func resolveBearer(ctx context.Context, source credential.Source) (credential.BearerTokenFunc, error) {
	resolved, err := source.Resolve(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve credential: %w", err)
	}
	if resolved == nil || resolved.BearerToken == nil {
		return nil, &permanentError{errors.New("credential source produced no bearer token")}
	}
	return resolved.BearerToken, nil
}

// marshalResult renders result as the JSON document the remote sinks
// deliver: the stdout-json payload without its sentinel, and without the
// stdout line cap, which exists only for log transports.
func marshalResult(result types.RunResult) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(result); err != nil {
		return nil, fmt.Errorf("marshal RunResult: %w", err)
	}
	return buf.Bytes(), nil
}
