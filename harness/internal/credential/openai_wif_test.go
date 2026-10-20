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
	check := assert.New(t)
	return func(w http.ResponseWriter, r *http.Request) {
		// Runs on the server goroutine, so failures are reported with assert, not require.
		check.Equal(http.MethodPost, r.Method)
		check.Equal("application/json", r.Header.Get("Content-Type"))
		raw, err := io.ReadAll(r.Body)
		if !check.NoError(err, "read request body") {
			return
		}
		var parsed openAIOAuthRequest
		if !check.NoError(json.Unmarshal(raw, &parsed), "unmarshal request body") {
			return
		}
		check.Equal(openAITokenExchangeGrantType, parsed.GrantType)
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

func TestOpenAIWIFSource_ExchangesJWTForBearerToken(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	const oidcJWT = "eyJ.fake-jwt.signature"
	const accessToken = "openai-federated-access-token"

	srv := httptest.NewServer(openAIOAuthHandler(t, accessToken, 3600, func(req openAIOAuthRequest, _ []byte) {
		check.Equal(oidcJWT, req.SubjectToken, "subject_token")
		check.Equal(openAIDefaultSubjectTokenType, req.SubjectTokenType, "subject_token_type")
		check.Equal(testOpenAIIdentityProviderID, req.IdentityProviderID, "identity_provider_id")
		check.Equal(testOpenAIServiceAccountID, req.ServiceAccountID, "service_account_id")
	}))
	defer srv.Close()

	src := newOpenAIWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte(oidcJWT)},
		testOpenAIIdentityProviderID, testOpenAIServiceAccountID, "",
		srv.URL,
	)

	cred, err := src.Resolve(context.Background())
	must.NoError(err)
	must.NotNil(cred.BearerToken, "expected BearerToken closure")

	got, err := cred.BearerToken(context.Background())
	must.NoError(err)
	must.Equal(accessToken, got)
}

// TestOpenAIWIFSource_NoAudienceOrOrgInBody asserts the exchange body carries
// only the five documented fields. OpenAI binds the audience via the subject
// token's aud claim and org/project via the service-account mapping, so
// sending audience / organization_id / workspace_id would be undocumented
// and likely rejected. Inspect the raw bytes since unmarshalling would erase
// the distinction.
func TestOpenAIWIFSource_NoAudienceOrOrgInBody(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.NoError(err)
	for _, forbidden := range []string{"audience", "organization_id", "project", "workspace_id", "resource"} {
		must.NotContains(string(capturedBody), forbidden, "request body must not contain %q", forbidden)
	}
}

// TestOpenAIWIFSource_SubjectTokenTypeOverride verifies a non-default
// subject_token_type rides through to the wire unchanged.
func TestOpenAIWIFSource_SubjectTokenTypeOverride(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	const custom = "urn:ietf:params:oauth:token-type:id_token"
	srv := httptest.NewServer(openAIOAuthHandler(t, "tok", 3600, func(req openAIOAuthRequest, _ []byte) {
		check.Equal(custom, req.SubjectTokenType)
	}))
	defer srv.Close()

	src := newOpenAIWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testOpenAIIdentityProviderID, testOpenAIServiceAccountID, custom,
		srv.URL,
	)
	cred, err := src.Resolve(context.Background())
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.NoError(err)
}

// TestOpenAIWIFSource_DefaultSubjectTokenTypeApplied pins that the
// constructor fills in the JWT default when subjectTokenType is empty.
func TestOpenAIWIFSource_DefaultSubjectTokenTypeApplied(t *testing.T) {
	must := require.New(t)
	src := NewOpenAIWIFSource(&stubTokenSource{token: []byte("x")}, testOpenAIIdentityProviderID, testOpenAIServiceAccountID, "")
	must.Equal(openAIDefaultSubjectTokenType, src.subjectTokenType)
}

// TestOpenAIWIFSource_ExpiresInSetsTokenExpiry verifies the parsed expires_in feeds
// into oauth2.Token.Expiry so ReuseTokenSource caches correctly.
func TestOpenAIWIFSource_ExpiresInSetsTokenExpiry(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	expectedFloor := before.Add(time.Duration(customExpiresIn) * time.Second)
	expectedCeil := after.Add(time.Duration(customExpiresIn) * time.Second)
	must.WithinRange(tok.Expiry, expectedFloor, expectedCeil)
}

// TestOpenAIWIFSource_MissingExpiresInFallback verifies the 1-hour fallback
// when the server omits expires_in.
func TestOpenAIWIFSource_MissingExpiresInFallback(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err)
	tok1, err := cred.BearerToken(context.Background())
	must.NoError(err, "BearerToken first")
	tok2, err := cred.BearerToken(context.Background())
	must.NoError(err, "BearerToken second")
	check.Equal("tok", tok1)
	check.Equal("tok", tok2)
	must.EqualValues(1, atomic.LoadInt32(&calls), "1-hour fallback should let cache absorb second call")
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
			must := require.New(t)
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
			must.NoError(err)
			_, err = cred.BearerToken(context.Background())
			must.Error(err)
			must.ErrorContains(err, "OpenAI WIF", "error should be prefixed with \"OpenAI WIF\"")
			must.ErrorContains(err, "returned ", "error should name the status code")
			bodyExcerpt := strings.TrimSpace(tc.body)
			bodyExcerpt = bodyExcerpt[:min(20, len(bodyExcerpt))]
			must.ErrorContains(err, bodyExcerpt, "error should include body excerpt")
		})
	}
}

// TestOpenAIWIFSource_HTTPErrorBodyTruncated verifies the response body cap.
func TestOpenAIWIFSource_HTTPErrorBodyTruncated(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.Error(err, "expected error for huge body")
	msg := err.Error()
	check.LessOrEqual(len(msg), 2*stsErrorBodyLimit, "error message should be capped near %d", stsErrorBodyLimit)
	check.Contains(msg, "…", "error should end with truncation ellipsis when body exceeds cap")
}

// TestOpenAIWIFSource_HTTPErrorIncludesRequestID verifies the x-request-id
// response header (read opportunistically) rides through to the error so
// operators can correlate a failed exchange.
func TestOpenAIWIFSource_HTTPErrorIncludesRequestID(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.ErrorContains(err, reqID)
}

// TestOpenAIWIFSource_MalformedJSONResponse covers the json.Unmarshal branch.
func TestOpenAIWIFSource_MalformedJSONResponse(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.ErrorContains(err, "parse token response")
}

// TestOpenAIWIFSource_EmptyAccessToken covers the empty-string guard.
func TestOpenAIWIFSource_EmptyAccessToken(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.ErrorContains(err, "empty access_token")
}

// TestOpenAIWIFSource_JWTReadFreshOnEveryExchange verifies the underlying
// TokenSource is consulted on each exchange — projected k8s tokens and GHA
// OIDC tokens rotate ahead of expiry, and the OpenAI access token never
// outlives the subject token.
func TestOpenAIWIFSource_JWTReadFreshOnEveryExchange(t *testing.T) {
	must := require.New(t)
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

	_, err := inner.Token()
	must.NoError(err, "Token first")
	_, err = inner.Token()
	must.NoError(err, "Token second")
	must.EqualValues(2, atomic.LoadInt32(&stub.calls), "each exchange must re-read the JWT")
}

// TestOpenAIWIFSource_ReuseTokenSourceCachesExchange pins the caching
// contract: while the cached access token is fresh, repeated BearerToken
// calls must not round-trip to the exchange endpoint.
func TestOpenAIWIFSource_ReuseTokenSourceCachesExchange(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err)

	for i := 0; i < 5; i++ {
		got, err := cred.BearerToken(context.Background())
		must.NoError(err, "BearerToken %d", i)
		check.Equal("cached", got, "call %d", i)
	}
	must.EqualValues(1, atomic.LoadInt32(&exchanges), "ReuseTokenSource should cache the exchange across 5 calls")
}

func TestOpenAIWIFSource_EmptyJWTRejectedWithoutExchange(t *testing.T) {
	must := require.New(t)
	src := newOpenAIWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte{}},
		testOpenAIIdentityProviderID, testOpenAIServiceAccountID, "",
		"http://unused.invalid",
	)
	cred, err := src.Resolve(context.Background())
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.ErrorContains(err, "OpenAI WIF:", "error should be prefixed with \"OpenAI WIF:\"")
	must.ErrorContains(err, "empty token")
}

func TestOpenAIWIFSource_NilTokenSourceRejectedAtResolve(t *testing.T) {
	must := require.New(t)
	src := NewOpenAIWIFSource(nil, testOpenAIIdentityProviderID, testOpenAIServiceAccountID, "")
	_, err := src.Resolve(context.Background())
	must.ErrorContains(err, "token source is required")
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
			must := require.New(t)
			src := NewOpenAIWIFSource(&stubTokenSource{token: []byte("x")}, tc.idpID, tc.saID, "")
			_, err := src.Resolve(context.Background())
			must.ErrorContains(err, tc.errSubstr)
		})
	}
}

// TestOpenAIWIFSource_ConcurrentBearerCallsSingleFlight pins the
// oauth2.ReuseTokenSource single-flight contract under contention.
func TestOpenAIWIFSource_ConcurrentBearerCallsSingleFlight(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	close(start)

	for i := 0; i < goroutines; i++ {
		select {
		case tok := <-results:
			check.Equal("concurrent-tok", tok, "goroutine result %d", i)
		case err := <-errs:
			must.NoError(err, "goroutine returned error")
		case <-time.After(5 * time.Second):
			must.Fail("timeout waiting for goroutines")
		}
	}
	must.EqualValues(1, exchanges.Load(), "ReuseTokenSource must serialise refresh")
}

// TestOpenAIWIFSource_NetworkError covers the http.Client.Do failure path.
func TestOpenAIWIFSource_NetworkError(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		check.Fail("handler must not be reached after server is closed")
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
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.ErrorContains(err, "OpenAI WIF:")
	must.ErrorContains(err, "token request")
}

// TestOpenAIWIFSource_NegativeExpiresInFallback verifies a negative
// expires_in triggers the shared 1-hour fallback rather than yielding a
// token whose Expiry is already in the past.
func TestOpenAIWIFSource_NegativeExpiresInFallback(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":-600}`))
	}))
	defer srv.Close()

	src := newOpenAIWIFSourceForTest(
		t,
		&stubTokenSource{token: []byte("oidc")},
		testOpenAIIdentityProviderID, testOpenAIServiceAccountID, "",
		srv.URL,
	)
	cred, err := src.Resolve(context.Background())
	must.NoError(err)
	tok1, err := cred.BearerToken(context.Background())
	must.NoError(err, "BearerToken first")
	tok2, err := cred.BearerToken(context.Background())
	must.NoError(err, "BearerToken second")
	check.Equal("tok", tok1)
	check.Equal("tok", tok2)
	must.EqualValues(1, atomic.LoadInt32(&calls), "negative expires_in must trigger 1-hour fallback")
}

// TestOpenAIWIFSource_CorrelationHeaderCapped verifies a server-controlled
// correlation header is capped via sanitiseCorrelationID before embedding
// in the error, so a hostile endpoint cannot pad the error (and any slog
// line it reaches) with an oversized value.
func TestOpenAIWIFSource_CorrelationHeaderCapped(t *testing.T) {
	must := require.New(t)
	oversized := strings.Repeat("A", maxCorrelationIDLen*3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-request-id", oversized)
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
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.Error(err)
	must.NotContains(err.Error(), strings.Repeat("A", maxCorrelationIDLen+1),
		"correlation id should be capped at %d chars", maxCorrelationIDLen)
}

// TestOpenAIWIFSource_WrapsTokenSourceError verifies an underlying TokenSource
// failure is wrapped, not swallowed.
func TestOpenAIWIFSource_WrapsTokenSourceError(t *testing.T) {
	must := require.New(t)
	src := newOpenAIWIFSourceForTest(
		t,
		&stubTokenSource{err: errors.New("idp boom")},
		testOpenAIIdentityProviderID, testOpenAIServiceAccountID, "",
		"http://unused.invalid",
	)
	cred, err := src.Resolve(context.Background())
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.ErrorContains(err, "idp boom", "error should wrap underlying token source failure")
}
