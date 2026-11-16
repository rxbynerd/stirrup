package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

type tableSecrets map[string]string

func (s tableSecrets) Resolve(_ context.Context, ref string) (string, error) {
	v, ok := s[ref]
	if !ok {
		return "", fmt.Errorf("secret %q is not set", ref)
	}
	return v, nil
}

const traceSignedQuery = "?X-Amz-Signature=deadbeefcafe"

func newTestHTTPSPutTraceEmitter(t *testing.T, srv *gcsCaptureServer, secrets tableSecrets) (*ObjectTraceEmitter, string) {
	t.Helper()
	httpSrv := httptest.NewServer(srv.handler())
	t.Cleanup(httpSrv.Close)
	secrets["secret://TRACE_URL"] = httpSrv.URL + "/bucket/trace.jsonl" + traceSignedQuery
	secrets["secret://TRACE_AUTH"] = "Bearer trace-token"
	emitter, err := NewHTTPSPutTraceEmitter(HTTPSPutTraceEmitterOptions{
		Target: &types.HTTPSPutConfig{
			URLRef:  "secret://TRACE_URL",
			Headers: map[string]string{"Authorization": "secret://TRACE_AUTH"},
		},
		Secrets: secrets,
	})
	require.New(t).NoError(err)
	return emitter, httpSrv.URL
}

func traceRunConfig(runID string) *types.RunConfig {
	timeout := 60
	return &types.RunConfig{
		RunID:    runID,
		Mode:     "execution",
		Provider: types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://K"},
		TraceEmitter: types.TraceEmitterConfig{
			Type:     "https-put",
			HTTPSPut: &types.HTTPSPutConfig{URLRef: "secret://TRACE_URL", Headers: map[string]string{"Authorization": "secret://TRACE_AUTH"}},
		},
		Timeout: &timeout,
	}
}

func TestHTTPSPutTraceEmitter_FinishPutsTheRunSummary(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv := newGCSCaptureServer()
	emitter, _ := newTestHTTPSPutTraceEmitter(t, srv, tableSecrets{})

	emitter.Start("run-abc", traceRunConfig("run-abc"))
	emitter.RecordTurn(types.TurnTrace{Turn: 1, Tokens: types.TokenUsage{Input: 50, Output: 25}})
	tr, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)
	check.Equal("run-abc", tr.ID)
	check.Equal(1, tr.Turns)

	got := srv.last()
	check.Equal(http.MethodPut, got.Method)
	check.Equal("/bucket/trace.jsonl"+traceSignedQuery, got.URL)
	check.Equal("Bearer trace-token", got.Auth)
	check.Equal("application/x-ndjson", got.ContentType)

	body := strings.TrimRight(string(got.Body), "\n")
	check.NotContains(body, "\n", "the trace is one JSON line")
	var decoded types.RunTrace
	must.NoError(json.Unmarshal([]byte(body), &decoded), "body=%q", body)
	check.Equal("run-abc", decoded.ID)
	check.Equal("success", decoded.Outcome)

	must.NotNil(decoded.Config.TraceEmitter.HTTPSPut)
	check.Equal("secret://[REDACTED]", decoded.Config.TraceEmitter.HTTPSPut.URLRef)
	check.Equal(map[string]string{"Authorization": "secret://[REDACTED]"}, decoded.Config.TraceEmitter.HTTPSPut.Headers)
	for _, capability := range []string{"deadbeefcafe", "trace-token", "trace.jsonl"} {
		check.NotContains(body, capability, "the uploaded trace must not carry its own destination")
	}
}

func TestHTTPSPutTraceEmitter_RejectedUploadReturnsTheTraceAndAnErrorWithoutTheURL(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv := newGCSCaptureServer()
	srv.statusCode = http.StatusForbidden
	srv.respondWith = "<Error><Code>AccessDenied</Code><Message>deadbeefcafe</Message></Error>"
	emitter, origin := newTestHTTPSPutTraceEmitter(t, srv, tableSecrets{})

	emitter.Start("run-abc", traceRunConfig("run-abc"))
	tr, err := emitter.Finish(context.Background(), "success")
	must.ErrorContains(err, "https-put trace emitter: upload trace: put to "+origin)
	must.ErrorContains(err, "HTTP 403 Forbidden (AccessDenied)")
	must.NotNil(tr, "the trace is returned even when its upload fails")
	check.Equal("run-abc", tr.ID)
	check.NotContains(err.Error(), "deadbeefcafe")
	check.NotContains(err.Error(), "trace.jsonl")
}

func TestHTTPSPutTraceEmitter_EachRunUploadsToTheCurrentURL(t *testing.T) {
	must := require.New(t)
	srv := newGCSCaptureServer()
	secrets := tableSecrets{}
	emitter, origin := newTestHTTPSPutTraceEmitter(t, srv, secrets)

	emitter.Start("run-1", traceRunConfig("run-1"))
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)
	must.Equal("/bucket/trace.jsonl"+traceSignedQuery, srv.last().URL)

	secrets["secret://TRACE_URL"] = origin + "/bucket/follow-up.jsonl"
	emitter.Start("run-2", traceRunConfig("run-2"))
	_, err = emitter.Finish(context.Background(), "success")
	must.NoError(err)
	got := srv.last()
	must.Equal("/bucket/follow-up.jsonl", got.URL)
	must.Contains(string(got.Body), `"id":"run-2"`)
}

func TestHTTPSPutTraceEmitter_UnresolvedReferenceFailsTheUpload(t *testing.T) {
	must := require.New(t)
	srv := newGCSCaptureServer()
	secrets := tableSecrets{}
	emitter, _ := newTestHTTPSPutTraceEmitter(t, srv, secrets)
	delete(secrets, "secret://TRACE_URL")

	emitter.Start("run-abc", traceRunConfig("run-abc"))
	_, err := emitter.Finish(context.Background(), "success")
	must.ErrorContains(err, "https-put trace emitter: resolve urlRef")
	must.Empty(srv.last().URL)
}

func TestHTTPSPutTraceEmitter_ProbeResolvesWithoutContactingTheDestination(t *testing.T) {
	must := require.New(t)
	srv := newGCSCaptureServer()
	secrets := tableSecrets{}
	emitter, _ := newTestHTTPSPutTraceEmitter(t, srv, secrets)

	must.NoError(emitter.Probe(context.Background()))
	must.Empty(srv.last().URL, "a probe must send nothing")

	secrets["secret://TRACE_URL"] = "http://uploads.example/trace.jsonl" + traceSignedQuery
	err := emitter.Probe(context.Background())
	must.ErrorContains(err, "accepted only for a loopback host")
	must.NotContains(err.Error(), "deadbeefcafe")
}

func TestNewHTTPSPutTraceEmitter_RequiresTargetAndSecrets(t *testing.T) {
	must := require.New(t)
	_, err := NewHTTPSPutTraceEmitter(HTTPSPutTraceEmitterOptions{Secrets: tableSecrets{}})
	must.ErrorContains(err, "httpsPut is required")
	_, err = NewHTTPSPutTraceEmitter(HTTPSPutTraceEmitterOptions{Target: &types.HTTPSPutConfig{URLRef: "secret://U"}})
	must.ErrorContains(err, "secret store is required")
}
