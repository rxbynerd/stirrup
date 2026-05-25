package workspaceexport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGCSExporter_Probe_OK(t *testing.T) {
	var sawBucketGet bool
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/storage/v1/b/") {
			sawBucketGet = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"name":"runs"}`))
			return
		}
		// An upload POST during a probe would mean the probe is not
		// read-only — fail loudly.
		t.Errorf("unexpected request %s %s (probe must be read-only)", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer httpSrv.Close()

	exp, err := NewGCSExporter(GCSExporterOptions{
		CredentialSource: &staticBearerSource{token: "tok"},
		EndpointBaseURL:  httpSrv.URL,
	})
	if err != nil {
		t.Fatalf("NewGCSExporter: %v", err)
	}
	if err := exp.Probe(context.Background(), "gs://runs/run-1/workspace.tar.gz"); err != nil {
		t.Fatalf("Probe: unexpected error: %v", err)
	}
	if !sawBucketGet {
		t.Error("Probe should issue a bucket-metadata GET")
	}
}

func TestGCSExporter_Probe_BadURI(t *testing.T) {
	exp, _ := NewGCSExporter(GCSExporterOptions{
		CredentialSource: &staticBearerSource{token: "tok"},
	})
	err := exp.Probe(context.Background(), "s3://bucket/key")
	if err == nil {
		t.Fatal("Probe: expected error for non-gs:// URI")
	}
}

func TestGCSExporter_Probe_BucketDenied(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"bucket not found"}}`))
	}))
	defer httpSrv.Close()

	exp, _ := NewGCSExporter(GCSExporterOptions{
		CredentialSource: &staticBearerSource{token: "tok"},
		EndpointBaseURL:  httpSrv.URL,
	})
	err := exp.Probe(context.Background(), "gs://missing/run-1/workspace.tar.gz")
	if err == nil {
		t.Fatal("Probe: expected error for missing bucket")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should carry the GCS status, got: %v", err)
	}
}
