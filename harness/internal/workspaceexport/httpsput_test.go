package workspaceexport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

type tableSecrets map[string]string

func (s tableSecrets) Resolve(_ context.Context, ref string) (string, error) {
	v, ok := s[ref]
	if !ok {
		return "", fmt.Errorf("secret %q is not set", ref)
	}
	return v, nil
}

const presignedQuery = "?X-Amz-Signature=deadbeefcafe"

func newHTTPSPutTestExporter(t *testing.T, srv *captureUploadServer, secrets tableSecrets) (*HTTPSPutExporter, string) {
	t.Helper()
	must := require.New(t)
	httpSrv := httptest.NewServer(srv.handler())
	t.Cleanup(httpSrv.Close)
	secrets["secret://EXPORT_URL"] = httpSrv.URL + "/bucket/workspace.tar.gz" + presignedQuery
	exp, err := NewHTTPSPutExporter(HTTPSPutExporterOptions{
		Target:  &types.HTTPSPutConfig{URLRef: "secret://EXPORT_URL"},
		Secrets: secrets,
	})
	must.NoError(err)
	return exp, httpSrv.URL
}

func TestHTTPSPutExporter_UploadsTheArchiveAndReturnsAnOriginOnlyReceipt(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "alpha")
	writeFile(t, filepath.Join(dir, "sub", "b.txt"), "beta")
	srv := &captureUploadServer{}
	exp, origin := newHTTPSPutTestExporter(t, srv, tableSecrets{})

	receipt, err := exp.Export(context.Background(), localArchiver(t, dir))
	must.NoError(err)

	upload := srv.last()
	check.Equal("/bucket/workspace.tar.gz"+presignedQuery, upload.URL)
	sum := sha256.Sum256(upload.Body)
	check.Equal(types.ExportReceipt{
		URI:    origin,
		Status: types.ExportStatusUploaded,
		Bytes:  int64(len(upload.Body)),
		SHA256: hex.EncodeToString(sum[:]),
		Files:  2,
	}, receipt)
}

func TestHTTPSPutExporter_EmptyWorkspaceUploadsAnEmptyArchive(t *testing.T) {
	must := require.New(t)
	srv := &captureUploadServer{}
	exp, _ := newHTTPSPutTestExporter(t, srv, tableSecrets{})

	receipt, err := exp.Export(context.Background(), localArchiver(t, t.TempDir()))
	must.NoError(err)
	must.Equal(types.ExportStatusEmpty, receipt.Status)
	must.NotEmpty(srv.last().Body)
}

func TestHTTPSPutExporter_RejectedUploadFailsWithoutDisclosingTheURL(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "alpha")
	srv := &captureUploadServer{statusCode: http.StatusForbidden}
	exp, origin := newHTTPSPutTestExporter(t, srv, tableSecrets{})

	receipt, err := exp.Export(context.Background(), localArchiver(t, dir))
	must.ErrorContains(err, "HTTP 403")
	check.Equal(types.ExportReceipt{URI: origin, Status: types.ExportStatusFailed}, receipt)
	check.NotContains(err.Error(), "deadbeefcafe")
	check.NotContains(err.Error(), "workspace.tar.gz")
}

func TestHTTPSPutExporter_UnresolvedReferenceFailsBeforeArchiving(t *testing.T) {
	must := require.New(t)
	exp, err := NewHTTPSPutExporter(HTTPSPutExporterOptions{
		Target:  &types.HTTPSPutConfig{URLRef: "secret://MISSING"},
		Secrets: tableSecrets{},
	})
	must.NoError(err)

	receipt, err := exp.Export(context.Background(), nil)
	must.ErrorContains(err, "resolve urlRef")
	must.Equal(types.ExportReceipt{Status: types.ExportStatusFailed}, receipt)
}

func TestHTTPSPutExporter_ResolvesTheReferenceOnEveryExport(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "alpha")
	srv := &captureUploadServer{}
	secrets := tableSecrets{}
	exp, origin := newHTTPSPutTestExporter(t, srv, secrets)

	_, err := exp.Export(context.Background(), localArchiver(t, dir))
	must.NoError(err)
	secrets["secret://EXPORT_URL"] = origin + "/bucket/second.tar.gz"
	_, err = exp.Export(context.Background(), localArchiver(t, dir))
	must.NoError(err)
	must.Equal("/bucket/second.tar.gz", srv.last().URL)
}

func TestHTTPSPutExporter_ProbeResolvesWithoutContactingTheDestination(t *testing.T) {
	must := require.New(t)
	srv := &captureUploadServer{}
	exp, origin := newHTTPSPutTestExporter(t, srv, tableSecrets{})

	got, err := exp.Probe(context.Background())
	must.NoError(err)
	must.Equal(origin, got)
	must.Empty(srv.last().URL, "a probe must send nothing")
}

func TestNewHTTPSPutExporter_RequiresTargetAndSecrets(t *testing.T) {
	must := require.New(t)
	_, err := NewHTTPSPutExporter(HTTPSPutExporterOptions{Secrets: tableSecrets{}})
	must.Error(err)
	_, err = NewHTTPSPutExporter(HTTPSPutExporterOptions{Target: &types.HTTPSPutConfig{URLRef: "secret://U"}})
	must.Error(err)
}
