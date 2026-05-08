package credential

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGKEMetadataTokenSource_Success(t *testing.T) {
	fakeToken := "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJhdWQiOiJzdHMuYW1hem9uYXdzLmNvbSJ9.sig"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			t.Error("missing Metadata-Flavor: Google header")
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if !strings.Contains(r.URL.RawQuery, "audience=sts.amazonaws.com") {
			t.Errorf("unexpected query: %s", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fakeToken))
	}))
	defer srv.Close()

	ts := NewGKEMetadataTokenSource("sts.amazonaws.com", srv.URL)
	token, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(token) != fakeToken {
		t.Errorf("token = %q, want %q", string(token), fakeToken)
	}
}

func TestGKEMetadataTokenSource_TrimsWhitespace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("  token-value  \n"))
	}))
	defer srv.Close()

	ts := NewGKEMetadataTokenSource("test-audience", srv.URL)
	token, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(token) != "token-value" {
		t.Errorf("token = %q, want %q", string(token), "token-value")
	}
}

func TestGKEMetadataTokenSource_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	ts := NewGKEMetadataTokenSource("sts.amazonaws.com", srv.URL)
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention status code 404: %v", err)
	}
}

func TestGKEMetadataTokenSource_EmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ts := NewGKEMetadataTokenSource("sts.amazonaws.com", srv.URL)
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for empty token body")
	}
}

func TestGKEMetadataTokenSource_ServerDown(t *testing.T) {
	ts := NewGKEMetadataTokenSource("sts.amazonaws.com", "http://127.0.0.1:1")
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error when metadata server is unreachable")
	}
}

func TestFileTokenSource_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("file-token-value\n"), 0600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	ts := &FileTokenSource{path: path}
	token, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(token) != "file-token-value" {
		t.Errorf("token = %q, want %q", string(token), "file-token-value")
	}
}

func TestFileTokenSource_Missing(t *testing.T) {
	ts := &FileTokenSource{path: "/nonexistent/path/token"}
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestFileTokenSource_Empty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty")
	if err := os.WriteFile(path, []byte("  \n"), 0600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	ts := &FileTokenSource{path: path}
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for empty token file")
	}
}

func TestEnvTokenSource_Success(t *testing.T) {
	t.Setenv("TEST_OIDC_TOKEN", "env-token-value")
	ts := &EnvTokenSource{envVar: "TEST_OIDC_TOKEN"}
	token, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(token) != "env-token-value" {
		t.Errorf("token = %q, want %q", string(token), "env-token-value")
	}
}

func TestEnvTokenSource_Empty(t *testing.T) {
	t.Setenv("TEST_EMPTY_TOKEN", "")
	ts := &EnvTokenSource{envVar: "TEST_EMPTY_TOKEN"}
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for empty env var")
	}
}

func TestEnvTokenSource_Unset(t *testing.T) {
	ts := &EnvTokenSource{envVar: "DEFINITELY_NOT_SET_XYZ_123"}
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for unset env var")
	}
}

func TestAWSIRSATokenSource_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "irsa-token")
	if err := os.WriteFile(path, []byte("irsa-token-value\n"), 0600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", path)

	ts := &AWSIRSATokenSource{}
	token, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(token) != "irsa-token-value" {
		t.Errorf("token = %q, want %q", string(token), "irsa-token-value")
	}
}

func TestAWSIRSATokenSource_MissingEnv(t *testing.T) {
	// t.Setenv with empty string both sets and remembers to restore. The
	// runtime treats "" as unset for our purposes (os.Getenv returns "").
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")

	ts := &AWSIRSATokenSource{}
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error when AWS_WEB_IDENTITY_TOKEN_FILE is unset")
	}
	if !strings.Contains(err.Error(), "AWS_WEB_IDENTITY_TOKEN_FILE") {
		t.Errorf("error should name the missing env var: %v", err)
	}
}

func TestAzureIMDSTokenSource_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata") != "true" {
			t.Error("missing Metadata: true header")
			http.Error(w, "forbidden", http.StatusBadRequest)
			return
		}
		q := r.URL.Query()
		if got := q.Get("api-version"); got != "2018-02-01" {
			t.Errorf("api-version = %q, want %q", got, "2018-02-01")
		}
		if got := q.Get("resource"); got != "https://management.azure.com/" {
			t.Errorf("resource = %q, want %q", got, "https://management.azure.com/")
		}
		if r.URL.Path != "/metadata/identity/oauth2/token" {
			t.Errorf("path = %q, want %q", r.URL.Path, "/metadata/identity/oauth2/token")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"test-token-xyz","expires_on":"1735689600","resource":"https://management.azure.com/","token_type":"Bearer"}`))
	}))
	defer srv.Close()

	ts := NewAzureIMDSTokenSource("https://management.azure.com/", "", srv.URL)
	token, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(token) != "test-token-xyz" {
		t.Errorf("token = %q, want %q", string(token), "test-token-xyz")
	}
}

func TestAzureIMDSTokenSource_WithClientID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if got := q.Get("client_id"); got != "11111111-2222-3333-4444-555555555555" {
			t.Errorf("client_id = %q, want %q", got, "11111111-2222-3333-4444-555555555555")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"client-token","expires_on":"0","resource":"r"}`))
	}))
	defer srv.Close()

	ts := NewAzureIMDSTokenSource("https://management.azure.com/", "11111111-2222-3333-4444-555555555555", srv.URL)
	token, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(token) != "client-token" {
		t.Errorf("token = %q, want %q", string(token), "client-token")
	}
}

func TestAzureIMDSTokenSource_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	ts := NewAzureIMDSTokenSource("https://management.azure.com/", "", srv.URL)
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention status code 401: %v", err)
	}
}

func TestAzureIMDSTokenSource_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	ts := NewAzureIMDSTokenSource("https://management.azure.com/", "", srv.URL)
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error should mention parse failure: %v", err)
	}
}

func TestAzureIMDSTokenSource_EmptyAccessToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"","expires_on":"0"}`))
	}))
	defer srv.Close()

	ts := NewAzureIMDSTokenSource("https://management.azure.com/", "", srv.URL)
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for empty access_token")
	}
}

func TestGitHubActionsOIDCTokenSource_Success(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-runner-token" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer test-runner-token")
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		q := r.URL.Query()
		if got := q.Get("audience"); got != "test-aud" {
			t.Errorf("audience = %q, want %q", got, "test-aud")
		}
		if got := q.Get("api-version"); got != "2.0" {
			t.Errorf("api-version = %q, want %q", got, "2.0")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"value":"gha-oidc-token","count":1}`))
	}))
	defer srv.Close()

	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", srv.URL+"?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "test-runner-token")

	ts, err := NewGitHubActionsOIDCTokenSource("test-aud")
	if err != nil {
		t.Fatalf("NewGitHubActionsOIDCTokenSource: %v", err)
	}
	ts.httpClient = srv.Client()
	token, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(token) != "gha-oidc-token" {
		t.Errorf("token = %q, want %q", string(token), "gha-oidc-token")
	}
}

func TestGitHubActionsOIDCTokenSource_AudienceURLEscaped(t *testing.T) {
	// Audiences for cross-cloud federation often include slashes,
	// colons, or other reserved characters (e.g. Anthropic federation
	// audiences look like "https://anthropic.com/aud/...").
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// r.URL.Query() does the unescaping for us; if the source is
		// not escaping properly, the value here will be wrong or the
		// query parser will fail entirely.
		if got := r.URL.Query().Get("audience"); got != "https://anthropic.com/aud/x y" {
			t.Errorf("audience = %q, want %q", got, "https://anthropic.com/aud/x y")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"value":"escaped-token","count":1}`))
	}))
	defer srv.Close()

	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", srv.URL+"?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "test-runner-token")

	ts, err := NewGitHubActionsOIDCTokenSource("https://anthropic.com/aud/x y")
	if err != nil {
		t.Fatalf("NewGitHubActionsOIDCTokenSource: %v", err)
	}
	ts.httpClient = srv.Client()
	token, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(token) != "escaped-token" {
		t.Errorf("token = %q, want %q", string(token), "escaped-token")
	}
}

func TestGitHubActionsOIDCTokenSource_MissingURLEnv(t *testing.T) {
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "test-runner-token")

	_, err := NewGitHubActionsOIDCTokenSource("test-aud")
	if err == nil {
		t.Fatal("expected error when ACTIONS_ID_TOKEN_REQUEST_URL is unset")
	}
	if !strings.Contains(err.Error(), "ACTIONS_ID_TOKEN_REQUEST_URL") {
		t.Errorf("error should name the missing env var: %v", err)
	}
}

func TestGitHubActionsOIDCTokenSource_MissingTokenEnv(t *testing.T) {
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "https://example.invalid/?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")

	ts, err := NewGitHubActionsOIDCTokenSource("test-aud")
	if err != nil {
		t.Fatalf("NewGitHubActionsOIDCTokenSource: %v", err)
	}
	_, err = ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error when ACTIONS_ID_TOKEN_REQUEST_TOKEN is unset")
	}
	if !strings.Contains(err.Error(), "ACTIONS_ID_TOKEN_REQUEST_TOKEN") {
		t.Errorf("error should name the missing env var: %v", err)
	}
}

func TestGitHubActionsOIDCTokenSource_NonOK(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", srv.URL+"?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "test-runner-token")

	ts, err := NewGitHubActionsOIDCTokenSource("test-aud")
	if err != nil {
		t.Fatalf("NewGitHubActionsOIDCTokenSource: %v", err)
	}
	ts.httpClient = srv.Client()
	_, err = ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should mention status code 403: %v", err)
	}
}

func TestGitHubActionsOIDCTokenSource_MalformedJSON(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", srv.URL+"?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "test-runner-token")

	ts, err := NewGitHubActionsOIDCTokenSource("test-aud")
	if err != nil {
		t.Fatalf("NewGitHubActionsOIDCTokenSource: %v", err)
	}
	ts.httpClient = srv.Client()
	_, err = ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error should mention parse failure: %v", err)
	}
}

func TestGitHubActionsOIDCTokenSource_EmptyValue(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"value":"","count":0}`))
	}))
	defer srv.Close()

	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", srv.URL+"?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "test-runner-token")

	ts, err := NewGitHubActionsOIDCTokenSource("test-aud")
	if err != nil {
		t.Fatalf("NewGitHubActionsOIDCTokenSource: %v", err)
	}
	ts.httpClient = srv.Client()
	_, err = ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for empty value")
	}
}

// TestGitHubActionsOIDCTokenSource_RejectsHTTPURL guards the HTTPS
// invariant on the runner OIDC issuance URL. A self-hosted runner
// where an attacker can persuade the harness to read
// ACTIONS_ID_TOKEN_REQUEST_URL=http://attacker.lan/... captures the
// runner bearer (sent as Authorization: Bearer) and exchanges it for
// a valid OIDC JWT with the harness's audience claim. The constructor
// must refuse non-https schemes and never reach the network.
func TestGitHubActionsOIDCTokenSource_RejectsHTTPURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("HTTP server should never have been hit; constructor must reject http scheme")
	}))
	defer srv.Close()

	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", srv.URL+"?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "test-runner-token")

	_, err := NewGitHubActionsOIDCTokenSource("test-aud")
	if err == nil {
		t.Fatal("expected error for http (non-https) issuance URL")
	}
	if !strings.Contains(err.Error(), "must be an https URL") {
		t.Errorf("error should mention https requirement, got: %v", err)
	}
}

// TestGitHubActionsOIDCTokenSource_RejectsMalformedURL exercises the
// url.Parse error branch of the constructor.
func TestGitHubActionsOIDCTokenSource_RejectsMalformedURL(t *testing.T) {
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "://not a url")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "test-runner-token")

	_, err := NewGitHubActionsOIDCTokenSource("test-aud")
	if err == nil {
		t.Fatal("expected error for malformed issuance URL")
	}
	if !strings.Contains(err.Error(), "must be an https URL") {
		t.Errorf("error should mention https requirement, got: %v", err)
	}
}

// TestGitHubActionsOIDCTokenSource_URLFixedAtConstruction asserts the
// URL is captured at construction and not re-read on every Token()
// call. This closes the env-mutation window an in-process malicious
// sidecar could otherwise exploit between calls.
func TestGitHubActionsOIDCTokenSource_URLFixedAtConstruction(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"value":"original-token","count":1}`))
	}))
	defer srv.Close()

	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", srv.URL+"?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "test-runner-token")

	ts, err := NewGitHubActionsOIDCTokenSource("test-aud")
	if err != nil {
		t.Fatalf("NewGitHubActionsOIDCTokenSource: %v", err)
	}
	ts.httpClient = srv.Client()

	// Mutate the env var after construction. If the constructor leaked
	// the URL read into Token(), this would either error (unset) or
	// redirect the request to attacker.lan.
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "http://attacker.lan/")

	tok, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error after env mutation: %v", err)
	}
	if string(tok) != "original-token" {
		t.Errorf("token = %q, want original-token", string(tok))
	}
}

// TestGKEMetadataTokenSource_OversizedBody verifies the io.LimitReader
// cap on the GKE metadata success path. Without it, a hostile or
// misconfigured metadata endpoint can exhaust memory by streaming an
// unbounded payload. The cap silently truncates the excess.
func TestGKEMetadataTokenSource_OversizedBody(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Token() error: %v", err)
	}
	if len(tok) != limit {
		t.Errorf("token length = %d, want %d (LimitReader cap)", len(tok), limit)
	}
}
