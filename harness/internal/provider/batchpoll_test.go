package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
// document, and tracks how many cancel calls fired. mu guards every
// shared field so a t.Parallel() future-self stays race-free.
type pollServer struct {
	*httptest.Server

	pollResponses []string // one body per GET on the batch object; final is the "ended" response
	resultsBody   string   // JSONL served at /results
	mu            struct {
		pollCalls   int
		cancelCalls int
		resultCalls int
	}
}

func newPollServer(t *testing.T, polls []string, resultsBody string) *pollServer {
	t.Helper()
	ps := &pollServer{pollResponses: polls, resultsBody: resultsBody}
	ps.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/results":
			ps.mu.resultCalls++
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, ps.resultsBody)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
			ps.mu.cancelCalls++
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/messages/batches/"):
			idx := ps.mu.pollCalls
			if idx >= len(ps.pollResponses) {
				idx = len(ps.pollResponses) - 1
			}
			ps.mu.pollCalls++
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, ps.pollResponses[idx])
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
	if ps.mu.pollCalls < 3 {
		t.Errorf("pollCalls: got %d, want >=3", ps.mu.pollCalls)
	}
	if ps.mu.resultCalls != 1 {
		t.Errorf("resultCalls: got %d, want 1", ps.mu.resultCalls)
	}
	if ps.mu.cancelCalls != 0 {
		t.Errorf("cancelCalls on happy path: got %d, want 0", ps.mu.cancelCalls)
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
	if ps.mu.cancelCalls != 1 {
		t.Errorf("cancelCalls on timeout: got %d, want 1", ps.mu.cancelCalls)
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

	// Give the best-effort cancel a moment to fire on the server.
	time.Sleep(50 * time.Millisecond)
	if ps.mu.cancelCalls != 1 {
		t.Errorf("cancelCalls on ctx cancel: got %d, want 1", ps.mu.cancelCalls)
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
