package provider

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// newHTTP2TLSServer starts a TLS httptest server that offers HTTP/2.
func newHTTP2TLSServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// trustServer makes client's transport accept srv's self-signed certificate.
func trustServer(t *testing.T, client *http.Client, srv *httptest.Server) {
	t.Helper()
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", client.Transport)
	}
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	tr.TLSClientConfig = &tls.Config{RootCAs: pool}
}

func getProto(t *testing.T, client *http.Client, url string) int {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	client.CloseIdleConnections()
	return resp.ProtoMajor
}

func TestStreamingHTTPClients_NegotiateHTTP2(t *testing.T) {
	srv := newHTTP2TLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	clients := map[string]*http.Client{
		"anthropic":         NewAnthropicAdapter(staticBearer("k"), AuthModeAPIKey).httpClient,
		"openai-compatible": NewOpenAICompatibleAdapter(staticBearer("k"), "", OpenAIAuthConfig{}, RetryPolicy{}).httpClient,
		"openai-responses":  NewOpenAIResponsesAdapter(staticBearer("k"), "", OpenAIAuthConfig{}).httpClient,
		"gemini":            NewGeminiAdapter(bearerFromTokenSource(&stubTokenSource{}), "p", "global", nil).httpClient,
	}
	for name, client := range clients {
		t.Run(name, func(t *testing.T) {
			trustServer(t, client, srv)
			if got := getProto(t, client, srv.URL); got != 2 {
				t.Errorf("negotiated HTTP/%d, want HTTP/2", got)
			}
		})
	}
}

func TestStreamingHTTPClient_CustomDialerNeedsForceAttemptHTTP2(t *testing.T) {
	srv := newHTTP2TLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	client := newStreamingHTTPClient()
	client.Transport.(*http.Transport).ForceAttemptHTTP2 = false
	trustServer(t, client, srv)
	if got := getProto(t, client, srv.URL); got != 1 {
		t.Errorf("without ForceAttemptHTTP2 negotiated HTTP/%d, want HTTP/1.1", got)
	}
}

func TestAnthropicAdapter_IdleTimeoutOverHTTP2(t *testing.T) {
	const idle = 150 * time.Millisecond
	disconnected := make(chan struct{})
	srv := newHTTP2TLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("request arrived over %s, want HTTP/2", r.Proto)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, ": keepalive\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(disconnected)
		case <-time.After(5 * time.Second):
		}
	}))

	a := NewAnthropicAdapter(staticBearer("k"), AuthModeAPIKey)
	a.baseURL = srv.URL
	a.streamIdleTimeout = idle
	trustServer(t, a.httpClient, srv)
	defer a.httpClient.CloseIdleConnections()

	ch, err := a.Stream(context.Background(), types.StreamParams{Model: "claude-sonnet-4-6", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	events := collectEvents(t, ch)
	if len(events) != 1 || !errors.Is(events[0].Error, errStreamIdle) {
		t.Fatalf("events = %+v, want exactly one idle error", events)
	}
	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Error("server never observed the client resetting the stalled HTTP/2 stream")
	}
}
