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
// request before responding.
func stsHandler(t *testing.T, accessToken string, expiresIn int64, verify func(stsRequest)) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("STS got %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("STS Content-Type = %q, want application/json", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("STS read body: %v", err)
		}
		var parsed stsRequest
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("STS unmarshal body: %v", err)
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
// a service-account access token.
func iamHandler(t *testing.T, expectedFederated, accessToken string, expireTime time.Time) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("IAM got %s, want POST", r.Method)
		}
		if got, want := r.Header.Get("Authorization"), "Bearer "+expectedFederated; got != want {
			t.Errorf("IAM Authorization = %q, want %q", got, want)
		}
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

	stsCalls := int32(0)
	sts := httptest.NewServer(stsHandler(t, stsToken, 3600, func(req stsRequest) {
		atomic.AddInt32(&stsCalls, 1)
		if req.Audience != validWIFAudience {
			t.Errorf("audience = %q, want %q", req.Audience, validWIFAudience)
		}
		if req.GrantType != "urn:ietf:params:oauth:grant-type:token-exchange" {
			t.Errorf("grantType = %q", req.GrantType)
		}
		if req.RequestedTokenType != "urn:ietf:params:oauth:token-type:access_token" {
			t.Errorf("requestedTokenType = %q", req.RequestedTokenType)
		}
		if req.SubjectTokenType != "urn:ietf:params:oauth:token-type:jwt" {
			t.Errorf("subjectTokenType = %q", req.SubjectTokenType)
		}
		if req.SubjectToken != oidcToken {
			t.Errorf("subjectToken = %q, want %q", req.SubjectToken, oidcToken)
		}
		if !strings.Contains(req.Scope, "cloud-platform") {
			t.Errorf("scope = %q, want cloud-platform", req.Scope)
		}
	}))
	defer sts.Close()

	src := newWIFSource(t, &stubTokenSource{token: []byte(oidcToken)}, validWIFAudience, "", sts.URL, "")

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
	if got != stsToken {
		t.Errorf("bearer = %q, want %q", got, stsToken)
	}
	if atomic.LoadInt32(&stsCalls) != 1 {
		t.Errorf("STS called %d times, want 1", stsCalls)
	}
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
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	got, err := cred.BearerToken(context.Background())
	if err != nil {
		t.Fatalf("BearerToken: %v", err)
	}
	if got != saToken {
		t.Errorf("bearer = %q, want %q (impersonated SA token)", got, saToken)
	}
}

// TestGCPWIFSource_RefreshUsesCache verifies that oauth2.ReuseTokenSource
// caches the federated token until expiry. The first BearerToken call
// must round-trip to STS exactly once; subsequent calls within the
// cached lifetime must NOT hit the wire.
func TestGCPWIFSource_RefreshUsesCache(t *testing.T) {
	stsCalls := int32(0)
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
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	for i := 0; i < 5; i++ {
		got, err := cred.BearerToken(context.Background())
		if err != nil {
			t.Fatalf("BearerToken call %d: %v", i, err)
		}
		if got != "cached-token" {
			t.Errorf("call %d: bearer = %q", i, got)
		}
	}

	if c := atomic.LoadInt32(&stsCalls); c != 1 {
		t.Errorf("after 5 BearerToken calls, STS hit count = %d, want 1 (ReuseTokenSource should cache)", c)
	}
}

func TestGCPWIFSource_STSError(t *testing.T) {
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"audience not configured"}`))
	}))
	defer sts.Close()

	src := newWIFSource(t, &stubTokenSource{token: []byte("oidc")}, validWIFAudience, "", sts.URL, "")
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	_, err = cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected STS error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "STS returned 400") {
		t.Errorf("error should name status code, got: %v", err)
	}
	if !strings.Contains(msg, "audience not configured") {
		t.Errorf("error should include body excerpt, got: %v", err)
	}
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
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	_, err = cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected impersonation error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "impersonation") {
		t.Errorf("error should name impersonation step, got: %v", err)
	}
	if !strings.Contains(msg, "403") {
		t.Errorf("error should include status code, got: %v", err)
	}
}

func TestGCPWIFSource_TokenSourceError(t *testing.T) {
	src := newWIFSource(t, &stubTokenSource{err: errors.New("boom")}, validWIFAudience, "", "http://unused.invalid", "")
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, err = cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected token source error, got nil")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should wrap underlying token source failure, got: %v", err)
	}
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
			if err == nil {
				t.Fatalf("expected error for audience %q, got nil", tc.audience)
			}
			if !strings.Contains(err.Error(), "audience") {
				t.Errorf("error should name the audience field, got: %v", err)
			}
		})
	}
}

// TestGCPWIFSource_OIDCTokenForwardedToSTS pins the contract that
// whatever bytes the underlying TokenSource returns are forwarded
// verbatim as the STS subject_token. A subtle bug here would let
// tampered-with subject tokens slip past — assert the round-trip.
func TestGCPWIFSource_OIDCTokenForwardedToSTS(t *testing.T) {
	weirdToken := []byte("eyJ.fake-jwt.with-special-chars=+/&")
	var captured string
	sts := httptest.NewServer(stsHandler(t, "issued", 3600, func(req stsRequest) {
		captured = req.SubjectToken
	}))
	defer sts.Close()

	src := newWIFSource(t, &stubTokenSource{token: weirdToken}, validWIFAudience, "", sts.URL, "")
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := cred.BearerToken(context.Background()); err != nil {
		t.Fatalf("BearerToken: %v", err)
	}
	if captured != string(weirdToken) {
		t.Errorf("STS subjectToken = %q, want %q (OIDC bytes must round-trip verbatim)", captured, string(weirdToken))
	}
}

func TestGCPWIFSource_NilTokenSource(t *testing.T) {
	src := NewGCPWorkloadIdentityFederationSource(nil, validWIFAudience, "")
	_, err := src.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected error for nil token source, got nil")
	}
}

func TestGCPWIFSource_EmptyOIDCToken(t *testing.T) {
	src := newWIFSource(t, &stubTokenSource{token: []byte{}}, validWIFAudience, "", "http://unused.invalid", "")
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, err = cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected error for empty OIDC token, got nil")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should mention empty token, got: %v", err)
	}
}

func TestGCPWIFSource_EmptySTSAccessToken(t *testing.T) {
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer sts.Close()

	src := newWIFSource(t, &stubTokenSource{token: []byte("oidc")}, validWIFAudience, "", sts.URL, "")
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, err = cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected error for empty access_token, got nil")
	}
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
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, err = cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected error for malformed STS JSON")
	}
	if !strings.Contains(err.Error(), "parse STS response") {
		t.Errorf("error should mention parse STS response, got: %v", err)
	}
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
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	tok1, err := cred.BearerToken(context.Background())
	if err != nil {
		t.Fatalf("BearerToken (first): %v", err)
	}
	if tok1 != "sts-tok" {
		t.Errorf("token = %q, want sts-tok", tok1)
	}

	// Second call should hit the cache, not STS, because the 1-hour
	// fallback gives ReuseTokenSource a non-zero expiry to inspect.
	tok2, err := cred.BearerToken(context.Background())
	if err != nil {
		t.Fatalf("BearerToken (second): %v", err)
	}
	if tok2 != "sts-tok" {
		t.Errorf("second token = %q, want sts-tok", tok2)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("STS hit %d times, want 1 (cache should absorb the second call)", got)
	}
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
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, err = cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected error for malformed impersonation JSON")
	}
	if !strings.Contains(err.Error(), "parse impersonation response") {
		t.Errorf("error should mention parse impersonation response, got: %v", err)
	}
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
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, err = cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected error for empty impersonation accessToken")
	}
	if !strings.Contains(err.Error(), "empty accessToken") {
		t.Errorf("error should mention empty accessToken, got: %v", err)
	}
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
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, err = cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected error for malformed expireTime")
	}
	if !strings.Contains(err.Error(), "parse impersonation expireTime") {
		t.Errorf("error should mention parse impersonation expireTime, got: %v", err)
	}
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

	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated output should end with ellipsis, got: %q (...)", got[len(got)-min(20, len(got)):])
	}
	const ellipsisBytes = 3
	wantBytes := stsErrorBodyLimit + ellipsisBytes
	if len(got) != wantBytes {
		t.Errorf("truncated length = %d bytes, want %d (cap %d + ellipsis %d)", len(got), wantBytes, stsErrorBodyLimit, ellipsisBytes)
	}
}
