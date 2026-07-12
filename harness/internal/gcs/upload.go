// Package gcs is a thin GCS JSON-API client used by the trace emitter
// and the workspace exporter. Per the project's "no vendor cloud SDKs"
// policy (CLAUDE.md), this package wraps net/http directly rather than
// taking a dependency on cloud.google.com/go/storage.
//
// The package intentionally exposes a single function (UploadObject)
// against the single-shot media-upload endpoint. Resumable uploads,
// metadata APIs, and signed URLs are out of scope — the trace emitter
// pushes a single JSONL line, and the workspace exporter pushes a
// single gzipped tarball; both fit cleanly into one PUT.
package gcs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
)

// DefaultEndpointBaseURL is the production GCS JSON-API host. Tests
// override this by passing a different value to UploadObject (via the
// EndpointBaseURL field on UploadOptions) so an httptest.NewServer can
// intercept the request without touching package-level state.
const DefaultEndpointBaseURL = "https://storage.googleapis.com"

// UploadOptions configures a single object upload. Bucket, Object, and
// Body are required; the rest fall back to sane defaults documented in
// each field.
type UploadOptions struct {
	// Bucket is the GCS bucket name. Validated at config-load time by
	// types.ValidateRunConfig; not re-validated here so the caller
	// surfaces shape errors at boot rather than at run-end.
	Bucket string

	// Object is the GCS object name (the "name=" query parameter).
	// May contain '/' (treated as a path separator by GCS).
	Object string

	// Body is the bytes to upload. Held by reference; the function does
	// not retain a reference after returning.
	Body []byte

	// BodyReader streams an object without materialising it in memory. When
	// non-nil it takes precedence over Body and BodySize must contain the
	// exact byte length so the request can set Content-Length.
	BodyReader io.Reader
	BodySize   int64

	// ContentType is the Content-Type header sent with the upload.
	// Defaults to application/octet-stream when empty.
	ContentType string

	// Bearer returns the current bearer token (typically a closure from
	// credential.Resolved). Required — the function will not attempt
	// anonymous uploads.
	Bearer credential.BearerTokenFunc

	// EndpointBaseURL overrides the upload host. Empty means use
	// DefaultEndpointBaseURL. Tests set this to point at an
	// httptest.NewServer; production callers leave it empty.
	EndpointBaseURL string
}

// UploadObject PUTs Body to gs://{Bucket}/{Object} via the JSON-API
// media upload endpoint. The HTTP client is passed in by the caller so
// each adapter can configure its own timeout (trace emitter: 60s,
// workspace exporter: 5 min — driven by expected payload size).
//
// Returns an error wrapping the GCS HTTP status on non-2xx responses,
// capped at 4 KiB of body capture so a misbehaving intermediary cannot
// pin a huge buffer in memory just to construct the error message.
// Callers MUST surface upload failures — silent drop produces an
// observability gap that is unrecoverable after the run completes.
func UploadObject(ctx context.Context, client *http.Client, opts UploadOptions) error {
	if client == nil {
		return fmt.Errorf("gcs: http client is required")
	}
	if opts.Bucket == "" {
		return fmt.Errorf("gcs: bucket is required")
	}
	if opts.Object == "" {
		return fmt.Errorf("gcs: object is required")
	}
	if opts.Bearer == nil {
		return fmt.Errorf("gcs: bearer is required")
	}

	contentType := opts.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	endpoint := opts.EndpointBaseURL
	if endpoint == "" {
		endpoint = DefaultEndpointBaseURL
	}

	token, err := opts.Bearer(ctx)
	if err != nil {
		return fmt.Errorf("acquire bearer token: %w", err)
	}

	// uploadType=media is the single-shot upload mode; name= sets the
	// object key. Both must be query parameters per the GCS REST API
	// contract (the path locates the bucket, not the object).
	url := fmt.Sprintf(
		"%s/upload/storage/v1/b/%s/o?uploadType=media&name=%s",
		strings.TrimRight(endpoint, "/"),
		opts.Bucket,
		urlPathEscape(opts.Object),
	)

	var body io.Reader = bytes.NewReader(opts.Body)
	contentLength := int64(len(opts.Body))
	if opts.BodyReader != nil {
		if opts.BodySize < 0 {
			return fmt.Errorf("gcs: body size must not be negative")
		}
		body = opts.BodyReader
		contentLength = opts.BodySize
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = contentLength

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post to gcs: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Drain to allow connection reuse on the underlying transport.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	const maxErrBody = 4 * 1024
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	return fmt.Errorf("gcs upload returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
}

// BucketProbeOptions configures a single bucket-access probe.
type BucketProbeOptions struct {
	// Bucket is the GCS bucket name to check.
	Bucket string

	// Bearer returns the current bearer token. Required.
	Bearer credential.BearerTokenFunc

	// EndpointBaseURL overrides the GCS JSON-API host. Empty means use
	// DefaultEndpointBaseURL. Tests set this to an httptest.NewServer.
	EndpointBaseURL string
}

// BucketAccessible performs a read-only bucket-metadata GET
// (GET /storage/v1/b/{bucket}) to verify the credential can see the
// bucket, for a dry-run preflight. A 2xx confirms access without
// uploading anything; a non-2xx (404 missing bucket, 403 denied, 401 bad
// credential) is surfaced as an error carrying the GCS diagnostic. This
// is the cheapest authenticated bucket check the JSON API offers and is
// shared by the gcs trace emitter and the workspace exporter so both
// report bucket access identically.
func BucketAccessible(ctx context.Context, client *http.Client, opts BucketProbeOptions) error {
	if client == nil {
		return fmt.Errorf("gcs: http client is required")
	}
	if opts.Bucket == "" {
		return fmt.Errorf("gcs: bucket is required")
	}
	if opts.Bearer == nil {
		return fmt.Errorf("gcs: bearer is required")
	}
	endpoint := opts.EndpointBaseURL
	if endpoint == "" {
		endpoint = DefaultEndpointBaseURL
	}
	token, err := opts.Bearer(ctx)
	if err != nil {
		return fmt.Errorf("acquire bearer token: %w", err)
	}

	// PathEscape the bucket so a name carrying reserved bytes cannot
	// rewrite the request path. TraceEmitter.Bucket is regex-validated, but
	// WorkspaceExportTo's bucket has weaker validation, so escape here as
	// defence-in-depth rather than relying on the caller having checked.
	reqURL := fmt.Sprintf("%s/storage/v1/b/%s", strings.TrimRight(endpoint, "/"), url.PathEscape(opts.Bucket))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("get gcs bucket metadata: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	const maxErrBody = 4 * 1024
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	return fmt.Errorf("gcs bucket %q access returned HTTP %d: %s", opts.Bucket, resp.StatusCode, strings.TrimSpace(string(msg)))
}

// urlPathEscape escapes an object name for use in the name= query
// parameter. GCS allows '/' unescaped in this position (the value goes
// into the bucket as the object key, '/'-separators preserved). Other
// characters outside the RFC 3986 unreserved set are %-encoded.
func urlPathEscape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c)
		case c >= 'a' && c <= 'z':
			b.WriteByte(c)
		case c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == '-' || c == '_' || c == '.' || c == '~' || c == '/':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
