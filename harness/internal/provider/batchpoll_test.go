package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
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
	c := NewHarnessPollingBatchClient("secret://test", src, maxWait)
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

// -----------------------------------------------------------------------------
// Submit
// -----------------------------------------------------------------------------

func TestHarnessPollingBatch_SubmitHappyPath(t *testing.T) {
	src := &fakeCredentialSource{token: "sk-ant-test"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method: got %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/messages/batches" {
			t.Errorf("path: got %q, want /v1/messages/batches", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "sk-ant-test" {
			t.Errorf("x-api-key: got %q, want sk-ant-test", got)
		}
		if got := r.Header.Get("anthropic-version"); got != anthropicAPIVersion {
			t.Errorf("anthropic-version: got %q, want %s", got, anthropicAPIVersion)
		}

		// Confirm the wire body shape: a single "requests" entry with
		// the supplied custom_id and a passthrough params blob.
		var body anthropicBatchSubmitRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode submit body: %v", err)
		}
		if len(body.Requests) != 1 {
			t.Fatalf("expected 1 request, got %d", len(body.Requests))
		}
		if body.Requests[0].CustomID != "stirrup-run-1-turn-1" {
			t.Errorf("custom_id: got %q", body.Requests[0].CustomID)
		}

		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"id":"batch_abc","processing_status":"in_progress"}`)
	}))
	defer srv.Close()

	c, teardown := newTestPollingClient(t, srv, src, time.Second)
	defer teardown()

	id, err := c.Submit(context.Background(), anthropicSubmitEntries("stirrup-run-1-turn-1"))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if id != "batch_abc" {
		t.Errorf("batchID: got %q, want batch_abc", id)
	}
}

func TestHarnessPollingBatch_SubmitErrorResponse(t *testing.T) {
	src := &fakeCredentialSource{token: "sk-ant-test"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"bad request"}`)
	}))
	defer srv.Close()

	c, teardown := newTestPollingClient(t, srv, src, time.Second)
	defer teardown()

	_, err := c.Submit(context.Background(), anthropicSubmitEntries("stirrup-run-1-turn-1"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("expected status 400 in error, got: %v", err)
	}
}

func TestHarnessPollingBatch_RejectsMultiEntrySubmit(t *testing.T) {
	src := &fakeCredentialSource{token: "sk-ant-test"}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be hit on validation failure")
	}))
	defer srv.Close()

	c, teardown := newTestPollingClient(t, srv, src, time.Second)
	defer teardown()

	_, err := c.Submit(context.Background(), []BatchEntry{
		{CustomID: "a", Provider: "anthropic", Body: json.RawMessage(`{}`)},
		{CustomID: "b", Provider: "anthropic", Body: json.RawMessage(`{}`)},
	})
	if err == nil {
		t.Fatal("expected error for multi-entry submit, got nil")
	}
	if !strings.Contains(err.Error(), "exactly 1") {
		t.Errorf("expected 'exactly 1' in error, got: %v", err)
	}
}

func TestHarnessPollingBatch_SubmitMissingID(t *testing.T) {
	src := &fakeCredentialSource{token: "sk-ant-test"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"processing_status":"in_progress"}`)
	}))
	defer srv.Close()

	c, teardown := newTestPollingClient(t, srv, src, time.Second)
	defer teardown()

	_, err := c.Submit(context.Background(), anthropicSubmitEntries("stirrup-run-1-turn-1"))
	if err == nil || !strings.Contains(err.Error(), "missing id") {
		t.Fatalf("expected 'missing id' error, got: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Result (poll + fetch JSONL)
// -----------------------------------------------------------------------------

// pollServer is a tiny stateful test fixture: it serves a configurable
// sequence of /v1/messages/batches/{id} polls, then a results JSONL
// document, and tracks how many cancel calls fired. httptest dispatches
// each request on its own goroutine, so every shared counter sits behind
// a real sync.Mutex — the prior anonymous-struct grouping was a data
// race waiting for go test -race to catch it.
type pollServer struct {
	*httptest.Server

	pollResponses []string // one body per GET on the batch object; final is the "ended" response
	resultsBody   string   // JSONL served at /results

	mu          sync.Mutex
	pollCalls   int
	cancelCalls int
	resultCalls int
}

// pollCount, cancelCount, resultCount are accessor helpers used by tests
// to inspect the counters under the lock. The bare fields stay
// addressable (no getter for the handler-side writes) so the handler can
// take and release the lock as a single critical section.
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

// waitForCancelCount blocks (up to timeout) until cancelCalls reaches the
// expected count. With bestEffortCancel now detached into a goroutine,
// tests must wait on the side-effect rather than reading the counter
// immediately after Result returns.
func (ps *pollServer) waitForCancelCount(t *testing.T, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ps.cancelCount() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
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

func TestHarnessPollingBatch_ResultEventually(t *testing.T) {
	src := &fakeCredentialSource{token: "sk-ant-test"}
	polls := []string{
		`{"id":"batch_xyz","processing_status":"in_progress"}`,
		`{"id":"batch_xyz","processing_status":"in_progress"}`,
		// Note: results_url is set to ps.Server.URL + "/results" after
		// the server starts. We rewrite it in-place below.
		`{"id":"batch_xyz","processing_status":"ended","results_url":"REPLACE"}`,
	}
	resultsBody := `{"custom_id":"stirrup-run-1-turn-1","result":{"type":"succeeded","message":{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}}}` + "\n"

	ps := newPollServer(t, polls, resultsBody)
	defer ps.Close()

	// Rewrite the placeholder once the server has an URL.
	ps.pollResponses[2] = strings.ReplaceAll(ps.pollResponses[2], "REPLACE", ps.URL+"/results")

	c, teardown := newTestPollingClient(t, ps.Server, src, time.Second)
	defer teardown()

	results, err := c.Result(context.Background(), "batch_xyz")
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	entry, ok := results["stirrup-run-1-turn-1"]
	if !ok {
		t.Fatalf("missing entry; got keys %v", keysOf(results))
	}
	if entry.Err != nil {
		t.Fatalf("entry.Err: %+v", entry.Err)
	}
	if !strings.Contains(string(entry.Response), `"text":"ok"`) {
		t.Errorf("response body: %s", entry.Response)
	}
	if n := ps.pollCount(); n < 3 {
		t.Errorf("pollCalls: got %d, want >=3", n)
	}
	if n := ps.resultCount(); n != 1 {
		t.Errorf("resultCalls: got %d, want 1", n)
	}
	if n := ps.cancelCount(); n != 0 {
		t.Errorf("cancelCalls on happy path: got %d, want 0", n)
	}
}

func TestHarnessPollingBatch_ResultTimeout(t *testing.T) {
	src := &fakeCredentialSource{token: "sk-ant-test"}
	polls := []string{`{"id":"batch_xyz","processing_status":"in_progress"}`}
	ps := newPollServer(t, polls, "")
	defer ps.Close()

	// maxWait short enough that even a 2ms poll loop fires the
	// timeout within the test budget.
	c, teardown := newTestPollingClient(t, ps.Server, src, 50*time.Millisecond)
	defer teardown()

	_, err := c.Result(context.Background(), "batch_xyz")
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, errBatchExpired) {
		t.Errorf("error must wrap errBatchExpired so isBatchTimeout routes correctly; got: %v", err)
	}
	// bestEffortCancel is detached into a goroutine (B3), so wait for the
	// side-effect rather than reading the counter immediately.
	ps.waitForCancelCount(t, 1, time.Second)
	if n := ps.cancelCount(); n != 1 {
		t.Errorf("cancelCalls on timeout: got %d, want 1", n)
	}
}

func TestHarnessPollingBatch_ResultCtxCancel(t *testing.T) {
	src := &fakeCredentialSource{token: "sk-ant-test"}
	polls := []string{`{"id":"batch_xyz","processing_status":"in_progress"}`}
	ps := newPollServer(t, polls, "")
	defer ps.Close()

	c, teardown := newTestPollingClient(t, ps.Server, src, time.Second)
	defer teardown()

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after the first poll has had time to fire.
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
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Result did not return promptly after ctx cancel")
	}

	// bestEffortCancel runs in a detached goroutine (B3); wait on the
	// side-effect rather than a sleep-and-pray window.
	ps.waitForCancelCount(t, 1, time.Second)
	if n := ps.cancelCount(); n != 1 {
		t.Errorf("cancelCalls on ctx cancel: got %d, want 1", n)
	}
}

// -----------------------------------------------------------------------------
// Per-poll credential resolution
// -----------------------------------------------------------------------------

func TestHarnessPollingBatch_CredentialResolvedPerPoll(t *testing.T) {
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

	if _, err := c.Result(context.Background(), "batch_xyz"); err != nil {
		t.Fatalf("Result: %v", err)
	}

	// Three poll calls + one results GET = at least four resolves.
	// Allow >= 4 rather than ==4 in case the bestEffortCancel-on-
	// success path ever changes (it currently does not, but a future
	// "cancel on succeeded" refactor should not break this test for
	// the credential-rotation invariant it actually guards).
	calls := src.resolveCalls.Load()
	if calls < 4 {
		t.Errorf("Resolve should be called per HTTP request; got %d, want >=4", calls)
	}
}

// -----------------------------------------------------------------------------
// HTTP-error surfaces (B6)
// -----------------------------------------------------------------------------

// TestHarnessPollingBatch_ResultsURLNonOKStatus confirms a 500 from
// results_url is propagated up — it must NOT be silently converted into
// errBatchExpired (which would route to the FallbackOnTimeout branch
// and mask a real upstream failure).
func TestHarnessPollingBatch_ResultsURLNonOKStatus(t *testing.T) {
	src := &fakeCredentialSource{token: "sk-ant-test"}

	// resultsURL is captured by the handler closure; populated after the
	// httptest.Server starts so the same server serves both the poll and
	// the failing results_url endpoint (loopback-relaxation branch in
	// validateResultsURL accepts the same-host URL).
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
	if err == nil {
		t.Fatal("expected error for 500 results_url, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should include status 500, got: %v", err)
	}
	if errors.Is(err, errBatchExpired) {
		t.Errorf("non-200 results_url must not be classified as batch_expired, got: %v", err)
	}
}

// TestHarnessPollingBatch_PollOnceNonOKStatus confirms a 503 on the
// first poll is surfaced as an HTTP error (not silently converted to
// errBatchExpired). Anthropic's batch API is durable, so a transient
// 5xx is the upstream transport's problem; the harness must surface it
// so the operator (and any retrying caller above) sees the real status.
func TestHarnessPollingBatch_PollOnceNonOKStatus(t *testing.T) {
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
	if err == nil {
		t.Fatal("expected error for 503 poll, got nil")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error should include status 503, got: %v", err)
	}
	if errors.Is(err, errBatchExpired) {
		t.Errorf("non-200 poll status must not be classified as batch_expired, got: %v", err)
	}
}

// TestHarnessPollingBatch_BestEffortCancel_HangsForCancelTimeout asserts
// the B3 non-blocking guarantee: when the cancel endpoint hangs longer
// than batchCancelTimeout, Result must still return promptly because
// bestEffortCancel runs in a detached goroutine. Before B3 this test
// would have blocked for the full batchCancelTimeout (10s).
func TestHarnessPollingBatch_BestEffortCancel_HangsForCancelTimeout(t *testing.T) {
	src := &fakeCredentialSource{token: "sk-ant-test"}

	// Use a chan to release the hanging cancel handler when the test ends,
	// so we do not leak goroutines beyond the test boundary.
	release := make(chan struct{})
	defer close(release)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/messages/batches/"):
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"batch_xyz","processing_status":"in_progress"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
			// Block until the test releases — simulates a slow Anthropic
			// cancel endpoint that would otherwise pin the goroutine to
			// the full batchCancelTimeout.
			<-release
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// 30 ms maxWait lets the timeout fire well before the test budget.
	c, teardown := newTestPollingClient(t, srv, src, 30*time.Millisecond)
	defer teardown()

	start := time.Now()
	_, err := c.Result(context.Background(), "batch_xyz")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, errBatchExpired) {
		t.Errorf("expected errBatchExpired, got: %v", err)
	}
	// The cancel handler is still blocked; without B3 we would be inside
	// httpClient.Do here for up to batchCancelTimeout (10s). The
	// generous-but-bounded 2s upper bound is well under batchCancelTimeout
	// so a regression that re-synchronises the call will trip it.
	if elapsed > 2*time.Second {
		t.Errorf("Result must not block on bestEffortCancel; took %s", elapsed)
	}
}

// TestHarnessPollingBatch_CtxCancelledDuringPoll covers the
// errors.Is(ctx-error) branch added in R7: cancel the parent ctx after
// the poll handler has accepted the request but before it writes a
// response, forcing the request context to be cancelled mid-flight.
// Without the R7 fix, http.Client.Timeout-driven DeadlineExceeded
// errors from a per-request ctx would have fallen through the branch.
func TestHarnessPollingBatch_CtxCancelledDuringPoll(t *testing.T) {
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
		t.Fatal("poll handler never entered")
	}

	// Cancel the parent context mid-flight so the in-flight request
	// fails with context.Canceled — the load-bearing case for the R7
	// errors.Is branch in Result.
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Result did not return after ctx cancel during poll")
	}

	// bestEffortCancel runs in a detached goroutine with
	// context.Background(), so it should reach the server even though
	// the parent ctx is now cancelled. Wait briefly for it to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cancelHits.Load() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if cancelHits.Load() != 1 {
		t.Errorf("expected exactly 1 cancel hit, got %d", cancelHits.Load())
	}
}

// -----------------------------------------------------------------------------
// Result type mapping
// -----------------------------------------------------------------------------

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
			var line anthropicBatchResultLine
			if err := json.Unmarshal([]byte(`{"custom_id":"id","result":`+tc.resultJSON+`}`), &line); err != nil {
				t.Fatalf("unmarshal fixture: %v", err)
			}
			got := mapBatchResultLine(line)
			if tc.wantSucc {
				if got.Err != nil {
					t.Errorf("expected success; got Err=%+v", got.Err)
				}
				if got.Response == nil {
					t.Errorf("expected non-nil Response")
				}
				return
			}
			if got.Err == nil || got.Err.Type != tc.wantErrTy {
				t.Errorf("Err.Type: got %+v, want %q", got.Err, tc.wantErrTy)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Credential failure surfaces
// -----------------------------------------------------------------------------

func TestHarnessPollingBatch_CredentialResolveFails(t *testing.T) {
	src := &fakeCredentialSource{resolveErr: errors.New("vault down")}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be hit when credential resolve fails")
	}))
	defer srv.Close()

	c, teardown := newTestPollingClient(t, srv, src, time.Second)
	defer teardown()

	_, err := c.Submit(context.Background(), anthropicSubmitEntries("id"))
	if err == nil || !strings.Contains(err.Error(), "vault down") {
		t.Fatalf("expected vault down error, got: %v", err)
	}
}

func TestHarnessPollingBatch_CredentialNilBearer(t *testing.T) {
	src := &fakeCredentialSource{nilBearer: true}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be hit when no bearer token is produced")
	}))
	defer srv.Close()

	c, teardown := newTestPollingClient(t, srv, src, time.Second)
	defer teardown()

	_, err := c.Submit(context.Background(), anthropicSubmitEntries("id"))
	if err == nil || !strings.Contains(err.Error(), "no bearer token") {
		t.Fatalf("expected no-bearer-token error, got: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Result body sanity
// -----------------------------------------------------------------------------

func TestHarnessPollingBatch_ResultsMissingCustomID(t *testing.T) {
	src := &fakeCredentialSource{token: "sk-ant-test"}
	polls := []string{`{"id":"batch_xyz","processing_status":"ended","results_url":"REPLACE"}`}
	// Empty JSONL — well-formed HTTP 200 but no lines.
	ps := newPollServer(t, polls, "")
	defer ps.Close()
	ps.pollResponses[0] = strings.ReplaceAll(ps.pollResponses[0], "REPLACE", ps.URL+"/results")

	c, teardown := newTestPollingClient(t, ps.Server, src, time.Second)
	defer teardown()

	_, err := c.Result(context.Background(), "batch_xyz")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty-results error, got: %v", err)
	}
}

func TestHarnessPollingBatch_ResultsURLMissing(t *testing.T) {
	src := &fakeCredentialSource{token: "sk-ant-test"}
	polls := []string{`{"id":"batch_xyz","processing_status":"ended"}`}
	ps := newPollServer(t, polls, "")
	defer ps.Close()

	c, teardown := newTestPollingClient(t, ps.Server, src, time.Second)
	defer teardown()

	_, err := c.Result(context.Background(), "batch_xyz")
	if err == nil || !strings.Contains(err.Error(), "results_url") {
		t.Fatalf("expected missing results_url error, got: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Jitter
// -----------------------------------------------------------------------------

func TestHarnessPollingBatch_JitterStaysWithinBounds(t *testing.T) {
	// Enable jitter for this test only; restore on exit.
	prev := setBatchPollJitterDisabled(false)
	defer setBatchPollJitterDisabled(prev)

	d := 100 * time.Millisecond
	low := d - d/5
	high := d + d/5
	for i := 0; i < 200; i++ {
		got := jitter(d)
		if got < low || got > high {
			t.Fatalf("iter %d: jitter produced %v outside [%v, %v]", i, got, low, high)
		}
	}
}

// -----------------------------------------------------------------------------
// results_url origin validation
// -----------------------------------------------------------------------------

// TestHarnessPollingBatch_ResultsURLBadOrigin confirms an "ended" batch
// whose results_url points off-domain is rejected before fetchResults
// would send the credential. The test server records whether any GET
// reached the (would-be exfiltration) path; the assertion must observe
// zero hits.
func TestHarnessPollingBatch_ResultsURLBadOrigin(t *testing.T) {
	src := &fakeCredentialSource{token: "sk-ant-test"}

	var exfilHits atomic.Int64
	exfilSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		exfilHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer exfilSrv.Close()

	// Use a non-loopback off-domain URL so the test-mode relaxation does
	// not paper over the check. evil.com is registered but should never
	// be reached because validation fires first.
	const badURL = "https://evil.com/exfil"

	polls := []string{
		fmt.Sprintf(`{"id":"batch_xyz","processing_status":"ended","results_url":%q}`, badURL),
	}
	ps := newPollServer(t, polls, "")
	defer ps.Close()

	c, teardown := newTestPollingClient(t, ps.Server, src, time.Second)
	defer teardown()

	_, err := c.Result(context.Background(), "batch_xyz")
	if err == nil {
		t.Fatal("expected error for off-domain results_url, got nil")
	}
	if !strings.Contains(err.Error(), "results_url host") {
		t.Errorf("error should mention results_url host, got: %v", err)
	}
	if exfilHits.Load() != 0 {
		t.Errorf("validation must fire before the GET; got %d hits", exfilHits.Load())
	}
}

// TestHarnessPollingBatch_ResultsURLNonHTTPS confirms an http:// scheme
// (even on anthropic.com) is rejected — the credential is bearer-class
// and must not be sent in cleartext.
func TestHarnessPollingBatch_ResultsURLNonHTTPS(t *testing.T) {
	src := &fakeCredentialSource{token: "sk-ant-test"}

	polls := []string{
		`{"id":"batch_xyz","processing_status":"ended","results_url":"http://anthropic.com/results"}`,
	}
	ps := newPollServer(t, polls, "")
	defer ps.Close()

	c, teardown := newTestPollingClient(t, ps.Server, src, time.Second)
	defer teardown()

	_, err := c.Result(context.Background(), "batch_xyz")
	if err == nil {
		t.Fatal("expected error for http results_url, got nil")
	}
	if !strings.Contains(err.Error(), "scheme") {
		t.Errorf("error should mention scheme, got: %v", err)
	}
}

// TestHarnessPollingBatch_ResultsURLAnthropicHostAccepted is a unit-level
// check on validateResultsURL — when the caller's baseURL is the
// production Anthropic root, an *.anthropic.com results_url must pass.
// The end-to-end happy-path test still uses an httptest base URL (relaxed
// branch); this case exercises the strict-host branch directly.
func TestHarnessPollingBatch_ResultsURLAnthropicHostAccepted(t *testing.T) {
	cases := []string{
		"https://api.anthropic.com/v1/messages/batches/abc/results",
		"https://anthropic.com/results",
		"https://eu.api.anthropic.com/results",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			if err := validateResultsURL(raw, "https://api.anthropic.com"); err != nil {
				t.Errorf("expected acceptance, got: %v", err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// URL escaping
// -----------------------------------------------------------------------------

// TestHarnessPollingBatch_BatchIDPathEscaped confirms a batchID containing
// path-sensitive characters is escaped into a single path segment before
// being concatenated into the poll / cancel URLs. A bare concatenation
// would let an attacker-supplied (or upstream-mangled) batchID navigate
// to an unintended endpoint; the gemini.go adapter applies the same
// defence at every path component and the polling client mirrors it.
func TestHarnessPollingBatch_BatchIDPathEscaped(t *testing.T) {
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
			// First poll returns "ended" with an empty results_url so
			// Result errors out before fetchResults is exercised — we
			// only care about the URL escaping here.
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"id":%q,"processing_status":"ended"}`, sneakyID)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.EscapedPath(), "/cancel"):
			obs.cancelHits++
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, teardown := newTestPollingClient(t, srv, src, time.Second)
	defer teardown()

	_, err := c.Result(context.Background(), sneakyID)
	if err == nil {
		t.Fatal("expected results_url-missing error, got nil")
	}

	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.pollPaths) == 0 {
		t.Fatal("expected at least one poll request")
	}
	wantPath := "/v1/messages/batches/" + wantPathSegment
	if obs.pollPaths[0] != wantPath {
		t.Errorf("poll path: got %q, want %q", obs.pollPaths[0], wantPath)
	}
}

// TestHarnessPollingBatch_CancelURLEscaped confirms the cancel POST also
// escapes batchID into a single path segment.
func TestHarnessPollingBatch_CancelURLEscaped(t *testing.T) {
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
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, teardown := newTestPollingClient(t, srv, src, 30*time.Millisecond)
	defer teardown()

	_, err := c.Result(context.Background(), sneakyID)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}

	// bestEffortCancel is now async (B3); wait briefly for it to fire.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		hit := len(cancelPaths) > 0
		mu.Unlock()
		if hit {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(cancelPaths) == 0 {
		t.Fatal("expected bestEffortCancel to fire and reach the server")
	}
	if cancelPaths[0] != wantCancelPath {
		t.Errorf("cancel path: got %q, want %q", cancelPaths[0], wantCancelPath)
	}
}

// -----------------------------------------------------------------------------
// Transport / decoder edge cases (R5)
// -----------------------------------------------------------------------------

// TestHarnessPollingBatch_SubmitTransportError covers the path where the
// initial POST to /v1/messages/batches fails at the transport layer (the
// upstream server is gone before Submit fires). The harness must surface
// the failure as a wrapped "submit batch" error rather than returning a
// bare "" batchID or panicking on the nil response.
func TestHarnessPollingBatch_SubmitTransportError(t *testing.T) {
	src := &fakeCredentialSource{token: "sk-ant-test"}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not receive a request after Close()")
	}))
	// Close immediately so the client's first connection attempt fails.
	srv.Close()

	c, teardown := newTestPollingClient(t, srv, src, time.Second)
	defer teardown()

	_, err := c.Submit(context.Background(), anthropicSubmitEntries("id"))
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
	if !strings.Contains(err.Error(), "submit batch") {
		t.Errorf("error should mention 'submit batch', got: %v", err)
	}
}

// TestHarnessPollingBatch_MalformedJSONLLine confirms a non-JSON line in
// the JSONL results document fails fast rather than being silently
// skipped. The Scanner reads each line in turn; a bad line is the only
// signal that the upstream document has been truncated or framed
// incorrectly.
func TestHarnessPollingBatch_MalformedJSONLLine(t *testing.T) {
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
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "decode batch result line") {
		t.Errorf("error should mention 'decode batch result line', got: %v", err)
	}
}

// TestHarnessPollingBatch_BackoffCapped exercises the interval-doubling
// cap at batchPollMaxInterval. With initialInterval set just above
// half the cap, the *third* interval would (uncapped) exceed the cap;
// the loop must clamp it. We observe by spacing between successive poll
// requests on the test server.
func TestHarnessPollingBatch_BackoffCapped(t *testing.T) {
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

	c := NewHarnessPollingBatchClient("secret://test", src, 40*time.Millisecond)
	c.baseURL = srv.URL

	_, _ = c.Result(context.Background(), "batch_xyz")

	mu.Lock()
	defer mu.Unlock()
	if len(callTimes) < 4 {
		t.Fatalf("expected >=4 poll calls within the 40ms budget, got %d", len(callTimes))
	}
	// From the third sleep onwards each interval should be <= cap + slack.
	// Slack covers scheduler jitter on busy CI runners.
	const slack = 20 * time.Millisecond
	for i := 2; i < len(callTimes); i++ {
		gap := callTimes[i].Sub(callTimes[i-1])
		if gap > 5*time.Millisecond+slack {
			t.Errorf("poll interval %d exceeded cap+slack: got %s", i, gap)
		}
	}
}

// TestHarnessPollingBatch_TimeoutFiringAlignedToDeadline covers the
// sleep>remaining clamp at Result. With maxWait set to slightly under
// 3 × initialInterval, the third sleep would (uncapped) exceed the
// remaining budget; the loop must clamp it and fire the timeout on the
// documented cap rather than one full interval past it.
func TestHarnessPollingBatch_TimeoutFiringAlignedToDeadline(t *testing.T) {
	src := &fakeCredentialSource{token: "sk-ant-test"}

	polls := []string{`{"id":"batch_xyz","processing_status":"in_progress"}`}
	ps := newPollServer(t, polls, "")
	defer ps.Close()

	prevInterval := setBatchPollInitialInterval(10 * time.Millisecond)
	defer setBatchPollInitialInterval(prevInterval)
	prevJitter := setBatchPollJitterDisabled(true)
	defer setBatchPollJitterDisabled(prevJitter)

	maxWait := 30 * time.Millisecond
	c := NewHarnessPollingBatchClient("secret://test", src, maxWait)
	c.baseURL = ps.URL

	start := time.Now()
	_, err := c.Result(context.Background(), "batch_xyz")
	elapsed := time.Since(start)

	if err == nil || !errors.Is(err, errBatchExpired) {
		t.Fatalf("expected errBatchExpired, got: %v", err)
	}
	// The deadline should fire within ~maxWait + scheduler slack. A
	// generous 50 ms slack tolerates loaded CI runners without hiding a
	// genuine regression (which would overshoot by a full interval +).
	if elapsed > maxWait+50*time.Millisecond {
		t.Errorf("timeout fired %s past maxWait; want <= maxWait + 50ms", elapsed-maxWait)
	}
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
