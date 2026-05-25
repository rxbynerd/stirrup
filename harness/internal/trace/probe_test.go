package trace

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGCSTraceEmitter_Probe_OK(t *testing.T) {
	var sawBucketGet bool
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/storage/v1/b/") {
			sawBucketGet = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"name":"my-bucket"}`))
			return
		}
		// Any upload POST here would mean the probe is not read-only.
		t.Errorf("unexpected request %s %s (probe must be read-only)", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer httpSrv.Close()

	emitter, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
		Bucket:           "my-bucket",
		CredentialSource: &staticBearerSource{token: "tok"},
		EndpointBaseURL:  httpSrv.URL,
	})
	if err != nil {
		t.Fatalf("NewGCSTraceEmitter: %v", err)
	}
	if err := emitter.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: unexpected error: %v", err)
	}
	if !sawBucketGet {
		t.Error("Probe should issue a bucket-metadata GET")
	}
}

func TestGCSTraceEmitter_Probe_Denied(t *testing.T) {
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
	if err != nil {
		t.Fatalf("NewGCSTraceEmitter: %v", err)
	}
	err = emitter.Probe(context.Background())
	if err == nil {
		t.Fatal("Probe: expected error for 403, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should carry the GCS status, got: %v", err)
	}
}

func TestOTelTraceEmitter_Probe_NilProviderIsNoop(t *testing.T) {
	// A zero-value emitter (nil provider) must not panic; Probe is a no-op.
	emitter := &OTelTraceEmitter{}
	if err := emitter.Probe(context.Background()); err != nil {
		t.Fatalf("Probe with nil provider should be a no-op, got: %v", err)
	}
}
