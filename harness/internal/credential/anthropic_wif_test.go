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

// Federation identifiers used across the happy-path tests; the exact
// values are arbitrary and only need to round-trip unchanged.
const (
	testFederationRuleID = "fdrl_abc123"
	testOrganizationID   = "550e8400-e29b-41d4-a716-446655440000"
	testServiceAccountID = "svac_xyz789"
	testWorkspaceID      = "wrkspc_def456"
)

// newAnthropicWIFSourceForTest builds a source pointed at a test
// server by patching tokenURL after construction.
func newAnthropicWIFSourceForTest(t *testing.T, ts TokenSource, ruleID, orgID, saID, wsID, tokenURL string) *AnthropicWIFSource {
	t.Helper()
	src := NewAnthropicWIFSource(ts, ruleID, orgID, saID, wsID)
	src.tokenURL = tokenURL
	return src
}

// anthropicOAuthHandler returns an httptest handler that asserts the
// documented request shape and responds with a synthesised access
// token. The verify callback runs against the parsed request before
// the response is built, allowing the caller to make field-by-field
// assertions in addition to the universal shape check applied here.
func anthropicOAuthHandler(t *testing.T, accessToken string, expiresIn int64, verify func(req anthropicOAuthRequest, raw []byte)) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("Anthropic OAuth got %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		if got := r.Header.Get("anthropic-version"); got != anthropicVersionHeader {
			t.Errorf("anthropic-version header = %q, want %q", got, anthropicVersionHeader)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		var parsed anthropicOAuthRequest
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("unmarshal request body: %v", err)
		}
		if parsed.GrantType != anthropicJWTBearerGrantType {
			t.Errorf("grant_type = %q, want %q", parsed.GrantType, anthropicJWTBearerGrantType)
		}
		if verify != nil {
			verify(parsed, raw)
		}
		resp := oauthExchangeResponse{
			AccessToken: accessToken,
			TokenType:   "Bearer",
			ExpiresIn:   expiresIn,
			Scope:       "workspace:developer",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func TestAnthropicWIFSource_HappyPath(t *testing.T) {
	const oidcJWT = "eyJ.fake-jwt.signature"
	const accessToken = "sk-ant-oat01-fake-access-token"

	srv := httptest.NewServer(anthropicOAuthHandler(t, accessToken, 3600, func(req anthropicOAuthRequest, _ []byte) {
		if req.Assertion != oidcJWT {
			t.Errorf("assertion = %q, want %q", req.Assertion, oidcJWT)
		}
		if req.FederationRuleID != testFederationRuleID {
			t.Errorf("federation_rule_id = %q, want %q", req.FederationRuleID, testFederationRuleID)
		}
		if req.OrganizationID != testOrganizationID {
			t.Errorf("organization_id = %q, want %q", req.OrganizationID, testOrganizationID)
		}
		if req.ServiceAccountID != testServiceAccountID {
			t.Errorf("service_account_id = %q, want %q", req.ServiceAccountID, testServiceAccountID)
		}
		if req.WorkspaceID != testWorkspaceID {
			t.Errorf("workspace_id = %q, want %q", req.WorkspaceID, testWorkspaceID)
		}
	}))
	defer srv.Close()

	src := newAnthropicWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte(oidcJWT)},
		testFederationRuleID, testOrganizationID, testServiceAccountID, testWorkspaceID,
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

// Verifies the parsed expires_in feeds into oauth2.Token.Expiry.
func TestAnthropicWIFSource_ExpiresInHonoured(t *testing.T) {
	const customExpiresIn int64 = 600 // 10 minutes — well outside the 1-hour fallback
	srv := httptest.NewServer(anthropicOAuthHandler(t, "tok", customExpiresIn, nil))
	defer srv.Close()

	src := newAnthropicWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testFederationRuleID, testOrganizationID, testServiceAccountID, "",
		srv.URL,
	)

	inner := &anthropicWIFTokenSource{src: src}
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

// Verifies the 1-hour expiry fallback when the server omits expires_in.
func TestAnthropicWIFSource_MissingExpiresInFallback(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		// expires_in omitted entirely.
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer"}`))
	}))
	defer srv.Close()

	src := newAnthropicWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testFederationRuleID, testOrganizationID, testServiceAccountID, "",
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

// Asserts workspace_id is omitted (not sent empty) when unset; inspects
// raw bytes since unmarshalling would erase the distinction.
func TestAnthropicWIFSource_WorkspaceIDOmittedWhenEmpty(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(anthropicOAuthHandler(t, "tok", 3600, func(_ anthropicOAuthRequest, raw []byte) {
		capturedBody = append(capturedBody[:0], raw...)
	}))
	defer srv.Close()

	src := newAnthropicWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testFederationRuleID, testOrganizationID, testServiceAccountID, "",
		srv.URL,
	)

	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := cred.BearerToken(context.Background()); err != nil {
		t.Fatalf("BearerToken: %v", err)
	}
	if strings.Contains(string(capturedBody), "workspace_id") {
		t.Errorf("workspace_id must be omitted from request when empty, got body: %s", capturedBody)
	}
}

// Verifies the "default" workspace ID rides through as a literal value.
func TestAnthropicWIFSource_WorkspaceIDDefaultLiteralSent(t *testing.T) {
	srv := httptest.NewServer(anthropicOAuthHandler(t, "tok", 3600, func(req anthropicOAuthRequest, _ []byte) {
		if req.WorkspaceID != "default" {
			t.Errorf("workspace_id = %q, want %q", req.WorkspaceID, "default")
		}
	}))
	defer srv.Close()

	src := newAnthropicWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testFederationRuleID, testOrganizationID, testServiceAccountID, "default",
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

// Covers the four status codes Anthropic uses for federation failures
// and asserts the body is included in the wrapped error.
func TestAnthropicWIFSource_HTTPErrorStatusSurfacesBody(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"bad request", http.StatusBadRequest, `{"error":"invalid_grant","error_description":"signature mismatch"}`},
		{"unauthorized", http.StatusUnauthorized, `{"error":"invalid_client"}`},
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

			src := newAnthropicWIFSourceForTest(
				t,
				&stubTokenSource{token: []byte("oidc")},
				testFederationRuleID, testOrganizationID, testServiceAccountID, "",
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
			if !strings.Contains(msg, "Anthropic WIF") {
				t.Errorf("error should be prefixed with \"Anthropic WIF\", got: %v", err)
			}

			expectedStatus := http.StatusText(tc.status)
			_ = expectedStatus
			if !strings.Contains(msg, "returned ") {
				t.Errorf("error should name the status code, got: %v", err)
			}

			if tc.body != "" && !strings.Contains(msg, strings.TrimSpace(tc.body)[:min(20, len(strings.TrimSpace(tc.body)))]) {
				t.Errorf("error should include body excerpt, got: %v", err)
			}
		})
	}
}

// Verifies the response body cap is enforced on the error path.
func TestAnthropicWIFSource_HTTPErrorBodyTruncated(t *testing.T) {
	huge := strings.Repeat("A", stsErrorBodyLimit*4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(huge))
	}))
	defer srv.Close()

	src := newAnthropicWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testFederationRuleID, testOrganizationID, testServiceAccountID, "",
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
	// Error message must be bounded — header overhead + truncated body
	// + ellipsis, well under twice stsErrorBodyLimit.
	if len(msg) > 2*stsErrorBodyLimit {
		t.Errorf("error message length %d should be capped near %d (truncateForError should bound the body)", len(msg), stsErrorBodyLimit)
	}
	if !strings.Contains(msg, "…") {
		t.Errorf("error should end with truncation ellipsis when body exceeds cap, got: %q", msg[len(msg)-min(40, len(msg)):])
	}
}

// Verifies the request_id from the Anthropic response header rides
// through to the error message.
func TestAnthropicWIFSource_HTTPErrorIncludesRequestID(t *testing.T) {
	const reqID = "req_01ABCDEFGHJKMNPQRSTVWXYZ12"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", reqID)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	src := newAnthropicWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testFederationRuleID, testOrganizationID, testServiceAccountID, "",
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

// A 200 with non-JSON content must produce a parse error, not a panic.
func TestAnthropicWIFSource_MalformedJSONResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	src := newAnthropicWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testFederationRuleID, testOrganizationID, testServiceAccountID, "",
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

// A 200 response omitting access_token must surface as an error, not
// an empty bearer.
func TestAnthropicWIFSource_EmptyAccessToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"","token_type":"Bearer","expires_in":3600}`))
	}))
	defer srv.Close()

	src := newAnthropicWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testFederationRuleID, testOrganizationID, testServiceAccountID, "",
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

// Verifies the underlying TokenSource is consulted on each exchange,
// not cached at construction, since projected tokens rotate ahead of
// expiry.
func TestAnthropicWIFSource_JWTReadFreshOnEveryExchange(t *testing.T) {
	srv := httptest.NewServer(anthropicOAuthHandler(t, "tok", 1, nil)) // 1s expiry forces refresh
	defer srv.Close()

	stub := &stubTokenSource{token: []byte("jwt")}
	src := newAnthropicWIFSourceForTest(
		t,
		stub,
		testFederationRuleID, testOrganizationID, testServiceAccountID, "",
		srv.URL,
	)
	inner := &anthropicWIFTokenSource{src: src}

	// Two direct Token() calls bypass ReuseTokenSource caching.
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

// Pins the ReuseTokenSource contract: repeated BearerToken calls must
// not round-trip to the exchange endpoint while the token is fresh.
func TestAnthropicWIFSource_ReuseTokenSourceCachesExchange(t *testing.T) {
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

	src := newAnthropicWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testFederationRuleID, testOrganizationID, testServiceAccountID, "",
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

func TestAnthropicWIFSource_EmptyJWTBytes(t *testing.T) {
	src := newAnthropicWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte{}},
		testFederationRuleID, testOrganizationID, testServiceAccountID, "",
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
	if !strings.HasPrefix(err.Error(), "Anthropic WIF:") && !strings.Contains(err.Error(), "Anthropic WIF:") {
		t.Errorf("error should be prefixed with \"Anthropic WIF:\", got: %v", err)
	}
	if !strings.Contains(err.Error(), "empty token") {
		t.Errorf("error should mention empty token, got: %v", err)
	}
}

func TestAnthropicWIFSource_NilTokenSource(t *testing.T) {
	src := NewAnthropicWIFSource(nil, testFederationRuleID, testOrganizationID, testServiceAccountID, "")
	_, err := src.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected error for nil token source")
	}
	if !strings.Contains(err.Error(), "token source is required") {
		t.Errorf("error should mention token source, got: %v", err)
	}
}

func TestAnthropicWIFSource_MissingRequiredFieldsAtResolve(t *testing.T) {
	cases := []struct {
		name      string
		ruleID    string
		orgID     string
		saID      string
		errSubstr string
	}{
		{"missing federation rule", "", testOrganizationID, testServiceAccountID, "federation_rule_id is required"},
		{"missing organization", testFederationRuleID, "", testServiceAccountID, "organization_id is required"},
		{"missing service account", testFederationRuleID, testOrganizationID, "", "service_account_id is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := NewAnthropicWIFSource(&stubTokenSource{token: []byte("x")}, tc.ruleID, tc.orgID, tc.saID, "")
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

// Pins the oauth2.ReuseTokenSource single-flight contract: twenty
// concurrent BearerToken callers must trigger exactly one exchange.
func TestAnthropicWIFSource_ConcurrentBearerCallsSingleFlight(t *testing.T) {
	var exchanges atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Sleep widens the contention window so a non-locking
		// implementation would reliably produce >1 exchange.
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

	src := newAnthropicWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testFederationRuleID, testOrganizationID, testServiceAccountID, "",
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
	close(start) // all 20 race on the cached-token slot at once

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

// Covers the http.Client.Do failure path against a refused connection.
func TestAnthropicWIFSource_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler must not be reached after server is closed")
	}))
	url := srv.URL
	srv.Close() // server is now refusing connections

	src := newAnthropicWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testFederationRuleID, testOrganizationID, testServiceAccountID, "",
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
	if !strings.Contains(err.Error(), "Anthropic WIF:") {
		t.Errorf("error should be prefixed with \"Anthropic WIF:\", got: %v", err)
	}
	if !strings.Contains(err.Error(), "token request") {
		t.Errorf("error should mention \"token request\", got: %v", err)
	}
}

// Pins the lifetime <= 0 → 1h fallback for a negative expires_in,
// guarding against a future refactor narrowing the check to == 0.
func TestAnthropicWIFSource_NegativeExpiresInFallback(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":-600}`))
	}))
	defer srv.Close()

	src := newAnthropicWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testFederationRuleID, testOrganizationID, testServiceAccountID, "",
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
		t.Errorf("exchange hit %d times, want 1 (negative expires_in must trigger 1-hour fallback)", got)
	}
}

// Verifies an underlying TokenSource failure is wrapped, not swallowed.
func TestAnthropicWIFSource_TokenSourceError(t *testing.T) {
	src := newAnthropicWIFSourceForTest(
		t,
		&stubTokenSource{err: errors.New("idp boom")},
		testFederationRuleID, testOrganizationID, testServiceAccountID, "",
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
