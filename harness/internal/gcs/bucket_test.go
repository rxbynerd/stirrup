package gcs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBucketAccessible_OK(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"my-bucket"}`))
	}))
	defer srv.Close()

	err := BucketAccessible(context.Background(), srv.Client(), BucketProbeOptions{
		Bucket:          "my-bucket",
		Bearer:          staticBearer("tok"),
		EndpointBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("BucketAccessible: unexpected error: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET (read-only)", gotMethod)
	}
	if !strings.HasSuffix(gotPath, "/storage/v1/b/my-bucket") {
		t.Errorf("path = %q, want bucket-metadata route", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q, want bearer token applied", gotAuth)
	}
}

func TestBucketAccessible_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"not found"}}`))
	}))
	defer srv.Close()

	err := BucketAccessible(context.Background(), srv.Client(), BucketProbeOptions{
		Bucket:          "missing",
		Bearer:          staticBearer("tok"),
		EndpointBaseURL: srv.URL,
	})
	if err == nil {
		t.Fatal("BucketAccessible: expected error for 404")
	}
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "missing") {
		t.Errorf("error should carry status and bucket name, got: %v", err)
	}
}

func TestBucketAccessible_RequiresBearer(t *testing.T) {
	err := BucketAccessible(context.Background(), http.DefaultClient, BucketProbeOptions{Bucket: "b"})
	if err == nil {
		t.Fatal("expected error when bearer is nil")
	}
}
