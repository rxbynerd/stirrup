package commandoutput

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestGCSUploaderStreamsArchiveBesideTracePrefix(t *testing.T) {
	var got []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		if r.URL.Query().Get("name") != "traces/run-1.command-output.tar.gz" {
			t.Errorf("name=%q", r.URL.Query().Get("name"))
		}
		var err error
		got, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	want := []byte("compressed archive bytes")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	uploader, err := NewGCSUploader(GCSUploaderOptions{
		Bucket: "bucket-name", ObjectPrefix: "traces/", EndpointBaseURL: server.URL,
		Bearer: func(context.Context) (string, error) { return "token", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	uri, err := uploader.UploadCommandOutputArchive(context.Background(), path, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if uri != "gs://bucket-name/traces/run-1.command-output.tar.gz" {
		t.Fatalf("uri=%q", uri)
	}
	if string(got) != string(want) {
		t.Fatalf("body=%q", got)
	}
}

func TestGCSUploaderNormalisesUnslashedPrefix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("name") != "traces/run-1.command-output.tar.gz" {
			t.Errorf("name=%q", r.URL.Query().Get("name"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := os.WriteFile(path, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	// ValidateRunConfig deliberately does not rewrite the configured
	// prefix; the uploader owns slash normalisation.
	uploader, err := NewGCSUploader(GCSUploaderOptions{
		Bucket: "bucket-name", ObjectPrefix: "traces", EndpointBaseURL: server.URL,
		Bearer: func(context.Context) (string, error) { return "token", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	uri, err := uploader.UploadCommandOutputArchive(context.Background(), path, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if uri != "gs://bucket-name/traces/run-1.command-output.tar.gz" {
		t.Fatalf("uri=%q", uri)
	}
}
