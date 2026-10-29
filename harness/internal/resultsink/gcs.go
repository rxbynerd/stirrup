package resultsink

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/harness/internal/gcs"
	"github.com/rxbynerd/stirrup/types"
)

// GCSSink writes the RunResult as one JSON object at
// gs://{bucket}/{objectPrefix}{runId}.result.json. The upload only
// creates the object, never replaces it, so an object planted under the
// run's name fails delivery instead of being trusted or overwritten.
type GCSSink struct {
	bucket           string
	objectPrefix     string
	credentialSource credential.Source
	httpClient       *http.Client
	endpointBaseURL  string
	retryDelays      []time.Duration
}

// GCSSinkOptions configures a GCSSink. Bucket and CredentialSource are
// required.
type GCSSinkOptions struct {
	Bucket           string
	ObjectPrefix     string
	CredentialSource credential.Source

	// HTTPClient is optional; a client bounded per request is
	// constructed when nil.
	HTTPClient *http.Client

	// EndpointBaseURL overrides the GCS upload host. Tests point it at an
	// httptest.NewServer; production callers leave it empty.
	EndpointBaseURL string
}

// NewGCSSink validates opts and constructs the sink. Credential
// resolution is deferred to Emit so it is retried with the upload.
func NewGCSSink(opts GCSSinkOptions) (*GCSSink, error) {
	if opts.Bucket == "" {
		return nil, errors.New("gcs result sink: bucket is required")
	}
	if opts.CredentialSource == nil {
		return nil, errors.New("gcs result sink: credential source is required")
	}
	client := opts.HTTPClient
	if client == nil {
		client = newRemoteHTTPClient()
	}
	return &GCSSink{
		bucket:           opts.Bucket,
		objectPrefix:     opts.ObjectPrefix,
		credentialSource: opts.CredentialSource,
		httpClient:       client,
		endpointBaseURL:  opts.EndpointBaseURL,
		retryDelays:      defaultRetryDelays,
	}, nil
}

// Emit uploads result, retrying transient failures. A nil return means
// GCS acknowledged the object.
func (s *GCSSink) Emit(ctx context.Context, result types.RunResult) error {
	if result.RunID == "" {
		return errors.New("gcs result sink: RunResult has no runId to name the object")
	}
	body, err := marshalResult(result)
	if err != nil {
		return err
	}
	object := resultObjectName(s.objectPrefix, result.RunID)
	uri := fmt.Sprintf("gs://%s/%s", s.bucket, object)

	// mayHaveWritten is set once a sent request fails without a GCS
	// response, so it could have created the object before failing.
	mayHaveWritten := false
	err = deliver(ctx, "gcs", s.retryDelays, func(ctx context.Context) error {
		bearer, err := resolveBearer(ctx, s.credentialSource)
		if err != nil {
			return err
		}
		token, err := bearer(ctx)
		if err != nil {
			return fmt.Errorf("acquire bearer token: %w", err)
		}
		err = gcs.UploadObject(ctx, s.httpClient, gcs.UploadOptions{
			Bucket:          s.bucket,
			Object:          object,
			Body:            body,
			ContentType:     "application/json",
			Bearer:          func(context.Context) (string, error) { return token, nil },
			EndpointBaseURL: s.endpointBaseURL,
			CreateOnly:      true,
		})
		var status *gcs.UploadStatusError
		switch {
		case err == nil:
			return nil
		case errors.As(err, &status) && status.StatusCode == http.StatusPreconditionFailed:
			if mayHaveWritten {
				return &permanentError{fmt.Errorf("object already exists after an interrupted attempt; it is probably this run's result, but that cannot be confirmed: %w", err)}
			}
			return &permanentError{fmt.Errorf("object already exists, written by another writer (a reused run ID, or a write from inside the run): %w", err)}
		case !errors.As(err, &status) && !isDialError(err):
			mayHaveWritten = true
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("gcs result sink: upload %s: %w", uri, err)
	}
	slog.InfoContext(ctx, "resultSink: uploaded RunResult", "sink", "gcs", "uri", uri, "bytes", len(body))
	return nil
}

// isDialError reports whether err ended the request before a connection
// was made, so nothing reached GCS.
func isDialError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// resultObjectName joins prefix and runID into the result object's
// name. A prefix without a trailing slash gains one.
func resultObjectName(prefix, runID string) string {
	name := runID + ".result.json"
	if prefix == "" {
		return name
	}
	if strings.HasSuffix(prefix, "/") {
		return prefix + name
	}
	return prefix + "/" + name
}
