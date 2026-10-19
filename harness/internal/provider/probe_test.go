package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// probeRecorder counts requests to a metadata path vs a completion path so
// the no-completion-endpoint invariant can be asserted.
type probeRecorder struct {
	metadataHits   atomic.Int64
	completionHits atomic.Int64
}

func TestAnthropicAdapter_Probe_UsesMetadataEndpointOnly(t *testing.T) {
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

	require.NoError(t, adapter.Probe(context.Background()))
	assert.EqualValues(t, 1, rec.metadataHits.Load(), "metadata endpoint hits")
	assert.EqualValues(t, 0, rec.completionHits.Load(),
		"completion endpoint hits (dry-run must not spend tokens)")
}

func TestAnthropicAdapter_Probe_RejectedKeyReturnsDiagnostic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid x-api-key"}}`))
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("bad"), AuthModeAPIKey)
	adapter.baseURL = srv.URL + "/v1/messages"

	err := adapter.Probe(context.Background())
	require.Error(t, err)
	require.ErrorContains(t, err, "401")
	require.ErrorContains(t, err, "invalid x-api-key")
}

func TestOpenAICompatibleAdapter_Probe_UsesMetadataEndpointOnly(t *testing.T) {
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
	require.NoError(t, adapter.Probe(context.Background()))
	assert.EqualValues(t, 1, rec.metadataHits.Load(), "metadata endpoint hits")
	assert.EqualValues(t, 0, rec.completionHits.Load(), "completion endpoint hits")
}

func TestOpenAIResponsesAdapter_Probe_UsesMetadataEndpointOnly(t *testing.T) {
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
	require.NoError(t, adapter.Probe(context.Background()))
	assert.EqualValues(t, 1, rec.metadataHits.Load(), "metadata endpoint hits")
	assert.EqualValues(t, 0, rec.completionHits.Load(), "completion endpoint hits")
}

func TestGeminiAdapter_Probe_UsesMetadataEndpointOnly(t *testing.T) {
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
	require.NoError(t, adapter.Probe(context.Background()))
	assert.EqualValues(t, 1, rec.metadataHits.Load(), "metadata endpoint hits")
	assert.EqualValues(t, 0, rec.completionHits.Load(), "completion endpoint hits")
}

func TestAnthropicAdapter_Probe_PropagatesCredentialError(t *testing.T) {
	adapter := NewAnthropicAdapter(erroringBearer("no creds"), AuthModeAPIKey)
	require.Error(t, adapter.Probe(context.Background()))
}

func TestBedrockAdapter_Probe_NilCredentialsIsNoOp(t *testing.T) {
	// The mock-client construction path leaves credentials nil; Probe must
	// then be a no-op rather than panicking or failing.
	adapter := &BedrockAdapter{}
	require.NoError(t, adapter.Probe(context.Background()))
}

func TestBedrockAdapter_Probe_CredentialErrorNamesCauseAndRegion(t *testing.T) {
	adapter := &BedrockAdapter{
		region: "us-east-1",
		credentials: aws.CredentialsProviderFunc(func(_ context.Context) (aws.Credentials, error) {
			return aws.Credentials{}, errors.New("STS AssumeRole denied")
		}),
	}
	err := adapter.Probe(context.Background())
	require.Error(t, err)
	require.ErrorContains(t, err, "STS AssumeRole denied", "error should wrap the credential failure")
	assert.ErrorContains(t, err, "us-east-1", "error should name the region for diagnosis")
}

func TestProbe_Unauthorized_ErrorNamesStatusAndProvider(t *testing.T) {
	// 401/error responses from the metadata endpoint must surface as an
	// error for every dialect, carrying the status and the provider's
	// diagnostic.
	newServer := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
		}))
	}

	t.Run("openai-compatible", func(t *testing.T) {
		srv := newServer()
		defer srv.Close()
		a := NewOpenAICompatibleAdapter(staticBearer("bad"), srv.URL+"/v1", OpenAIAuthConfig{}, RetryPolicy{})
		requireProbeStatusError(t, a.Probe(context.Background()), "openai-compatible")
	})

	t.Run("openai-responses", func(t *testing.T) {
		srv := newServer()
		defer srv.Close()
		a := NewOpenAIResponsesAdapter(staticBearer("bad"), srv.URL+"/v1", OpenAIAuthConfig{})
		requireProbeStatusError(t, a.Probe(context.Background()), "openai-responses")
	})

	t.Run("gemini", func(t *testing.T) {
		srv := newServer()
		defer srv.Close()
		a := NewGeminiAdapter(staticBearer("ya29.bad"), "proj", "us-central1", nil)
		a.baseURLOverride = srv.URL
		requireProbeStatusError(t, a.Probe(context.Background()), "gemini")
	})
}

// requireProbeStatusError fails the calling test unless err carries the 401
// status and names providerLabel.
func requireProbeStatusError(t *testing.T, err error, providerLabel string) {
	t.Helper()
	require.Error(t, err, "expected error for 401")
	require.ErrorContains(t, err, "401", "error should carry the 401 status")
	require.ErrorContains(t, err, providerLabel, "error should name the provider")
}
