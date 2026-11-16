package resultsink

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

const resultSignedQuery = "X-Amz-Signature=deadbeefcafe"

func newTestHTTPSPutSink(t *testing.T, srv *httptest.Server, secrets tableSecrets) *HTTPSPutSink {
	t.Helper()
	secrets["secret://RESULT_URL"] = srv.URL + "/bucket/result.json?" + resultSignedQuery
	secrets["secret://RESULT_AUTH"] = "Bearer result-token"
	sink, err := NewHTTPSPutSink(HTTPSPutSinkOptions{
		Target: &types.HTTPSPutConfig{
			URLRef:  "secret://RESULT_URL",
			Headers: map[string]string{"Authorization": "secret://RESULT_AUTH"},
		},
		Secrets: secrets,
	})
	require.New(t).NoError(err)
	sink.retryDelays = noRetryDelay
	return sink
}

func TestHTTPSPutSink_Emit_PutsTheRunResultDocument(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	logs := captureWarnings(t)
	backend := &scriptedServer{statuses: []int{http.StatusOK}}
	srv := httptest.NewServer(backend)
	defer srv.Close()

	result := sampleResult()
	must.NoError(newTestHTTPSPutSink(t, srv, tableSecrets{}).Emit(context.Background(), result))

	reqs := backend.recorded()
	must.Len(reqs, 1)
	req := reqs[0]
	check.Equal(http.MethodPut, req.method)
	check.Equal("/bucket/result.json", req.path)
	check.Equal(resultSignedQuery, req.query)
	check.Equal("Bearer result-token", req.auth)
	check.Equal("application/json", req.ctype)

	var decoded types.RunResult
	must.NoError(json.Unmarshal(req.body, &decoded))
	check.Equal(result, decoded)
	check.Contains(string(req.body), "<ok> & dusted", "HTML escaping is disabled, as on the stdout-json line")

	check.Contains(logs.String(), srv.URL, "a successful upload logs the destination's origin")
	check.NotContains(logs.String(), "deadbeefcafe")
	check.NotContains(logs.String(), "result.json")
	check.NotContains(logs.String(), "result-token")
}

func TestHTTPSPutSink_Emit_RetriesTransientStatuses(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	logs := captureWarnings(t)
	backend := &scriptedServer{statuses: []int{http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusOK}}
	srv := httptest.NewServer(backend)
	defer srv.Close()

	must.NoError(newTestHTTPSPutSink(t, srv, tableSecrets{}).Emit(context.Background(), sampleResult()))
	check.Len(backend.recorded(), 3)
	check.NotContains(logs.String(), "deadbeefcafe", "retry warnings must not carry the URL")
}

func TestHTTPSPutSink_Emit_PermanentStatusFailsAtOnceWithoutTheURL(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	backend := &scriptedServer{statuses: []int{http.StatusForbidden}}
	srv := httptest.NewServer(backend)
	defer srv.Close()

	err := newTestHTTPSPutSink(t, srv, tableSecrets{}).Emit(context.Background(), sampleResult())
	must.ErrorContains(err, "https-put result sink")
	must.ErrorContains(err, "HTTP 403")
	check.Len(backend.recorded(), 1)
	check.NotContains(err.Error(), "deadbeefcafe")
	check.NotContains(err.Error(), "result.json")
	check.NotContains(err.Error(), "backend says no", "the response body is discarded")
}

func TestHTTPSPutSink_Emit_GivesUpAfterTheLastAttempt(t *testing.T) {
	must := require.New(t)
	backend := &scriptedServer{statuses: []int{http.StatusBadGateway}}
	srv := httptest.NewServer(backend)
	defer srv.Close()

	err := newTestHTTPSPutSink(t, srv, tableSecrets{}).Emit(context.Background(), sampleResult())
	must.ErrorContains(err, "after 3 attempt(s)")
	must.Len(backend.recorded(), 3)
}

func TestHTTPSPutSink_Emit_UnresolvedReferenceFailsWithoutARequest(t *testing.T) {
	must := require.New(t)
	backend := &scriptedServer{statuses: []int{http.StatusOK}}
	srv := httptest.NewServer(backend)
	defer srv.Close()

	sink := newTestHTTPSPutSink(t, srv, tableSecrets{})
	sink.target = &types.HTTPSPutConfig{URLRef: "secret://RESULT_URL_NOT_SET"}
	err := sink.Emit(context.Background(), sampleResult())
	must.ErrorContains(err, "resolve urlRef")
	must.Empty(backend.recorded())
}

func TestHTTPSPutSink_Emit_ResolvesTheReferenceForEveryRun(t *testing.T) {
	must := require.New(t)
	backend := &scriptedServer{statuses: []int{http.StatusOK}}
	srv := httptest.NewServer(backend)
	defer srv.Close()
	secrets := tableSecrets{}
	sink := newTestHTTPSPutSink(t, srv, secrets)

	must.NoError(sink.Emit(context.Background(), sampleResult()))
	secrets["secret://RESULT_URL"] = srv.URL + "/bucket/follow-up.json"
	must.NoError(sink.Emit(context.Background(), sampleResult()))

	reqs := backend.recorded()
	must.Len(reqs, 2)
	must.Equal("/bucket/result.json", reqs[0].path)
	must.Equal("/bucket/follow-up.json", reqs[1].path)
}

func TestHTTPSPutSink_Emit_PreconditionFailedOnAFirstAttemptIsPermanent(t *testing.T) {
	must := require.New(t)
	backend := &scriptedServer{statuses: []int{http.StatusPreconditionFailed}}
	srv := httptest.NewServer(backend)
	defer srv.Close()

	err := newTestHTTPSPutSink(t, srv, tableSecrets{}).Emit(context.Background(), sampleResult())
	must.ErrorContains(err, "HTTP 412")
	must.NotContains(err.Error(), "probably this run's result")
	must.Len(backend.recorded(), 1)
}

func TestNewHTTPSPutSink_RequiresTargetAndSecrets(t *testing.T) {
	must := require.New(t)
	_, err := NewHTTPSPutSink(HTTPSPutSinkOptions{Secrets: tableSecrets{}})
	must.ErrorContains(err, "httpsPut is required")
	_, err = NewHTTPSPutSink(HTTPSPutSinkOptions{Target: &types.HTTPSPutConfig{URLRef: "secret://U"}})
	must.ErrorContains(err, "secret store is required")
}
