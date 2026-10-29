package resultsink

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/types"
)

const testTopic = "projects/acme-prod/topics/stirrup-results"

// tokenSource is a credential.Source that yields a fixed bearer token,
// or fails with err.
type tokenSource struct {
	token string
	err   error
}

func (s *tokenSource) Resolve(context.Context) (*credential.Resolved, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &credential.Resolved{
		BearerToken: func(context.Context) (string, error) { return s.token, nil },
	}, nil
}

// noRetryDelay keeps retry tests fast without changing the attempt count.
var noRetryDelay = []time.Duration{time.Millisecond, time.Millisecond}

// scriptedServer answers each request with the next status in statuses,
// repeating the last one, and records every request body.
type scriptedServer struct {
	mu       sync.Mutex
	statuses []int
	okBody   string
	requests []recordedRequest
}

type recordedRequest struct {
	method string
	path   string
	query  string
	auth   string
	ctype  string
	body   []byte
}

func (s *scriptedServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body bytes.Buffer
	_, _ = body.ReadFrom(r.Body)
	s.mu.Lock()
	s.requests = append(s.requests, recordedRequest{
		method: r.Method,
		path:   r.URL.EscapedPath(),
		query:  r.URL.RawQuery,
		auth:   r.Header.Get("Authorization"),
		ctype:  r.Header.Get("Content-Type"),
		body:   body.Bytes(),
	})
	status := s.statuses[min(len(s.requests), len(s.statuses))-1]
	s.mu.Unlock()
	w.WriteHeader(status)
	if status >= 200 && status < 300 {
		_, _ = w.Write([]byte(s.okBody))
		return
	}
	_, _ = w.Write([]byte("backend says no"))
}

func (s *scriptedServer) recorded() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedRequest(nil), s.requests...)
}

// captureWarnings routes slog's default logger to a buffer for the test.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	return &buf
}

func sampleResult() types.RunResult {
	return types.RunResult{
		SchemaVersion:      1,
		RunID:              "run-42",
		Outcome:            "success",
		Turns:              3,
		FinalAssistantText: "done <ok> & dusted",
	}
}

func TestDeliver_StopsOnPermanentStatus(t *testing.T) {
	must := require.New(t)
	attempts := 0
	err := deliver(context.Background(), "test", noRetryDelay, func(context.Context) error {
		attempts++
		return &statusError{StatusCode: http.StatusForbidden}
	})
	must.ErrorContains(err, "after 1 attempt(s)")
	must.Equal(1, attempts)
}

func TestDeliver_StopsWhenContextEnds(t *testing.T) {
	must := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	err := deliver(ctx, "test", []time.Duration{time.Hour, time.Hour}, func(context.Context) error {
		attempts++
		cancel()
		return errors.New("connection reset")
	})
	must.ErrorContains(err, "after 1 attempt(s)")
	must.Equal(1, attempts)
}

func TestDeliver_StopsWhenContextEndsDuringBackoff(t *testing.T) {
	must := require.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	attempts := 0
	start := time.Now()
	err := deliver(ctx, "test", []time.Duration{time.Hour, time.Hour}, func(context.Context) error {
		attempts++
		return errors.New("connection reset")
	})
	must.ErrorContains(err, "after 1 attempt(s)")
	must.Equal(1, attempts)
	must.Less(time.Since(start), time.Minute, "the backoff wait ends with the context")
}

func TestDefaultRetryDelays_AllowThreeAttempts(t *testing.T) {
	must := require.New(t)
	must.Len(defaultRetryDelays, 2)
	gcsSink, err := NewGCSSink(GCSSinkOptions{Bucket: "b", CredentialSource: &tokenSource{}})
	must.NoError(err)
	must.Equal(defaultRetryDelays, gcsSink.retryDelays)
	pubSubSink, err := NewPubSubSink(PubSubSinkOptions{Topic: testTopic, CredentialSource: &tokenSource{}})
	must.NoError(err)
	must.Equal(defaultRetryDelays, pubSubSink.retryDelays)
}

func TestResolveBearer_NilBearerIsPermanent(t *testing.T) {
	must := require.New(t)
	_, err := resolveBearer(context.Background(), nilBearerSource{})
	must.ErrorContains(err, "produced no bearer token")
	must.False(retryable(err))
}

// nilBearerSource resolves to a credential without a bearer token.
type nilBearerSource struct{}

func (nilBearerSource) Resolve(context.Context) (*credential.Resolved, error) {
	return &credential.Resolved{}, nil
}

func TestRetryable_ClassifiesStatuses(t *testing.T) {
	check := assert.New(t)
	for _, code := range []int{http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		check.True(retryable(&statusError{StatusCode: code}), "status %d", code)
	}
	for _, code := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		check.False(retryable(&statusError{StatusCode: code}), "status %d", code)
	}
	check.True(retryable(&statusError{StatusCode: http.StatusConflict}), "Pub/Sub ABORTED")
	check.True(retryable(&statusError{StatusCode: 499}), "Pub/Sub CANCELLED")
	check.True(retryable(errors.New("dial tcp: connection refused")))
	check.False(retryable(&permanentError{errors.New("malformed response")}))
}
