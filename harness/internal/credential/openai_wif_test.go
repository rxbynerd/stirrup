package credential

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Identifier values used across the OpenAI WIF happy-path tests. The exact
// values are arbitrary; they only need to round-trip through the request
// body unchanged. OpenAI does not document a stable prefix, so these are
// deliberately opaque.
const (
	testOpenAIIdentityProviderID = "idp_test123"
	testOpenAIServiceAccountID   = "sa_test456"
)

// newOpenAIWIFSourceForTest builds a source pointed at a test server.
// Mirrors NewOpenAIWIFSource but patches tokenURL so we never hit the real
// auth.openai.com endpoint.
func newOpenAIWIFSourceForTest(t *testing.T, ts TokenSource, idpID, saID, subjectTokenType, tokenURL string) *OpenAIWIFSource {
	t.Helper()
	src := NewOpenAIWIFSource(ts, idpID, saID, subjectTokenType)
	src.tokenURL = tokenURL
	return src
}

// openAIOAuthHandler returns an httptest handler that asserts the documented
// request shape and responds with a synthesised access token. The verify
// callback runs against the parsed request before the response is built.
func openAIOAuthHandler(t *testing.T, accessToken string, expiresIn int64, verify func(req openAIOAuthRequest, raw []byte)) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("OpenAI OAuth got %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		var parsed openAIOAuthRequest
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("unmarshal request body: %v", err)
		}
		if parsed.GrantType != openAITokenExchangeGrantType {
			t.Errorf("grant_type = %q, want %q", parsed.GrantType, openAITokenExchangeGrantType)
		}
		if verify != nil {
			verify(parsed, raw)
		}
		resp := oauthExchangeResponse{
			AccessToken: accessToken,
			TokenType:   "Bearer",
			ExpiresIn:   expiresIn,
			Scope:       "api.model.read api.model.request",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func TestOpenAIWIFSource_HappyPath(t *testing.T) {
	const oidcJWT = "eyJ.fake-jwt.signature"
	const accessToken = "openai-federated-access-token"

	srv := httptest.NewServer(openAIOAuthHandler(t, accessToken, 3600, func(req openAIOAuthRequest, _ []byte) {
		if req.SubjectToken != oidcJWT {
			t.Errorf("subject_token = %q, want %q", req.SubjectToken, oidcJWT)
		}
		if req.SubjectTokenType != openAIDefaultSubjectTokenType {
			t.Errorf("subject_token_type = %q, want default %q", req.SubjectTokenType, openAIDefaultSubjectTokenType)
		}
		if req.IdentityProviderID != testOpenAIIdentityProviderID {
			t.Errorf("identity_provider_id = %q, want %q", req.IdentityProviderID, testOpenAIIdentityProviderID)
		}
		if req.ServiceAccountID != testOpenAIServiceAccountID {
			t.Errorf("service_account_id = %q, want %q", req.ServiceAccountID, testOpenAIServiceAccountID)
		}
	}))
	defer srv.Close()

	src := newOpenAIWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte(oidcJWT)},
		testOpenAIIdentityProviderID, testOpenAIServiceAccountID, "",
		srv.URL,
	)

	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.BearerToken == nil {
		t.Fatal("expected BearerToken closure")
	}

	got, err := cred.BearerToken(context.Background())
	if err != nil {
		t.Fatalf("BearerToken: %v", err)
	}
	if got != accessToken {
		t.Errorf("bearer = %q, want %q", got, accessToken)
	}
}

// TestOpenAIWIFSource_NoAudienceOrOrgInBody asserts the exchange body carries
// only the five documented fields. OpenAI binds the audience via the subject
// token's aud claim and org/project via the service-account mapping, so
// sending audience / organization_id / workspace_id would be undocumented
// and likely rejected. Inspect the raw bytes since unmarshalling would erase
// the distinction.
func TestOpenAIWIFSource_NoAudienceOrOrgInBody(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(openAIOAuthHandler(t, "tok", 3600, func(_ openAIOAuthRequest, raw []byte) {
		capturedBody = append(capturedBody[:0], raw...)
	}))
	defer srv.Close()

	src := newOpenAIWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testOpenAIIdentityProviderID, testOpenAIServiceAccountID, "",
		srv.URL,
	)
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := cred.BearerToken(context.Background()); err != nil {
		t.Fatalf("BearerToken: %v", err)
	}
	for _, forbidden := range []string{"audience", "organization_id", "project", "workspace_id", "resource"} {
		if strings.Contains(string(capturedBody), forbidden) {
			t.Errorf("request body must not contain %q, got body: %s", forbidden, capturedBody)
		}
	}
}

// TestOpenAIWIFSource_SubjectTokenTypeOverride verifies a non-default
// subject_token_type rides through to the wire unchanged.
func TestOpenAIWIFSource_SubjectTokenTypeOverride(t *testing.T) {
	const custom = "urn:ietf:params:oauth:token-type:id_token"
	srv := httptest.NewServer(openAIOAuthHandler(t, "tok", 3600, func(req openAIOAuthRequest, _ []byte) {
		if req.SubjectTokenType != custom {
			t.Errorf("subject_token_type = %q, want %q", req.SubjectTokenType, custom)
		}
	}))
	defer srv.Close()

	src := newOpenAIWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testOpenAIIdentityProviderID, testOpenAIServiceAccountID, custom,
		srv.URL,
	)
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := cred.BearerToken(context.Background()); err != nil {
		t.Fatalf("BearerToken: %v", err)
	}
}

// TestOpenAIWIFSource_DefaultSubjectTokenTypeApplied pins that the
// constructor fills in the JWT default when subjectTokenType is empty.
func TestOpenAIWIFSource_DefaultSubjectTokenTypeApplied(t *testing.T) {
	src := NewOpenAIWIFSource(&stubTokenSource{token: []byte("x")}, testOpenAIIdentityProviderID, testOpenAIServiceAccountID, "")
	if src.subjectTokenType != openAIDefaultSubjectTokenType {
		t.Errorf("subjectTokenType = %q, want default %q", src.subjectTokenType, openAIDefaultSubjectTokenType)
	}
}

// TestOpenAIWIFSource_ExpiresInHonoured verifies the parsed expires_in feeds
// into oauth2.Token.Expiry so ReuseTokenSource caches correctly.
func TestOpenAIWIFSource_ExpiresInHonoured(t *testing.T) {
	const customExpiresIn int64 = 600
	srv := httptest.NewServer(openAIOAuthHandler(t, "tok", customExpiresIn, nil))
	defer srv.Close()

	src := newOpenAIWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testOpenAIIdentityProviderID, testOpenAIServiceAccountID, "",
		srv.URL,
	)

	inner := &openAIWIFTokenSource{src: src}
	before := time.Now()
	tok, err := inner.Token()
	after := time.Now()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	expectedFloor := before.Add(time.Duration(customExpiresIn) * time.Second)
	expectedCeil := after.Add(time.Duration(customExpiresIn) * time.Second)
	if tok.Expiry.Before(expectedFloor) || tok.Expiry.After(expectedCeil) {
		t.Errorf("expiry = %v, want in [%v, %v]", tok.Expiry, expectedFloor, expectedCeil)
	}
}

// TestOpenAIWIFSource_MissingExpiresInFallback verifies the 1-hour fallback
// when the server omits expires_in.
func TestOpenAIWIFSource_MissingExpiresInFallback(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer"}`))
	}))
	defer srv.Close()

	src := newOpenAIWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testOpenAIIdentityProviderID, testOpenAIServiceAccountID, "",
		srv.URL,
	)
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	tok1, err := cred.BearerToken(context.Background())
	if err != nil {
		t.Fatalf("BearerToken first: %v", err)
	}
	tok2, err := cred.BearerToken(context.Background())
	if err != nil {
		t.Fatalf("BearerToken second: %v", err)
	}
	if tok1 != "tok" || tok2 != "tok" {
		t.Errorf("bearer = (%q, %q), want both \"tok\"", tok1, tok2)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("exchange hit %d times, want 1 (1-hour fallback should let cache absorb second call)", got)
	}
}

// TestOpenAIWIFSource_HTTPErrorStatusSurfacesBody covers the status codes
// OpenAI uses for federation failures and asserts the body is included.
func TestOpenAIWIFSource_HTTPErrorStatusSurfacesBody(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"bad request", http.StatusBadRequest, `{"error":"invalid_request","error_description":"bad subject token"}`},
		{"unauthorized", http.StatusUnauthorized, `{"error":"invalid_grant"}`},
		{"forbidden", http.StatusForbidden, `{"error":"access_denied"}`},
		{"server error", http.StatusInternalServerError, "oops"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			src := newOpenAIWIFSourceForTest(
				t,
				&stubTokenSource{token: []byte("oidc")},
				testOpenAIIdentityProviderID, testOpenAIServiceAccountID, "",
				srv.URL,
			)
			cred, err := src.Resolve(context.Background())
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			_, err = cred.BearerToken(context.Background())
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			msg := err.Error()
			if !strings.Contains(msg, "OpenAI WIF") {
				t.Errorf("error should be prefixed with \"OpenAI WIF\", got: %v", err)
			}
			if !strings.Contains(msg, "returned ") {
				t.Errorf("error should name the status code, got: %v", err)
			}
			if tc.body != "" && !strings.Contains(msg, strings.TrimSpace(tc.body)[:min(20, len(strings.TrimSpace(tc.body)))]) {
				t.Errorf("error should include body excerpt, got: %v", err)
			}
		})
	}
}

// TestOpenAIWIFSource_HTTPErrorBodyTruncated verifies the response body cap.
func TestOpenAIWIFSource_HTTPErrorBodyTruncated(t *testing.T) {
	huge := strings.Repeat("A", stsErrorBodyLimit*4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(huge))
	}))
	defer srv.Close()

	src := newOpenAIWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testOpenAIIdentityProviderID, testOpenAIServiceAccountID, "",
		srv.URL,
	)
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, err = cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected error for huge body")
	}
	msg := err.Error()
	if len(msg) > 2*stsErrorBodyLimit {
		t.Errorf("error message length %d should be capped near %d", len(msg), stsErrorBodyLimit)
	}
	if !strings.Contains(msg, "…") {
		t.Errorf("error should end with truncation ellipsis when body exceeds cap, got: %q", msg[len(msg)-min(40, len(msg)):])
	}
}

// TestOpenAIWIFSource_HTTPErrorIncludesRequestID verifies the x-request-id
// response header (read opportunistically) rides through to the error so
// operators can correlate a failed exchange.
func TestOpenAIWIFSource_HTTPErrorIncludesRequestID(t *testing.T) {
	const reqID = "req_openai_abc123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-request-id", reqID)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
	}))
	defer srv.Close()

	src := newOpenAIWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testOpenAIIdentityProviderID, testOpenAIServiceAccountID, "",
		srv.URL,
	)
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, err = cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), reqID) {
		t.Errorf("error should include request_id %q, got: %v", reqID, err)
	}
}

// TestOpenAIWIFSource_MalformedJSONResponse covers the json.Unmarshal branch.
func TestOpenAIWIFSource_MalformedJSONResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	src := newOpenAIWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testOpenAIIdentityProviderID, testOpenAIServiceAccountID, "",
		srv.URL,
	)
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, err = cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "parse token response") {
		t.Errorf("error should mention parse token response, got: %v", err)
	}
}

// TestOpenAIWIFSource_EmptyAccessToken covers the empty-string guard.
func TestOpenAIWIFSource_EmptyAccessToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"","token_type":"Bearer","expires_in":3600}`))
	}))
	defer srv.Close()

	src := newOpenAIWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testOpenAIIdentityProviderID, testOpenAIServiceAccountID, "",
		srv.URL,
	)
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, err = cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected error for empty access_token")
	}
	if !strings.Contains(err.Error(), "empty access_token") {
		t.Errorf("error should mention empty access_token, got: %v", err)
	}
}

// TestOpenAIWIFSource_JWTReadFreshOnEveryExchange verifies the underlying
// TokenSource is consulted on each exchange — projected k8s tokens and GHA
// OIDC tokens rotate ahead of expiry, and the OpenAI access token never
// outlives the subject token.
func TestOpenAIWIFSource_JWTReadFreshOnEveryExchange(t *testing.T) {
	srv := httptest.NewServer(openAIOAuthHandler(t, "tok", 1, nil))
	defer srv.Close()

	stub := &stubTokenSource{token: []byte("jwt")}
	src := newOpenAIWIFSourceForTest(
		t,
		stub,
		testOpenAIIdentityProviderID, testOpenAIServiceAccountID, "",
		srv.URL,
	)
	inner := &openAIWIFTokenSource{src: src}

	if _, err := inner.Token(); err != nil {
		t.Fatalf("Token first: %v", err)
	}
	if _, err := inner.Token(); err != nil {
		t.Fatalf("Token second: %v", err)
	}
	if got := atomic.LoadInt32(&stub.calls); got != 2 {
		t.Errorf("token source consulted %d times, want 2 (each exchange must re-read the JWT)", got)
	}
}

// TestOpenAIWIFSource_ReuseTokenSourceCachesExchange pins the caching
// contract: while the cached access token is fresh, repeated BearerToken
// calls must not round-trip to the exchange endpoint.
func TestOpenAIWIFSource_ReuseTokenSourceCachesExchange(t *testing.T) {
	var exchanges int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&exchanges, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(oauthExchangeResponse{
			AccessToken: "cached",
			TokenType:   "Bearer",
			ExpiresIn:   3600,
		})
	}))
	defer srv.Close()

	src := newOpenAIWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testOpenAIIdentityProviderID, testOpenAIServiceAccountID, "",
		srv.URL,
	)
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	for i := 0; i < 5; i++ {
		got, err := cred.BearerToken(context.Background())
		if err != nil {
			t.Fatalf("BearerToken %d: %v", i, err)
		}
		if got != "cached" {
			t.Errorf("call %d: bearer = %q", i, got)
		}
	}
	if c := atomic.LoadInt32(&exchanges); c != 1 {
		t.Errorf("after 5 BearerToken calls, exchange hit count = %d, want 1 (ReuseTokenSource should cache)", c)
	}
}

func TestOpenAIWIFSource_EmptyJWTBytes(t *testing.T) {
	src := newOpenAIWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte{}},
		testOpenAIIdentityProviderID, testOpenAIServiceAccountID, "",
		"http://unused.invalid",
	)
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, err = cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected error for empty JWT")
	}
	if !strings.Contains(err.Error(), "OpenAI WIF:") {
		t.Errorf("error should be prefixed with \"OpenAI WIF:\", got: %v", err)
	}
	if !strings.Contains(err.Error(), "empty token") {
		t.Errorf("error should mention empty token, got: %v", err)
	}
}

func TestOpenAIWIFSource_NilTokenSource(t *testing.T) {
	src := NewOpenAIWIFSource(nil, testOpenAIIdentityProviderID, testOpenAIServiceAccountID, "")
	_, err := src.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected error for nil token source")
	}
	if !strings.Contains(err.Error(), "token source is required") {
		t.Errorf("error should mention token source, got: %v", err)
	}
}

func TestOpenAIWIFSource_MissingRequiredFieldsAtResolve(t *testing.T) {
	cases := []struct {
		name      string
		idpID     string
		saID      string
		errSubstr string
	}{
		{"missing identity provider", "", testOpenAIServiceAccountID, "identity_provider_id is required"},
		{"missing service account", testOpenAIIdentityProviderID, "", "service_account_id is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := NewOpenAIWIFSource(&stubTokenSource{token: []byte("x")}, tc.idpID, tc.saID, "")
			_, err := src.Resolve(context.Background())
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("error should mention %q, got: %v", tc.errSubstr, err)
			}
		})
	}
}

// TestOpenAIWIFSource_ConcurrentBearerCallsSingleFlight pins the
// oauth2.ReuseTokenSource single-flight contract under contention.
func TestOpenAIWIFSource_ConcurrentBearerCallsSingleFlight(t *testing.T) {
	var exchanges atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		exchanges.Add(1)
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(oauthExchangeResponse{
			AccessToken: "concurrent-tok",
			TokenType:   "Bearer",
			ExpiresIn:   3600,
		})
	}))
	defer srv.Close()

	src := newOpenAIWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testOpenAIIdentityProviderID, testOpenAIServiceAccountID, "",
		srv.URL,
	)
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	const goroutines = 20
	results := make(chan string, goroutines)
	errs := make(chan error, goroutines)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		go func() {
			<-start
			tok, err := cred.BearerToken(context.Background())
			if err != nil {
				errs <- err
				return
			}
			results <- tok
		}()
	}
	close(start)

	for i := 0; i < goroutines; i++ {
		select {
		case tok := <-results:
			if tok != "concurrent-tok" {
				t.Errorf("goroutine %d got %q, want concurrent-tok", i, tok)
			}
		case err := <-errs:
			t.Fatalf("goroutine returned error: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for goroutines")
		}
	}
	if got := exchanges.Load(); got != 1 {
		t.Errorf("exchange count = %d, want 1 (ReuseTokenSource must serialise refresh)", got)
	}
}

// TestOpenAIWIFSource_NetworkError covers the http.Client.Do failure path.
func TestOpenAIWIFSource_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler must not be reached after server is closed")
	}))
	url := srv.URL
	srv.Close()

	src := newOpenAIWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testOpenAIIdentityProviderID, testOpenAIServiceAccountID, "",
		url,
	)
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, err = cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected error against closed server")
	}
	if !strings.Contains(err.Error(), "OpenAI WIF:") {
		t.Errorf("error should be prefixed with \"OpenAI WIF:\", got: %v", err)
	}
	if !strings.Contains(err.Error(), "token request") {
		t.Errorf("error should mention \"token request\", got: %v", err)
	}
}

// TestOpenAIWIFSource_TokenSourceError verifies an underlying TokenSource
// failure is wrapped, not swallowed.
func TestOpenAIWIFSource_TokenSourceError(t *testing.T) {
	src := newOpenAIWIFSourceForTest(
		t,
		&stubTokenSource{err: errors.New("idp boom")},
		testOpenAIIdentityProviderID, testOpenAIServiceAccountID, "",
		"http://unused.invalid",
	)
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, err = cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected error from underlying token source")
	}
	if !strings.Contains(err.Error(), "idp boom") {
		t.Errorf("error should wrap underlying token source failure, got: %v", err)
	}
}
