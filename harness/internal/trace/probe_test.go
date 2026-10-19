package trace

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGCSTraceEmitter_Probe_ReadsBucketMetadata(t *testing.T) {
	var sawBucketGet bool
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/storage/v1/b/") {
			sawBucketGet = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"name":"my-bucket"}`))
			return
		}
		// Any upload POST here would mean the probe is not read-only.
		assert.Failf(t, "unexpected request", "%s %s (probe must be read-only)", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer httpSrv.Close()

	emitter, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
		Bucket:           "my-bucket",
		CredentialSource: &staticBearerSource{token: "tok"},
		EndpointBaseURL:  httpSrv.URL,
	})
	require.NoError(t, err)
	require.NoError(t, emitter.Probe(context.Background()))
	require.True(t, sawBucketGet, "Probe should issue a bucket-metadata GET")
}

func TestGCSTraceEmitter_Probe_ReturnsErrorOnForbidden(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"permission denied"}}`))
	}))
	defer httpSrv.Close()

	emitter, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
		Bucket:           "denied-bucket",
		CredentialSource: &staticBearerSource{token: "tok"},
		EndpointBaseURL:  httpSrv.URL,
	})
	require.NoError(t, err)

	err = emitter.Probe(context.Background())
	require.Error(t, err, "Probe should fail when GCS returns 403")
	require.ErrorContains(t, err, "403", "error should carry the GCS status")
}

func TestOTelTraceEmitter_Probe_NilProviderIsNoop(t *testing.T) {
	// A zero-value emitter (nil provider) must not panic; Probe is a no-op.
	emitter := &OTelTraceEmitter{}
	require.NoError(t, emitter.Probe(context.Background()), "Probe with nil provider should be a no-op")
}
