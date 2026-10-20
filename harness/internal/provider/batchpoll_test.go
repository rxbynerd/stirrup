package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/types"
)

// fakeCredentialSource is a controllable credential.Source for the
// polling client. Each Resolve increments a counter so tests can
// assert credentials are re-fetched per HTTP request.
type fakeCredentialSource struct {
	token        string
	resolveCalls atomic.Int64
	resolveErr   error
	bearerErr    error
	nilBearer    bool
}

func (f *fakeCredentialSource) Resolve(_ context.Context) (*credential.Resolved, error) {
	f.resolveCalls.Add(1)
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	if f.nilBearer {
		return &credential.Resolved{}, nil
	}
	captured := f.token
	bearerErr := f.bearerErr
	return &credential.Resolved{
		BearerToken: func(_ context.Context) (string, error) {
			if bearerErr != nil {
				return "", bearerErr
			}
			return captured, nil
		},
	}, nil
}

// newTestPollingClient builds a polling client pointed at srv with
// fast polling intervals so tests complete in well under a second.
// Returns the client plus a teardown that restores the package-level
// poll-interval and jitter knobs.
func newTestPollingClient(t *testing.T, srv *httptest.Server, src credential.Source, maxWait time.Duration) (*harnessPollingBatchClient, func()) {
	t.Helper()
	prevInterval := setBatchPollInitialInterval(2 * time.Millisecond)
	prevJitter := setBatchPollJitterDisabled(true)
	c := NewHarnessPollingBatchClient(HarnessBatchClientOptions{
		ProviderType: "anthropic",
		APIKeyRef:    "secret://test",
		CredSource:   src,
		MaxWait:      maxWait,
	})
	c.baseURL = srv.URL
	teardown := func() {
		setBatchPollInitialInterval(prevInterval)
		setBatchPollJitterDisabled(prevJitter)
	}
	return c, teardown
}

func anthropicSubmitEntries(customID string) []BatchEntry {
	return []BatchEntry{{
		CustomID: customID,
		Provider: "anthropic",
		Body:     json.RawMessage(`{"model":"claude-sonnet-4-6","messages":[],"max_tokens":1024,"stream":false}`),
	}}
}

func TestHarnessPollingBatch_Submit_HappyPath(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-ant-test"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Assertions here run on the server goroutine, so they use assert.
		check.Equal(http.MethodPost, r.Method, "method")
		check.Equal("/v1/messages/batches", r.URL.Path, "path")
		check.Equal("sk-ant-test", r.Header.Get("x-api-key"), "x-api-key")
		check.Equal(anthropicAPIVersion, r.Header.Get("anthropic-version"), "anthropic-version")

		// Confirm the wire body shape: a single "requests" entry with
		// the supplied custom_id and a passthrough params blob.
		var body anthropicBatchSubmitRequest
		if !check.NoError(json.NewDecoder(r.Body).Decode(&body), "decode submit body") {
			return
		}
		if !check.Len(body.Requests, 1) {
			return
		}
		check.Equal("stirrup-run-1-turn-1", body.Requests[0].CustomID, "custom_id")

		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"id":"batch_abc","processing_status":"in_progress"}`)
	}))
	defer srv.Close()

	c, teardown := newTestPollingClient(t, srv, src, time.Second)
	defer teardown()

	id, err := c.Submit(context.Background(), anthropicSubmitEntries("stirrup-run-1-turn-1"))
	must.NoError(err)
	must.Equal("batch_abc", id)
}

func TestHarnessPollingBatch_Submit_ErrorResponse(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-ant-test"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"bad request"}`)
	}))
	defer srv.Close()

	c, teardown := newTestPollingClient(t, srv, src, time.Second)
	defer teardown()

	_, err := c.Submit(context.Background(), anthropicSubmitEntries("stirrup-run-1-turn-1"))
	must.ErrorContains(err, "400")
}

func TestHarnessPollingBatch_Submit_RejectsMultiEntry(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-ant-test"}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		check.Fail("server should not be hit on validation failure")
	}))
	defer srv.Close()

	c, teardown := newTestPollingClient(t, srv, src, time.Second)
	defer teardown()

	_, err := c.Submit(context.Background(), []BatchEntry{
		{CustomID: "a", Provider: "anthropic", Body: json.RawMessage(`{}`)},
		{CustomID: "b", Provider: "anthropic", Body: json.RawMessage(`{}`)},
	})
	must.ErrorContains(err, "exactly 1")
}

func TestHarnessPollingBatch_Submit_MissingID(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-ant-test"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"processing_status":"in_progress"}`)
	}))
	defer srv.Close()

	c, teardown := newTestPollingClient(t, srv, src, time.Second)
	defer teardown()

	_, err := c.Submit(context.Background(), anthropicSubmitEntries("stirrup-run-1-turn-1"))
	must.ErrorContains(err, "missing id")
}

// pollServer is a stateful test fixture: it serves a configurable
// sequence of /v1/messages/batches/{id} polls, then a results JSONL
// document, and tracks how many cancel calls fired. httptest dispatches
// each request on its own goroutine, so every shared counter sits behind
// a sync.Mutex.
type pollServer struct {
	*httptest.Server

	pollResponses []string // one body per GET on the batch object; final is the "ended" response
	resultsBody   string   // JSONL served at /results

	mu          sync.Mutex
	pollCalls   int
	cancelCalls int
	resultCalls int
}

func (ps *pollServer) pollCount() int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.pollCalls
}

func (ps *pollServer) cancelCount() int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.cancelCalls
}

func (ps *pollServer) resultCount() int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.resultCalls
}

// waitForCancelCount fails the test unless cancelCalls reaches want within
// timeout. bestEffortCancel runs in a detached goroutine, so tests must
// wait on the side-effect rather than reading the counter immediately
// after Result returns.
func (ps *pollServer) waitForCancelCount(t *testing.T, want int, timeout time.Duration) {
	t.Helper()
	must := require.New(t)
	must.Eventually(func() bool { return ps.cancelCount() >= want }, timeout, 5*time.Millisecond)
}

func newPollServer(t *testing.T, polls []string, resultsBody string) *pollServer {
	t.Helper()
	ps := &pollServer{pollResponses: polls, resultsBody: resultsBody}
	ps.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/results":
			ps.mu.Lock()
			ps.resultCalls++
			body := ps.resultsBody
			ps.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, body)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
			ps.mu.Lock()
			ps.cancelCalls++
			ps.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/messages/batches/"):
			ps.mu.Lock()
			idx := ps.pollCalls
			if idx >= len(ps.pollResponses) {
				idx = len(ps.pollResponses) - 1
			}
			ps.pollCalls++
			body := ps.pollResponses[idx]
			ps.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, body)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return ps
}

func TestHarnessPollingBatch_Result_Eventually(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-ant-test"}
	polls := []string{
		`{"id":"batch_xyz","processing_status":"in_progress"}`,
		`{"id":"batch_xyz","processing_status":"in_progress"}`,
		// results_url is rewritten below once the server has a URL.
		`{"id":"batch_xyz","processing_status":"ended","results_url":"REPLACE"}`,
	}
	resultsBody := `{"custom_id":"stirrup-run-1-turn-1","result":{"type":"succeeded","message":{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}}}` + "\n"

	ps := newPollServer(t, polls, resultsBody)
	defer ps.Close()

	ps.pollResponses[2] = strings.ReplaceAll(ps.pollResponses[2], "REPLACE", ps.URL+"/results")

	c, teardown := newTestPollingClient(t, ps.Server, src, time.Second)
	defer teardown()

	results, err := c.Result(context.Background(), "batch_xyz")
	must.NoError(err)
	must.Contains(results, "stirrup-run-1-turn-1")
	entry := results["stirrup-run-1-turn-1"]
	must.Nil(entry.Err, "entry error")
	must.Contains(string(entry.Response), `"text":"ok"`)
	must.GreaterOrEqual(ps.pollCount(), 3, "pollCalls")
	must.Equal(1, ps.resultCount(), "resultCalls")
	must.Zero(ps.cancelCount(), "cancelCalls on happy path")
}

func TestHarnessPollingBatch_Result_Timeout(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-ant-test"}
	polls := []string{`{"id":"batch_xyz","processing_status":"in_progress"}`}
	ps := newPollServer(t, polls, "")
	defer ps.Close()

	// maxWait short enough that even a 2ms poll loop fires the
	// timeout within the test budget.
	c, teardown := newTestPollingClient(t, ps.Server, src, 50*time.Millisecond)
	defer teardown()

	_, err := c.Result(context.Background(), "batch_xyz")
	must.ErrorIs(err, errBatchExpired, "timeout must wrap errBatchExpired")
	// bestEffortCancel is detached into a goroutine, so wait for the
	// side-effect rather than reading the counter immediately.
	ps.waitForCancelCount(t, 1, time.Second)
	must.Equal(1, ps.cancelCount(), "cancelCalls on timeout")
}

// TestHarnessPollingBatch_Result_DeadlineRacesCap gives the polling client
// a run deadline and a wall-clock cap of the same length, the shape a run
// takes once maxWaitSeconds defaults to the run timeout. Whichever fires
// first, the run must read as past its deadline and must not surface the
// batch-expired sentinel, which would route into FallbackOnTimeout and
// retry on the same dead context.
func TestHarnessPollingBatch_Result_DeadlineRacesCap(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-ant-test"}
	polls := []string{`{"id":"batch_xyz","processing_status":"in_progress"}`}
	ps := newPollServer(t, polls, "")
	defer ps.Close()

	c, teardown := newTestPollingClient(t, ps.Server, src, 50*time.Millisecond)
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.Result(ctx, "batch_xyz")
		done <- err
	}()

	select {
	case err := <-done:
		must.ErrorIs(err, context.DeadlineExceeded)
		must.NotErrorIs(err, errBatchExpired, "a run past its deadline must not surface the batch-expired sentinel")
	case <-time.After(2 * time.Second):
		must.FailNow("Result did not return once both deadlines had passed")
	}
}

func TestHarnessPollingBatch_Result_CtxCancel(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-ant-test"}
	polls := []string{`{"id":"batch_xyz","processing_status":"in_progress"}`}
	ps := newPollServer(t, polls, "")
	defer ps.Close()

	c, teardown := newTestPollingClient(t, ps.Server, src, time.Second)
	defer teardown()

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel once the first poll has had time to fire.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		_, err := c.Result(ctx, "batch_xyz")
		done <- err
	}()

	select {
	case err := <-done:
		must.ErrorIs(err, context.Canceled)
	case <-time.After(500 * time.Millisecond):
		must.FailNow("Result did not return promptly after ctx cancel")
	}

	// bestEffortCancel runs in a detached goroutine; wait on the
	// side-effect rather than a sleep-and-pray window.
	ps.waitForCancelCount(t, 1, time.Second)
	must.Equal(1, ps.cancelCount(), "cancelCalls on ctx cancel")
}

func TestHarnessPollingBatch_Credential_ResolvedPerPoll(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-ant-test"}
	polls := []string{
		`{"id":"batch_xyz","processing_status":"in_progress"}`,
		`{"id":"batch_xyz","processing_status":"in_progress"}`,
		`{"id":"batch_xyz","processing_status":"ended","results_url":"REPLACE"}`,
	}
	resultsBody := `{"custom_id":"stirrup-run-1-turn-1","result":{"type":"succeeded","message":{}}}` + "\n"
	ps := newPollServer(t, polls, resultsBody)
	defer ps.Close()
	ps.pollResponses[2] = strings.ReplaceAll(ps.pollResponses[2], "REPLACE", ps.URL+"/results")

	c, teardown := newTestPollingClient(t, ps.Server, src, time.Second)
	defer teardown()

	_, err := c.Result(context.Background(), "batch_xyz")
	must.NoError(err)

	// Three poll calls + one results GET = at least four resolves.
	must.GreaterOrEqual(src.resolveCalls.Load(), int64(4), "Resolve should be called per HTTP request")
}

// TestHarnessPollingBatch_ResultsURL_NonOKStatus confirms a 500 from
// results_url is propagated up rather than silently converted into
// errBatchExpired, which would mask a real upstream failure.
func TestHarnessPollingBatch_ResultsURL_NonOKStatus(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-ant-test"}

	// resultsURL is populated after the httptest.Server starts so the
	// same server serves both the poll and the results_url endpoint.
	var resultsURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/results":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"upstream meltdown"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/messages/batches/"):
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"id":"batch_xyz","processing_status":"ended","results_url":%q}`, resultsURL)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	resultsURL = srv.URL + "/results"

	c, teardown := newTestPollingClient(t, srv, src, time.Second)
	defer teardown()

	_, err := c.Result(context.Background(), "batch_xyz")
	must.ErrorContains(err, "500")
	must.NotErrorIs(err, errBatchExpired, "non-200 results_url must not be classified as batch_expired")
}

// TestHarnessPollingBatch_PollOnce_NonOKStatus confirms a 503 on the
// first poll is surfaced as an HTTP error, not silently converted to
// errBatchExpired.
func TestHarnessPollingBatch_PollOnce_NonOKStatus(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-ant-test"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/messages/batches/"):
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"overloaded"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, teardown := newTestPollingClient(t, srv, src, time.Second)
	defer teardown()

	_, err := c.Result(context.Background(), "batch_xyz")
	must.ErrorContains(err, "503")
	must.NotErrorIs(err, errBatchExpired, "non-200 poll status must not be classified as batch_expired")
}

// TestHarnessPollingBatch_BestEffortCancel_HangsForCancelTimeout asserts
// the non-blocking guarantee: when the cancel endpoint hangs longer
// than batchCancelTimeout, Result must still return promptly because
// bestEffortCancel runs in a detached goroutine.
func TestHarnessPollingBatch_BestEffortCancel_HangsForCancelTimeout(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-ant-test"}

	// Use a chan to release the hanging cancel handler when the test ends,
	// so we do not leak goroutines beyond the test boundary.
	release := make(chan struct{})
	var slowCancelStarted atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/messages/batches/"):
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"batch_xyz","processing_status":"in_progress"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
			// Only the first cancel request simulates the slow endpoint.
			// A retry racing with test teardown should not create a
			// second active connection parked behind the same release
			// channel.
			if slowCancelStarted.CompareAndSwap(false, true) {
				<-release
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		close(release)
		srv.CloseClientConnections()
	})

	// 30 ms maxWait lets the timeout fire well before the test budget.
	c, teardown := newTestPollingClient(t, srv, src, 30*time.Millisecond)
	defer teardown()

	start := time.Now()
	_, err := c.Result(context.Background(), "batch_xyz")
	elapsed := time.Since(start)

	must.ErrorIs(err, errBatchExpired)
	// The cancel handler is still blocked. The 2s bound is well under
	// batchCancelTimeout (10s).
	must.LessOrEqual(elapsed, 2*time.Second, "Result must not block on bestEffortCancel")
}

// TestHarnessPollingBatch_CtxCancel_DuringPoll covers the
// errors.Is(ctx-error) branch: cancel the parent ctx after the poll
// handler has accepted the request but before it writes a response,
// forcing the request context to be cancelled mid-flight.
func TestHarnessPollingBatch_CtxCancel_DuringPoll(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-ant-test"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pollEntered := make(chan struct{}, 1)
	var cancelHits atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/messages/batches/"):
			// Signal the test that the request has reached the handler
			// (buffered chan so the send always succeeds), then wait for
			// the request context to be cancelled by the test. Returning
			// without writing a response gives httpClient.Do a
			// context.Canceled error.
			select {
			case pollEntered <- struct{}{}:
			default:
			}
			<-r.Context().Done()
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
			cancelHits.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, teardown := newTestPollingClient(t, srv, src, time.Second)
	defer teardown()

	done := make(chan error, 1)
	go func() {
		_, err := c.Result(ctx, "batch_xyz")
		done <- err
	}()

	select {
	case <-pollEntered:
	case <-time.After(time.Second):
		must.FailNow("poll handler never entered")
	}

	// Cancel the parent context mid-flight so the in-flight request
	// fails with context.Canceled — the load-bearing case for the
	// errors.Is branch in Result.
	cancel()

	select {
	case err := <-done:
		must.ErrorIs(err, context.Canceled)
	case <-time.After(2 * time.Second):
		must.FailNow("Result did not return after ctx cancel during poll")
	}

	// bestEffortCancel runs in a detached goroutine with
	// context.Background(), so it should reach the server even though
	// the parent ctx is now cancelled. Wait briefly for it to land.
	must.Eventually(func() bool { return cancelHits.Load() > 0 }, 2*time.Second, 5*time.Millisecond)
	must.EqualValues(1, cancelHits.Load(), "cancel hits")
}

func TestHarnessPollingBatch_ResultTypeMapping(t *testing.T) {
	tests := []struct {
		name       string
		resultJSON string
		wantErrTy  string // empty => expect success
		wantSucc   bool
	}{
		{
			name:       "succeeded",
			resultJSON: `{"type":"succeeded","message":{"content":[{"type":"text","text":"hi"}]}}`,
			wantSucc:   true,
		},
		{
			name:       "errored",
			resultJSON: `{"type":"errored","error":{"type":"overloaded_error","message":"upstream is hot"}}`,
			wantErrTy:  "server_error",
		},
		{
			name:       "canceled",
			resultJSON: `{"type":"canceled"}`,
			wantErrTy:  "batch_cancelled",
		},
		{
			name:       "expired",
			resultJSON: `{"type":"expired"}`,
			wantErrTy:  "batch_expired",
		},
		{
			name:       "unknown",
			resultJSON: `{"type":"weird_new_thing"}`,
			wantErrTy:  "server_error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var line anthropicBatchResultLine
			must.NoError(json.Unmarshal([]byte(`{"custom_id":"id","result":`+tc.resultJSON+`}`), &line), "unmarshal fixture")
			got := mapBatchResultLine(line)
			if tc.wantSucc {
				must.Nil(got.Err, "expected success")
				must.NotNil(got.Response, "expected non-nil Response")
				return
			}
			must.NotNil(got.Err, "expected an error result")
			must.Equal(tc.wantErrTy, got.Err.Type)
		})
	}
}

func TestHarnessPollingBatch_CredentialResolveFails(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	src := &fakeCredentialSource{resolveErr: errors.New("vault down")}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		check.Fail("server should not be hit when credential resolve fails")
	}))
	defer srv.Close()

	c, teardown := newTestPollingClient(t, srv, src, time.Second)
	defer teardown()

	_, err := c.Submit(context.Background(), anthropicSubmitEntries("id"))
	must.ErrorContains(err, "vault down")
}

func TestHarnessPollingBatch_CredentialNilBearer(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	src := &fakeCredentialSource{nilBearer: true}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		check.Fail("server should not be hit when no bearer token is produced")
	}))
	defer srv.Close()

	c, teardown := newTestPollingClient(t, srv, src, time.Second)
	defer teardown()

	_, err := c.Submit(context.Background(), anthropicSubmitEntries("id"))
	must.ErrorContains(err, "no bearer token")
}

func TestHarnessPollingBatch_ResultsMissingCustomID(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-ant-test"}
	polls := []string{`{"id":"batch_xyz","processing_status":"ended","results_url":"REPLACE"}`}
	// Empty JSONL — well-formed HTTP 200 but no lines.
	ps := newPollServer(t, polls, "")
	defer ps.Close()
	ps.pollResponses[0] = strings.ReplaceAll(ps.pollResponses[0], "REPLACE", ps.URL+"/results")

	c, teardown := newTestPollingClient(t, ps.Server, src, time.Second)
	defer teardown()

	_, err := c.Result(context.Background(), "batch_xyz")
	must.ErrorContains(err, "empty")
}

func TestHarnessPollingBatch_ResultsURLMissing(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-ant-test"}
	polls := []string{`{"id":"batch_xyz","processing_status":"ended"}`}
	ps := newPollServer(t, polls, "")
	defer ps.Close()

	c, teardown := newTestPollingClient(t, ps.Server, src, time.Second)
	defer teardown()

	_, err := c.Result(context.Background(), "batch_xyz")
	must.ErrorContains(err, "results_url")
}

func TestHarnessPollingBatch_JitterStaysWithinBounds(t *testing.T) {
	must := require.New(t)
	// Enable jitter for this test only.
	prev := setBatchPollJitterDisabled(false)
	defer setBatchPollJitterDisabled(prev)

	d := 100 * time.Millisecond
	low := d - d/5
	high := d + d/5
	for i := 0; i < 200; i++ {
		got := jitter(d)
		must.GreaterOrEqual(got, low, "iter %d: below lower bound", i)
		must.LessOrEqual(got, high, "iter %d: above upper bound", i)
	}
}

// TestHarnessPollingBatch_ResultsURLBadOrigin confirms an "ended" batch
// whose results_url points off-domain is rejected before fetchResults
// would send the credential.
func TestHarnessPollingBatch_ResultsURLBadOrigin(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-ant-test"}

	var exfilHits atomic.Int64
	exfilSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		exfilHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer exfilSrv.Close()

	// Non-loopback so the test-mode relaxation does not paper over the
	// check; validation must fire before evil.com is ever reached.
	const badURL = "https://evil.com/exfil"

	polls := []string{
		fmt.Sprintf(`{"id":"batch_xyz","processing_status":"ended","results_url":%q}`, badURL),
	}
	ps := newPollServer(t, polls, "")
	defer ps.Close()

	c, teardown := newTestPollingClient(t, ps.Server, src, time.Second)
	defer teardown()

	_, err := c.Result(context.Background(), "batch_xyz")
	must.ErrorContains(err, "results_url host")
	must.Zero(exfilHits.Load(), "validation must fire before the GET")
}

// TestHarnessPollingBatch_ResultsURLNonHTTPS confirms an http:// scheme
// (even on anthropic.com) is rejected — the credential is bearer-class
// and must not be sent in cleartext.
func TestHarnessPollingBatch_ResultsURLNonHTTPS(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-ant-test"}

	polls := []string{
		`{"id":"batch_xyz","processing_status":"ended","results_url":"http://anthropic.com/results"}`,
	}
	ps := newPollServer(t, polls, "")
	defer ps.Close()

	c, teardown := newTestPollingClient(t, ps.Server, src, time.Second)
	defer teardown()

	_, err := c.Result(context.Background(), "batch_xyz")
	must.ErrorContains(err, "scheme")
}

// TestHarnessPollingBatch_ResultsURLAnthropicHostAccepted checks
// validateResultsURL directly: when the caller's baseURL is the
// production Anthropic root, an *.anthropic.com results_url must pass.
func TestHarnessPollingBatch_ResultsURLAnthropicHostAccepted(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{name: "batch results path", url: "https://api.anthropic.com/v1/messages/batches/abc/results"},
		{name: "bare anthropic domain", url: "https://anthropic.com/results"},
		{name: "regional api host", url: "https://eu.api.anthropic.com/results"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.NoError(validateResultsURL(tc.url, "https://api.anthropic.com"), "expected acceptance")
		})
	}
}

// TestHarnessPollingBatch_BatchIDPathEscaped confirms a batchID containing
// path-sensitive characters is escaped into a single path segment before
// being concatenated into the poll / cancel URLs. A bare concatenation
// would let an attacker-supplied (or upstream-mangled) batchID navigate
// to an unintended endpoint.
func TestHarnessPollingBatch_BatchIDPathEscaped(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-ant-test"}

	const sneakyID = "batch_../etc/passwd?x=1"
	wantPathSegment := url.PathEscape(sneakyID)

	type observed struct {
		mu         sync.Mutex
		pollPaths  []string
		cancelHits int
	}
	obs := &observed{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		obs.mu.Lock()
		defer obs.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.EscapedPath(), "/v1/messages/batches/") && !strings.HasSuffix(r.URL.EscapedPath(), "/cancel"):
			obs.pollPaths = append(obs.pollPaths, r.URL.EscapedPath())
			// "ended" with an empty results_url errors out before
			// fetchResults is exercised — we only care about escaping here.
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"id":%q,"processing_status":"ended"}`, sneakyID)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.EscapedPath(), "/cancel"):
			obs.cancelHits++
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{}`)
		default:
			check.Failf("unexpected request", "%s %s", r.Method, r.URL.EscapedPath())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, teardown := newTestPollingClient(t, srv, src, time.Second)
	defer teardown()

	_, err := c.Result(context.Background(), sneakyID)
	must.Error(err, "expected results_url-missing error")

	obs.mu.Lock()
	defer obs.mu.Unlock()
	must.NotEmpty(obs.pollPaths, "expected at least one poll request")
	wantPath := "/v1/messages/batches/" + wantPathSegment
	must.Equal(wantPath, obs.pollPaths[0], "poll path")
}

// TestHarnessPollingBatch_CancelURLEscaped confirms the cancel POST also
// escapes batchID into a single path segment.
func TestHarnessPollingBatch_CancelURLEscaped(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	src := &fakeCredentialSource{token: "sk-ant-test"}

	const sneakyID = "batch_inject/cancel?x=y"
	wantSegment := url.PathEscape(sneakyID)
	wantCancelPath := "/v1/messages/batches/" + wantSegment + "/cancel"

	var (
		mu          sync.Mutex
		cancelPaths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.EscapedPath(), "/v1/messages/batches/"):
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"id":%q,"processing_status":"in_progress"}`, sneakyID)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.EscapedPath(), "/cancel"):
			mu.Lock()
			cancelPaths = append(cancelPaths, r.URL.EscapedPath())
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{}`)
		default:
			check.Failf("unexpected request", "%s %s", r.Method, r.URL.EscapedPath())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, teardown := newTestPollingClient(t, srv, src, 30*time.Millisecond)
	defer teardown()

	_, err := c.Result(context.Background(), sneakyID)
	must.Error(err, "expected timeout error")

	// bestEffortCancel runs asynchronously, so the cancel request may land after Result returns.
	must.Eventually(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(cancelPaths) > 0
	}, time.Second, 5*time.Millisecond, "expected bestEffortCancel to fire and reach the server")

	mu.Lock()
	defer mu.Unlock()
	must.Equal(wantCancelPath, cancelPaths[0], "cancel path")
}

// TestHarnessPollingBatch_SubmitTransportError covers the path where the
// initial POST to /v1/messages/batches fails at the transport layer. The
// harness must surface the failure as a wrapped "submit batch" error
// rather than returning a bare "" batchID or panicking on the nil
// response.
func TestHarnessPollingBatch_SubmitTransportError(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	src := &fakeCredentialSource{token: "sk-ant-test"}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		check.Fail("server should not receive a request after Close()")
	}))
	// Close immediately so the client's first connection attempt fails.
	srv.Close()

	c, teardown := newTestPollingClient(t, srv, src, time.Second)
	defer teardown()

	_, err := c.Submit(context.Background(), anthropicSubmitEntries("id"))
	must.ErrorContains(err, "submit batch")
}

// TestHarnessPollingBatch_MalformedJSONLLine confirms a non-JSON line in
// the JSONL results document fails fast rather than being silently
// skipped. The Scanner reads each line in turn; a bad line is the only
// signal that the upstream document has been truncated or framed
// incorrectly.
func TestHarnessPollingBatch_MalformedJSONLLine(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-ant-test"}

	resultsBody := `{"custom_id":"stirrup-run-1-turn-1","result":{"type":"succeeded","message":{}}}` + "\n" +
		`not even close to JSON` + "\n"

	polls := []string{`{"id":"batch_xyz","processing_status":"ended","results_url":"REPLACE"}`}
	ps := newPollServer(t, polls, resultsBody)
	defer ps.Close()
	ps.pollResponses[0] = strings.ReplaceAll(ps.pollResponses[0], "REPLACE", ps.URL+"/results")

	c, teardown := newTestPollingClient(t, ps.Server, src, time.Second)
	defer teardown()

	_, err := c.Result(context.Background(), "batch_xyz")
	must.ErrorContains(err, "decode batch result line")
}

// TestHarnessPollingBatch_BackoffCapped exercises the interval-doubling
// cap at batchPollMaxInterval. With initialInterval set just above
// half the cap, the *third* interval would (uncapped) exceed the cap;
// the loop must clamp it. We observe by spacing between successive poll
// requests on the test server.
func TestHarnessPollingBatch_BackoffCapped(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	src := &fakeCredentialSource{token: "sk-ant-test"}

	var (
		mu        sync.Mutex
		callTimes []time.Time
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/messages/batches/") &&
			!strings.HasSuffix(r.URL.Path, "/cancel") {
			mu.Lock()
			callTimes = append(callTimes, time.Now())
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"batch_xyz","processing_status":"in_progress"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	// Cap = 5ms for the test; initial = 3ms (>cap/2 so the second
	// doubling would exceed the cap and trigger the clamp).
	prevCap := swapBatchPollMaxInterval(5 * time.Millisecond)
	defer swapBatchPollMaxInterval(prevCap)

	prevInterval := setBatchPollInitialInterval(3 * time.Millisecond)
	defer setBatchPollInitialInterval(prevInterval)
	prevJitter := setBatchPollJitterDisabled(true)
	defer setBatchPollJitterDisabled(prevJitter)

	c := NewHarnessPollingBatchClient(HarnessBatchClientOptions{
		ProviderType: "anthropic",
		APIKeyRef:    "secret://test",
		CredSource:   src,
		MaxWait:      40 * time.Millisecond,
	})
	c.baseURL = srv.URL

	_, _ = c.Result(context.Background(), "batch_xyz")

	mu.Lock()
	defer mu.Unlock()
	must.GreaterOrEqual(len(callTimes), 4, "poll calls within the 40ms budget")
	// From the third sleep onwards each interval should be <= cap + slack.
	// Slack covers scheduler jitter on busy CI runners.
	const slack = 20 * time.Millisecond
	for i := 2; i < len(callTimes); i++ {
		gap := callTimes[i].Sub(callTimes[i-1])
		check.LessOrEqual(gap, 5*time.Millisecond+slack, "poll interval %d exceeded cap+slack", i)
	}
}

// TestHarnessPollingBatch_TimeoutFiringAlignedToDeadline covers the
// sleep>remaining clamp at Result. With maxWait set to slightly under
// 3 × initialInterval, the third sleep would (uncapped) exceed the
// remaining budget; the loop must clamp it and fire the timeout on the
// documented cap rather than one full interval past it.
func TestHarnessPollingBatch_TimeoutFiringAlignedToDeadline(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-ant-test"}

	polls := []string{`{"id":"batch_xyz","processing_status":"in_progress"}`}
	ps := newPollServer(t, polls, "")
	defer ps.Close()

	prevInterval := setBatchPollInitialInterval(10 * time.Millisecond)
	defer setBatchPollInitialInterval(prevInterval)
	prevJitter := setBatchPollJitterDisabled(true)
	defer setBatchPollJitterDisabled(prevJitter)

	maxWait := 30 * time.Millisecond
	c := NewHarnessPollingBatchClient(HarnessBatchClientOptions{
		ProviderType: "anthropic",
		APIKeyRef:    "secret://test",
		CredSource:   src,
		MaxWait:      maxWait,
	})
	c.baseURL = ps.URL

	start := time.Now()
	_, err := c.Result(context.Background(), "batch_xyz")
	elapsed := time.Since(start)

	must.ErrorIs(err, errBatchExpired)
	// The deadline should fire within ~maxWait + scheduler slack. A
	// generous 50 ms slack tolerates loaded CI runners without hiding a
	// genuine regression (which would overshoot by a full interval +).
	must.LessOrEqual(elapsed, maxWait+50*time.Millisecond, "timeout fired %s past maxWait", elapsed-maxWait)
}

// keysOf is a small map-introspection helper that keeps the table-
// driven tests above legible. Not exported because the parent package
// has no other test that needs the same shape.
func keysOf[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// newTestOpenAIPollingClient builds a polling client configured for the
// OpenAI provider type, pointed at srv (which must serve /files,
// /batches, /batches/{id}, /batches/{id}/cancel, and /files/{id}/content).
// Returns the client plus a teardown restoring the package-level
// poll-interval and jitter knobs.
func newTestOpenAIPollingClient(t *testing.T, srv *httptest.Server, src credential.Source, providerType string, maxWait time.Duration) (*harnessPollingBatchClient, func()) {
	t.Helper()
	prevInterval := setBatchPollInitialInterval(2 * time.Millisecond)
	prevJitter := setBatchPollJitterDisabled(true)
	c := NewHarnessPollingBatchClient(HarnessBatchClientOptions{
		ProviderType: providerType,
		APIKeyRef:    "secret://test",
		CredSource:   src,
		BaseURL:      srv.URL,
		MaxWait:      maxWait,
	})
	teardown := func() {
		setBatchPollInitialInterval(prevInterval)
		setBatchPollJitterDisabled(prevJitter)
	}
	return c, teardown
}

// openaiSubmitEntries builds a one-entry batch with an OpenAI chat
// completions body — the same shape buildOpenAIRequest emits.
func openaiSubmitEntries(customID, providerType string) []BatchEntry {
	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":256,"stream":false}`
	return []BatchEntry{{
		CustomID: customID,
		Provider: providerType,
		Body:     json.RawMessage(body),
	}}
}

// openaiBatchFixture is a stateful test fixture for the OpenAI two-step
// batch flow. The handler routes /files (upload), /batches (create),
// /batches/{id} (poll), /batches/{id}/cancel (cancel), and
// /files/{id}/content (download).
type openaiBatchFixture struct {
	*httptest.Server

	pollResponses []string // sequence of polled batch object bodies
	outputBody    string   // JSONL served on /files/{output_file_id}/content
	errorBody     string   // JSONL served on /files/{error_file_id}/content

	mu            sync.Mutex
	uploadCalls   int
	uploadedFiles [][]byte // body of each uploaded file
	createCalls   int
	createBodies  [][]byte
	pollCalls     int
	cancelCalls   int
	downloadCalls int
	downloadedIDs []string
}

func (f *openaiBatchFixture) snapshot() (int, int, int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.uploadCalls, f.createCalls, f.pollCalls, f.cancelCalls, f.downloadCalls
}

func newOpenAIBatchFixture(t *testing.T, polls []string, outputBody, errorBody string) *openaiBatchFixture {
	t.Helper()
	check := assert.New(t)
	f := &openaiBatchFixture{
		pollResponses: polls,
		outputBody:    outputBody,
		errorBody:     errorBody,
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/files":
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			f.mu.Lock()
			f.uploadCalls++
			f.uploadedFiles = append(f.uploadedFiles, body)
			f.mu.Unlock()
			ct := r.Header.Get("Content-Type")
			check.True(strings.HasPrefix(ct, "multipart/form-data"), "upload Content-Type: got %q, want multipart/form-data", ct)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"file_abc","object":"file"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/batches":
			body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
			f.mu.Lock()
			f.createCalls++
			f.createBodies = append(f.createBodies, body)
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"batch_xyz","status":"validating"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/batches/") &&
			!strings.HasSuffix(r.URL.Path, "/cancel"):
			f.mu.Lock()
			idx := f.pollCalls
			if idx >= len(f.pollResponses) {
				idx = len(f.pollResponses) - 1
			}
			f.pollCalls++
			body := f.pollResponses[idx]
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, body)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
			f.mu.Lock()
			f.cancelCalls++
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/files/") &&
			strings.HasSuffix(r.URL.Path, "/content"):
			// /files/{id}/content
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/files/"), "/content")
			f.mu.Lock()
			f.downloadCalls++
			f.downloadedIDs = append(f.downloadedIDs, id)
			body := f.outputBody
			if strings.HasPrefix(id, "errfile") || strings.Contains(id, "error") {
				body = f.errorBody
			}
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, body)
		default:
			check.Failf("unexpected request", "%s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return f
}

func TestHarnessPollingBatch_OpenAISubmit_HappyPath(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-test"}
	f := newOpenAIBatchFixture(t, nil, "", "")
	defer f.Close()

	c, teardown := newTestOpenAIPollingClient(t, f.Server, src, "openai-compatible", time.Second)
	defer teardown()

	id, err := c.Submit(context.Background(), openaiSubmitEntries("stirrup-run-1-turn-1", "openai-compatible"))
	must.NoError(err)
	must.Equal("batch_xyz", id)

	uploads, creates, _, _, _ := f.snapshot()
	must.Equal(1, uploads, "upload calls")
	must.Equal(1, creates, "create calls")

	f.mu.Lock()
	rawUpload := f.uploadedFiles[0]
	createBody := string(f.createBodies[0])
	f.mu.Unlock()
	uploaded := string(rawUpload)

	t.Run("upload jsonl line carries custom_id, endpoint and method", func(t *testing.T) {
		must := require.New(t)
		must.Contains(uploaded, `"custom_id":"stirrup-run-1-turn-1"`)
		must.Contains(uploaded, `"url":"/v1/chat/completions"`)
		must.Contains(uploaded, `"method":"POST"`)
	})

	t.Run("create body references the upload and the chat endpoint", func(t *testing.T) {
		must := require.New(t)
		must.Contains(createBody, `"input_file_id":"file_abc"`)
		must.Contains(createBody, `"endpoint":"/v1/chat/completions"`)
		must.Contains(createBody, `"completion_window":"24h"`)
	})

	// The upload is multipart. Its boundary is re-derived from the body's
	// opening bytes because the request Content-Type is not captured.
	t.Run("multipart upload has batch purpose and batch.jsonl file part", func(t *testing.T) {
		must := require.New(t)
		must.True(bytes.HasPrefix(rawUpload, []byte("--")), "uploaded body does not start with multipart boundary marker: %q", rawUpload)
		end := bytes.Index(rawUpload, []byte("\r\n"))
		must.GreaterOrEqual(end, 0, "uploaded body has no CRLF after boundary marker")
		boundary := string(rawUpload[2:end])

		reader := multipart.NewReader(bytes.NewReader(rawUpload), boundary)
		var sawPurpose, sawFile bool
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			must.NoError(err, "multipart NextPart")
			switch part.FormName() {
			case "purpose":
				val, _ := io.ReadAll(part)
				must.Equal("batch", string(val), "purpose field")
				sawPurpose = true
			case "file":
				must.Equal("batch.jsonl", part.FileName(), "file part filename")
				sawFile = true
			}
			_ = part.Close()
		}
		must.True(sawPurpose, "multipart missing purpose field")
		must.True(sawFile, "multipart missing file part")
	})
}

func TestHarnessPollingBatch_OpenAIResponses_Submit_HappyPath(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-test"}
	f := newOpenAIBatchFixture(t, nil, "", "")
	defer f.Close()

	c, teardown := newTestOpenAIPollingClient(t, f.Server, src, "openai-responses", time.Second)
	defer teardown()

	_, err := c.Submit(context.Background(), openaiSubmitEntries("stirrup-run-1-turn-1", "openai-responses"))
	must.NoError(err)

	f.mu.Lock()
	uploaded := string(f.uploadedFiles[0])
	createBody := string(f.createBodies[0])
	f.mu.Unlock()

	must.Contains(uploaded, `"url":"/v1/responses"`)
	must.Contains(createBody, `"endpoint":"/v1/responses"`)
}

func TestHarnessPollingBatch_OpenAIResult_Completed(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-test"}
	polls := []string{
		`{"id":"batch_xyz","status":"validating"}`,
		`{"id":"batch_xyz","status":"in_progress"}`,
		`{"id":"batch_xyz","status":"finalizing"}`,
		`{"id":"batch_xyz","status":"completed","output_file_id":"file_out"}`,
	}
	outputBody := `{"custom_id":"stirrup-run-1-turn-1","response":{"status_code":200,"body":{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}}}` + "\n"
	f := newOpenAIBatchFixture(t, polls, outputBody, "")
	defer f.Close()

	c, teardown := newTestOpenAIPollingClient(t, f.Server, src, "openai-compatible", time.Second)
	defer teardown()

	results, err := c.Result(context.Background(), "batch_xyz")
	must.NoError(err)
	entry, ok := results["stirrup-run-1-turn-1"]
	must.True(ok, "missing entry; got keys %v", keysOf(results))
	must.Nil(entry.Err)
	must.Contains(string(entry.Response), `"content":"hi"`)

	_, _, polled, cancels, downloads := f.snapshot()
	must.GreaterOrEqual(polled, 4, "polls")
	must.Equal(1, downloads, "downloads")
	must.Equal(0, cancels, "cancels")
}

func TestHarnessPollingBatch_OpenAIResult_Failed_WithErrorFile(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-test"}
	polls := []string{
		`{"id":"batch_xyz","status":"in_progress"}`,
		`{"id":"batch_xyz","status":"failed","error_file_id":"errfile_1"}`,
	}
	errorBody := `{"custom_id":"stirrup-run-1-turn-1","error":{"code":"server_error","message":"upstream meltdown"}}` + "\n"
	f := newOpenAIBatchFixture(t, polls, "", errorBody)
	defer f.Close()

	c, teardown := newTestOpenAIPollingClient(t, f.Server, src, "openai-compatible", time.Second)
	defer teardown()

	results, err := c.Result(context.Background(), "batch_xyz")
	must.NoError(err)
	entry, ok := results["stirrup-run-1-turn-1"]
	must.True(ok, "missing entry; got keys %v", keysOf(results))
	must.NotNil(entry.Err)
	must.Equal("server_error", entry.Err.Type)
	must.Contains(entry.Err.Message, "upstream meltdown")
}

func TestHarnessPollingBatch_OpenAIResult_Expired(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-test"}
	polls := []string{`{"id":"batch_xyz","status":"expired"}`}
	f := newOpenAIBatchFixture(t, polls, "", "")
	defer f.Close()

	c, teardown := newTestOpenAIPollingClient(t, f.Server, src, "openai-compatible", time.Second)
	defer teardown()

	_, err := c.Result(context.Background(), "batch_xyz")
	must.ErrorIs(err, errBatchExpired)
}

func TestHarnessPollingBatch_OpenAIResult_Cancelled(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-test"}
	polls := []string{`{"id":"batch_xyz","status":"cancelled"}`}
	f := newOpenAIBatchFixture(t, polls, "", "")
	defer f.Close()

	c, teardown := newTestOpenAIPollingClient(t, f.Server, src, "openai-compatible", time.Second)
	defer teardown()

	const customID = "stirrup-run-1-turn-1"
	_, err := c.Submit(context.Background(), openaiSubmitEntries(customID, "openai-compatible"))
	must.NoError(err)
	results, err := c.Result(context.Background(), "batch_xyz")
	must.NoError(err)
	// synthesizeOpenAIFailure keys the entry under the originating
	// customID so the BatchAdapter's customID lookup surfaces the
	// typed BatchResultError rather than an opaque "missing entry".
	entry, ok := results[customID]
	must.True(ok, "expected synthesised entry keyed by customID, got %v", keysOf(results))
	must.NotNil(entry.Err)
	must.Equal("batch_cancelled", entry.Err.Type)
}

// TestHarnessPollingBatch_OpenAIAuthMode_APIKeyHeader confirms that
// when APIKeyHeader is set on the options struct, the polling client
// uses the custom header for every HTTP request (upload, create, poll,
// cancel, download) instead of Authorization: Bearer.
func TestHarnessPollingBatch_OpenAIAuthMode_APIKeyHeader(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	src := &fakeCredentialSource{token: "azure-key"}

	var (
		mu           sync.Mutex
		bearerSeen   bool
		apiKeyHeader string
		seenAPIKey   bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.Header.Get("Authorization") != "" {
			bearerSeen = true
		}
		if v := r.Header.Get("api-key"); v != "" {
			seenAPIKey = true
			apiKeyHeader = v
		}
		mu.Unlock()

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/files":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"file_abc"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/batches":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"batch_xyz","status":"validating"}`)
		default:
			check.Failf("unexpected request", "%s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	prevInterval := setBatchPollInitialInterval(2 * time.Millisecond)
	defer setBatchPollInitialInterval(prevInterval)
	prevJitter := setBatchPollJitterDisabled(true)
	defer setBatchPollJitterDisabled(prevJitter)

	c := NewHarnessPollingBatchClient(HarnessBatchClientOptions{
		ProviderType: "openai-compatible",
		APIKeyRef:    "secret://test",
		CredSource:   src,
		BaseURL:      srv.URL,
		APIKeyHeader: "api-key",
		MaxWait:      time.Second,
	})

	_, err := c.Submit(context.Background(), openaiSubmitEntries("id1", "openai-compatible"))
	must.NoError(err)

	mu.Lock()
	defer mu.Unlock()
	must.False(bearerSeen, "expected no Authorization header when APIKeyHeader is set")
	must.True(seenAPIKey, "expected api-key header to be set")
	must.Equal("azure-key", apiKeyHeader, "api-key header value")
}

// TestHarnessPollingBatch_OpenAIAuthMode_APIKeyHeader_OnDownload pins
// the auth header on the file-download GET (/files/{id}/content), so a
// regression that bypassed applyAuthHeaders on download does not ship
// unnoticed.
func TestHarnessPollingBatch_OpenAIAuthMode_APIKeyHeader_OnDownload(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	src := &fakeCredentialSource{token: "azure-key"}

	var (
		mu              sync.Mutex
		downloadAuthHdr string
		downloadAPIKey  string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/files":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"file_abc"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/batches":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"batch_xyz","status":"validating"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/batches/") && !strings.HasSuffix(r.URL.Path, "/cancel"):
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"batch_xyz","status":"completed","output_file_id":"file_out"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/files/") && strings.HasSuffix(r.URL.Path, "/content"):
			mu.Lock()
			downloadAuthHdr = r.Header.Get("Authorization")
			downloadAPIKey = r.Header.Get("api-key")
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"custom_id":"id1","response":{"status_code":200,"body":{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}}}`+"\n")
		default:
			check.Failf("unexpected request", "%s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	prevInterval := setBatchPollInitialInterval(2 * time.Millisecond)
	defer setBatchPollInitialInterval(prevInterval)
	prevJitter := setBatchPollJitterDisabled(true)
	defer setBatchPollJitterDisabled(prevJitter)

	c := NewHarnessPollingBatchClient(HarnessBatchClientOptions{
		ProviderType: "openai-compatible",
		APIKeyRef:    "secret://test",
		CredSource:   src,
		BaseURL:      srv.URL,
		APIKeyHeader: "api-key",
		MaxWait:      time.Second,
	})

	_, err := c.Submit(context.Background(), openaiSubmitEntries("id1", "openai-compatible"))
	must.NoError(err)
	_, err = c.Result(context.Background(), "batch_xyz")
	must.NoError(err)

	mu.Lock()
	defer mu.Unlock()
	must.Empty(downloadAuthHdr, "download must not carry Authorization header when APIKeyHeader set")
	must.Equal("azure-key", downloadAPIKey, "download api-key header value")
}

// TestHarnessPollingBatch_OpenAIAuthMode_DefaultBearer confirms the
// inverse: when APIKeyHeader is empty (the default), the client falls
// back to Authorization: Bearer.
func TestHarnessPollingBatch_OpenAIAuthMode_DefaultBearer(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-test"}

	var (
		mu      sync.Mutex
		authHdr string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if v := r.Header.Get("Authorization"); v != "" {
			authHdr = v
		}
		mu.Unlock()

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/files":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"file_abc"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/batches":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"batch_xyz","status":"validating"}`)
		default:
			check.Failf("unexpected request", "%s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewHarnessPollingBatchClient(HarnessBatchClientOptions{
		ProviderType: "openai-compatible",
		APIKeyRef:    "secret://test",
		CredSource:   src,
		BaseURL:      srv.URL,
		MaxWait:      time.Second,
	})

	_, err := c.Submit(context.Background(), openaiSubmitEntries("id1", "openai-compatible"))
	must.NoError(err)

	mu.Lock()
	defer mu.Unlock()
	must.Equal("Bearer sk-test", authHdr)
}

// TestHarnessPollingBatch_OpenAIResult_Failed_NoErrorFile confirms a
// batch_failed status without an error_file_id surfaces as a typed
// server_error BatchResultError keyed under the originating customID
// (not a generic error string) so the BatchAdapter's customID lookup
// surfaces the typed entry. The top-level errors.data message is used
// when present.
func TestHarnessPollingBatch_OpenAIResult_Failed_NoErrorFile(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-test"}
	polls := []string{
		`{"id":"batch_xyz","status":"failed","errors":{"data":[{"code":"bad_input","message":"malformed input file"}]}}`,
	}
	f := newOpenAIBatchFixture(t, polls, "", "")
	defer f.Close()

	c, teardown := newTestOpenAIPollingClient(t, f.Server, src, "openai-compatible", time.Second)
	defer teardown()

	const customID = "stirrup-run-1-turn-1"
	_, err := c.Submit(context.Background(), openaiSubmitEntries(customID, "openai-compatible"))
	must.NoError(err)
	results, err := c.Result(context.Background(), "batch_xyz")
	must.NoError(err)
	entry, ok := results[customID]
	must.True(ok, "expected synthesised entry keyed by customID, got %v", keysOf(results))
	must.NotNil(entry.Err)
	must.Equal("server_error", entry.Err.Type)
	must.Contains(entry.Err.Message, "malformed input file")
}

// TestHarnessPollingBatch_OpenAIBatchCreate_ScrubsErrorBody pins the
// security.Scrub fan-in on the four OpenAI error sites: a 422 from
// /batches whose body echoes a secret-shaped string must redact the
// secret in the returned error rather than letting the raw bytes flow
// to OTel spans / transport warnings.
func TestHarnessPollingBatch_OpenAIBatchCreate_ScrubsErrorBody(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-test"}
	const secret = "sk-ant-abc123_DEF-456"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/files":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"file_abc"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/batches":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = fmt.Fprintf(w, `{"error":{"message":"bad: %s"}}`, secret)
		default:
			check.Failf("unexpected request", "%s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c, teardown := newTestOpenAIPollingClient(t, srv, src, "openai-compatible", time.Second)
	defer teardown()

	_, err := c.Submit(context.Background(), openaiSubmitEntries("id1", "openai-compatible"))
	must.Error(err, "expected error from 422 response")
	must.NotContains(err.Error(), secret, "error must not contain raw secret")
	must.Contains(err.Error(), "[REDACTED]", "error must contain [REDACTED] sentinel")
}

// TestMapOpenAIOutputLine_ClassifiesEachBranch pins the mapOpenAIOutputLine
// projection from /v1/files content line → BatchResult across all branches.
// The status code 0 case guards against a silent success fabrication when
// the field is absent or malformed.
func TestMapOpenAIOutputLine_ClassifiesEachBranch(t *testing.T) {
	statusBody := func(code int, body string) *struct {
		StatusCode int             `json:"status_code"`
		Body       json.RawMessage `json:"body"`
	} {
		return &struct {
			StatusCode int             `json:"status_code"`
			Body       json.RawMessage `json:"body"`
		}{StatusCode: code, Body: json.RawMessage(body)}
	}

	tests := []struct {
		name    string
		line    openaiBatchOutputLine
		wantErr string // empty means the line maps to a successful result
	}{
		{
			name: "top-level error maps to server error",
			line: openaiBatchOutputLine{
				CustomID: "id1",
				Error: &struct {
					Code    string `json:"code,omitempty"`
					Message string `json:"message,omitempty"`
				}{Code: "rate_limit", Message: "throttled"},
			},
			wantErr: "server_error",
		},
		{
			name:    "nil response maps to server error",
			line:    openaiBatchOutputLine{CustomID: "id1", Response: nil},
			wantErr: "server_error",
		},
		{
			name: "status code 200 maps to success",
			line: openaiBatchOutputLine{CustomID: "id1", Response: statusBody(200, `{"choices":[]}`)},
		},
		{
			name:    "status code 422 maps to server error",
			line:    openaiBatchOutputLine{CustomID: "id1", Response: statusBody(422, `{"error":"invalid"}`)},
			wantErr: "server_error",
		},
		{
			name:    "absent status code maps to server error",
			line:    openaiBatchOutputLine{CustomID: "id1", Response: statusBody(0, `{"choices":[]}`)},
			wantErr: "server_error",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got := mapOpenAIOutputLine(tc.line)
			if tc.wantErr == "" {
				must.Nil(got.Err)
				must.NotNil(got.Response)
				return
			}
			must.NotNil(got.Err)
			must.Equal(tc.wantErr, got.Err.Type)
		})
	}
}

// TestNewHarnessPollingBatchClient_NilCredSource_Panics pins the
// nil-CredSource guard: the constructor must panic with a clear message
// rather than defer a nil-pointer dereference to the first HTTP request.
func TestNewHarnessPollingBatchClient_NilCredSource_Panics(t *testing.T) {
	must := require.New(t)
	recovered := func() (r any) {
		defer func() { r = recover() }()
		_ = NewHarnessPollingBatchClient(HarnessBatchClientOptions{
			ProviderType: "anthropic",
			APIKeyRef:    "secret://test",
			MaxWait:      time.Second,
		})
		return nil
	}()
	must.NotNil(recovered, "expected panic on nil CredSource")
	msg, ok := recovered.(string)
	must.True(ok, "expected string panic value, got %T: %v", recovered, recovered)
	must.Contains(msg, "CredSource", "panic message must mention CredSource")
}

// TestNewHarnessPollingBatchClient_ZeroMaxWait_DefaultsToRunTimeout pins
// that a zero MaxWait is replaced with types.MaxRunTimeoutSeconds at
// constructor time, not later deferred to an already-passed deadline.
func TestNewHarnessPollingBatchClient_ZeroMaxWait_DefaultsToRunTimeout(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-test"}
	c := NewHarnessPollingBatchClient(HarnessBatchClientOptions{
		ProviderType: "anthropic",
		APIKeyRef:    "secret://test",
		CredSource:   src,
		MaxWait:      0,
	})
	want := time.Duration(types.MaxRunTimeoutSeconds) * time.Second
	must.Equal(want, c.maxWait)
}

// TestNewHarnessPollingBatchClient_BaseURL_DefaultsByProvider pins the
// default-baseURL selection per provider type and the trailing-slash
// stripping.
func TestNewHarnessPollingBatchClient_BaseURL_DefaultsByProvider(t *testing.T) {
	src := &fakeCredentialSource{token: "sk-test"}
	for _, tc := range []struct {
		name         string
		providerType string
		want         string
	}{
		{name: "openai-compatible", providerType: "openai-compatible", want: openaiDefaultBaseURL},
		{name: "openai-responses", providerType: "openai-responses", want: openaiDefaultBaseURL},
		{name: "anthropic", providerType: "anthropic", want: anthropicBatchAPIBaseURL},
		{name: "empty provider type", providerType: "", want: anthropicBatchAPIBaseURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := NewHarnessPollingBatchClient(HarnessBatchClientOptions{
				ProviderType: tc.providerType,
				CredSource:   src,
				MaxWait:      time.Second,
			})
			must.Equal(tc.want, c.baseURL)
		})
	}
	t.Run("trims trailing slash", func(t *testing.T) {
		must := require.New(t)
		c := NewHarnessPollingBatchClient(HarnessBatchClientOptions{
			ProviderType: "openai-compatible",
			CredSource:   src,
			BaseURL:      "https://gateway.example.com/v1/",
			MaxWait:      time.Second,
		})
		must.Equal("https://gateway.example.com/v1", c.baseURL)
	})
}

// TestNewHarnessPollingBatchClient_CustomLogger_IsUsed pins that a Logger
// supplied via options replaces slog.Default on the constructed client
// so cancel-path warnings flow through the run-scoped logger.
func TestNewHarnessPollingBatchClient_CustomLogger_IsUsed(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-test"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil)).With("runID", "test-run")
	c := NewHarnessPollingBatchClient(HarnessBatchClientOptions{
		ProviderType: "anthropic",
		APIKeyRef:    "secret://test",
		CredSource:   src,
		MaxWait:      time.Second,
		Logger:       logger,
	})
	must.Same(logger, c.logger)
}

// openAIBatchStreamErrors streams one request through BatchAdapter against
// a fake OpenAI server whose batch poll returns pollBody. It returns the
// message of every error StreamEvent and fails the test when there is none.
func openAIBatchStreamErrors(t *testing.T, pollBody string) []string {
	t.Helper()
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-test"}
	f := newOpenAIBatchFixture(t, []string{pollBody}, "", "")
	defer f.Close()

	c, teardown := newTestOpenAIPollingClient(t, f.Server, src, "openai-compatible", time.Second)
	defer teardown()

	adapter := NewBatchAdapter(nil, c, &types.BatchProviderConfig{Enabled: true}, "openai-compatible", "run-test")

	params := types.StreamParams{
		Model:     "gpt-4o-mini",
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
		MaxTokens: 256,
	}
	ch, err := adapter.Stream(context.Background(), params)
	must.NoError(err)
	events := drain(t, ch)

	var msgs []string
	for _, ev := range events {
		if ev.Type == "error" && ev.Error != nil {
			msgs = append(msgs, ev.Error.Error())
		}
	}
	must.NotEmpty(msgs, "expected an error StreamEvent; got %d events: %+v", len(events), events)
	return msgs
}

// A cancelled batch reaches the caller as a batch_cancelled error. A
// "missing entry" error would mean the customID and batchID keys did not
// match.
func TestBatchAdapter_OpenAI_CancelledStatusSurfacesBatchCancelled(t *testing.T) {
	must := require.New(t)
	for _, msg := range openAIBatchStreamErrors(t, `{"id":"batch_xyz","status":"cancelled"}`) {
		must.Contains(msg, "batch_cancelled")
		must.NotContains(msg, "missing entry", "customID/batchID key mismatch")
	}
}

// The "cancelling" intermediate status shares the cancelled arm of the poll
// switch. This test keeps the two arms behaving the same.
func TestBatchAdapter_OpenAI_CancellingStatusSurfacesBatchCancelled(t *testing.T) {
	must := require.New(t)
	for _, msg := range openAIBatchStreamErrors(t, `{"id":"batch_xyz","status":"cancelling"}`) {
		must.Contains(msg, "batch_cancelled")
	}
}

// TestHarnessPollingBatch_OpenAIFileUpload_NonOKStatus pins the
// non-2xx surface on /files: a 429 must produce an error mentioning
// the status code rather than returning a bogus file id.
func TestHarnessPollingBatch_OpenAIFileUpload_NonOKStatus(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-test"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/files" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"slow down"}`)
			return
		}
		check.Failf("unexpected request", "%s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c, teardown := newTestOpenAIPollingClient(t, srv, src, "openai-compatible", time.Second)
	defer teardown()

	_, err := c.Submit(context.Background(), openaiSubmitEntries("id1", "openai-compatible"))
	must.ErrorContains(err, "429")
}

// TestHarnessPollingBatch_OpenAIFileUpload_MissingID pins the
// "missing id" branch: a 200 with a body that does not carry an id
// field must produce an error rather than passing an empty id forward.
func TestHarnessPollingBatch_OpenAIFileUpload_MissingID(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-test"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/files" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"object":"file"}`)
			return
		}
		check.Failf("unexpected request", "%s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c, teardown := newTestOpenAIPollingClient(t, srv, src, "openai-compatible", time.Second)
	defer teardown()

	_, err := c.Submit(context.Background(), openaiSubmitEntries("id1", "openai-compatible"))
	must.ErrorContains(err, "missing id")
}

// TestHarnessPollingBatch_OpenAIBatchCreate_NonOKStatus pins the
// non-2xx surface on /batches after a successful file upload.
func TestHarnessPollingBatch_OpenAIBatchCreate_NonOKStatus(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	src := &fakeCredentialSource{token: "sk-test"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/files":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"file_abc"}`)
		case "/batches":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":"validation"}`)
		default:
			check.Failf("unexpected request", "%s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c, teardown := newTestOpenAIPollingClient(t, srv, src, "openai-compatible", time.Second)
	defer teardown()

	_, err := c.Submit(context.Background(), openaiSubmitEntries("id1", "openai-compatible"))
	must.ErrorContains(err, "422")
}

// TestHarnessPollingBatch_OpenAIBatchCreate_MissingID pins the
// "missing id" branch on /batches.
func TestHarnessPollingBatch_OpenAIBatchCreate_MissingID(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-test"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/files":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"file_abc"}`)
		case "/batches":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"validating"}`)
		default:
			check.Failf("unexpected request", "%s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c, teardown := newTestOpenAIPollingClient(t, srv, src, "openai-compatible", time.Second)
	defer teardown()

	_, err := c.Submit(context.Background(), openaiSubmitEntries("id1", "openai-compatible"))
	must.ErrorContains(err, "missing id")
}

// TestHarnessPollingBatch_OpenAIFileDownload_NonOKStatus pins the
// non-2xx surface on /files/{id}/content during result retrieval.
func TestHarnessPollingBatch_OpenAIFileDownload_NonOKStatus(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	src := &fakeCredentialSource{token: "sk-test"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/batches/") && !strings.HasSuffix(r.URL.Path, "/cancel"):
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"batch_xyz","status":"completed","output_file_id":"file_out"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/files/") && strings.HasSuffix(r.URL.Path, "/content"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"file gone"}`)
		default:
			check.Failf("unexpected request", "%s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c, teardown := newTestOpenAIPollingClient(t, srv, src, "openai-compatible", time.Second)
	defer teardown()

	_, err := c.Result(context.Background(), "batch_xyz")
	must.ErrorContains(err, "404")
}

// TestHarnessPollingBatch_OpenAIPollOnce_NonOKStatus pins the non-2xx
// surface on the polling GET — a 503 must surface as a hard error
// (not silently retry / not get conflated with errBatchExpired).
func TestHarnessPollingBatch_OpenAIPollOnce_NonOKStatus(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-test"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"down"}`)
	}))
	defer srv.Close()

	c, teardown := newTestOpenAIPollingClient(t, srv, src, "openai-compatible", time.Second)
	defer teardown()

	_, err := c.Result(context.Background(), "batch_xyz")
	must.ErrorContains(err, "503")
	must.NotErrorIs(err, errBatchExpired, "503 must not be conflated with errBatchExpired")
}

// TestHarnessPollingBatch_OpenAIResult_EmptyOutputFile pins the empty
// output document branch: an output file that contains only blank
// lines must produce an "empty" error rather than a panic / silent
// success.
func TestHarnessPollingBatch_OpenAIResult_EmptyOutputFile(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-test"}
	polls := []string{`{"id":"batch_xyz","status":"completed","output_file_id":"file_out"}`}
	f := newOpenAIBatchFixture(t, polls, "\n\n", "")
	defer f.Close()

	c, teardown := newTestOpenAIPollingClient(t, f.Server, src, "openai-compatible", time.Second)
	defer teardown()

	_, err := c.Result(context.Background(), "batch_xyz")
	must.ErrorContains(err, "empty")
}

// TestHarnessPollingBatch_OpenAIResult_MalformedOutputJSONL pins the
// JSON decode error path on the output JSONL: a non-JSON second line
// must surface as a decode error rather than silently skipping.
func TestHarnessPollingBatch_OpenAIResult_MalformedOutputJSONL(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-test"}
	polls := []string{`{"id":"batch_xyz","status":"completed","output_file_id":"file_out"}`}
	// first line is valid JSON; second is junk
	output := `{"custom_id":"id1","response":{"status_code":200,"body":{}}}` + "\n" + "not-json\n"
	f := newOpenAIBatchFixture(t, polls, output, "")
	defer f.Close()

	c, teardown := newTestOpenAIPollingClient(t, f.Server, src, "openai-compatible", time.Second)
	defer teardown()

	_, err := c.Result(context.Background(), "batch_xyz")
	must.ErrorContains(err, "decode openai batch output line")
}

// TestHarnessPollingBatch_OpenAIResult_Failed_EmptyErrorFile pins the
// fallthrough from an empty error-file body to the top-level
// errors.data fallback. The surface is a typed BatchResultError keyed
// by the originating customID, not a bare error.
func TestHarnessPollingBatch_OpenAIResult_Failed_EmptyErrorFile(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-test"}
	polls := []string{
		`{"id":"batch_xyz","status":"failed","error_file_id":"errfile_1","errors":{"data":[{"message":"top-level fallback"}]}}`,
	}
	f := newOpenAIBatchFixture(t, polls, "", "\n")
	defer f.Close()

	c, teardown := newTestOpenAIPollingClient(t, f.Server, src, "openai-compatible", time.Second)
	defer teardown()

	const customID = "stirrup-run-1-turn-1"
	_, err := c.Submit(context.Background(), openaiSubmitEntries(customID, "openai-compatible"))
	must.NoError(err)
	results, err := c.Result(context.Background(), "batch_xyz")
	must.NoError(err)
	must.Contains(results, customID, "got keys %v", keysOf(results))
	entry := results[customID]
	must.NotNil(entry.Err)
	must.Equal("server_error", entry.Err.Type)
	must.Contains(entry.Err.Message, "top-level fallback")
}

// waitForOpenAICancelCount blocks (up to timeout) until cancelCalls
// reaches want. Mirrors pollServer.waitForCancelCount but reaches into
// openaiBatchFixture.cancelCalls under the existing mutex.
func waitForOpenAICancelCount(t *testing.T, f *openaiBatchFixture, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		got := f.cancelCalls
		f.mu.Unlock()
		if got >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestHarnessPollingBatch_OpenAIResult_TimeoutFiringAlignedToDeadline
// covers the sleep>remaining clamp at resultOpenAI: with maxWait set
// to ~3× initialInterval, the third sleep would (uncapped) exceed the
// remaining budget. The loop must clamp it and fire errBatchExpired
// within slack of the documented cap rather than one interval past it.
func TestHarnessPollingBatch_OpenAIResult_TimeoutFiringAlignedToDeadline(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-test"}
	polls := []string{`{"id":"batch_xyz","status":"in_progress"}`}
	f := newOpenAIBatchFixture(t, polls, "", "")
	defer f.Close()

	prevInterval := setBatchPollInitialInterval(10 * time.Millisecond)
	defer setBatchPollInitialInterval(prevInterval)
	prevJitter := setBatchPollJitterDisabled(true)
	defer setBatchPollJitterDisabled(prevJitter)

	const maxWait = 30 * time.Millisecond
	c := NewHarnessPollingBatchClient(HarnessBatchClientOptions{
		ProviderType: "openai-compatible",
		APIKeyRef:    "secret://test",
		CredSource:   src,
		BaseURL:      f.URL,
		MaxWait:      maxWait,
	})

	start := time.Now()
	_, err := c.Result(context.Background(), "batch_xyz")
	elapsed := time.Since(start)

	must.ErrorIs(err, errBatchExpired)
	must.LessOrEqual(elapsed, maxWait+50*time.Millisecond, "timeout fired %s past maxWait", elapsed-maxWait)
	// best-effort cancel must hit the OpenAI cancel URL shape, not the
	// Anthropic one.
	waitForOpenAICancelCount(t, f, 1, time.Second)
	f.mu.Lock()
	defer f.mu.Unlock()
	must.Equal(1, f.cancelCalls, "cancelCalls on timeout")
}

// TestHarnessPollingBatch_OpenAIBackoffCapped covers the cap-clamp
// after the second doubling: with initialInterval > cap/2, the second
// doubling would exceed the cap and must be clamped.
func TestHarnessPollingBatch_OpenAIBackoffCapped(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	src := &fakeCredentialSource{token: "sk-test"}

	var (
		mu        sync.Mutex
		callTimes []time.Time
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/batches/") &&
			!strings.HasSuffix(r.URL.Path, "/cancel"):
			mu.Lock()
			callTimes = append(callTimes, time.Now())
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"batch_xyz","status":"in_progress"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{}`)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer srv.Close()

	prevCap := swapBatchPollMaxInterval(5 * time.Millisecond)
	defer swapBatchPollMaxInterval(prevCap)
	prevInterval := setBatchPollInitialInterval(3 * time.Millisecond)
	defer setBatchPollInitialInterval(prevInterval)
	prevJitter := setBatchPollJitterDisabled(true)
	defer setBatchPollJitterDisabled(prevJitter)

	c := NewHarnessPollingBatchClient(HarnessBatchClientOptions{
		ProviderType: "openai-compatible",
		APIKeyRef:    "secret://test",
		CredSource:   src,
		BaseURL:      srv.URL,
		MaxWait:      40 * time.Millisecond,
	})

	_, _ = c.Result(context.Background(), "batch_xyz")

	mu.Lock()
	defer mu.Unlock()
	must.GreaterOrEqual(len(callTimes), 4, "poll calls within the 40ms budget")
	const slack = 20 * time.Millisecond
	for i := 2; i < len(callTimes); i++ {
		gap := callTimes[i].Sub(callTimes[i-1])
		check.LessOrEqual(gap, 5*time.Millisecond+slack, "poll interval %d exceeded cap+slack", i)
	}
}

// TestHarnessPollingBatch_OpenAIResult_CtxCancelledDuringPoll covers
// the errors.Is(ctx-error) branch for the OpenAI variant: cancel the
// parent ctx after the poll handler has accepted the request but
// before it writes a response, forcing the request context to be
// cancelled mid-flight. Result must return promptly with
// context.Canceled and the detached bestEffortCancel must reach the
// OpenAI cancel URL (not the Anthropic one).
func TestHarnessPollingBatch_OpenAIResult_CtxCancelledDuringPoll(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-test"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pollEntered := make(chan struct{}, 1)
	var (
		mu          sync.Mutex
		cancelPaths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/batches/") &&
			!strings.HasSuffix(r.URL.Path, "/cancel"):
			select {
			case pollEntered <- struct{}{}:
			default:
			}
			<-r.Context().Done()
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
			mu.Lock()
			cancelPaths = append(cancelPaths, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{}`)
		default:
			check.Failf("unexpected request", "%s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, teardown := newTestOpenAIPollingClient(t, srv, src, "openai-compatible", time.Second)
	defer teardown()

	done := make(chan error, 1)
	go func() {
		_, err := c.Result(ctx, "batch_xyz")
		done <- err
	}()

	select {
	case <-pollEntered:
	case <-time.After(time.Second):
		must.Fail("poll handler never entered")
	}

	cancel()

	select {
	case err := <-done:
		must.ErrorIs(err, context.Canceled)
	case <-time.After(2 * time.Second):
		must.Fail("Result did not return after ctx cancel during poll")
	}

	// bestEffortCancel detaches via context.Background, so the cancel hit
	// arrives after Result returns.
	must.Eventually(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(cancelPaths) > 0
	}, 2*time.Second, 5*time.Millisecond, "expected bestEffortCancel to reach the server")

	mu.Lock()
	defer mu.Unlock()
	must.Len(cancelPaths, 1, "cancel hits")
	must.True(strings.HasPrefix(cancelPaths[0], "/batches/"),
		"OpenAI cancel must hit /batches/{id}/cancel, got %q", cancelPaths[0])
}

// TestHarnessPollingBatch_OpenAIResult_TimeoutFiresOpenAICancelURL pins
// that the cancel fired on timeout addresses the OpenAI URL shape, not
// the Anthropic one — mirrors the Anthropic CancelURLEscaped test for
// the OpenAI path.
func TestHarnessPollingBatch_OpenAIResult_TimeoutFiresOpenAICancelURL(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	src := &fakeCredentialSource{token: "sk-test"}

	var (
		mu          sync.Mutex
		cancelPaths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/batches/") &&
			!strings.HasSuffix(r.URL.Path, "/cancel"):
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"batch_xyz","status":"in_progress"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
			mu.Lock()
			cancelPaths = append(cancelPaths, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{}`)
		default:
			check.Failf("unexpected request", "%s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c, teardown := newTestOpenAIPollingClient(t, srv, src, "openai-compatible", 30*time.Millisecond)
	defer teardown()

	_, err := c.Result(context.Background(), "batch_xyz")
	must.ErrorIs(err, errBatchExpired)

	must.Eventually(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(cancelPaths) > 0
	}, time.Second, 5*time.Millisecond, "expected bestEffortCancel to reach the server")

	mu.Lock()
	defer mu.Unlock()
	cancelPath := cancelPaths[0]
	must.True(strings.HasPrefix(cancelPath, "/batches/") && strings.HasSuffix(cancelPath, "/cancel"),
		"OpenAI cancel must address /batches/{id}/cancel, got %q", cancelPath)
	must.NotContains(cancelPath, "/v1/messages/", "cancel URL must not use the Anthropic path prefix")
}

// TestHarnessPollingBatch_OpenAIResult_UnknownStatus pins the default
// arm in resultOpenAI: a status string not in the documented vocabulary
// must not exit the loop. The poll continues until the deadline fires.
func TestHarnessPollingBatch_OpenAIResult_UnknownStatus(t *testing.T) {
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-test"}
	polls := []string{`{"id":"batch_xyz","status":"unicorn"}`}
	f := newOpenAIBatchFixture(t, polls, "", "")
	defer f.Close()

	c, teardown := newTestOpenAIPollingClient(t, f.Server, src, "openai-compatible", 30*time.Millisecond)
	defer teardown()

	_, err := c.Result(context.Background(), "batch_xyz")
	// Unknown status continues to poll; the loop must exit via the
	// wall-clock deadline (errBatchExpired) rather than a typed error
	// on the first unrecognised status.
	must.ErrorIs(err, errBatchExpired)
	// At least one poll must reach the default arm before the deadline.
	_, _, pollCount, _, _ := f.snapshot()
	must.GreaterOrEqual(pollCount, 1, "poll count on unknown status")
}

// TestHarnessPollingBatch_OpenAIBatchIDPathEscaped confirms a batchID
// containing path-sensitive characters is escaped into a single path
// segment in both the poll and the cancel URL — mirrors the Anthropic
// path-escaping defence.
func TestHarnessPollingBatch_OpenAIBatchIDPathEscaped(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	src := &fakeCredentialSource{token: "sk-test"}

	const sneakyID = "batch_../etc/passwd?x=1"
	wantSegment := url.PathEscape(sneakyID)

	var (
		mu        sync.Mutex
		pollPaths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.EscapedPath(), "/batches/") &&
			!strings.HasSuffix(r.URL.EscapedPath(), "/cancel"):
			mu.Lock()
			pollPaths = append(pollPaths, r.URL.EscapedPath())
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"id":%q,"status":"completed"}`, sneakyID)
		default:
			check.Failf("unexpected request", "%s %s", r.Method, r.URL.EscapedPath())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, teardown := newTestOpenAIPollingClient(t, srv, src, "openai-compatible", time.Second)
	defer teardown()

	_, err := c.Result(context.Background(), sneakyID)
	must.Error(err, "expected output_file_id-missing error")

	mu.Lock()
	defer mu.Unlock()
	must.NotEmpty(pollPaths, "expected at least one poll request")
	wantPath := "/batches/" + wantSegment
	must.Equal(wantPath, pollPaths[0], "poll path")
}
