package workspaceexport

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/types"
)

// staticBearerSource yields a fixed bearer token.
type staticBearerSource struct{ token string }

func (s *staticBearerSource) Resolve(_ context.Context) (*credential.Resolved, error) {
	return &credential.Resolved{
		BearerToken: func(_ context.Context) (string, error) { return s.token, nil },
	}, nil
}

type captureUploadServer struct {
	mu         sync.Mutex
	requests   []capturedUpload
	statusCode int
}

type capturedUpload struct {
	URL  string
	Body []byte
}

func (c *captureUploadServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		c.mu.Lock()
		c.requests = append(c.requests, capturedUpload{URL: r.URL.String(), Body: body})
		status := c.statusCode
		c.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
	})
}

func (c *captureUploadServer) last() capturedUpload {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.requests) == 0 {
		return capturedUpload{}
	}
	return c.requests[len(c.requests)-1]
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	must := require.New(t)
	must.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	must.NoError(os.WriteFile(path, []byte(content), 0o644))
}

// localArchiver returns a local executor rooted at dir, the archiver the
// host-walk tests export through.
func localArchiver(t *testing.T, dir string) *executor.LocalExecutor {
	t.Helper()
	must := require.New(t)
	exec, err := executor.NewLocalExecutor(dir)
	must.NoError(err)
	return exec
}

// exportFrom exports dir through a local executor and checks that a
// failed export always carries a failed receipt.
func exportFrom(t *testing.T, exp *GCSExporter, dir, uri string) error {
	t.Helper()
	must := require.New(t)
	receipt, err := exp.Export(context.Background(), localArchiver(t, dir), uri)
	if err != nil {
		must.Equal(types.ExportStatusFailed, receipt.Status, "receipt status on error %v", err)
	}
	return err
}

func newTestExporter(t *testing.T, srv *captureUploadServer) *GCSExporter {
	t.Helper()
	must := require.New(t)
	httpSrv := httptest.NewServer(srv.handler())
	t.Cleanup(httpSrv.Close)
	exp, err := NewGCSExporter(GCSExporterOptions{
		CredentialSource: &staticBearerSource{token: "tok"},
		EndpointBaseURL:  httpSrv.URL,
	})
	must.NoError(err)
	return exp
}

// regularFileContents returns the contents of every regular file in a
// gzipped tar archive, keyed by entry name.
func regularFileContents(t *testing.T, archive []byte) map[string]string {
	t.Helper()
	must := require.New(t)
	gzr, err := gzip.NewReader(bytes.NewReader(archive))
	must.NoError(err)
	tr := tar.NewReader(gzr)
	files := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		must.NoError(err)
		if h.Typeflag == tar.TypeReg {
			data, _ := io.ReadAll(tr)
			files[h.Name] = string(data)
		}
	}
	return files
}

func TestGCSExporter_UploadsArchiveWithReceipt(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "hello.txt"), "hello world")
	writeFile(t, filepath.Join(dir, "subdir/nested.txt"), "nested content")

	srv := &captureUploadServer{}
	exp := newTestExporter(t, srv)

	receipt, err := exp.Export(context.Background(), localArchiver(t, dir), "gs://my-bucket/runs/run-1/workspace.tar.gz")
	must.NoError(err)

	got := srv.last()
	sum := sha256.Sum256(got.Body)
	want := types.ExportReceipt{
		URI:    "gs://my-bucket/runs/run-1/workspace.tar.gz",
		Status: types.ExportStatusUploaded,
		Bytes:  int64(len(got.Body)),
		SHA256: hex.EncodeToString(sum[:]),
		Files:  2,
	}
	must.Equal(want, receipt)
	must.Contains(got.URL, "/upload/storage/v1/b/my-bucket/o")
	must.Contains(got.URL, "name=runs/run-1/workspace.tar.gz")

	files := regularFileContents(t, got.Body)
	must.Equal("hello world", files["hello.txt"], "hello.txt content")
	must.Equal("nested content", files["subdir/nested.txt"], "subdir/nested.txt content")
}

func TestGCSExporter_UploadServerErrorFailsExport(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f.txt"), "x")

	srv := &captureUploadServer{statusCode: http.StatusServiceUnavailable}
	exp := newTestExporter(t, srv)

	err := exportFrom(t, exp, dir, "gs://b/o.tar.gz")
	must.ErrorContains(err, "HTTP 503")
}

func TestGCSExporter_RefusesSymlinkEscape(t *testing.T) {
	must := require.New(t)
	parent := t.TempDir()
	outside := filepath.Join(parent, "outside")
	must.NoError(os.MkdirAll(outside, 0o755))
	writeFile(t, filepath.Join(outside, "secret.txt"), "this should never be exported")

	workspace := filepath.Join(parent, "workspace")
	must.NoError(os.MkdirAll(workspace, 0o755))
	must.NoError(os.Symlink(outside, filepath.Join(workspace, "escape")))
	writeFile(t, filepath.Join(workspace, "innocent.txt"), "ok")

	srv := &captureUploadServer{}
	exp := newTestExporter(t, srv)

	err := exportFrom(t, exp, workspace, "gs://b/o.tar.gz")
	must.ErrorContains(err, "outside workspace root")
	must.Empty(srv.requests, "refused export should produce no upload")
}

// Symlinks that stay inside the workspace are recorded as symlink entries,
// not dereferenced.
func TestGCSExporter_KeepsInternalSymlinkAsLink(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "real.txt"), "real")
	must.NoError(os.Symlink("real.txt", filepath.Join(dir, "link.txt")))

	srv := &captureUploadServer{}
	exp := newTestExporter(t, srv)

	err := exportFrom(t, exp, dir, "gs://b/o.tar.gz")
	must.NoError(err)

	gzr, err := gzip.NewReader(bytes.NewReader(srv.last().Body))
	must.NoError(err)
	tr := tar.NewReader(gzr)
	sawLink := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		must.NoError(err)
		if h.Name == "link.txt" && h.Typeflag == tar.TypeSymlink {
			sawLink = true
			must.Equal("real.txt", h.Linkname, "symlink target")
		}
	}
	must.True(sawLink, "internal symlink missing from archive or wrong type")
}

// A symlink whose target does not exist must be refused at archive time.
func TestGCSExporter_RefusesDanglingSymlink(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "innocent.txt"), "ok")
	target := filepath.Join(dir, "definitely-does-not-exist", "secret")
	must.NoError(os.Symlink(target, filepath.Join(dir, "dangling")))

	srv := &captureUploadServer{}
	exp := newTestExporter(t, srv)

	err := exportFrom(t, exp, dir, "gs://b/o.tar.gz")
	must.ErrorContains(err, "cannot resolve symlink target")
	must.Empty(srv.requests, "refused export should produce no upload")
}

func TestGCSExporter_RefusesNonGCSURI(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f.txt"), "x")

	exp, err := NewGCSExporter(GCSExporterOptions{
		CredentialSource: &staticBearerSource{token: "tok"},
	})
	must.NoError(err)

	cases := []struct {
		name string
		uri  string
	}{
		{"rejects s3 scheme", "s3://bucket/key"},
		{"rejects http scheme", "http://example.com/x"},
		{"rejects missing slash after scheme", "gs:/missing-slash"},
		{"rejects empty bucket", "gs://"},
		{"rejects bucket without object", "gs://bucket-only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			err := exportFrom(t, exp, dir, tc.uri)
			must.Error(err)
		})
	}
}

func TestParseGCSURI_AcceptsGCSAndRejectsMalformed(t *testing.T) {
	cases := []struct {
		name    string
		uri     string
		bucket  string
		object  string
		wantErr bool
	}{
		{"splits bucket and object", "gs://b/o", "b", "o", false},
		{"keeps nested object path", "gs://b/a/b/c.tar.gz", "b", "a/b/c.tar.gz", false},
		{"rejects wrong scheme", "s3://b/o", "", "", true},
		{"rejects missing bucket", "gs://", "", "", true},
		{"rejects missing object separator", "gs://b", "", "", true},
		{"rejects empty object", "gs://b/", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			bucket, object, err := parseGCSURI(tc.uri)
			if tc.wantErr {
				must.Error(err)
				return
			}
			must.NoError(err)
			must.Equal(tc.bucket, bucket)
			must.Equal(tc.object, object)
		})
	}
}
