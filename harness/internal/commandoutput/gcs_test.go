package commandoutput

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGCSUploader_StreamsArchiveBesideTracePrefix(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	var got []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The handler runs on the server goroutine, so it uses assert rather than require.
		check.Equal("Bearer token", r.Header.Get("Authorization"))
		check.Equal("traces/run-1.command-output.tar.gz", r.URL.Query().Get("name"))
		body, err := io.ReadAll(r.Body)
		check.NoError(err)
		got = body
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	want := []byte("compressed archive bytes")
	must.NoError(os.WriteFile(path, want, 0o600))

	uploader, err := NewGCSUploader(GCSUploaderOptions{
		Bucket: "bucket-name", ObjectPrefix: "traces/", EndpointBaseURL: server.URL,
		Bearer: func(context.Context) (string, error) { return "token", nil },
	})
	must.NoError(err)

	uri, err := uploader.UploadCommandOutputArchive(context.Background(), path, "run-1")
	must.NoError(err)
	must.Equal("gs://bucket-name/traces/run-1.command-output.tar.gz", uri)
	must.Equal(string(want), string(got))
}

func TestGCSUploader_NormalisesUnslashedPrefix(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The handler runs on the server goroutine, so it uses assert rather than require.
		check.Equal("traces/run-1.command-output.tar.gz", r.URL.Query().Get("name"))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	must.NoError(os.WriteFile(path, []byte("bytes"), 0o600))

	// ValidateRunConfig deliberately does not rewrite the configured
	// prefix; the uploader owns slash normalisation.
	uploader, err := NewGCSUploader(GCSUploaderOptions{
		Bucket: "bucket-name", ObjectPrefix: "traces", EndpointBaseURL: server.URL,
		Bearer: func(context.Context) (string, error) { return "token", nil },
	})
	must.NoError(err)

	uri, err := uploader.UploadCommandOutputArchive(context.Background(), path, "run-1")
	must.NoError(err)
	must.Equal("gs://bucket-name/traces/run-1.command-output.tar.gz", uri)
}
