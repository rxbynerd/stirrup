package workspaceexport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGCSExporter_Probe_IssuesReadOnlyBucketGet(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
		check.Failf("unexpected request", "%s %s (probe must be read-only)", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer httpSrv.Close()

	exp, err := NewGCSExporter(GCSExporterOptions{
		CredentialSource: &staticBearerSource{token: "tok"},
		EndpointBaseURL:  httpSrv.URL,
	})
	must.NoError(err)
	must.NoError(exp.Probe(context.Background(), "gs://runs/run-1/workspace.tar.gz"))
	must.True(sawBucketGet, "Probe should issue a bucket-metadata GET")
}

func TestGCSExporter_Probe_RejectsNonGSURI(t *testing.T) {
	must := require.New(t)
	exp, _ := NewGCSExporter(GCSExporterOptions{
		CredentialSource: &staticBearerSource{token: "tok"},
	})
	err := exp.Probe(context.Background(), "s3://bucket/key")
	must.Error(err)
}

func TestGCSExporter_Probe_ReportsStatusForMissingBucket(t *testing.T) {
	must := require.New(t)
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
	must.Error(err)
	must.ErrorContains(err, "404", "error should carry the GCS status")
}
