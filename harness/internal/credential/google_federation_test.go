package credential

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// validWIFAudience is a syntactically correct WIF audience used across
// the federation tests. Real audiences embed an existing project
// number, pool, and provider; the value here just satisfies the shape
// check.
const validWIFAudience = "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/aws-pool/providers/aws-provider"

// stubTokenSource is a minimal TokenSource for tests. It records the
// number of Token() calls so refresh-caching tests can assert how
// often the underlying source is hit.
type stubTokenSource struct {
	token []byte
	err   error
	calls int32
}

func (s *stubTokenSource) Token(_ context.Context) ([]byte, error) {
	atomic.AddInt32(&s.calls, 1)
	if s.err != nil {
		return nil, s.err
	}
	return s.token, nil
}

// newWIFSource is a helper that builds a federation source pointed at
// test servers. It mirrors NewGCPWorkloadIdentityFederationSource but
// patches stsURL/iamCredURL so we never hit real Google endpoints.
func newWIFSource(t *testing.T, ts TokenSource, audience, sa, stsURL, iamURL string) *GCPWorkloadIdentityFederationSource {
	t.Helper()
	src := NewGCPWorkloadIdentityFederationSource(ts, audience, sa)
	src.stsURL = stsURL
	if iamURL != "" {
		src.iamCredURL = iamURL
	}
	return src
}

// stsHandler returns an httptest handler that asserts the documented
// STS request shape and responds with a federated access token. The
// caller supplies a verifier the handler runs against the parsed
// request before responding. The handler runs on the server goroutine,
// so it reports failures with assert and returns early instead of
// calling require.
func stsHandler(t *testing.T, accessToken string, expiresIn int64, verify func(stsRequest)) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err, "read STS request body") {
			return
		}
		var parsed stsRequest
		if !assert.NoError(t, json.Unmarshal(body, &parsed), "parse STS request body") {
			return
		}
		if verify != nil {
			verify(parsed)
		}
		resp := stsResponse{
			AccessToken: accessToken,
			ExpiresIn:   expiresIn,
			TokenType:   "Bearer",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// iamHandler returns an httptest handler for the impersonation step.
// It asserts the federated bearer arrives in Authorization and returns
// a service-account access token. Like stsHandler, it reports failures
// with assert because it runs on the server goroutine.
func iamHandler(t *testing.T, expectedFederated, accessToken string, expireTime time.Time) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "Bearer "+expectedFederated, r.Header.Get("Authorization"))
		resp := iamCredResponse{
			AccessToken: accessToken,
			ExpireTime:  expireTime.UTC().Format(time.RFC3339),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func TestGCPWIFSource_STSExchangeOnly(t *testing.T) {
	const oidcToken = "fake-oidc-jwt"
	const stsToken = "sts-issued-access-token"

	var stsCalls int32
	sts := httptest.NewServer(stsHandler(t, stsToken, 3600, func(req stsRequest) {
		atomic.AddInt32(&stsCalls, 1)
		assert.Equal(t, validWIFAudience, req.Audience)
		assert.Equal(t, "urn:ietf:params:oauth:grant-type:token-exchange", req.GrantType)
		assert.Equal(t, "urn:ietf:params:oauth:token-type:access_token", req.RequestedTokenType)
		assert.Equal(t, "urn:ietf:params:oauth:token-type:jwt", req.SubjectTokenType)
		assert.Equal(t, oidcToken, req.SubjectToken)
		assert.Contains(t, req.Scope, "cloud-platform")
	}))
	defer sts.Close()

	src := newWIFSource(t, &stubTokenSource{token: []byte(oidcToken)}, validWIFAudience, "", sts.URL, "")

	cred, err := src.Resolve(context.Background())
	require.NoError(t, err)
	require.NotNil(t, cred.BearerToken, "expected BearerToken closure")

	got, err := cred.BearerToken(context.Background())
	require.NoError(t, err)
	require.Equal(t, stsToken, got)
	require.Equal(t, int32(1), atomic.LoadInt32(&stsCalls), "STS call count")
}

func TestGCPWIFSource_STSPlusImpersonation(t *testing.T) {
	const oidcToken = "fake-oidc"
	const stsToken = "fed-token"
	const saToken = "sa-token"
	const targetSA = "stirrup-vertex@my-project.iam.gserviceaccount.com"

	expireTime := time.Now().Add(45 * time.Minute)

	sts := httptest.NewServer(stsHandler(t, stsToken, 3600, nil))
	defer sts.Close()

	iamMux := http.NewServeMux()
	expectedPath := fmt.Sprintf("/v1/projects/-/serviceAccounts/%s:generateAccessToken", targetSA)
	iamMux.HandleFunc(expectedPath, iamHandler(t, stsToken, saToken, expireTime))
	iam := httptest.NewServer(iamMux)
	defer iam.Close()

	iamTemplate := iam.URL + "/v1/projects/-/serviceAccounts/%s:generateAccessToken"

	src := newWIFSource(t, &stubTokenSource{token: []byte(oidcToken)}, validWIFAudience, targetSA, sts.URL, iamTemplate)
	cred, err := src.Resolve(context.Background())
	require.NoError(t, err)

	got, err := cred.BearerToken(context.Background())
	require.NoError(t, err)
	require.Equal(t, saToken, got, "bearer should be the impersonated service-account token")
}

// TestGCPWIFSource_RefreshUsesCache verifies that oauth2.ReuseTokenSource
// caches the federated token until expiry. The first BearerToken call
// must round-trip to STS exactly once; subsequent calls within the
// cached lifetime must NOT hit the wire.
func TestGCPWIFSource_RefreshUsesCache(t *testing.T) {
	var stsCalls int32
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&stsCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stsResponse{
			AccessToken: "cached-token",
			ExpiresIn:   3600, // 1h cache window
			TokenType:   "Bearer",
		})
	}))
	defer sts.Close()

	src := newWIFSource(t, &stubTokenSource{token: []byte("oidc")}, validWIFAudience, "", sts.URL, "")
	cred, err := src.Resolve(context.Background())
	require.NoError(t, err)

	for i := 0; i < 5; i++ {
		got, err := cred.BearerToken(context.Background())
		require.NoError(t, err, "BearerToken call %d", i)
		require.Equal(t, "cached-token", got, "BearerToken call %d", i)
	}

	require.Equal(t, int32(1), atomic.LoadInt32(&stsCalls), "STS hit count after 5 BearerToken calls")
}

func TestGCPWIFSource_STSError(t *testing.T) {
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"audience not configured"}`))
	}))
	defer sts.Close()

	src := newWIFSource(t, &stubTokenSource{token: []byte("oidc")}, validWIFAudience, "", sts.URL, "")
	cred, err := src.Resolve(context.Background())
	require.NoError(t, err)

	_, err = cred.BearerToken(context.Background())
	require.ErrorContains(t, err, "STS returned 400", "error should name the status code")
	require.ErrorContains(t, err, "audience not configured", "error should include the body excerpt")
}

func TestGCPWIFSource_ImpersonationError(t *testing.T) {
	sts := httptest.NewServer(stsHandler(t, "fed-tok", 3600, nil))
	defer sts.Close()

	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"missing iam.serviceAccountTokenCreator"}}`))
	}))
	defer iam.Close()

	const targetSA = "narrow-sa@proj.iam.gserviceaccount.com"
	src := newWIFSource(t, &stubTokenSource{token: []byte("oidc")}, validWIFAudience, targetSA, sts.URL, iam.URL+"/v1/projects/-/serviceAccounts/%s:generateAccessToken")
	cred, err := src.Resolve(context.Background())
	require.NoError(t, err)

	_, err = cred.BearerToken(context.Background())
	require.ErrorContains(t, err, "impersonation", "error should name the impersonation step")
	require.ErrorContains(t, err, "403", "error should include the status code")
}

func TestGCPWIFSource_TokenSourceError(t *testing.T) {
	src := newWIFSource(t, &stubTokenSource{err: errors.New("boom")}, validWIFAudience, "", "http://unused.invalid", "")
	cred, err := src.Resolve(context.Background())
	require.NoError(t, err)

	_, err = cred.BearerToken(context.Background())
	require.ErrorContains(t, err, "boom", "error should wrap the underlying token source failure")
}

func TestGCPWIFSource_InvalidAudience(t *testing.T) {
	cases := []struct {
		name     string
		audience string
	}{
		{"plain string", "not-an-audience"},
		{"wrong host", "//example.com/projects/1/locations/global/workloadIdentityPools/p/providers/q"},
		{"missing pool segment", "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/providers/q"},
		{"non-numeric project", "//iam.googleapis.com/projects/abc/locations/global/workloadIdentityPools/p/providers/q"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := NewGCPWorkloadIdentityFederationSource(&stubTokenSource{token: []byte("x")}, tc.audience, "")
			_, err := src.Resolve(context.Background())
			require.ErrorContains(t, err, "audience", "error should name the audience field")
		})
	}
}

// TestGCPWIFSource_OIDCTokenForwardedToSTS pins the contract that
// whatever bytes the underlying TokenSource returns are forwarded
// verbatim as the STS subject_token. A subtle bug here would let
// tampered-with subject tokens slip past — assert the round-trip.
func TestGCPWIFSource_OIDCTokenForwardedToSTS(t *testing.T) {
	const weirdToken = "eyJ.fake-jwt.with-special-chars=+/&"
	var captured string
	sts := httptest.NewServer(stsHandler(t, "issued", 3600, func(req stsRequest) {
		captured = req.SubjectToken
	}))
	defer sts.Close()

	src := newWIFSource(t, &stubTokenSource{token: []byte(weirdToken)}, validWIFAudience, "", sts.URL, "")
	cred, err := src.Resolve(context.Background())
	require.NoError(t, err)

	_, err = cred.BearerToken(context.Background())
	require.NoError(t, err)
	require.Equal(t, weirdToken, captured, "OIDC bytes must round-trip verbatim")
}

func TestGCPWIFSource_NilTokenSource(t *testing.T) {
	src := NewGCPWorkloadIdentityFederationSource(nil, validWIFAudience, "")
	_, err := src.Resolve(context.Background())
	require.Error(t, err, "nil token source should fail Resolve")
}

func TestGCPWIFSource_EmptyOIDCToken(t *testing.T) {
	src := newWIFSource(t, &stubTokenSource{token: []byte{}}, validWIFAudience, "", "http://unused.invalid", "")
	cred, err := src.Resolve(context.Background())
	require.NoError(t, err)

	_, err = cred.BearerToken(context.Background())
	require.ErrorContains(t, err, "empty", "error should mention the empty token")
}

func TestGCPWIFSource_EmptySTSAccessToken(t *testing.T) {
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer sts.Close()

	src := newWIFSource(t, &stubTokenSource{token: []byte("oidc")}, validWIFAudience, "", sts.URL, "")
	cred, err := src.Resolve(context.Background())
	require.NoError(t, err)

	_, err = cred.BearerToken(context.Background())
	require.Error(t, err, "empty access_token should fail BearerToken")
}

// TestGCPWIFSource_STSMalformedJSON verifies a 200 response with
// non-JSON content produces a clear "parse STS response" error rather
// than a nil-pointer panic in the access-token check.
func TestGCPWIFSource_STSMalformedJSON(t *testing.T) {
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not json"))
	}))
	defer sts.Close()

	src := newWIFSource(t, &stubTokenSource{token: []byte("oidc")}, validWIFAudience, "", sts.URL, "")
	cred, err := src.Resolve(context.Background())
	require.NoError(t, err)

	_, err = cred.BearerToken(context.Background())
	require.ErrorContains(t, err, "parse STS response")
}

// TestGCPWIFSource_STSZeroExpiresIn verifies the 1-hour fallback when
// the STS response omits or zeroes expires_in; without it,
// oauth2.ReuseTokenSource would treat the token as already expired and
// re-hit STS on every adapter request.
func TestGCPWIFSource_STSZeroExpiresIn(t *testing.T) {
	var calls int32
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"sts-tok","expires_in":0,"token_type":"Bearer"}`))
	}))
	defer sts.Close()

	src := newWIFSource(t, &stubTokenSource{token: []byte("oidc")}, validWIFAudience, "", sts.URL, "")
	cred, err := src.Resolve(context.Background())
	require.NoError(t, err)

	tok1, err := cred.BearerToken(context.Background())
	require.NoError(t, err)
	require.Equal(t, "sts-tok", tok1)

	// The second call should be served from the cache, not STS, because
	// the 1-hour fallback gives ReuseTokenSource a non-zero expiry.
	tok2, err := cred.BearerToken(context.Background())
	require.NoError(t, err)
	require.Equal(t, "sts-tok", tok2)

	require.Equal(t, int32(1), atomic.LoadInt32(&calls), "cache should absorb the second call")
}

// TestGCPWIFSource_ImpersonationMalformedJSON is the IAM Credentials
// counterpart to TestGCPWIFSource_STSMalformedJSON; the error label
// distinguishes the two hops for operators triaging logs.
func TestGCPWIFSource_ImpersonationMalformedJSON(t *testing.T) {
	sts := httptest.NewServer(stsHandler(t, "fed-tok", 3600, nil))
	defer sts.Close()

	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not json"))
	}))
	defer iam.Close()

	src := newWIFSource(
		t,
		&stubTokenSource{token: []byte("oidc")},
		validWIFAudience,
		"sa@p.iam.gserviceaccount.com",
		sts.URL,
		iam.URL+"/v1/projects/-/serviceAccounts/%s:generateAccessToken",
	)
	cred, err := src.Resolve(context.Background())
	require.NoError(t, err)

	_, err = cred.BearerToken(context.Background())
	require.ErrorContains(t, err, "parse impersonation response")
}

// TestGCPWIFSource_ImpersonationEmptyAccessToken verifies a 200
// response that omits the access token surfaces as a federation error
// rather than yielding an empty bearer to the provider adapter.
func TestGCPWIFSource_ImpersonationEmptyAccessToken(t *testing.T) {
	sts := httptest.NewServer(stsHandler(t, "fed-tok", 3600, nil))
	defer sts.Close()

	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"","expireTime":"2030-01-01T00:00:00Z"}`))
	}))
	defer iam.Close()

	src := newWIFSource(
		t,
		&stubTokenSource{token: []byte("oidc")},
		validWIFAudience,
		"sa@p.iam.gserviceaccount.com",
		sts.URL,
		iam.URL+"/v1/projects/-/serviceAccounts/%s:generateAccessToken",
	)
	cred, err := src.Resolve(context.Background())
	require.NoError(t, err)

	_, err = cred.BearerToken(context.Background())
	require.ErrorContains(t, err, "empty accessToken")
}

// TestGCPWIFSource_ImpersonationMalformedExpireTime verifies a
// non-RFC3339 expireTime surfaces as a parse error rather than a
// fabricated expiry — a fabricated expiry can't be cached against, and
// refreshing on every request would burn through IAM quota.
func TestGCPWIFSource_ImpersonationMalformedExpireTime(t *testing.T) {
	sts := httptest.NewServer(stsHandler(t, "fed-tok", 3600, nil))
	defer sts.Close()

	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"tok","expireTime":"not-a-date"}`))
	}))
	defer iam.Close()

	src := newWIFSource(
		t,
		&stubTokenSource{token: []byte("oidc")},
		validWIFAudience,
		"sa@p.iam.gserviceaccount.com",
		sts.URL,
		iam.URL+"/v1/projects/-/serviceAccounts/%s:generateAccessToken",
	)
	cred, err := src.Resolve(context.Background())
	require.NoError(t, err)

	_, err = cred.BearerToken(context.Background())
	require.ErrorContains(t, err, "parse impersonation expireTime")
}

// TestTruncateForError_TruncatesLongBody guards the error-body cap:
// without it, a hostile STS endpoint streaming a 1 MiB error body
// would propagate the full payload through every error wrapper into
// slog and OTel span statuses.
func TestTruncateForError_TruncatesLongBody(t *testing.T) {
	// Trailing characters must be dropped, not the leading ones —
	// operators read from the start.
	long := make([]byte, stsErrorBodyLimit+128)
	for i := range long {
		long[i] = 'A'
	}

	got := truncateForError(long)

	tail := got[len(got)-min(20, len(got)):]
	require.True(t, strings.HasSuffix(got, "…"), "truncated output should end with an ellipsis, got tail: %q", tail)

	const ellipsisBytes = 3
	require.Len(t, got, stsErrorBodyLimit+ellipsisBytes, "truncated output should be the body cap plus the ellipsis")
}
