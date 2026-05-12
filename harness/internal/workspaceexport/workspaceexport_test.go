package workspaceexport

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
)

// staticBearerSource yields a fixed bearer token. Mirrors the helper
// in trace/gcs_test.go but redeclared here so the two test packages
// stay independent.
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

func TestGCSExporter_Success(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "hello.txt"), "hello world")
	writeFile(t, filepath.Join(dir, "subdir/nested.txt"), "nested content")

	srv := &captureUploadServer{}
	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()

	exp, err := NewGCSExporter(GCSExporterOptions{
		CredentialSource: &staticBearerSource{token: "tok"},
		EndpointBaseURL:  httpSrv.URL,
	})
	if err != nil {
		t.Fatalf("NewGCSExporter: %v", err)
	}

	err = exp.Export(context.Background(), dir, "gs://my-bucket/runs/run-1/workspace.tar.gz")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	got := srv.last()
	if !strings.Contains(got.URL, "/upload/storage/v1/b/my-bucket/o") {
		t.Errorf("URL missing upload path: %q", got.URL)
	}
	if !strings.Contains(got.URL, "name=runs/run-1/workspace.tar.gz") {
		t.Errorf("URL missing expected name: %q", got.URL)
	}

	// The uploaded body must decode as a valid gzipped tar containing
	// hello.txt and subdir/nested.txt.
	gzr, err := gzip.NewReader(bytes.NewReader(got.Body))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	tr := tar.NewReader(gzr)
	entries := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		if h.Typeflag == tar.TypeReg {
			data, _ := io.ReadAll(tr)
			entries[h.Name] = string(data)
		}
	}
	if entries["hello.txt"] != "hello world" {
		t.Errorf("hello.txt content: got %q, want %q", entries["hello.txt"], "hello world")
	}
	if entries["subdir/nested.txt"] != "nested content" {
		t.Errorf("subdir/nested.txt content: got %q, want %q", entries["subdir/nested.txt"], "nested content")
	}
}

func TestGCSExporter_ServerError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f.txt"), "x")

	srv := &captureUploadServer{statusCode: http.StatusServiceUnavailable}
	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()

	exp, _ := NewGCSExporter(GCSExporterOptions{
		CredentialSource: &staticBearerSource{token: "tok"},
		EndpointBaseURL:  httpSrv.URL,
	})

	err := exp.Export(context.Background(), dir, "gs://b/o.tar.gz")
	if err == nil {
		t.Fatal("want error from 503, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("error should mention HTTP 503, got %v", err)
	}
}

func TestGCSExporter_EmptyDirectorySkip(t *testing.T) {
	dir := t.TempDir()
	// Don't write any files; just have the empty dir.

	srv := &captureUploadServer{}
	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()

	exp, _ := NewGCSExporter(GCSExporterOptions{
		CredentialSource: &staticBearerSource{token: "tok"},
		EndpointBaseURL:  httpSrv.URL,
	})

	if err := exp.Export(context.Background(), dir, "gs://b/o.tar.gz"); err != nil {
		t.Fatalf("Export on empty dir should be nil-op, got %v", err)
	}
	if len(srv.requests) != 0 {
		t.Errorf("empty dir should produce no upload, got %d requests", len(srv.requests))
	}
}

func TestGCSExporter_MissingDirectorySkip(t *testing.T) {
	srv := &captureUploadServer{}
	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()

	exp, _ := NewGCSExporter(GCSExporterOptions{
		CredentialSource: &staticBearerSource{token: "tok"},
		EndpointBaseURL:  httpSrv.URL,
	})

	if err := exp.Export(context.Background(), "/does/not/exist", "gs://b/o.tar.gz"); err != nil {
		t.Errorf("missing dir should be silent skip, got %v", err)
	}
	if len(srv.requests) != 0 {
		t.Errorf("missing dir should produce no upload, got %d requests", len(srv.requests))
	}
}

func TestGCSExporter_RefuseSymlinkEscape(t *testing.T) {
	parent := t.TempDir()
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	writeFile(t, filepath.Join(outside, "secret.txt"), "this should never be exported")

	workspace := filepath.Join(parent, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	// Symlink inside the workspace pointing to a sibling directory
	// outside it. The exporter must refuse this rather than
	// dereferencing the link and capturing the parent dir's contents.
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// Also a regular file inside the workspace so the walk has
	// something to do.
	writeFile(t, filepath.Join(workspace, "innocent.txt"), "ok")

	// The exporter refuses the entire archive when a symlink in the
	// walked tree resolves outside the workspace root. Silent
	// record-as-symlink is too permissive: an untar on the consumer
	// side that follows symlinks during extraction would then read
	// the parent dir's contents. Failing the whole export forces the
	// operator to fix the source rather than ship a half-trusted
	// tarball.
	srv := &captureUploadServer{}
	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()

	exp, _ := NewGCSExporter(GCSExporterOptions{
		CredentialSource: &staticBearerSource{token: "tok"},
		EndpointBaseURL:  httpSrv.URL,
	})

	err := exp.Export(context.Background(), workspace, "gs://b/o.tar.gz")
	if err == nil {
		t.Fatal("Export should refuse a symlink escape, got nil")
	}
	if !strings.Contains(err.Error(), "outside workspace root") {
		t.Errorf("error should mention workspace escape, got %v", err)
	}
	if len(srv.requests) != 0 {
		t.Errorf("refused export should produce no upload, got %d requests", len(srv.requests))
	}
}

// TestGCSExporter_InternalSymlinkOK pins that symlinks staying inside
// the workspace are recorded as symlink entries (not dereferenced),
// matching the typical "go.sum -> ../go.sum" pattern in monorepo
// sub-modules. Without this, every internal symlink would fail the
// escape check and surprise an operator whose workspace happens to
// contain a relative symlink.
func TestGCSExporter_InternalSymlinkOK(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "real.txt"), "real")
	if err := os.Symlink("real.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	srv := &captureUploadServer{}
	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()

	exp, _ := NewGCSExporter(GCSExporterOptions{
		CredentialSource: &staticBearerSource{token: "tok"},
		EndpointBaseURL:  httpSrv.URL,
	})

	if err := exp.Export(context.Background(), dir, "gs://b/o.tar.gz"); err != nil {
		t.Fatalf("Export: %v", err)
	}

	gzr, _ := gzip.NewReader(bytes.NewReader(srv.last().Body))
	tr := tar.NewReader(gzr)
	sawLink := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		if h.Name == "link.txt" && h.Typeflag == tar.TypeSymlink {
			sawLink = true
			if h.Linkname != "real.txt" {
				t.Errorf("symlink target: got %q, want real.txt", h.Linkname)
			}
		}
	}
	if !sawLink {
		t.Error("internal symlink missing from archive or wrong type")
	}
}

func TestGCSExporter_RefusesNonGCSURI(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f.txt"), "x")

	exp, _ := NewGCSExporter(GCSExporterOptions{
		CredentialSource: &staticBearerSource{token: "tok"},
	})

	cases := []string{
		"s3://bucket/key",
		"http://example.com/x",
		"gs:/missing-slash",
		"gs://", // empty bucket
		"gs://bucket-only",
	}
	for _, uri := range cases {
		t.Run(uri, func(t *testing.T) {
			err := exp.Export(context.Background(), dir, uri)
			if err == nil {
				t.Fatalf("expected error for URI %q", uri)
			}
		})
	}
}

func TestParseGCSURI(t *testing.T) {
	cases := []struct {
		uri    string
		bucket string
		object string
		err    bool
	}{
		{"gs://b/o", "b", "o", false},
		{"gs://b/a/b/c.tar.gz", "b", "a/b/c.tar.gz", false},
		{"s3://b/o", "", "", true},
		{"gs://", "", "", true},
		{"gs://b", "", "", true},
		{"gs://b/", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.uri, func(t *testing.T) {
			b, o, err := parseGCSURI(tc.uri)
			if tc.err {
				if err == nil {
					t.Fatalf("want error for %q", tc.uri)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if b != tc.bucket || o != tc.object {
				t.Errorf("got (%q, %q), want (%q, %q)", b, o, tc.bucket, tc.object)
			}
		})
	}
}
