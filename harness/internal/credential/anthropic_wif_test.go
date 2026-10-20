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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
//
// The handler runs on the server's goroutine, so it reports failures
// with assert rather than require.
func anthropicOAuthHandler(t *testing.T, accessToken string, expiresIn int64, verify func(req anthropicOAuthRequest, raw []byte)) http.HandlerFunc {
	t.Helper()
	check := assert.New(t)
	return func(w http.ResponseWriter, r *http.Request) {
		check.Equal(http.MethodPost, r.Method, "Anthropic OAuth request method")
		check.Equal("application/json", r.Header.Get("Content-Type"), "Content-Type header")
		check.Equal(anthropicVersionHeader, r.Header.Get("anthropic-version"), "anthropic-version header")
		raw, err := io.ReadAll(r.Body)
		if !check.NoError(err, "read request body") {
			return
		}
		var parsed anthropicOAuthRequest
		if !check.NoError(json.Unmarshal(raw, &parsed), "unmarshal request body") {
			return
		}
		check.Equal(anthropicJWTBearerGrantType, parsed.GrantType, "grant_type")
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

func TestAnthropicWIFSource_Resolve_ReturnsExchangedAccessToken(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	const oidcJWT = "eyJ.fake-jwt.signature"
	const accessToken = "sk-ant-oat01-fake-access-token"

	srv := httptest.NewServer(anthropicOAuthHandler(t, accessToken, 3600, func(req anthropicOAuthRequest, _ []byte) {
		check.Equal(oidcJWT, req.Assertion, "assertion")
		check.Equal(testFederationRuleID, req.FederationRuleID, "federation_rule_id")
		check.Equal(testOrganizationID, req.OrganizationID, "organization_id")
		check.Equal(testServiceAccountID, req.ServiceAccountID, "service_account_id")
		check.Equal(testWorkspaceID, req.WorkspaceID, "workspace_id")
	}))
	defer srv.Close()

	src := newAnthropicWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte(oidcJWT)},
		testFederationRuleID, testOrganizationID, testServiceAccountID, testWorkspaceID,
		srv.URL,
	)

	cred, err := src.Resolve(context.Background())
	must.NoError(err)
	must.NotNil(cred.BearerToken, "expected BearerToken closure")

	got, err := cred.BearerToken(context.Background())
	must.NoError(err)
	must.Equal(accessToken, got)
}

// Verifies the parsed expires_in feeds into oauth2.Token.Expiry.
func TestAnthropicWIFSource_Token_HonoursServerExpiresIn(t *testing.T) {
	must := require.New(t)
	const customExpiresIn int64 = 600 // 10 minutes, well outside the 1-hour fallback
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
	must.NoError(err)
	lifetime := time.Duration(customExpiresIn) * time.Second
	must.WithinRange(tok.Expiry, before.Add(lifetime), after.Add(lifetime), "expiry")
}

// Verifies the 1-hour expiry fallback when the server omits expires_in.
func TestAnthropicWIFSource_MissingExpiresIn_CachesForOneHour(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	tok1, err := cred.BearerToken(context.Background())
	must.NoError(err, "BearerToken first")
	tok2, err := cred.BearerToken(context.Background())
	must.NoError(err, "BearerToken second")
	must.Equal("tok", tok1, "first bearer")
	must.Equal("tok", tok2, "second bearer")
	must.Equal(int32(1), atomic.LoadInt32(&calls), "the 1-hour fallback should let the cache absorb the second call")
}

// Asserts workspace_id is omitted (not sent empty) when unset; inspects
// raw bytes since unmarshalling would erase the distinction.
func TestAnthropicWIFSource_WorkspaceIDOmittedWhenEmpty(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.NoError(err)
	must.NotContains(string(capturedBody), "workspace_id", "workspace_id must be omitted from the request when empty")
}

// Verifies the "default" workspace ID rides through as a literal value.
func TestAnthropicWIFSource_WorkspaceIDDefaultLiteralSent(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv := httptest.NewServer(anthropicOAuthHandler(t, "tok", 3600, func(req anthropicOAuthRequest, _ []byte) {
		check.Equal("default", req.WorkspaceID, "workspace_id")
	}))
	defer srv.Close()

	src := newAnthropicWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testFederationRuleID, testOrganizationID, testServiceAccountID, "default",
		srv.URL,
	)
	cred, err := src.Resolve(context.Background())
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.NoError(err)
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
			must := require.New(t)
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
			must.NoError(err)
			_, err = cred.BearerToken(context.Background())
			must.Error(err)
			must.ErrorContains(err, "Anthropic WIF", "error should be prefixed with \"Anthropic WIF\"")
			must.ErrorContains(err, "returned ", "error should name the status code")

			excerpt := strings.TrimSpace(tc.body)
			excerpt = excerpt[:min(20, len(excerpt))]
			must.ErrorContains(err, excerpt, "error should include the body excerpt")
		})
	}
}

// Verifies the response body cap is enforced on the error path.
func TestAnthropicWIFSource_HTTPErrorBodyTruncated(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.Error(err, "expected error for huge body")
	// Header overhead plus the truncated body plus the ellipsis stays
	// well under twice the body limit.
	must.LessOrEqual(len(err.Error()), 2*stsErrorBodyLimit,
		"error message should be capped near %d (truncateForError should bound the body)", stsErrorBodyLimit)
	must.ErrorContains(err, "…", "error should carry the truncation ellipsis when the body exceeds the cap")
}

// Verifies the request_id from the Anthropic response header rides
// through to the error message.
func TestAnthropicWIFSource_HTTPErrorIncludesRequestID(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.Error(err)
	must.ErrorContains(err, reqID)
}

// A 200 with non-JSON content must produce a parse error, not a panic.
func TestAnthropicWIFSource_BearerToken_MalformedJSONIsParseError(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.ErrorContains(err, "parse token response")
}

// A 200 response omitting access_token must surface as an error, not
// an empty bearer.
func TestAnthropicWIFSource_BearerToken_EmptyAccessTokenIsError(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.ErrorContains(err, "empty access_token")
}

// Verifies the underlying TokenSource is consulted on each exchange,
// not cached at construction, since projected tokens rotate ahead of
// expiry.
func TestAnthropicWIFSource_JWTReadFreshOnEveryExchange(t *testing.T) {
	must := require.New(t)
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
	_, err := inner.Token()
	must.NoError(err, "Token first")
	_, err = inner.Token()
	must.NoError(err, "Token second")
	must.Equal(int32(2), atomic.LoadInt32(&stub.calls), "each exchange must re-read the JWT")
}

// Pins the ReuseTokenSource contract: repeated BearerToken calls must
// not round-trip to the exchange endpoint while the token is fresh.
func TestAnthropicWIFSource_ReuseTokenSourceCachesExchange(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)

	for i := 0; i < 5; i++ {
		got, err := cred.BearerToken(context.Background())
		must.NoError(err, "BearerToken %d", i)
		must.Equal("cached", got, "call %d", i)
	}
	must.Equal(int32(1), atomic.LoadInt32(&exchanges), "ReuseTokenSource should cache the exchange across calls")
}

func TestAnthropicWIFSource_BearerToken_EmptyJWTIsError(t *testing.T) {
	must := require.New(t)
	src := newAnthropicWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte{}},
		testFederationRuleID, testOrganizationID, testServiceAccountID, "",
		"http://unused.invalid",
	)
	cred, err := src.Resolve(context.Background())
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.ErrorContains(err, "Anthropic WIF:")
	must.ErrorContains(err, "empty token")
}

func TestAnthropicWIFSource_Resolve_NilTokenSourceIsError(t *testing.T) {
	must := require.New(t)
	src := NewAnthropicWIFSource(nil, testFederationRuleID, testOrganizationID, testServiceAccountID, "")
	_, err := src.Resolve(context.Background())
	must.ErrorContains(err, "token source is required")
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
			must := require.New(t)
			src := NewAnthropicWIFSource(&stubTokenSource{token: []byte("x")}, tc.ruleID, tc.orgID, tc.saID, "")
			_, err := src.Resolve(context.Background())
			must.ErrorContains(err, tc.errSubstr)
		})
	}
}

// Pins the oauth2.ReuseTokenSource single-flight contract: twenty
// concurrent BearerToken callers must trigger exactly one exchange.
func TestAnthropicWIFSource_ConcurrentBearerCallsSingleFlight(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err)

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
			check.Equal("concurrent-tok", tok, "goroutine result %d", i)
		case err := <-errs:
			must.NoError(err, "goroutine returned error")
		case <-time.After(5 * time.Second):
			must.FailNow("timeout waiting for goroutines")
		}
	}
	must.Equal(int32(1), exchanges.Load(), "ReuseTokenSource must serialise refresh")
}

// Covers the http.Client.Do failure path against a refused connection.
func TestAnthropicWIFSource_BearerToken_NetworkErrorIsWrapped(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		check.Fail("handler must not be reached after server is closed")
	}))
	closedURL := srv.URL
	srv.Close() // server is now refusing connections

	src := newAnthropicWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testFederationRuleID, testOrganizationID, testServiceAccountID, "",
		closedURL,
	)
	cred, err := src.Resolve(context.Background())
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.Error(err, "expected error against closed server")
	must.ErrorContains(err, "Anthropic WIF:")
	must.ErrorContains(err, "token request")
}

// Pins the lifetime <= 0 → 1h fallback for a negative expires_in,
// guarding against a future refactor narrowing the check to == 0.
func TestAnthropicWIFSource_NegativeExpiresIn_CachesForOneHour(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	tok1, err := cred.BearerToken(context.Background())
	must.NoError(err, "BearerToken first")
	tok2, err := cred.BearerToken(context.Background())
	must.NoError(err, "BearerToken second")
	must.Equal("tok", tok1, "first bearer")
	must.Equal("tok", tok2, "second bearer")
	must.Equal(int32(1), atomic.LoadInt32(&calls), "negative expires_in must trigger the 1-hour fallback")
}

// Verifies an underlying TokenSource failure is wrapped, not swallowed.
func TestAnthropicWIFSource_BearerToken_WrapsTokenSourceError(t *testing.T) {
	must := require.New(t)
	src := newAnthropicWIFSourceForTest(
		t,
		&stubTokenSource{err: errors.New("idp boom")},
		testFederationRuleID, testOrganizationID, testServiceAccountID, "",
		"http://unused.invalid",
	)
	cred, err := src.Resolve(context.Background())
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.ErrorContains(err, "idp boom", "error should wrap the underlying token source failure")
}
