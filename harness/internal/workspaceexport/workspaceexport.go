// Package workspaceexport uploads the executor's workspace as a
// gzipped tarball to a remote destination URI. Only "gs://"
// destinations are supported in v1; see docs/cloud-run-jobs.md.
package workspaceexport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/gcs"
	"github.com/rxbynerd/stirrup/types"
)

// MaxArchiveBytes caps both the workspace's uncompressed tar stream and the
// uploaded tarball, guarding against a runaway workspace exhausting memory
// or upload bandwidth. Hard-coded for v1.
const MaxArchiveBytes = 1 << 30

// uploadTimeout bounds the GCS PUT round-trip.
const uploadTimeout = 5 * time.Minute

// Exporter uploads an executor's workspace to a remote destination URI.
type Exporter interface {
	// Export archives src's workspace and uploads it to destURI as a
	// gzipped tarball. The receipt's Status is uploaded, empty (the
	// workspace had no files; an empty archive was still uploaded) or
	// failed. A non-nil error always comes with a failed receipt, and a
	// missing workspace root wraps executor.ErrWorkspaceRootMissing.
	Export(ctx context.Context, src executor.WorkspaceArchiver, destURI string) (types.ExportReceipt, error)
}

// GCSExporter implements Exporter against a gs:// destination URI.
type GCSExporter struct {
	credentialSource credential.Source
	httpClient       *http.Client
	endpointBaseURL  string // override for tests
}

// GCSExporterOptions configures a GCSExporter. CredentialSource is required.
type GCSExporterOptions struct {
	CredentialSource credential.Source

	// HTTPClient is optional; a sensibly-bounded client is
	// constructed when nil.
	HTTPClient *http.Client

	// EndpointBaseURL overrides the GCS upload host. Tests point it
	// at an httptest.NewServer; production callers leave it empty.
	EndpointBaseURL string
}

// NewGCSExporter validates options and constructs an exporter.
// Credential resolution is deferred until Export so a transient
// metadata-server hiccup at boot doesn't block the run.
func NewGCSExporter(opts GCSExporterOptions) (*GCSExporter, error) {
	if opts.CredentialSource == nil {
		return nil, fmt.Errorf("workspace exporter: credential source is required")
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: uploadTimeout,
			Transport: &http.Transport{
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		}
	}
	return &GCSExporter{
		credentialSource: opts.CredentialSource,
		httpClient:       client,
		endpointBaseURL:  opts.EndpointBaseURL,
	}, nil
}

// Export reads src's tar stream through repackArchive (subject to
// MaxArchiveBytes), gzips it into memory and uploads it to destURI, which
// must be a gs:// URI.
func (e *GCSExporter) Export(ctx context.Context, src executor.WorkspaceArchiver, destURI string) (types.ExportReceipt, error) {
	receipt := types.ExportReceipt{URI: destURI, Status: types.ExportStatusFailed}
	bucket, object, err := parseGCSURI(destURI)
	if err != nil {
		return receipt, fmt.Errorf("workspace exporter: %w", err)
	}

	archive, err := archiveWorkspace(ctx, src, MaxArchiveBytes)
	if err != nil {
		return receipt, fmt.Errorf("workspace exporter: %w", err)
	}

	resolved, err := e.credentialSource.Resolve(ctx)
	if err != nil {
		return receipt, fmt.Errorf("workspace exporter: resolve credential: %w", err)
	}
	if resolved == nil || resolved.BearerToken == nil {
		return receipt, fmt.Errorf("workspace exporter: credential source produced no bearer token")
	}

	if err := gcs.UploadObject(ctx, e.httpClient, gcs.UploadOptions{
		Bucket:          bucket,
		Object:          object,
		Body:            archive.gz,
		ContentType:     "application/gzip",
		Bearer:          resolved.BearerToken,
		EndpointBaseURL: e.endpointBaseURL,
	}); err != nil {
		return receipt, fmt.Errorf("workspace exporter: upload tarball: %w", err)
	}

	sum := sha256.Sum256(archive.gz)
	receipt.Bytes = int64(len(archive.gz))
	receipt.SHA256 = hex.EncodeToString(sum[:])
	receipt.Files = archive.files
	receipt.Status = types.ExportStatusUploaded
	if archive.files == 0 {
		receipt.Status = types.ExportStatusEmpty
	}
	return receipt, nil
}

// Probe verifies the export destination is reachable for a dry-run
// preflight: it parses destURI, resolves the credential, and performs
// a read-only bucket-metadata GET. It tars and uploads nothing.
func (e *GCSExporter) Probe(ctx context.Context, destURI string) error {
	bucket, _, err := parseGCSURI(destURI)
	if err != nil {
		return fmt.Errorf("workspace exporter: %w", err)
	}
	resolved, err := e.credentialSource.Resolve(ctx)
	if err != nil {
		return fmt.Errorf("workspace exporter: resolve credential: %w", err)
	}
	if resolved == nil || resolved.BearerToken == nil {
		return fmt.Errorf("workspace exporter: credential source produced no bearer token")
	}
	if err := gcs.BucketAccessible(ctx, e.httpClient, gcs.BucketProbeOptions{
		Bucket:          bucket,
		Bearer:          resolved.BearerToken,
		EndpointBaseURL: e.endpointBaseURL,
	}); err != nil {
		return fmt.Errorf("workspace exporter: %w", err)
	}
	return nil
}

// parseGCSURI splits a gs://bucket/object URI into its parts. The
// object name must be non-empty so callers cannot accidentally
// overwrite the bucket root or trigger surprising GCS API behaviour.
func parseGCSURI(uri string) (bucket, object string, err error) {
	const prefix = "gs://"
	if !strings.HasPrefix(uri, prefix) {
		return "", "", fmt.Errorf("unsupported destination URI %q: only gs:// is supported in v1", uri)
	}
	rest := strings.TrimPrefix(uri, prefix)
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return "", "", fmt.Errorf("destination URI %q: missing object path", uri)
	}
	bucket = rest[:slash]
	object = rest[slash+1:]
	if bucket == "" {
		return "", "", fmt.Errorf("destination URI %q: empty bucket name", uri)
	}
	if object == "" {
		return "", "", fmt.Errorf("destination URI %q: empty object path", uri)
	}
	return bucket, object, nil
}
