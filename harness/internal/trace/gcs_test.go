package trace

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/types"
)

// staticBearerSource is a credential.Source that returns a fixed
// bearer token. Used so the test does not depend on a real metadata
// server or a Google API round-trip.
type staticBearerSource struct {
	token string
	err   error
}

func (s *staticBearerSource) Resolve(_ context.Context) (*credential.Resolved, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &credential.Resolved{
		BearerToken: func(_ context.Context) (string, error) { return s.token, nil },
	}, nil
}

// gcsCaptureServer is an httptest server that records every incoming
// request so tests can assert on path, headers, and body. Optionally
// returns a non-2xx status to exercise the error-propagation path.
type gcsCaptureServer struct {
	mu          sync.Mutex
	requests    []gcsCapturedRequest
	statusCode  int    // 0 means 200
	respondWith string // optional response body
}

type gcsCapturedRequest struct {
	Method      string
	URL         string
	ContentType string
	Auth        string
	Body        []byte
}

func newGCSCaptureServer() *gcsCaptureServer {
	return &gcsCaptureServer{}
}

func (c *gcsCaptureServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		c.mu.Lock()
		c.requests = append(c.requests, gcsCapturedRequest{
			Method:      r.Method,
			URL:         r.URL.String(),
			ContentType: r.Header.Get("Content-Type"),
			Auth:        r.Header.Get("Authorization"),
			Body:        body,
		})
		status := c.statusCode
		body2 := c.respondWith
		c.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body2)
	})
}

func (c *gcsCaptureServer) last() gcsCapturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.requests) == 0 {
		return gcsCapturedRequest{}
	}
	return c.requests[len(c.requests)-1]
}

func TestGCSTraceEmitter_Success(t *testing.T) {
	srv := newGCSCaptureServer()
	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()

	emitter, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
		Bucket:           "my-bucket",
		ObjectPrefix:     "traces/",
		CredentialSource: &staticBearerSource{token: "test-token"},
		EndpointBaseURL:  httpSrv.URL,
	})
	if err != nil {
		t.Fatalf("NewGCSTraceEmitter: %v", err)
	}

	timeout := 60
	emitter.Start("run-abc", &types.RunConfig{
		RunID:    "run-abc",
		Mode:     "execution",
		Provider: types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://K"},
		Timeout:  &timeout,
	})
	emitter.RecordTurn(types.TurnTrace{Turn: 1, Tokens: types.TokenUsage{Input: 50, Output: 25}})

	tr, err := emitter.Finish(context.Background(), "success")
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if tr.ID != "run-abc" {
		t.Errorf("returned trace ID: got %q, want run-abc", tr.ID)
	}
	if tr.Turns != 1 {
		t.Errorf("returned trace Turns: got %d, want 1", tr.Turns)
	}

	got := srv.last()
	if got.Method != http.MethodPost {
		t.Errorf("method: got %q, want POST", got.Method)
	}
	if got.Auth != "Bearer test-token" {
		t.Errorf("auth header: got %q, want Bearer test-token", got.Auth)
	}
	if got.ContentType != "application/x-ndjson" {
		t.Errorf("content-type: got %q, want application/x-ndjson", got.ContentType)
	}
	if !strings.Contains(got.URL, "/upload/storage/v1/b/my-bucket/o") {
		t.Errorf("URL missing upload path: %q", got.URL)
	}
	if !strings.Contains(got.URL, "uploadType=media") {
		t.Errorf("URL missing uploadType=media: %q", got.URL)
	}
	if !strings.Contains(got.URL, "name=traces/run-abc.jsonl") {
		t.Errorf("URL missing expected name=traces/run-abc.jsonl: %q", got.URL)
	}

	// Body must be a single valid JSON line that decodes back to a
	// RunTrace with the same ID.
	body := strings.TrimRight(string(got.Body), "\n")
	if body == "" {
		t.Fatal("uploaded body is empty")
	}
	var decoded types.RunTrace
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("unmarshal uploaded body: %v\nbody=%q", err, body)
	}
	if decoded.ID != "run-abc" {
		t.Errorf("decoded trace ID: got %q, want run-abc", decoded.ID)
	}
	if decoded.Config.Provider.APIKeyRef != "secret://[REDACTED]" {
		t.Errorf("APIKeyRef should be redacted, got %q", decoded.Config.Provider.APIKeyRef)
	}
}

func TestGCSTraceEmitter_PrefixWithoutTrailingSlash(t *testing.T) {
	srv := newGCSCaptureServer()
	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()

	emitter, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
		Bucket:           "b",
		ObjectPrefix:     "traces", // no trailing slash
		CredentialSource: &staticBearerSource{token: "t"},
		EndpointBaseURL:  httpSrv.URL,
	})
	if err != nil {
		t.Fatalf("NewGCSTraceEmitter: %v", err)
	}
	emitter.Start("r", nil)
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if !strings.Contains(srv.last().URL, "name=traces/r.jsonl") {
		t.Errorf("expected name=traces/r.jsonl in URL, got %q", srv.last().URL)
	}
}

func TestGCSTraceEmitter_EmptyPrefix(t *testing.T) {
	srv := newGCSCaptureServer()
	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()

	emitter, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
		Bucket:           "b",
		ObjectPrefix:     "",
		CredentialSource: &staticBearerSource{token: "t"},
		EndpointBaseURL:  httpSrv.URL,
	})
	if err != nil {
		t.Fatalf("NewGCSTraceEmitter: %v", err)
	}
	emitter.Start("only-run", nil)
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if !strings.Contains(srv.last().URL, "name=only-run.jsonl") {
		t.Errorf("expected name=only-run.jsonl in URL, got %q", srv.last().URL)
	}
}

func TestGCSTraceEmitter_ServerError(t *testing.T) {
	srv := newGCSCaptureServer()
	srv.statusCode = http.StatusForbidden
	srv.respondWith = `{"error":{"code":403,"message":"forbidden"}}`
	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()

	emitter, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
		Bucket:           "denied-bucket",
		CredentialSource: &staticBearerSource{token: "t"},
		EndpointBaseURL:  httpSrv.URL,
	})
	if err != nil {
		t.Fatalf("NewGCSTraceEmitter: %v", err)
	}
	emitter.Start("r", nil)
	if _, err := emitter.Finish(context.Background(), "success"); err == nil {
		t.Fatal("Finish: want error, got nil")
	} else if !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("error should mention HTTP 403, got %v", err)
	}
}

func TestGCSTraceEmitter_MissingBucketRejectedAtConstruction(t *testing.T) {
	_, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
		Bucket:           "",
		CredentialSource: &staticBearerSource{token: "t"},
	})
	if err == nil {
		t.Fatal("want error for empty bucket")
	}
	if !strings.Contains(err.Error(), "bucket is required") {
		t.Errorf("error should mention bucket, got %v", err)
	}
}

func TestGCSTraceEmitter_MissingCredentialRejectedAtConstruction(t *testing.T) {
	_, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
		Bucket:           "b",
		CredentialSource: nil,
	})
	if err == nil {
		t.Fatal("want error for nil credential source")
	}
}

func TestGCSObjectName(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		runID  string
		want   string
	}{
		{"empty prefix", "", "abc", "abc.jsonl"},
		{"no trailing slash", "traces", "abc", "traces/abc.jsonl"},
		{"with trailing slash", "traces/", "abc", "traces/abc.jsonl"},
		{"nested prefix", "a/b", "abc", "a/b/abc.jsonl"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gcsObjectName(tc.prefix, tc.runID); got != tc.want {
				t.Errorf("gcsObjectName(%q, %q) = %q, want %q", tc.prefix, tc.runID, got, tc.want)
			}
		})
	}
}
