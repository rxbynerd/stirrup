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

	"github.com/stretchr/testify/require"

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
		respBody := c.respondWith
		c.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
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

func TestGCSTraceEmitter_FinishUploadsRunSummary(t *testing.T) {
	srv := newGCSCaptureServer()
	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()

	emitter, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
		Bucket:           "my-bucket",
		ObjectPrefix:     "traces/",
		CredentialSource: &staticBearerSource{token: "test-token"},
		EndpointBaseURL:  httpSrv.URL,
	})
	require.NoError(t, err)

	timeout := 60
	emitter.Start("run-abc", &types.RunConfig{
		RunID:    "run-abc",
		Mode:     "execution",
		Provider: types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://K"},
		Timeout:  &timeout,
	})
	emitter.RecordTurn(types.TurnTrace{Turn: 1, Tokens: types.TokenUsage{Input: 50, Output: 25, CacheRead: 40, CacheWrite: 5, Reasoning: 3}})

	tr, err := emitter.Finish(context.Background(), "success")
	require.NoError(t, err)
	require.Equal(t, types.TokenUsage{Input: 50, Output: 25, CacheRead: 40, CacheWrite: 5, Reasoning: 3}, tr.TokenUsage)
	require.Equal(t, "run-abc", tr.ID)
	require.Equal(t, 1, tr.Turns)

	got := srv.last()
	require.Equal(t, http.MethodPost, got.Method)
	require.Equal(t, "Bearer test-token", got.Auth)
	require.Equal(t, "application/x-ndjson", got.ContentType)
	require.Contains(t, got.URL, "/upload/storage/v1/b/my-bucket/o")
	require.Contains(t, got.URL, "uploadType=media")
	require.Contains(t, got.URL, "name=traces/run-abc.jsonl")

	// Body must be a single valid JSON line that decodes back to a
	// RunTrace with the same ID.
	body := strings.TrimRight(string(got.Body), "\n")
	require.NotEmpty(t, body, "uploaded body is empty")
	var decoded types.RunTrace
	require.NoError(t, json.Unmarshal([]byte(body), &decoded), "body=%q", body)
	require.Equal(t, "run-abc", decoded.ID)
	require.Contains(t, body, `"tokenUsage":{"input":50,"output":25,"cacheRead":40,"cacheWrite":5,"reasoning":3}`)
	require.Equal(t, "secret://[REDACTED]", decoded.Config.Provider.APIKeyRef)
}

// TestGCSTraceEmitter_TurnRecordContentNotUploaded pins that the GCS
// emitter persists the run summary only: thinking text, signatures and
// redacted data recorded on a turn never reach the uploaded object.
func TestGCSTraceEmitter_TurnRecordContentNotUploaded(t *testing.T) {
	srv := newGCSCaptureServer()
	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()

	emitter, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
		Bucket:           "my-bucket",
		CredentialSource: &staticBearerSource{token: "test-token"},
		EndpointBaseURL:  httpSrv.URL,
	})
	require.NoError(t, err)

	const thinkingText = "gcs-thinking-text-not-uploaded"
	const signature = "gcs-thinking-signature-not-uploaded"
	const redacted = "gcs-redacted-data-not-uploaded"
	thinking := []types.ContentBlock{
		{Type: "thinking", Text: thinkingText, ThoughtSignature: signature},
		{Type: "redacted_thinking", ThoughtSignature: redacted},
	}
	emitter.Start("run-gcs-1", nil)
	emitter.RecordTurnRecord(types.TurnRecord{
		Turn:        1,
		ModelInput:  types.ModelInput{Messages: []types.Message{{Role: "assistant", Content: thinking}}},
		ModelOutput: thinking,
	})
	emitter.RecordTurn(types.TurnTrace{Turn: 1})
	_, err = emitter.Finish(context.Background(), "success")
	require.NoError(t, err)

	body := string(srv.last().Body)
	require.NotEmpty(t, body, "uploaded body is empty")

	leaks := []struct {
		name  string
		value string
	}{
		{"thinking text", thinkingText},
		{"thinking signature", signature},
		{"redacted data", redacted},
		{"thinking block type", "thinking"},
	}
	for _, tc := range leaks {
		t.Run(tc.name, func(t *testing.T) {
			require.NotContains(t, body, tc.value)
		})
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
	require.NoError(t, err)
	emitter.Start("r", nil)
	_, err = emitter.Finish(context.Background(), "success")
	require.NoError(t, err)
	require.Contains(t, srv.last().URL, "name=traces/r.jsonl")
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
	require.NoError(t, err)
	emitter.Start("only-run", nil)
	_, err = emitter.Finish(context.Background(), "success")
	require.NoError(t, err)
	require.Contains(t, srv.last().URL, "name=only-run.jsonl")
}

func TestGCSTraceEmitter_ServerErrorReturnsTraceWithUploadError(t *testing.T) {
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
	require.NoError(t, err)
	emitter.Start("r", nil)
	tr, err := emitter.Finish(context.Background(), "success")
	require.ErrorContains(t, err, "HTTP 403")
	require.NotNil(t, tr, "Finish should return the assembled trace alongside the upload error")
	require.Equal(t, "r", tr.ID)
	require.Equal(t, "success", tr.Outcome)
}

func TestGCSTraceEmitter_MissingBucketRejectedAtConstruction(t *testing.T) {
	_, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
		Bucket:           "",
		CredentialSource: &staticBearerSource{token: "t"},
	})
	require.ErrorContains(t, err, "bucket is required")
}

func TestGCSTraceEmitter_MissingCredentialRejectedAtConstruction(t *testing.T) {
	_, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
		Bucket:           "b",
		CredentialSource: nil,
	})
	require.Error(t, err)
}

func TestGCSObjectName_JoinsPrefixAndRunID(t *testing.T) {
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
			require.Equal(t, tc.want, gcsObjectName(tc.prefix, tc.runID))
		})
	}
}
