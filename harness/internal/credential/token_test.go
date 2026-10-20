package credential

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGKEMetadataTokenSource_ReturnsTokenFromServer(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	fakeToken := "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJhdWQiOiJzdHMuYW1hem9uYXdzLmNvbSJ9.sig"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !check.Equal("Google", r.Header.Get("Metadata-Flavor"), "missing Metadata-Flavor: Google header") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		check.Contains(r.URL.RawQuery, "audience=sts.amazonaws.com")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fakeToken))
	}))
	defer srv.Close()

	ts := NewGKEMetadataTokenSource("sts.amazonaws.com", srv.URL)
	token, err := ts.Token(context.Background())
	must.NoError(err)
	must.Equal(fakeToken, string(token))
}

func TestGKEMetadataTokenSource_TrimsWhitespace(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("  token-value  \n"))
	}))
	defer srv.Close()

	ts := NewGKEMetadataTokenSource("test-audience", srv.URL)
	token, err := ts.Token(context.Background())
	must.NoError(err)
	must.Equal("token-value", string(token))
}

func TestGKEMetadataTokenSource_FailsOnNonOKStatus(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	ts := NewGKEMetadataTokenSource("sts.amazonaws.com", srv.URL)
	_, err := ts.Token(context.Background())
	must.ErrorContains(err, "404")
}

func TestGKEMetadataTokenSource_RejectsEmptyBody(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ts := NewGKEMetadataTokenSource("sts.amazonaws.com", srv.URL)
	_, err := ts.Token(context.Background())
	must.Error(err)
}

func TestGKEMetadataTokenSource_FailsWhenServerUnreachable(t *testing.T) {
	must := require.New(t)
	ts := NewGKEMetadataTokenSource("sts.amazonaws.com", "http://127.0.0.1:1")
	_, err := ts.Token(context.Background())
	must.Error(err)
}

func TestFileTokenSource_ReturnsTrimmedContents(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	must.NoError(os.WriteFile(path, []byte("file-token-value\n"), 0600), "write temp file")

	ts := &FileTokenSource{path: path}
	token, err := ts.Token(context.Background())
	must.NoError(err)
	must.Equal("file-token-value", string(token))
}

func TestFileTokenSource_FailsWhenFileMissing(t *testing.T) {
	must := require.New(t)
	ts := &FileTokenSource{path: "/nonexistent/path/token"}
	_, err := ts.Token(context.Background())
	must.Error(err)
}

func TestFileTokenSource_RejectsBlankFile(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "empty")
	must.NoError(os.WriteFile(path, []byte("  \n"), 0600), "write temp file")

	ts := &FileTokenSource{path: path}
	_, err := ts.Token(context.Background())
	must.Error(err)
}

func TestEnvTokenSource_ReturnsVariableValue(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_OIDC_TOKEN", "env-token-value")
	ts := &EnvTokenSource{envVar: "TEST_OIDC_TOKEN"}
	token, err := ts.Token(context.Background())
	must.NoError(err)
	must.Equal("env-token-value", string(token))
}

func TestEnvTokenSource_RejectsEmptyVariable(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_EMPTY_TOKEN", "")
	ts := &EnvTokenSource{envVar: "TEST_EMPTY_TOKEN"}
	_, err := ts.Token(context.Background())
	must.Error(err)
}

func TestEnvTokenSource_RejectsUnsetVariable(t *testing.T) {
	must := require.New(t)
	ts := &EnvTokenSource{envVar: "DEFINITELY_NOT_SET_XYZ_123"}
	_, err := ts.Token(context.Background())
	must.Error(err)
}

func TestAWSIRSATokenSource_ReturnsTokenFromFile(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "irsa-token")
	must.NoError(os.WriteFile(path, []byte("irsa-token-value\n"), 0600), "write temp file")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", path)

	ts := &AWSIRSATokenSource{}
	token, err := ts.Token(context.Background())
	must.NoError(err)
	must.Equal("irsa-token-value", string(token))
}

func TestAWSIRSATokenSource_FailsWhenTokenFileEnvUnset(t *testing.T) {
	must := require.New(t)
	// t.Setenv with empty string both sets and remembers to restore. The
	// runtime treats "" as unset for our purposes (os.Getenv returns "").
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")

	ts := &AWSIRSATokenSource{}
	_, err := ts.Token(context.Background())
	must.ErrorContains(err, "AWS_WEB_IDENTITY_TOKEN_FILE")
}

func TestAzureIMDSTokenSource_SendsRequiredParamsAndReturnsToken(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !check.Equal("true", r.Header.Get("Metadata"), "missing Metadata: true header") {
			http.Error(w, "forbidden", http.StatusBadRequest)
			return
		}
		q := r.URL.Query()
		check.Equal("2018-02-01", q.Get("api-version"), "api-version query parameter")
		check.Equal("https://management.azure.com/", q.Get("resource"), "resource query parameter")
		check.Equal("/metadata/identity/oauth2/token", r.URL.Path, "request path")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"test-token-xyz","expires_on":"1735689600","resource":"https://management.azure.com/","token_type":"Bearer"}`))
	}))
	defer srv.Close()

	ts := NewAzureIMDSTokenSource("https://management.azure.com/", "", srv.URL)
	token, err := ts.Token(context.Background())
	must.NoError(err)
	must.Equal("test-token-xyz", string(token))
}

func TestAzureIMDSTokenSource_SendsClientID(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check.Equal("11111111-2222-3333-4444-555555555555", r.URL.Query().Get("client_id"), "client_id query parameter")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"client-token","expires_on":"0","resource":"r"}`))
	}))
	defer srv.Close()

	ts := NewAzureIMDSTokenSource("https://management.azure.com/", "11111111-2222-3333-4444-555555555555", srv.URL)
	token, err := ts.Token(context.Background())
	must.NoError(err)
	must.Equal("client-token", string(token))
}

func TestAzureIMDSTokenSource_FailsOnNonOKStatus(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	ts := NewAzureIMDSTokenSource("https://management.azure.com/", "", srv.URL)
	_, err := ts.Token(context.Background())
	must.ErrorContains(err, "401")
}

func TestAzureIMDSTokenSource_RejectsMalformedJSON(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	ts := NewAzureIMDSTokenSource("https://management.azure.com/", "", srv.URL)
	_, err := ts.Token(context.Background())
	must.ErrorContains(err, "parse")
}

func TestAzureIMDSTokenSource_RejectsEmptyAccessToken(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"","expires_on":"0"}`))
	}))
	defer srv.Close()

	ts := NewAzureIMDSTokenSource("https://management.azure.com/", "", srv.URL)
	_, err := ts.Token(context.Background())
	must.Error(err)
}

func TestGitHubActionsOIDCTokenSource_ReturnsTokenFromIssuer(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !check.Equal("Bearer test-runner-token", r.Header.Get("Authorization"), "Authorization header") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		q := r.URL.Query()
		check.Equal("test-aud", q.Get("audience"), "audience query parameter")
		check.Equal("2.0", q.Get("api-version"), "api-version query parameter")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"value":"gha-oidc-token","count":1}`))
	}))
	defer srv.Close()

	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", srv.URL+"?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "test-runner-token")

	ts, err := NewGitHubActionsOIDCTokenSource("test-aud")
	must.NoError(err)
	ts.httpClient = srv.Client()
	token, err := ts.Token(context.Background())
	must.NoError(err)
	must.Equal("gha-oidc-token", string(token))
}

func TestGitHubActionsOIDCTokenSource_EscapesAudienceInQuery(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	// Audiences for cross-cloud federation often include slashes,
	// colons, or other reserved characters (e.g. Anthropic federation
	// audiences look like "https://anthropic.com/aud/...").
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// r.URL.Query() does the unescaping for us; if the source is
		// not escaping properly, the value here will be wrong or the
		// query parser will fail entirely.
		check.Equal("https://anthropic.com/aud/x y", r.URL.Query().Get("audience"), "audience query parameter")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"value":"escaped-token","count":1}`))
	}))
	defer srv.Close()

	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", srv.URL+"?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "test-runner-token")

	ts, err := NewGitHubActionsOIDCTokenSource("https://anthropic.com/aud/x y")
	must.NoError(err)
	ts.httpClient = srv.Client()
	token, err := ts.Token(context.Background())
	must.NoError(err)
	must.Equal("escaped-token", string(token))
}

func TestGitHubActionsOIDCTokenSource_RejectsUnsetRequestURL(t *testing.T) {
	must := require.New(t)
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "test-runner-token")

	_, err := NewGitHubActionsOIDCTokenSource("test-aud")
	must.ErrorContains(err, "ACTIONS_ID_TOKEN_REQUEST_URL")
}

func TestGitHubActionsOIDCTokenSource_RejectsUnsetRequestToken(t *testing.T) {
	must := require.New(t)
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "https://example.invalid/?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")

	ts, err := NewGitHubActionsOIDCTokenSource("test-aud")
	must.NoError(err)
	_, err = ts.Token(context.Background())
	must.ErrorContains(err, "ACTIONS_ID_TOKEN_REQUEST_TOKEN")
}

func TestGitHubActionsOIDCTokenSource_FailsOnNonOKStatus(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", srv.URL+"?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "test-runner-token")

	ts, err := NewGitHubActionsOIDCTokenSource("test-aud")
	must.NoError(err)
	ts.httpClient = srv.Client()
	_, err = ts.Token(context.Background())
	must.ErrorContains(err, "403")
}

func TestGitHubActionsOIDCTokenSource_RejectsMalformedJSON(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", srv.URL+"?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "test-runner-token")

	ts, err := NewGitHubActionsOIDCTokenSource("test-aud")
	must.NoError(err)
	ts.httpClient = srv.Client()
	_, err = ts.Token(context.Background())
	must.ErrorContains(err, "parse")
}

func TestGitHubActionsOIDCTokenSource_RejectsEmptyValue(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"value":"","count":0}`))
	}))
	defer srv.Close()

	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", srv.URL+"?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "test-runner-token")

	ts, err := NewGitHubActionsOIDCTokenSource("test-aud")
	must.NoError(err)
	ts.httpClient = srv.Client()
	_, err = ts.Token(context.Background())
	must.Error(err)
}

// TestGitHubActionsOIDCTokenSource_RejectsHTTPURL guards the HTTPS
// invariant on the runner OIDC issuance URL: a self-hosted runner
// where ACTIONS_ID_TOKEN_REQUEST_URL points at http://attacker.lan
// would leak the runner bearer. The constructor must refuse non-https
// schemes and never reach the network.
func TestGitHubActionsOIDCTokenSource_RejectsHTTPURL(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		check.Fail("HTTP server should never have been hit; constructor must reject http scheme")
	}))
	defer srv.Close()

	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", srv.URL+"?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "test-runner-token")

	_, err := NewGitHubActionsOIDCTokenSource("test-aud")
	must.ErrorContains(err, "must be an https URL")
}

// TestGitHubActionsOIDCTokenSource_RejectsMalformedURL exercises the
// url.Parse error branch of the constructor.
func TestGitHubActionsOIDCTokenSource_RejectsMalformedURL(t *testing.T) {
	must := require.New(t)
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "://not a url")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "test-runner-token")

	_, err := NewGitHubActionsOIDCTokenSource("test-aud")
	must.ErrorContains(err, "must be an https URL")
}

// TestGitHubActionsOIDCTokenSource_URLFixedAtConstruction verifies the
// URL is captured at construction, not re-read on every Token() call,
// closing an env-mutation window a malicious sidecar could exploit.
func TestGitHubActionsOIDCTokenSource_URLFixedAtConstruction(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"value":"original-token","count":1}`))
	}))
	defer srv.Close()

	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", srv.URL+"?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "test-runner-token")

	ts, err := NewGitHubActionsOIDCTokenSource("test-aud")
	must.NoError(err)
	ts.httpClient = srv.Client()

	// Mutate the env var after construction. If the constructor leaked
	// the URL read into Token(), this would either error (unset) or
	// redirect the request to attacker.lan.
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "http://attacker.lan/")

	tok, err := ts.Token(context.Background())
	must.NoError(err)
	must.Equal("original-token", string(tok))
}

// TestGKEMetadataTokenSource_OversizedBody verifies the io.LimitReader
// cap on the GKE metadata success path. Without it, a hostile or
// misconfigured metadata endpoint can exhaust memory by streaming an
// unbounded payload. The cap silently truncates the excess.
func TestGKEMetadataTokenSource_OversizedBody(t *testing.T) {
	must := require.New(t)
	const limit = metadataResponseLimit
	payload := make([]byte, limit+1)
	for i := range payload {
		payload[i] = 'x'
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	ts := NewGKEMetadataTokenSource("sts.amazonaws.com", srv.URL)
	tok, err := ts.Token(context.Background())
	must.NoError(err)
	must.Len(tok, limit, "body should be truncated at the metadata response limit")
}
