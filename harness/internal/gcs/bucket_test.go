package gcs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBucketAccessible_GetsBucketMetadataWithBearer(t *testing.T) {
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
	require.NoError(t, err)

	assert.Equal(t, http.MethodGet, gotMethod, "probe must be read-only")
	assert.Truef(t, strings.HasSuffix(gotPath, "/storage/v1/b/my-bucket"),
		"path = %q, want bucket-metadata route", gotPath)
	assert.Equal(t, "Bearer tok", gotAuth, "bearer token must be applied")
}

func TestBucketAccessible_NotFoundErrorNamesStatusAndBucket(t *testing.T) {
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
	require.Error(t, err)
	require.ErrorContains(t, err, "404", "error should carry the HTTP status")
	require.ErrorContains(t, err, "missing", "error should carry the bucket name")
}

func TestBucketAccessible_RequiresBearer(t *testing.T) {
	err := BucketAccessible(context.Background(), http.DefaultClient, BucketProbeOptions{Bucket: "b"})
	require.Error(t, err, "bearer is nil")
}
