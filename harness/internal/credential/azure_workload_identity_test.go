package credential

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// parseForm wraps url.ParseQuery so test handlers can validate the
// form-encoded request body Microsoft Entra expects.
func parseForm(body string) (url.Values, error) {
	return url.ParseQuery(body)
}

const (
	testAzureTenantID = "00000000-1111-2222-3333-444444444444"
	testAzureClientID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

// newAzureSource is the test-side counterpart to
// NewAzureWorkloadIdentitySource: it patches tokenURL so requests
// land at an httptest server instead of login.microsoftonline.com.
func newAzureSource(t *testing.T, ts TokenSource, scope, tokenURL string) *AzureWorkloadIdentitySource {
	t.Helper()
	src := NewAzureWorkloadIdentitySource(ts, testAzureTenantID, testAzureClientID, scope)
	src.tokenURL = tokenURL
	return src
}

// rotatingTokenSource yields a different token byte slice on each
// Token() call, so refresh-rotation tests can prove the JWT is re-read
// rather than cached at construction.
type rotatingTokenSource struct {
	tokens [][]byte
	mu     sync.Mutex
	idx    int
	calls  int32
}

func (r *rotatingTokenSource) Token(_ context.Context) ([]byte, error) {
	atomic.AddInt32(&r.calls, 1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.idx >= len(r.tokens) {
		return r.tokens[len(r.tokens)-1], nil
	}
	tok := r.tokens[r.idx]
	r.idx++
	return tok, nil
}

func TestAzureWIFSource_SuccessExchange(t *testing.T) {
	const jwt = "fake.oidc.jwt"
	const accessToken = "azure-access-token"

	var capturedForm url.Values
	var capturedCT, capturedAccept string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("got method %s, want POST", r.Method)
		}
		capturedCT = r.Header.Get("Content-Type")
		capturedAccept = r.Header.Get("Accept")

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		parsed, err := parseForm(string(body))
		if err != nil {
			t.Fatalf("parse form: %v", err)
		}
		capturedForm = parsed

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(azureTokenResponse{
			AccessToken: accessToken,
			TokenType:   "Bearer",
			ExpiresIn:   3599,
		})
	}))
	defer srv.Close()

	src := newAzureSource(t, &stubTokenSource{token: []byte(jwt)}, "", srv.URL)
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

	if capturedCT != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", capturedCT)
	}
	if capturedAccept != "application/json" {
		t.Errorf("Accept = %q, want application/json", capturedAccept)
	}

	// Assert all five required fields with their expected values.
	checks := map[string]string{
		"grant_type":            "client_credentials",
		"client_id":             testAzureClientID,
		"client_assertion_type": "urn:ietf:params:oauth:client-assertion-type:jwt-bearer",
		"client_assertion":      jwt,
		"scope":                 azureDefaultScope,
	}
	for k, want := range checks {
		if got := capturedForm.Get(k); got != want {
			t.Errorf("form[%q] = %q, want %q", k, got, want)
		}
	}
}

func TestAzureWIFSource_DefaultScope(t *testing.T) {
	var capturedScope string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		parsed, _ := parseForm(string(body))
		capturedScope = parsed.Get("scope")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(azureTokenResponse{
			AccessToken: "tok", TokenType: "Bearer", ExpiresIn: 3600,
		})
	}))
	defer srv.Close()

	// Empty scope must default to azureDefaultScope.
	src := newAzureSource(t, &stubTokenSource{token: []byte("jwt")}, "", srv.URL)
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := cred.BearerToken(context.Background()); err != nil {
		t.Fatalf("BearerToken: %v", err)
	}

	if capturedScope != azureDefaultScope {
		t.Errorf("scope = %q, want %q (default)", capturedScope, azureDefaultScope)
	}
}

func TestAzureWIFSource_CustomScope(t *testing.T) {
	const customScope = "https://management.azure.com/.default"
	var capturedScope string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		parsed, _ := parseForm(string(body))
		capturedScope = parsed.Get("scope")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(azureTokenResponse{
			AccessToken: "tok", TokenType: "Bearer", ExpiresIn: 3600,
		})
	}))
	defer srv.Close()

	src := newAzureSource(t, &stubTokenSource{token: []byte("jwt")}, customScope, srv.URL)
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := cred.BearerToken(context.Background()); err != nil {
		t.Fatalf("BearerToken: %v", err)
	}

	if capturedScope != customScope {
		t.Errorf("scope = %q, want %q (custom)", capturedScope, customScope)
	}
}

// TestAzureWIFSource_RefreshUsesCache verifies that
// oauth2.ReuseTokenSource caches the access token until expiry: a
// non-zero expires_in must keep the second BearerToken call from
// hitting the wire.
func TestAzureWIFSource_RefreshUsesCache(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(azureTokenResponse{
			AccessToken: "cached", TokenType: "Bearer", ExpiresIn: 3600,
		})
	}))
	defer srv.Close()

	src := newAzureSource(t, &stubTokenSource{token: []byte("jwt")}, "", srv.URL)
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	for i := 0; i < 5; i++ {
		got, err := cred.BearerToken(context.Background())
		if err != nil {
			t.Fatalf("BearerToken call %d: %v", i, err)
		}
		if got != "cached" {
			t.Errorf("call %d: bearer = %q", i, got)
		}
	}
	if c := atomic.LoadInt32(&calls); c != 1 {
		t.Errorf("after 5 BearerToken calls, Entra hit count = %d, want 1 (cache should absorb)", c)
	}
}

// TestAzureWIFSource_ZeroExpiresInFallback exercises the documented
// 1-hour fallback when expires_in is missing or zero. Without it
// ReuseTokenSource would treat each issued token as already expired
// and burn through Entra's rate limits.
func TestAzureWIFSource_ZeroExpiresInFallback(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":0}`))
	}))
	defer srv.Close()

	src := newAzureSource(t, &stubTokenSource{token: []byte("jwt")}, "", srv.URL)
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// First call hits the server.
	if _, err := cred.BearerToken(context.Background()); err != nil {
		t.Fatalf("BearerToken (first): %v", err)
	}
	// Second call within the 1-hour fallback window should hit cache.
	if _, err := cred.BearerToken(context.Background()); err != nil {
		t.Fatalf("BearerToken (second): %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("Entra hit %d times, want 1 (1-hour fallback should absorb second call)", got)
	}
}

// TestAzureWIFSource_SingleFlightUnderConcurrency proves that 10
// concurrent BearerToken callers see exactly one Entra exchange.
// oauth2.ReuseTokenSource provides the serialisation; this test
// guards against a future refactor that strips the wrapper.
func TestAzureWIFSource_SingleFlightUnderConcurrency(t *testing.T) {
	var calls int32
	// Do not block the handler — we just want to count how often it
	// is invoked. ReuseTokenSource's mutex elides duplicate calls
	// before they reach the network.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(azureTokenResponse{
			AccessToken: "tok", TokenType: "Bearer", ExpiresIn: 3600,
		})
	}))
	defer srv.Close()

	src := newAzureSource(t, &stubTokenSource{token: []byte("jwt")}, "", srv.URL)
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	start := make(chan struct{})
	errs := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start
			if _, err := cred.BearerToken(context.Background()); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent BearerToken: %v", err)
	}

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("under %d concurrent callers, Entra hit count = %d, want 1", goroutines, got)
	}
}

// TestAzureWIFSource_JWTReReadBetweenExchanges proves the JWT is
// fetched fresh on each refresh, not cached at construction. With
// expires_in=0 the 1-hour fallback caches the issued access token,
// but a fresh Resolve forces a new exchange — and the second
// exchange must use the second JWT yielded by the rotating source.
func TestAzureWIFSource_JWTReReadBetweenExchanges(t *testing.T) {
	var capturedAssertions []string
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		parsed, _ := parseForm(string(body))
		mu.Lock()
		capturedAssertions = append(capturedAssertions, parsed.Get("client_assertion"))
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(azureTokenResponse{
			AccessToken: "tok", TokenType: "Bearer", ExpiresIn: 3600,
		})
	}))
	defer srv.Close()

	rot := &rotatingTokenSource{tokens: [][]byte{[]byte("jwt-1"), []byte("jwt-2")}}

	// First exchange, first source.
	src1 := newAzureSource(t, rot, "", srv.URL)
	cred1, err := src1.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve #1: %v", err)
	}
	if _, err := cred1.BearerToken(context.Background()); err != nil {
		t.Fatalf("BearerToken #1: %v", err)
	}

	// Second exchange via a fresh ReuseTokenSource wrapper. The
	// rotating source yields a different JWT this time; the wire
	// must reflect that.
	src2 := newAzureSource(t, rot, "", srv.URL)
	cred2, err := src2.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	if _, err := cred2.BearerToken(context.Background()); err != nil {
		t.Fatalf("BearerToken #2: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(capturedAssertions) != 2 {
		t.Fatalf("assertions captured = %d, want 2", len(capturedAssertions))
	}
	if capturedAssertions[0] != "jwt-1" {
		t.Errorf("first assertion = %q, want jwt-1", capturedAssertions[0])
	}
	if capturedAssertions[1] != "jwt-2" {
		t.Errorf("second assertion = %q, want jwt-2 (JWT must be re-read between exchanges)", capturedAssertions[1])
	}
}

func TestAzureWIFSource_HTTPErrorWithCorrelationID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"AADSTS70021: Token assertion expired","correlation_id":"abc-123-def"}`))
	}))
	defer srv.Close()

	src := newAzureSource(t, &stubTokenSource{token: []byte("jwt")}, "", srv.URL)
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, err = cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "401") {
		t.Errorf("error should name status code, got: %v", err)
	}
	if !strings.Contains(msg, "correlation_id=abc-123-def") {
		t.Errorf("error should surface correlation_id, got: %v", err)
	}
	if !strings.Contains(msg, "AADSTS70021") {
		t.Errorf("error should include body excerpt, got: %v", err)
	}
	// Defence against regression: the JWT must NEVER appear in error output.
	if strings.Contains(msg, "jwt") {
		t.Errorf("error must not leak the subject JWT, got: %v", err)
	}
}

// TestAzureWIFSource_HTTPErrorWithCorrelationIDAlt covers the
// camelCase variant Microsoft sometimes returns for the same field.
func TestAzureWIFSource_HTTPErrorWithCorrelationIDAlt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_request","correlationId":"camel-cased-id"}`))
	}))
	defer srv.Close()

	src := newAzureSource(t, &stubTokenSource{token: []byte("jwt")}, "", srv.URL)
	cred, _ := src.Resolve(context.Background())
	_, err := cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "correlation_id=camel-cased-id") {
		t.Errorf("error should surface camelCase correlationId, got: %v", err)
	}
}

func TestAzureWIFSource_HTTPErrorWithoutCorrelationID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_request","error_description":"foo"}`))
	}))
	defer srv.Close()

	src := newAzureSource(t, &stubTokenSource{token: []byte("jwt")}, "", srv.URL)
	cred, _ := src.Resolve(context.Background())
	_, err := cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "400") {
		t.Errorf("error should name status code, got: %v", err)
	}
	if !strings.Contains(msg, "invalid_request") {
		t.Errorf("error should include body excerpt, got: %v", err)
	}
	if strings.Contains(msg, "correlation_id=") {
		t.Errorf("error must not synthesise a correlation_id when absent, got: %v", err)
	}
}

// TestAzureWIFSource_HTTPErrorNonJSONBody guards correlationIDSuffix
// against panicking on non-JSON bodies (e.g. a fronting proxy that
// returns text/plain on 5xx).
func TestAzureWIFSource_HTTPErrorNonJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("Service Unavailable"))
	}))
	defer srv.Close()

	src := newAzureSource(t, &stubTokenSource{token: []byte("jwt")}, "", srv.URL)
	cred, _ := src.Resolve(context.Background())
	_, err := cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "503") {
		t.Errorf("error should include status code, got: %v", err)
	}
	if !strings.Contains(msg, "Service Unavailable") {
		t.Errorf("error should include body excerpt, got: %v", err)
	}
	if strings.Contains(msg, "correlation_id=") {
		t.Errorf("non-JSON body must not surface a correlation_id, got: %v", err)
	}
}

func TestAzureWIFSource_NilTokenSource(t *testing.T) {
	src := NewAzureWorkloadIdentitySource(nil, testAzureTenantID, testAzureClientID, "")
	_, err := src.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected error for nil token source, got nil")
	}
	if !strings.Contains(err.Error(), "token source") {
		t.Errorf("error should mention token source, got: %v", err)
	}
}

func TestAzureWIFSource_EmptyTenantID(t *testing.T) {
	src := NewAzureWorkloadIdentitySource(&stubTokenSource{token: []byte("jwt")}, "", testAzureClientID, "")
	_, err := src.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected error for empty tenantID, got nil")
	}
	if !strings.Contains(err.Error(), "tenantID") {
		t.Errorf("error should mention tenantID, got: %v", err)
	}
}

func TestAzureWIFSource_EmptyClientID(t *testing.T) {
	src := NewAzureWorkloadIdentitySource(&stubTokenSource{token: []byte("jwt")}, testAzureTenantID, "", "")
	_, err := src.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected error for empty clientID, got nil")
	}
	if !strings.Contains(err.Error(), "clientID") {
		t.Errorf("error should mention clientID, got: %v", err)
	}
}

func TestAzureWIFSource_TokenSourceReturnsEmpty(t *testing.T) {
	// httptest server isn't reached because the empty-JWT guard fires
	// before the HTTP call; an unreachable URL keeps the test honest.
	src := newAzureSource(t, &stubTokenSource{token: []byte{}}, "", "http://unused.invalid")
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, err = cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected error for empty subject token, got nil")
	}
	if !strings.Contains(err.Error(), "empty subject token") {
		t.Errorf("error should mention empty subject token, got: %v", err)
	}
}

func TestAzureWIFSource_TokenSourceErrorPropagates(t *testing.T) {
	src := newAzureSource(t, &stubTokenSource{err: errors.New("boom")}, "", "http://unused.invalid")
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

func TestAzureWIFSource_MalformedJSONSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not json`))
	}))
	defer srv.Close()

	src := newAzureSource(t, &stubTokenSource{token: []byte("jwt")}, "", srv.URL)
	cred, _ := src.Resolve(context.Background())
	_, err := cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse token response") {
		t.Errorf("error should mention parse token response, got: %v", err)
	}
}

func TestAzureWIFSource_EmptyAccessToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token_type":"Bearer","expires_in":3600}`))
	}))
	defer srv.Close()

	src := newAzureSource(t, &stubTokenSource{token: []byte("jwt")}, "", srv.URL)
	cred, _ := src.Resolve(context.Background())
	_, err := cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected error for empty access_token, got nil")
	}
	if !strings.Contains(err.Error(), "empty access_token") {
		t.Errorf("error should mention empty access_token, got: %v", err)
	}
}

// TestAzureWIFSource_ErrorBodyTruncated exercises the shared
// truncateForError cap on the error path. A 4 KiB body must be
// trimmed to 1 KiB + ellipsis when surfaced through the wrapped
// error — without the cap, hostile endpoints could push large
// payloads through every error handler into slog and OTel.
func TestAzureWIFSource_ErrorBodyTruncated(t *testing.T) {
	bigBody := strings.Repeat("X", stsErrorBodyLimit*4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(bigBody))
	}))
	defer srv.Close()

	src := newAzureSource(t, &stubTokenSource{token: []byte("jwt")}, "", srv.URL)
	cred, _ := src.Resolve(context.Background())
	_, err := cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "…") {
		t.Errorf("error excerpt should be truncated with ellipsis, got: %q", lastN(msg, 40))
	}
	// The full bigBody (4 KiB of X) must NOT appear verbatim — that
	// would mean the cap was bypassed.
	if strings.Contains(msg, bigBody) {
		t.Errorf("error must not include the full %d-byte body verbatim", len(bigBody))
	}
}

// TestAzureWIFSource_SuccessBodyLimitEnforced exercises the 64 KiB
// LimitReader on the success path. A 200 KiB body must be capped at
// stsResponseLimit before json.Unmarshal sees it; with the cap, a
// payload large enough to overflow the limiter cuts the JSON object
// off mid-string and surfaces as a parse error rather than driving
// allocator pressure.
func TestAzureWIFSource_SuccessBodyLimitEnforced(t *testing.T) {
	// Build a 200 KiB JSON object whose `access_token` value alone
	// is larger than the read cap. After truncation the JSON is
	// invalid, so json.Unmarshal must fail — which proves the
	// LimitReader is in effect (without it the parse would succeed
	// and we'd return a 200 KiB access token).
	huge := strings.Repeat("A", stsResponseLimit*4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":"%s","token_type":"Bearer","expires_in":3600}`, huge)
	}))
	defer srv.Close()

	src := newAzureSource(t, &stubTokenSource{token: []byte("jwt")}, "", srv.URL)
	cred, _ := src.Resolve(context.Background())
	_, err := cred.BearerToken(context.Background())
	if err == nil {
		t.Fatal("expected parse error from truncated oversized body, got nil")
	}
	if !strings.Contains(err.Error(), "parse token response") {
		t.Errorf("error should be a parse failure, got: %v", err)
	}
}

// lastN returns the last n characters of s; helper for diagnostics.
func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// TestAzureWIFSource_CustomTokenURL exercises the sovereign-cloud
// override path: when a non-empty tokenURLOverride is supplied to the
// constructor, the exchange must POST to that URL instead of the
// global-cloud authority at login.microsoftonline.com. The override
// path is the only way Azure Government / China / Germany workloads
// reach their authority — without coverage here, a regression that
// silently re-derives the global URL would break sovereign-cloud
// deployments without a single failing test.
func TestAzureWIFSource_CustomTokenURL(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(azureTokenResponse{
			AccessToken: "tok", TokenType: "Bearer", ExpiresIn: 3600,
		})
	}))
	defer srv.Close()

	// Construct directly (not via the test helper, which patches
	// tokenURL after the fact) so the variadic override path is
	// exercised end-to-end.
	src := NewAzureWorkloadIdentitySource(
		&stubTokenSource{token: []byte("jwt")},
		testAzureTenantID,
		testAzureClientID,
		"",
		srv.URL,
	)

	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := cred.BearerToken(context.Background()); err != nil {
		t.Fatalf("BearerToken: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("override URL hit %d times, want 1", got)
	}

	// Defensive cross-check: src.tokenURL must be the override, not a
	// derived login.microsoftonline.com URL.
	if src.tokenURL != srv.URL {
		t.Errorf("tokenURL = %q, want %q (override should win over global default)", src.tokenURL, srv.URL)
	}
}

// TestAzureWIFSource_EmptyTokenURLOverrideFallsBack confirms that an
// empty override leaves the constructor at its default global-cloud
// URL. This pins the variadic-arg semantics: empty means "use the
// default" rather than "use the empty string".
func TestAzureWIFSource_EmptyTokenURLOverrideFallsBack(t *testing.T) {
	src := NewAzureWorkloadIdentitySource(
		&stubTokenSource{token: []byte("jwt")},
		testAzureTenantID,
		testAzureClientID,
		"",
		"",
	)

	want := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", testAzureTenantID)
	if src.tokenURL != want {
		t.Errorf("tokenURL = %q, want %q (empty override must fall back to global default)", src.tokenURL, want)
	}
}

// TestAzureWIFSource_CorrelationIDSanitised guards correlationIDSuffix
// against a hostile / malfunctioning Entra-shaped endpoint that returns
// a correlation_id containing ANSI escape sequences, control bytes, or
// an oversized payload. These values would otherwise land verbatim in
// slog attributes, OTel span events, and the terminal — a scrubber
// further downstream would have to know to clean them up. Since the
// correlation_id is end-user-rendered (operators paste it into
// Microsoft support tickets), bounding it at the source is the
// cheapest fix.
func TestAzureWIFSource_CorrelationIDSanitised(t *testing.T) {
	t.Run("ANSI escape and control bytes stripped", func(t *testing.T) {
		// A real-world hostile body: a 401 with an "id" containing an
		// ANSI red sequence (ESC + [31m), an embedded carriage return,
		// and a NUL byte. Use json.Marshal so the wire-format escapes
		// are correct (a hand-typed raw string literal can't carry the
		// 0x1b control byte through the JSON parser).
		raw := "abc\x1b[31m\rde\x00f"
		jsonBody, err := json.Marshal(map[string]string{
			"error":          "x",
			"correlation_id": raw,
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		got := correlationIDSuffix(jsonBody)
		// Bytes < 0x20 (ESC, CR, NUL) must all be dropped. The "[31m"
		// printable tail of the ANSI sequence is permitted because in
		// isolation it's just brackets and digits — the danger is the
		// ESC byte that turns it into a control sequence.
		want := " (correlation_id=abc[31mdef)"
		if got != want {
			t.Errorf("correlationIDSuffix = %q, want %q", got, want)
		}
	})

	t.Run("oversized correlation_id truncated to 64 bytes", func(t *testing.T) {
		long := strings.Repeat("a", 200)
		body := []byte(fmt.Sprintf(`{"correlation_id":%q}`, long))
		got := correlationIDSuffix(body)
		want := " (correlation_id=" + strings.Repeat("a", 64) + ")"
		if got != want {
			t.Errorf("correlationIDSuffix length-cap failed:\n got: %q\nwant: %q", got, want)
		}
	})

	t.Run("normal UUID passes through unchanged", func(t *testing.T) {
		body := []byte(`{"correlation_id":"7c14b6a4-9d28-4a3a-b3a3-1234567890ab"}`)
		got := correlationIDSuffix(body)
		want := " (correlation_id=7c14b6a4-9d28-4a3a-b3a3-1234567890ab)"
		if got != want {
			t.Errorf("correlationIDSuffix = %q, want %q", got, want)
		}
	})
}
