package resultsink

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/types"
)

func newTestGCSSink(t *testing.T, srv *httptest.Server, prefix string) *GCSSink {
	t.Helper()
	sink, err := NewGCSSink(GCSSinkOptions{
		Bucket:           "stirrup-results",
		ObjectPrefix:     prefix,
		CredentialSource: &tokenSource{token: "test-token"},
		HTTPClient:       srv.Client(),
		EndpointBaseURL:  srv.URL,
	})
	require.New(t).NoError(err)
	sink.retryDelays = noRetryDelay
	return sink
}

func TestGCSSink_Emit_UploadsRunResultObject(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	logs := captureWarnings(t)
	backend := &scriptedServer{statuses: []int{http.StatusOK}, okBody: `{"name":"x"}`}
	srv := httptest.NewServer(backend)
	defer srv.Close()

	result := sampleResult()
	must.NoError(newTestGCSSink(t, srv, "results/").Emit(context.Background(), result))

	reqs := backend.recorded()
	must.Len(reqs, 1)
	req := reqs[0]
	check.Equal(http.MethodPost, req.method)
	check.Equal("/upload/storage/v1/b/stirrup-results/o", req.path)
	check.Contains(req.query, "name=results/run-42.result.json")
	check.Contains(req.query, "ifGenerationMatch=0", "the upload never replaces an existing object")
	check.Equal("Bearer test-token", req.auth)
	check.Equal("application/json", req.ctype)

	var decoded types.RunResult
	must.NoError(json.Unmarshal(req.body, &decoded))
	check.Equal(result, decoded)
	check.Contains(string(req.body), "<ok> & dusted", "HTML escaping is disabled, as on the stdout-json line")
	check.Contains(logs.String(), "gs://stirrup-results/results/run-42.result.json", "a successful upload logs its destination")
}

func TestGCSSink_Emit_RetriesTransientFailureThenSucceeds(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	logs := captureWarnings(t)
	backend := &scriptedServer{statuses: []int{http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusOK}}
	srv := httptest.NewServer(backend)
	defer srv.Close()

	must.NoError(newTestGCSSink(t, srv, "").Emit(context.Background(), sampleResult()))

	reqs := backend.recorded()
	must.Len(reqs, 3)
	check.Equal(reqs[0].body, reqs[2].body, "every attempt uploads the same bytes")
	check.Contains(reqs[2].query, "name=run-42.result.json")
	check.Contains(logs.String(), "delivery attempt failed; retrying")
}

func TestGCSSink_Emit_ReturnsErrorAfterExhaustingAttempts(t *testing.T) {
	must := require.New(t)
	captureWarnings(t)
	backend := &scriptedServer{statuses: []int{http.StatusInternalServerError}}
	srv := httptest.NewServer(backend)
	defer srv.Close()

	err := newTestGCSSink(t, srv, "").Emit(context.Background(), sampleResult())

	must.ErrorContains(err, "after 3 attempt(s)")
	must.ErrorContains(err, "gs://stirrup-results/run-42.result.json")
	must.ErrorContains(err, "HTTP 500")
	must.Len(backend.recorded(), 3)
}

func TestGCSSink_Emit_DoesNotRetryPermanentFailure(t *testing.T) {
	must := require.New(t)
	backend := &scriptedServer{statuses: []int{http.StatusForbidden}}
	srv := httptest.NewServer(backend)
	defer srv.Close()

	err := newTestGCSSink(t, srv, "").Emit(context.Background(), sampleResult())

	must.ErrorContains(err, "after 1 attempt(s)")
	must.ErrorContains(err, "HTTP 403")
	must.Len(backend.recorded(), 1)
}

func TestGCSSink_Emit_RetriesCredentialFailure(t *testing.T) {
	must := require.New(t)
	captureWarnings(t)
	backend := &scriptedServer{statuses: []int{http.StatusOK}}
	srv := httptest.NewServer(backend)
	defer srv.Close()
	sink := newTestGCSSink(t, srv, "")
	sink.credentialSource = &tokenSource{err: errors.New("metadata server unavailable")}

	err := sink.Emit(context.Background(), sampleResult())

	must.ErrorContains(err, "after 3 attempt(s)")
	must.ErrorContains(err, "metadata server unavailable")
	must.Empty(backend.recorded(), "no request is sent without a credential")
}

func TestGCSSink_Emit_RejectsResultWithoutRunID(t *testing.T) {
	must := require.New(t)
	backend := &scriptedServer{statuses: []int{http.StatusOK}}
	srv := httptest.NewServer(backend)
	defer srv.Close()

	err := newTestGCSSink(t, srv, "").Emit(context.Background(), types.RunResult{Outcome: "internal-error"})

	must.ErrorContains(err, "no runId")
	must.Empty(backend.recorded())
}

func TestGCSSink_Emit_RetriesDroppedConnection(t *testing.T) {
	must := require.New(t)
	captureWarnings(t)
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	must.NoError(newTestGCSSink(t, srv, "").Emit(context.Background(), sampleResult()))
	must.Equal(int32(2), attempts.Load())
}

func TestGCSSink_Emit_RejectsExistingObject(t *testing.T) {
	must := require.New(t)
	backend := &scriptedServer{statuses: []int{http.StatusPreconditionFailed}}
	srv := httptest.NewServer(backend)
	defer srv.Close()

	err := newTestGCSSink(t, srv, "").Emit(context.Background(), sampleResult())

	must.ErrorContains(err, "after 1 attempt(s)")
	must.ErrorContains(err, "object already exists")
	must.Len(backend.recorded(), 1)
}

func TestGCSSink_Emit_ReportsExistingObjectAfterDroppedAttemptAsUnconfirmed(t *testing.T) {
	must := require.New(t)
	captureWarnings(t)
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		w.WriteHeader(http.StatusPreconditionFailed)
	}))
	defer srv.Close()

	err := newTestGCSSink(t, srv, "").Emit(context.Background(), sampleResult())

	must.ErrorContains(err, "probably this run's result, but that cannot be confirmed")
	must.Equal(int32(2), attempts.Load())
}

func TestGCSSink_Emit_RejectsExistingObjectAfterTokenFetchFailure(t *testing.T) {
	must := require.New(t)
	captureWarnings(t)
	backend := &scriptedServer{statuses: []int{http.StatusPreconditionFailed}}
	srv := httptest.NewServer(backend)
	defer srv.Close()
	sink := newTestGCSSink(t, srv, "")
	sink.credentialSource = &failingTokenSource{failures: 1}

	err := sink.Emit(context.Background(), sampleResult())

	must.ErrorContains(err, "written by another writer", "an attempt that sent nothing cannot have written the object")
	must.Len(backend.recorded(), 1)
}

func TestIsDialError_RecognisesRefusedConnection(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	_, err := http.Get(url) //nolint:noctx // a refused dial needs no context
	must.Error(err)
	must.True(isDialError(err))
	must.False(isDialError(errors.New("connection reset by peer")))
}

// failingTokenSource resolves to a bearer whose token fetch fails for the
// first failures calls, as a metadata-server outage does.
type failingTokenSource struct {
	mu       sync.Mutex
	failures int
}

func (s *failingTokenSource) Resolve(context.Context) (*credential.Resolved, error) {
	return &credential.Resolved{BearerToken: func(context.Context) (string, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.failures > 0 {
			s.failures--
			return "", errors.New("metadata server unavailable")
		}
		return "t", nil
	}}, nil
}

func TestGCSSink_Emit_RejectsExistingObjectAfterAnsweredAttempt(t *testing.T) {
	must := require.New(t)
	captureWarnings(t)
	backend := &scriptedServer{statuses: []int{http.StatusServiceUnavailable, http.StatusPreconditionFailed}}
	srv := httptest.NewServer(backend)
	defer srv.Close()

	err := newTestGCSSink(t, srv, "").Emit(context.Background(), sampleResult())

	must.ErrorContains(err, "object already exists", "a 503 answer means the first attempt wrote nothing")
	must.Len(backend.recorded(), 2)
}

func TestNewGCSSink_RejectsInvalidOptions(t *testing.T) {
	must := require.New(t)
	_, err := NewGCSSink(GCSSinkOptions{CredentialSource: &tokenSource{}})
	must.ErrorContains(err, "bucket is required")
	_, err = NewGCSSink(GCSSinkOptions{Bucket: "b"})
	must.ErrorContains(err, "credential source is required")
}

func TestResultObjectName_JoinsPrefixAndRunID(t *testing.T) {
	check := assert.New(t)
	check.Equal("run-1.result.json", resultObjectName("", "run-1"))
	check.Equal("results/run-1.result.json", resultObjectName("results", "run-1"))
	check.Equal("results/run-1.result.json", resultObjectName("results/", "run-1"))
}
