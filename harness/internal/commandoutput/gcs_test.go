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
	var got []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The handler runs on the server goroutine, so it uses assert rather than require.
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))
		assert.Equal(t, "traces/run-1.command-output.tar.gz", r.URL.Query().Get("name"))
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		got = body
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	want := []byte("compressed archive bytes")
	require.NoError(t, os.WriteFile(path, want, 0o600))

	uploader, err := NewGCSUploader(GCSUploaderOptions{
		Bucket: "bucket-name", ObjectPrefix: "traces/", EndpointBaseURL: server.URL,
		Bearer: func(context.Context) (string, error) { return "token", nil },
	})
	require.NoError(t, err)

	uri, err := uploader.UploadCommandOutputArchive(context.Background(), path, "run-1")
	require.NoError(t, err)
	require.Equal(t, "gs://bucket-name/traces/run-1.command-output.tar.gz", uri)
	require.Equal(t, string(want), string(got))
}

func TestGCSUploader_NormalisesUnslashedPrefix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The handler runs on the server goroutine, so it uses assert rather than require.
		assert.Equal(t, "traces/run-1.command-output.tar.gz", r.URL.Query().Get("name"))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	require.NoError(t, os.WriteFile(path, []byte("bytes"), 0o600))

	// ValidateRunConfig deliberately does not rewrite the configured
	// prefix; the uploader owns slash normalisation.
	uploader, err := NewGCSUploader(GCSUploaderOptions{
		Bucket: "bucket-name", ObjectPrefix: "traces", EndpointBaseURL: server.URL,
		Bearer: func(context.Context) (string, error) { return "token", nil },
	})
	require.NoError(t, err)

	uri, err := uploader.UploadCommandOutputArchive(context.Background(), path, "run-1")
	require.NoError(t, err)
	require.Equal(t, "gs://bucket-name/traces/run-1.command-output.tar.gz", uri)
}
