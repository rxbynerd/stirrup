package workspaceexport

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/httpsput"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// HTTPSPutExporter uploads the workspace tarball with one HTTP PUT to the
// URL a types.HTTPSPutConfig resolves to.
type HTTPSPutExporter struct {
	target     *types.HTTPSPutConfig
	secrets    security.SecretStore
	httpClient *http.Client
}

// HTTPSPutExporterOptions configures an HTTPSPutExporter. Target and
// Secrets are required.
type HTTPSPutExporterOptions struct {
	Target  *types.HTTPSPutConfig
	Secrets security.SecretStore

	// HTTPClient is optional; httpsput.NewClient(uploadTimeout) is used
	// when nil.
	HTTPClient *http.Client
}

// NewHTTPSPutExporter validates opts and constructs an exporter. The URL
// reference is resolved on every Export, so a reference whose value is
// replaced during the run (a rotated presigned URL) uploads to the
// current one.
func NewHTTPSPutExporter(opts HTTPSPutExporterOptions) (*HTTPSPutExporter, error) {
	if opts.Target == nil {
		return nil, errors.New("workspace exporter: an HTTPS PUT destination is required")
	}
	if opts.Secrets == nil {
		return nil, errors.New("workspace exporter: a secret store is required")
	}
	client := opts.HTTPClient
	if client == nil {
		client = httpsput.NewClient(uploadTimeout)
	}
	return &HTTPSPutExporter{target: opts.Target, secrets: opts.Secrets, httpClient: client}, nil
}

// Export archives src's workspace exactly as GCSExporter.Export does and
// PUTs the tarball to the destination. The receipt's URI is the
// destination's origin; neither it nor a returned error carries the URL.
func (e *HTTPSPutExporter) Export(ctx context.Context, src executor.WorkspaceArchiver) (types.ExportReceipt, error) {
	receipt := types.ExportReceipt{Status: types.ExportStatusFailed}
	target, err := httpsput.Resolve(ctx, e.secrets, e.target)
	if err != nil {
		return receipt, fmt.Errorf("workspace exporter: %w", err)
	}
	receipt.URI = target.Origin()

	archive, err := archiveWorkspace(ctx, src, MaxArchiveBytes)
	if err != nil {
		return receipt, fmt.Errorf("workspace exporter: %w", err)
	}
	if err := httpsput.Put(ctx, e.httpClient, target, archive.gz, "application/gzip"); err != nil {
		return receipt, fmt.Errorf("workspace exporter: upload tarball: %w", err)
	}
	archive.fill(&receipt)
	return receipt, nil
}
