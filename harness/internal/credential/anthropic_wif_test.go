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

// validAnthropicWIFFields holds the four federation identifiers used
// across the happy-path tests. The exact values are arbitrary; they
// only need to round-trip through the request body unchanged.
const (
	testFederationRuleID = "fdrl_abc123"
	testOrganizationID   = "550e8400-e29b-41d4-a716-446655440000"
	testServiceAccountID = "svac_xyz789"
	testWorkspaceID      = "wrkspc_def456"
)

// newAnthropicWIFSourceForTest builds a source pointed at a test
// server. Mirrors NewAnthropicWIFSource but patches tokenURL so we
// never hit the real Anthropic endpoint.
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
		resp := anthropicOAuthResponse{
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

// TestAnthropicWIFSource_ExpiresInHonoured verifies that the parsed
// expires_in feeds into the oauth2.Token.Expiry field. ReuseTokenSource
// reads Expiry to decide whether to hit the exchange endpoint; without
// the field set correctly, every adapter call would re-exchange.
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

// TestAnthropicWIFSource_MissingExpiresInFallback verifies the 1-hour
// fallback when the server omits expires_in. Without this fallback,
// ReuseTokenSource would treat the token as already expired and
// re-hit the exchange on every BearerToken call.
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

// TestAnthropicWIFSource_WorkspaceIDOmittedWhenEmpty asserts the
// omitempty contract on the JSON body. Anthropic's endpoint requires
// workspace_id absent — not present-as-empty-string — when the
// federation rule is bound to a single workspace. Inspect the raw
// bytes the server received, since unmarshalling would erase the
// distinction.
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

// TestAnthropicWIFSource_WorkspaceIDDefaultLiteralSent verifies the
// magic string "default" rides through to the wire as a literal value
// (and is not coerced to anything else).
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

// TestAnthropicWIFSource_HTTPErrorStatusSurfacesBody covers the four
// status codes Anthropic uses for federation failures and asserts the
// body is included in the wrapped error.
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
			// Status code must be visible to operators triaging the failure.
			expectedStatus := http.StatusText(tc.status)
			_ = expectedStatus
			if !strings.Contains(msg, "returned ") {
				t.Errorf("error should name the status code, got: %v", err)
			}
			// The body excerpt must be present (or its leading prefix when
			// the body is short enough not to be truncated).
			if tc.body != "" && !strings.Contains(msg, strings.TrimSpace(tc.body)[:min(20, len(strings.TrimSpace(tc.body)))]) {
				t.Errorf("error should include body excerpt, got: %v", err)
			}
		})
	}
}

// TestAnthropicWIFSource_HTTPErrorBodyTruncated verifies the response
// body cap is enforced. A hostile or misconfigured endpoint that
// streams a multi-MiB error body must not propagate the full payload
// through every error wrapper into slog and OTel span statuses.
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

// TestAnthropicWIFSource_HTTPErrorIncludesRequestID verifies the
// request_id from the Anthropic response header rides through to the
// error message. Operators triaging a federation failure use the
// request_id to correlate with the Console authentication-history
// page (see issue #117 Risk #1).
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

// TestAnthropicWIFSource_MalformedJSONResponse covers the
// json.Unmarshal branch in Token(). A 200 with non-JSON content must
// produce a clear "parse token response" error rather than a
// nil-pointer panic in the access-token check.
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

// TestAnthropicWIFSource_EmptyAccessToken covers the empty-string
// guard. A 200 response that omits the access_token must surface as a
// federation error rather than yielding an empty bearer to the
// provider adapter.
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

// TestAnthropicWIFSource_JWTReadFreshOnEveryExchange verifies that the
// underlying TokenSource is consulted on each exchange — not just once
// at construction. Projected k8s tokens and GitHub Actions OIDC tokens
// rotate ahead of expiry; if we cached the JWT inside the source the
// federation flow would silently submit a stale assertion and fail
// after the IdP rotation window.
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

// TestAnthropicWIFSource_ReuseTokenSourceCachesExchange pins the
// ReuseTokenSource contract: while the cached access token is fresh,
// repeated BearerToken calls must NOT round-trip to the exchange
// endpoint.
func TestAnthropicWIFSource_ReuseTokenSourceCachesExchange(t *testing.T) {
	var exchanges int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&exchanges, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(anthropicOAuthResponse{
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

// TestAnthropicWIFSource_TokenSourceError verifies that an underlying
// TokenSource failure is wrapped, not swallowed. Operators need the
// inner cause to debug "the GHA OIDC endpoint is down" vs "Anthropic
// rejected our assertion".
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
