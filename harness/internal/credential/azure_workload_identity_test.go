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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// newAzureSource patches tokenURL so requests land at an httptest
// server instead of login.microsoftonline.com.
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
	check := assert.New(t)
	must := require.New(t)
	const jwt = "fake.oidc.jwt"
	const accessToken = "azure-access-token"

	var capturedForm url.Values
	var capturedCT, capturedAccept string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check.Equal(http.MethodPost, r.Method)
		capturedCT = r.Header.Get("Content-Type")
		capturedAccept = r.Header.Get("Accept")

		body, err := io.ReadAll(r.Body)
		check.NoError(err, "read request body")
		parsed, err := parseForm(string(body))
		check.NoError(err, "parse form body")
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
	must.NoError(err)
	must.NotNil(cred.BearerToken, "Resolve should return a BearerToken closure")

	got, err := cred.BearerToken(context.Background())
	must.NoError(err)
	must.Equal(accessToken, got)

	must.Equal("application/x-www-form-urlencoded", capturedCT)
	must.Equal("application/json", capturedAccept)

	wantForm := map[string]string{
		"grant_type":            "client_credentials",
		"client_id":             testAzureClientID,
		"client_assertion_type": "urn:ietf:params:oauth:client-assertion-type:jwt-bearer",
		"client_assertion":      jwt,
		"scope":                 azureDefaultScope,
	}
	for key, want := range wantForm {
		t.Run(key, func(t *testing.T) {
			must := require.New(t)
			must.Equal(want, capturedForm.Get(key))
		})
	}
}

func TestAzureWIFSource_DefaultScope(t *testing.T) {
	must := require.New(t)
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

	src := newAzureSource(t, &stubTokenSource{token: []byte("jwt")}, "", srv.URL)
	cred, err := src.Resolve(context.Background())
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.NoError(err)

	must.Equal(azureDefaultScope, capturedScope)
}

func TestAzureWIFSource_CustomScope(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.NoError(err)

	must.Equal(customScope, capturedScope)
}

// Verifies oauth2.ReuseTokenSource caches the access token until expiry.
func TestAzureWIFSource_RefreshUsesCache(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)

	for i := 0; i < 5; i++ {
		got, err := cred.BearerToken(context.Background())
		must.NoError(err, "BearerToken call %d", i)
		must.Equal("cached", got, "BearerToken call %d", i)
	}
	must.Equal(int32(1), atomic.LoadInt32(&calls), "cache should absorb repeated BearerToken calls")
}

// Exercises the 1-hour fallback when expires_in is missing or zero.
func TestAzureWIFSource_ZeroExpiresInFallback(t *testing.T) {
	must := require.New(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":0}`))
	}))
	defer srv.Close()

	src := newAzureSource(t, &stubTokenSource{token: []byte("jwt")}, "", srv.URL)
	cred, err := src.Resolve(context.Background())
	must.NoError(err)

	_, err = cred.BearerToken(context.Background())
	must.NoError(err, "first BearerToken call")
	_, err = cred.BearerToken(context.Background())
	must.NoError(err, "second BearerToken call")

	must.Equal(int32(1), atomic.LoadInt32(&calls), "1-hour fallback should absorb the second call")
}

// Proves 10 concurrent BearerToken callers see exactly one Entra
// exchange, guarding against a future refactor that drops
// oauth2.ReuseTokenSource's serialisation.
func TestAzureWIFSource_SingleFlightUnderConcurrency(t *testing.T) {
	must := require.New(t)
	var calls int32

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
	must.NoError(err)

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
		must.NoError(err, "concurrent BearerToken")
	}

	must.Equal(int32(1), atomic.LoadInt32(&calls), "%d concurrent callers should share one Entra exchange", goroutines)
}

// Proves the JWT is fetched fresh on each refresh: a fresh Resolve
// forces a new exchange, which must use the second JWT the rotating
// source yields.
func TestAzureWIFSource_RereadsJWTBetweenExchanges(t *testing.T) {
	must := require.New(t)
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

	src1 := newAzureSource(t, rot, "", srv.URL)
	cred1, err := src1.Resolve(context.Background())
	must.NoError(err)
	_, err = cred1.BearerToken(context.Background())
	must.NoError(err)

	src2 := newAzureSource(t, rot, "", srv.URL)
	cred2, err := src2.Resolve(context.Background())
	must.NoError(err)
	_, err = cred2.BearerToken(context.Background())
	must.NoError(err)

	mu.Lock()
	defer mu.Unlock()
	must.Equal([]string{"jwt-1", "jwt-2"}, capturedAssertions, "JWT must be re-read between exchanges")
}

func TestAzureWIFSource_HTTPErrorSurfacesCorrelationID(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"AADSTS70021: Token assertion expired","correlation_id":"abc-123-def"}`))
	}))
	defer srv.Close()

	src := newAzureSource(t, &stubTokenSource{token: []byte("jwt")}, "", srv.URL)
	cred, err := src.Resolve(context.Background())
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.ErrorContains(err, "401")
	must.ErrorContains(err, "correlation_id=abc-123-def")
	must.ErrorContains(err, "AADSTS70021")
	must.NotContains(err.Error(), "jwt", "error must not leak the subject JWT")
}

// Covers the camelCase correlationId variant Microsoft sometimes returns.
func TestAzureWIFSource_HTTPErrorCamelCaseCorrelationID(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_request","correlationId":"camel-cased-id"}`))
	}))
	defer srv.Close()

	src := newAzureSource(t, &stubTokenSource{token: []byte("jwt")}, "", srv.URL)
	cred, _ := src.Resolve(context.Background())
	_, err := cred.BearerToken(context.Background())
	must.ErrorContains(err, "correlation_id=camel-cased-id")
}

func TestAzureWIFSource_HTTPErrorOmitsAbsentCorrelationID(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_request","error_description":"foo"}`))
	}))
	defer srv.Close()

	src := newAzureSource(t, &stubTokenSource{token: []byte("jwt")}, "", srv.URL)
	cred, _ := src.Resolve(context.Background())
	_, err := cred.BearerToken(context.Background())
	must.ErrorContains(err, "400")
	must.ErrorContains(err, "invalid_request")
	must.NotContains(err.Error(), "correlation_id=", "must not synthesise a correlation_id when absent")
}

// Guards correlationIDSuffix against panicking on non-JSON bodies.
func TestAzureWIFSource_HTTPErrorNonJSONBody(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("Service Unavailable"))
	}))
	defer srv.Close()

	src := newAzureSource(t, &stubTokenSource{token: []byte("jwt")}, "", srv.URL)
	cred, _ := src.Resolve(context.Background())
	_, err := cred.BearerToken(context.Background())
	must.ErrorContains(err, "503")
	must.ErrorContains(err, "Service Unavailable")
	must.NotContains(err.Error(), "correlation_id=", "non-JSON body must not surface a correlation_id")
}

func TestAzureWIFSource_NilTokenSource(t *testing.T) {
	must := require.New(t)
	src := NewAzureWorkloadIdentitySource(nil, testAzureTenantID, testAzureClientID, "")
	_, err := src.Resolve(context.Background())
	must.ErrorContains(err, "token source")
}

func TestAzureWIFSource_EmptyTenantID(t *testing.T) {
	must := require.New(t)
	src := NewAzureWorkloadIdentitySource(&stubTokenSource{token: []byte("jwt")}, "", testAzureClientID, "")
	_, err := src.Resolve(context.Background())
	must.ErrorContains(err, "tenantID")
}

func TestAzureWIFSource_EmptyClientID(t *testing.T) {
	must := require.New(t)
	src := NewAzureWorkloadIdentitySource(&stubTokenSource{token: []byte("jwt")}, testAzureTenantID, "", "")
	_, err := src.Resolve(context.Background())
	must.ErrorContains(err, "clientID")
}

func TestAzureWIFSource_EmptySubjectTokenRejected(t *testing.T) {
	must := require.New(t)
	src := newAzureSource(t, &stubTokenSource{token: []byte{}}, "", "http://unused.invalid")
	cred, err := src.Resolve(context.Background())
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.ErrorContains(err, "empty subject token")
}

func TestAzureWIFSource_TokenSourceErrorPropagates(t *testing.T) {
	must := require.New(t)
	src := newAzureSource(t, &stubTokenSource{err: errors.New("boom")}, "", "http://unused.invalid")
	cred, err := src.Resolve(context.Background())
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.ErrorContains(err, "boom", "error should wrap the underlying token source failure")
}

func TestAzureWIFSource_MalformedJSONBodyFails(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not json`))
	}))
	defer srv.Close()

	src := newAzureSource(t, &stubTokenSource{token: []byte("jwt")}, "", srv.URL)
	cred, _ := src.Resolve(context.Background())
	_, err := cred.BearerToken(context.Background())
	must.ErrorContains(err, "parse token response")
}

func TestAzureWIFSource_EmptyAccessToken(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token_type":"Bearer","expires_in":3600}`))
	}))
	defer srv.Close()

	src := newAzureSource(t, &stubTokenSource{token: []byte("jwt")}, "", srv.URL)
	cred, _ := src.Resolve(context.Background())
	_, err := cred.BearerToken(context.Background())
	must.ErrorContains(err, "empty access_token")
}

// Exercises the shared truncateForError cap on the error path.
func TestAzureWIFSource_ErrorBodyTruncated(t *testing.T) {
	must := require.New(t)
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
	must.Error(err)
	msg := err.Error()
	must.Contains(msg, "…", "error excerpt should be truncated with an ellipsis")
	must.NotContains(msg, bigBody, "error must not include the full %d-byte body verbatim", len(bigBody))
}

// Exercises the LimitReader on the success path: a body larger than
// stsResponseLimit is truncated mid-JSON, so json.Unmarshal must fail
// rather than the parse silently succeeding with a truncated token.
func TestAzureWIFSource_SuccessBodyLimitEnforced(t *testing.T) {
	must := require.New(t)
	huge := strings.Repeat("A", stsResponseLimit*4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":"%s","token_type":"Bearer","expires_in":3600}`, huge)
	}))
	defer srv.Close()

	src := newAzureSource(t, &stubTokenSource{token: []byte("jwt")}, "", srv.URL)
	cred, _ := src.Resolve(context.Background())
	_, err := cred.BearerToken(context.Background())
	must.ErrorContains(err, "parse token response", "truncated oversized body should fail to parse")
}

// Exercises the sovereign-cloud tokenURLOverride path: the exchange
// must POST to the override, not the global-cloud authority.
func TestAzureWIFSource_CustomTokenURL(t *testing.T) {
	must := require.New(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(azureTokenResponse{
			AccessToken: "tok", TokenType: "Bearer", ExpiresIn: 3600,
		})
	}))
	defer srv.Close()

	// Construct directly so the variadic override path is exercised
	// end-to-end, rather than via the test helper's post-hoc patch.
	src := NewAzureWorkloadIdentitySource(
		&stubTokenSource{token: []byte("jwt")},
		testAzureTenantID,
		testAzureClientID,
		"",
		srv.URL,
	)

	cred, err := src.Resolve(context.Background())
	must.NoError(err)
	_, err = cred.BearerToken(context.Background())
	must.NoError(err)
	must.Equal(int32(1), atomic.LoadInt32(&calls), "override URL hit count")

	must.Equal(srv.URL, src.tokenURL, "override should win over the global default")
}

// Pins the variadic-arg semantics: an empty override means "use the
// default", not "use the empty string".
func TestAzureWIFSource_EmptyTokenURLOverrideFallsBack(t *testing.T) {
	must := require.New(t)
	src := NewAzureWorkloadIdentitySource(
		&stubTokenSource{token: []byte("jwt")},
		testAzureTenantID,
		testAzureClientID,
		"",
		"",
	)

	want := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", testAzureTenantID)
	must.Equal(want, src.tokenURL, "empty override must fall back to the global default")
}

// Guards correlationIDSuffix against a hostile endpoint returning a
// correlation_id with ANSI escapes, control bytes, or an oversized
// payload — bounding it at the source before it reaches slog/OTel/terminal.
func TestAzureWIFSource_CorrelationIDSanitised(t *testing.T) {
	t.Run("ANSI escape and control bytes stripped", func(t *testing.T) {
		must := require.New(t)
		// json.Marshal (not a raw string literal) carries the 0x1b
		// control byte through the JSON parser correctly.
		raw := "abc\x1b[31m\rde\x00f"
		jsonBody, err := json.Marshal(map[string]string{
			"error":          "x",
			"correlation_id": raw,
		})
		must.NoError(err)
		got := correlationIDSuffix(jsonBody)
		// Bytes < 0x20 (ESC, CR, NUL) are dropped; the printable "[31m"
		// tail survives since only the ESC byte was the control sequence.
		must.Equal(" (correlation_id=abc[31mdef)", got)
	})

	t.Run("oversized correlation_id truncated to 64 bytes", func(t *testing.T) {
		must := require.New(t)
		long := strings.Repeat("a", 200)
		body := []byte(fmt.Sprintf(`{"correlation_id":%q}`, long))
		got := correlationIDSuffix(body)
		must.Equal(" (correlation_id="+strings.Repeat("a", 64)+")", got)
	})

	t.Run("normal UUID passes through unchanged", func(t *testing.T) {
		must := require.New(t)
		body := []byte(`{"correlation_id":"7c14b6a4-9d28-4a3a-b3a3-1234567890ab"}`)
		got := correlationIDSuffix(body)
		must.Equal(" (correlation_id=7c14b6a4-9d28-4a3a-b3a3-1234567890ab)", got)
	})
}
