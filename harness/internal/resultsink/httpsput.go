package resultsink

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/httpsput"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// HTTPSPutSink uploads the RunResult as one JSON document with an HTTP
// PUT to the URL a types.HTTPSPutConfig resolves to. The destination
// names a single object, so the sink cannot give each run its own the
// way GCSSink does; whether a second upload replaces the first is the
// destination's decision.
type HTTPSPutSink struct {
	target      *types.HTTPSPutConfig
	secrets     security.SecretStore
	httpClient  *http.Client
	retryDelays []time.Duration
}

// HTTPSPutSinkOptions configures an HTTPSPutSink. Target and Secrets are
// required.
type HTTPSPutSinkOptions struct {
	Target  *types.HTTPSPutConfig
	Secrets security.SecretStore

	// HTTPClient is optional; a client bounded per request is
	// constructed when nil.
	HTTPClient *http.Client
}

// NewHTTPSPutSink validates opts and constructs the sink. The
// destination is resolved in Emit, so each run uploads to what the
// reference holds when that run ends.
func NewHTTPSPutSink(opts HTTPSPutSinkOptions) (*HTTPSPutSink, error) {
	if opts.Target == nil {
		return nil, errors.New("https-put result sink: httpsPut is required")
	}
	if opts.Secrets == nil {
		return nil, errors.New("https-put result sink: a secret store is required")
	}
	client := opts.HTTPClient
	if client == nil {
		client = httpsput.NewClient(remoteRequestTimeout)
	}
	return &HTTPSPutSink{
		target:      opts.Target,
		secrets:     opts.Secrets,
		httpClient:  client,
		retryDelays: defaultRetryDelays,
	}, nil
}

// Emit uploads result, retrying transient failures. A nil return means
// the destination answered 2xx. No error it returns carries the URL.
func (s *HTTPSPutSink) Emit(ctx context.Context, result types.RunResult) error {
	body, err := marshalResult(result)
	if err != nil {
		return err
	}
	// A reference that does not resolve to a usable URL is a
	// configuration fault another attempt cannot fix.
	target, err := httpsput.Resolve(ctx, s.secrets, s.target)
	if err != nil {
		return fmt.Errorf("https-put result sink: %w", err)
	}

	// mayHaveWritten is set once a sent request fails without a
	// response, so it could have stored the object before failing.
	mayHaveWritten := false
	err = deliver(ctx, "https-put", s.retryDelays, func(ctx context.Context) error {
		err := httpsput.Put(ctx, s.httpClient, target, body, "application/json")
		var status *httpsput.StatusError
		switch {
		case err == nil:
			return nil
		case errors.As(err, &status) && status.StatusCode == http.StatusPreconditionFailed && mayHaveWritten:
			return &permanentError{fmt.Errorf("the destination refused to replace an object after an interrupted attempt; it is probably this run's result, but that cannot be confirmed: %w", err)}
		case !errors.As(err, &status) && !isDialError(err):
			mayHaveWritten = true
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("https-put result sink: %w", err)
	}
	slog.InfoContext(ctx, "resultSink: uploaded RunResult", "sink", "https-put", "destination", target.Origin(), "bytes", len(body))
	return nil
}
