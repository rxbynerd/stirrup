package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// probeRecorder counts requests to a metadata path vs a completion path so
// the no-completion-endpoint invariant (issue #245 AC) can be asserted.
type probeRecorder struct {
	metadataHits   atomic.Int64
	completionHits atomic.Int64
}

func TestAnthropicAdapter_Probe(t *testing.T) {
	var rec probeRecorder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models"):
			rec.metadataHits.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages"):
			rec.completionHits.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("sk-test"), AuthModeAPIKey)
	adapter.baseURL = srv.URL + "/v1/messages"

	if err := adapter.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: unexpected error: %v", err)
	}
	if got := rec.metadataHits.Load(); got != 1 {
		t.Errorf("metadata endpoint hits = %d, want 1", got)
	}
	if got := rec.completionHits.Load(); got != 0 {
		t.Errorf("completion endpoint hits = %d, want 0 (dry-run must not spend tokens)", got)
	}
}

func TestAnthropicAdapter_Probe_BadKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid x-api-key"}}`))
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("bad"), AuthModeAPIKey)
	adapter.baseURL = srv.URL + "/v1/messages"

	err := adapter.Probe(context.Background())
	if err == nil {
		t.Fatal("Probe: expected error for 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid x-api-key") {
		t.Errorf("error should carry status and diagnostic, got: %v", err)
	}
}

func TestOpenAICompatibleAdapter_Probe(t *testing.T) {
	var rec probeRecorder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models"):
			rec.metadataHits.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			rec.completionHits.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("sk-test"), srv.URL+"/v1", OpenAIAuthConfig{}, RetryPolicy{})
	if err := adapter.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: unexpected error: %v", err)
	}
	if got := rec.metadataHits.Load(); got != 1 {
		t.Errorf("metadata endpoint hits = %d, want 1", got)
	}
	if got := rec.completionHits.Load(); got != 0 {
		t.Errorf("completion endpoint hits = %d, want 0", got)
	}
}

func TestOpenAIResponsesAdapter_Probe(t *testing.T) {
	var rec probeRecorder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models"):
			rec.metadataHits.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasSuffix(r.URL.Path, "/responses"):
			rec.completionHits.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	adapter := NewOpenAIResponsesAdapter(staticBearer("sk-test"), srv.URL+"/v1", OpenAIAuthConfig{})
	if err := adapter.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: unexpected error: %v", err)
	}
	if got := rec.metadataHits.Load(); got != 1 {
		t.Errorf("metadata endpoint hits = %d, want 1", got)
	}
	if got := rec.completionHits.Load(); got != 0 {
		t.Errorf("completion endpoint hits = %d, want 0", got)
	}
}

func TestGeminiAdapter_Probe(t *testing.T) {
	var rec probeRecorder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models"):
			rec.metadataHits.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"models":[]}`))
		case strings.Contains(r.URL.Path, ":streamGenerateContent"):
			rec.completionHits.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	adapter := NewGeminiAdapter(staticBearer("ya29.token"), "proj", "us-central1", nil)
	adapter.baseURLOverride = srv.URL
	if err := adapter.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: unexpected error: %v", err)
	}
	if got := rec.metadataHits.Load(); got != 1 {
		t.Errorf("metadata endpoint hits = %d, want 1", got)
	}
	if got := rec.completionHits.Load(); got != 0 {
		t.Errorf("completion endpoint hits = %d, want 0", got)
	}
}

func TestProbe_CredentialError(t *testing.T) {
	adapter := NewAnthropicAdapter(erroringBearer("no creds"), AuthModeAPIKey)
	if err := adapter.Probe(context.Background()); err == nil {
		t.Fatal("Probe: expected credential error, got nil")
	}
}

func TestBedrockAdapter_Probe_NilCredentials(t *testing.T) {
	// The mock-client construction path leaves credentials nil; Probe must
	// then be a no-op rather than panicking or failing.
	adapter := &BedrockAdapter{}
	if err := adapter.Probe(context.Background()); err != nil {
		t.Fatalf("Probe with nil credentials should be a no-op, got: %v", err)
	}
}
