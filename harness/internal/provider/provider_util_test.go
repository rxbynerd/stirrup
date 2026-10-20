package provider

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
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
	must := require.New(t)
	tr, ok := client.Transport.(*http.Transport)
	must.True(ok, "transport is %T, want *http.Transport", client.Transport)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	tr.TLSClientConfig = &tls.Config{RootCAs: pool}
}

// getProto performs a GET against url and returns the negotiated HTTP major version.
func getProto(t *testing.T, client *http.Client, url string) int {
	t.Helper()
	must := require.New(t)
	resp, err := client.Get(url)
	must.NoError(err, "GET %s", url)
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
			must := require.New(t)
			trustServer(t, client, srv)
			got := getProto(t, client, srv.URL)
			must.Equal(2, got, "negotiated HTTP major version")
		})
	}
}

func TestStreamingHTTPClient_CustomDialer_NoForceAttemptFallsBackToHTTP1(t *testing.T) {
	must := require.New(t)
	srv := newHTTP2TLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	client := newStreamingHTTPClient()
	client.Transport.(*http.Transport).ForceAttemptHTTP2 = false
	trustServer(t, client, srv)
	got := getProto(t, client, srv.URL)
	must.Equal(1, got, "negotiated HTTP major version")
}
