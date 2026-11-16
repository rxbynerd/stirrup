package httpsput

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// mapSecrets resolves references from a fixed table.
type mapSecrets map[string]string

func (m mapSecrets) Resolve(_ context.Context, ref string) (string, error) {
	v, ok := m[ref]
	if !ok {
		return "", fmt.Errorf("secret %q is not set", ref)
	}
	return v, nil
}

type capturedPut struct {
	Method string
	URL    string
	Header http.Header
	Body   []byte
}

// captureServer records every request and answers with status and body.
type captureServer struct {
	mu       sync.Mutex
	requests []capturedPut
	status   int
	body     string
	location string
}

func (c *captureServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.requests = append(c.requests, capturedPut{Method: r.Method, URL: r.URL.String(), Header: r.Header.Clone(), Body: body})
	status, respBody, location := c.status, c.body, c.location
	c.mu.Unlock()
	if location != "" {
		w.Header().Set("Location", location)
	}
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, respBody)
}

func (c *captureServer) captured() []capturedPut {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedPut(nil), c.requests...)
}

const signedQuery = "?X-Amz-Signature=deadbeefcafe&X-Amz-Credential=AKIDEXAMPLE"

func resolveTarget(t *testing.T, url string, headers map[string]string, extra mapSecrets) Target {
	t.Helper()
	secrets := mapSecrets{"secret://URL": url}
	for k, v := range extra {
		secrets[k] = v
	}
	target, err := Resolve(context.Background(), secrets, &types.HTTPSPutConfig{URLRef: "secret://URL", Headers: headers})
	require.NoError(t, err)
	return target
}

func TestPut_SendsOnePutWithTheBodyAndHeadersOverLoopbackHTTP(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	capture := &captureServer{}
	srv := httptest.NewServer(capture)
	t.Cleanup(srv.Close)

	target := resolveTarget(t, srv.URL+"/bucket/key.tar.gz"+signedQuery,
		map[string]string{"x-ms-blob-type": "BlockBlob", "Authorization": "secret://AUTH"},
		mapSecrets{"secret://AUTH": "Bearer upload-token\n"})
	must.NoError(Put(context.Background(), NewClient(10*time.Second), target, []byte("payload"), "application/gzip"))

	got := capture.captured()
	must.Len(got, 1)
	check.Equal(http.MethodPut, got[0].Method)
	check.Equal("/bucket/key.tar.gz"+signedQuery, got[0].URL)
	check.Equal([]byte("payload"), got[0].Body)
	check.Equal("application/gzip", got[0].Header.Get("Content-Type"))
	check.Equal("BlockBlob", got[0].Header.Get("x-ms-blob-type"))
	check.Equal("Bearer upload-token", got[0].Header.Get("Authorization"))
}

func TestPut_UploadsOverTLS(t *testing.T) {
	must := require.New(t)
	capture := &captureServer{}
	srv := httptest.NewTLSServer(capture)
	t.Cleanup(srv.Close)

	client := NewClient(10 * time.Second)
	client.Transport.(*http.Transport).TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()

	target := resolveTarget(t, srv.URL+"/key"+signedQuery, nil, nil)
	must.True(strings.HasPrefix(target.Origin(), "https://"))
	must.NoError(Put(context.Background(), client, target, []byte("payload"), "application/json"))
	must.Len(capture.captured(), 1)
}

func TestPut_ConfiguredContentTypeReplacesTheDefault(t *testing.T) {
	must := require.New(t)
	capture := &captureServer{}
	srv := httptest.NewServer(capture)
	t.Cleanup(srv.Close)

	target := resolveTarget(t, srv.URL+"/key", map[string]string{"content-type": "application/octet-stream"}, nil)
	must.NoError(Put(context.Background(), NewClient(10*time.Second), target, []byte("x"), "application/gzip"))
	must.Equal([]string{"application/octet-stream"}, capture.captured()[0].Header.Values("Content-Type"))
}

func TestPut_StatusErrorCarriesTheStoreCodeAndNothingElseFromTheBody(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	capture := &captureServer{
		status: http.StatusForbidden,
		body: `<?xml version="1.0"?><Error><Code>SignatureDoesNotMatch</Code>` +
			`<Message>The request signature we calculated does not match</Message>` +
			`<SignatureProvided>deadbeefcafe</SignatureProvided>` +
			`<CanonicalRequest>PUT /bucket/key X-Amz-Credential=AKIDEXAMPLE</CanonicalRequest></Error>`,
	}
	srv := httptest.NewServer(capture)
	t.Cleanup(srv.Close)

	target := resolveTarget(t, srv.URL+"/bucket/key"+signedQuery, nil, nil)
	err := Put(context.Background(), NewClient(10*time.Second), target, []byte("x"), "")
	must.Error(err)

	var status *StatusError
	must.ErrorAs(err, &status)
	check.Equal(http.StatusForbidden, status.StatusCode)
	check.Equal("SignatureDoesNotMatch", status.Code)
	check.False(status.Transient())
	check.Contains(err.Error(), "HTTP 403 Forbidden (SignatureDoesNotMatch)")
	check.Contains(err.Error(), srv.URL)
	for _, leak := range []string{"deadbeefcafe", "AKIDEXAMPLE", "/bucket/key", "X-Amz"} {
		check.NotContains(err.Error(), leak)
	}
}

func TestStatusError_TransientCoversRetryableStatuses(t *testing.T) {
	for code, want := range map[int]bool{408: true, 429: true, 500: true, 503: true, 400: false, 403: false, 404: false, 307: false} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			check := assert.New(t)
			check.Equal(want, (&StatusError{StatusCode: code}).Transient())
		})
	}
}

func TestPut_DoesNotFollowARedirect(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	elsewhere := &captureServer{}
	other := httptest.NewServer(elsewhere)
	t.Cleanup(other.Close)
	capture := &captureServer{status: http.StatusTemporaryRedirect, location: other.URL + "/stolen"}
	srv := httptest.NewServer(capture)
	t.Cleanup(srv.Close)

	target := resolveTarget(t, srv.URL+"/key", map[string]string{"Authorization": "secret://AUTH"}, mapSecrets{"secret://AUTH": "Bearer t"})
	err := Put(context.Background(), NewClient(10*time.Second), target, []byte("x"), "")

	var status *StatusError
	must.ErrorAs(err, &status)
	check.Equal(http.StatusTemporaryRedirect, status.StatusCode)
	check.Empty(elsewhere.captured(), "the redirect target must not be contacted")
}

func TestPut_TransportErrorDoesNotCarryTheURL(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	target := resolveTarget(t, url+"/bucket/key"+signedQuery, nil, nil)
	err := Put(context.Background(), NewClient(5*time.Second), target, []byte("x"), "")
	must.Error(err)
	check.Contains(err.Error(), "put to "+url)
	for _, leak := range []string{"deadbeefcafe", "AKIDEXAMPLE", "/bucket/key", "X-Amz"} {
		check.NotContains(err.Error(), leak)
	}
}

func TestResolve_RejectsUnusableDestinationsWithoutQuotingThem(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"plain http to a remote host", "http://uploads.example/key" + signedQuery, "accepted only for a loopback host"},
		{"other scheme", "s3://bucket/key" + signedQuery, "scheme is not https://"},
		{"no scheme", "uploads.example/key" + signedQuery, "scheme is not https://"},
		{"no host", "https:///key" + signedQuery, "has no host"},
		{"userinfo", "https://user:hunter2@uploads.example/key" + signedQuery, "carries userinfo"},
		{"fragment", "https://uploads.example/key" + signedQuery + "#frag", "carries a fragment"},
		{"unparseable", "https://uploads.example/%zz" + signedQuery, "does not parse as a URL"},
		{"empty", "  ", "scheme is not https://"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			_, err := Resolve(context.Background(), mapSecrets{"secret://URL": tc.url}, &types.HTTPSPutConfig{URLRef: "secret://URL"})
			must.ErrorContains(err, tc.want)
			for _, leak := range []string{"deadbeefcafe", "AKIDEXAMPLE", "hunter2", "uploads.example"} {
				must.NotContains(err.Error(), leak)
			}
		})
	}
}

func TestResolve_AcceptsHTTPSAndLoopbackHTTP(t *testing.T) {
	for url, origin := range map[string]string{
		"https://uploads.example/key" + signedQuery: "https://uploads.example",
		"https://10.0.0.7:8443/runs/r1/workspace":   "https://10.0.0.7:8443",
		"http://127.0.0.1:9000/key":                 "http://127.0.0.1:9000",
		"http://localhost:9000/key":                 "http://localhost:9000",
		"http://[::1]:9000/key":                     "http://[::1]:9000",
		"  https://uploads.example/key\n":           "https://uploads.example",
	} {
		t.Run(origin, func(t *testing.T) {
			must := require.New(t)
			target, err := Resolve(context.Background(), mapSecrets{"secret://URL": url}, &types.HTTPSPutConfig{URLRef: "secret://URL"})
			must.NoError(err)
			must.Equal(origin, target.Origin())
		})
	}
}

func TestResolve_ReportsAnInlineURLWithoutQuotingIt(t *testing.T) {
	must := require.New(t)
	_, err := Resolve(context.Background(), mapSecrets{}, &types.HTTPSPutConfig{URLRef: "https://uploads.example/key" + signedQuery})
	must.ErrorContains(err, "urlRef is not a secret reference")
	must.NotContains(err.Error(), "deadbeefcafe")
}

func TestResolve_ReportsUnresolvedReferencesByName(t *testing.T) {
	must := require.New(t)
	_, err := Resolve(context.Background(), mapSecrets{}, &types.HTTPSPutConfig{URLRef: "secret://MISSING_URL"})
	must.ErrorContains(err, "resolve urlRef")
	must.ErrorContains(err, "MISSING_URL")

	_, err = Resolve(context.Background(), mapSecrets{"secret://URL": "https://uploads.example/key"},
		&types.HTTPSPutConfig{URLRef: "secret://URL", Headers: map[string]string{"Authorization": "secret://MISSING_AUTH"}})
	must.ErrorContains(err, `resolve header "Authorization"`)
}

func TestResolve_RejectsAResolvedHeaderValueThatWouldSplitTheHeader(t *testing.T) {
	must := require.New(t)
	_, err := Resolve(context.Background(),
		mapSecrets{"secret://URL": "https://uploads.example/key", "secret://AUTH": "Bearer a\r\nX-Injected: b"},
		&types.HTTPSPutConfig{URLRef: "secret://URL", Headers: map[string]string{"Authorization": "secret://AUTH"}})
	must.ErrorContains(err, `header "Authorization" resolved to an empty value or one with a control character`)
	must.NotContains(err.Error(), "X-Injected")
}

func TestTarget_PrintsOnlyItsOrigin(t *testing.T) {
	check := assert.New(t)
	target := resolveTarget(t, "https://uploads.example/bucket/key"+signedQuery,
		map[string]string{"Authorization": "secret://AUTH"}, mapSecrets{"secret://AUTH": "Bearer upload-token"})

	var logged bytes.Buffer
	slog.New(slog.NewTextHandler(&logged, nil)).Info("upload", "target", target)
	printed := fmt.Sprintf("%v|%+v|%s|%#v|%v", target, target, target, target, &target) + logged.String()

	check.Contains(printed, "https://uploads.example")
	for _, leak := range []string{"deadbeefcafe", "AKIDEXAMPLE", "/bucket/key", "upload-token"} {
		check.NotContains(printed, leak)
	}
	check.Empty(Target{}.Origin())
}

// observeDials wraps the dial check for the test and returns a func
// reporting, per dialled address, whether the dial was held to loopback.
func observeDials(t *testing.T) func() map[string]bool {
	t.Helper()
	var mu sync.Mutex
	seen := map[string]bool{}
	prev := dialControl
	t.Cleanup(func() { dialControl = prev })
	dialControl = func(ctx context.Context, network, address string, c syscall.RawConn) error {
		loopbackOnly, _ := ctx.Value(loopbackOnlyKey{}).(bool)
		mu.Lock()
		seen[address] = loopbackOnly
		mu.Unlock()
		return prev(ctx, network, address, c)
	}
	return func() map[string]bool {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]bool{}
		for k, v := range seen {
			out[k] = v
		}
		return out
	}
}

func TestPut_HoldsAnHTTPDestinationToLoopbackAtTheRealDial(t *testing.T) {
	must := require.New(t)
	dials := observeDials(t)
	plain := httptest.NewServer(&captureServer{})
	t.Cleanup(plain.Close)
	secure := httptest.NewTLSServer(&captureServer{})
	t.Cleanup(secure.Close)

	must.NoError(Put(context.Background(), NewClient(10*time.Second), resolveTarget(t, plain.URL+"/key", nil, nil), []byte("x"), ""))
	client := NewClient(10 * time.Second)
	client.Transport.(*http.Transport).TLSClientConfig = secure.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	must.NoError(Put(context.Background(), client, resolveTarget(t, secure.URL+"/key", nil, nil), []byte("x"), ""))

	must.Equal(map[string]bool{
		strings.TrimPrefix(plain.URL, "http://"):   true,
		strings.TrimPrefix(secure.URL, "https://"): false,
	}, dials())
}

func TestTarget_NestedInAnotherStructDoesNotPrintTheCapability(t *testing.T) {
	check := assert.New(t)
	target := resolveTarget(t, "https://uploads.example/bucket/key"+signedQuery,
		map[string]string{"Authorization": "secret://AUTH"}, mapSecrets{"secret://AUTH": "Bearer upload-token"})
	holder := struct{ target Target }{target: target}

	printed := fmt.Sprintf("%v|%+v|%#v", holder, holder, holder)
	for _, leak := range []string{"deadbeefcafe", "AKIDEXAMPLE", "/bucket/key", "upload-token"} {
		check.NotContains(printed, leak)
	}
}

func TestResolve_RejectsAnOversizedResolvedHeaderValue(t *testing.T) {
	must := require.New(t)
	_, err := Resolve(context.Background(),
		mapSecrets{"secret://URL": "https://uploads.example/key", "secret://AUTH": strings.Repeat("a", types.MaxHTTPSPutHeaderValueBytes+1)},
		&types.HTTPSPutConfig{URLRef: "secret://URL", Headers: map[string]string{"Authorization": "secret://AUTH"}})
	must.ErrorContains(err, `header "Authorization" resolved to more than 8192 bytes`)
}

func TestCheckDialAddress_RefusesMetadataAndNonLoopbackHTTPAddresses(t *testing.T) {
	loopbackOnly := context.WithValue(context.Background(), loopbackOnlyKey{}, true)
	cases := []struct {
		name    string
		ctx     context.Context
		address string
		refused bool
	}{
		{"cloud metadata endpoint", context.Background(), "169.254.169.254:443", true},
		{"mapped metadata endpoint", context.Background(), "[::ffff:169.254.169.254]:443", true},
		{"IPv6 link-local", context.Background(), "[fe80::1]:443", true},
		{"unspecified", context.Background(), "0.0.0.0:443", true},
		{"multicast", context.Background(), "224.0.0.1:443", true},
		{"public", context.Background(), "93.184.216.34:443", false},
		{"private in-cluster", context.Background(), "10.96.0.12:8443", false},
		{"loopback", context.Background(), "127.0.0.1:9000", false},
		{"http destination resolving to loopback", loopbackOnly, "127.0.0.1:9000", false},
		{"http destination resolving off loopback", loopbackOnly, "10.96.0.12:80", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			check := assert.New(t)
			err := checkDialAddress(tc.ctx, "tcp", tc.address, nil)
			check.Equal(tc.refused, err != nil, "err = %v", err)
		})
	}
}

func TestPut_RefusesALinkLocalDestinationAtDialTime(t *testing.T) {
	must := require.New(t)
	target := resolveTarget(t, "https://169.254.169.254/latest/api/token", nil, nil)
	err := Put(context.Background(), NewClient(5*time.Second), target, []byte("x"), "")
	must.ErrorContains(err, "refusing to connect to 169.254.169.254")
}

func TestPut_RejectsAnUnresolvedTargetAndAMissingClient(t *testing.T) {
	must := require.New(t)
	must.Error(Put(context.Background(), NewClient(time.Second), Target{}, nil, ""))
	target := resolveTarget(t, "https://uploads.example/key", nil, nil)
	must.Error(Put(context.Background(), nil, target, nil, ""))
}

func TestResolve_RequiresAConfigAndASecretStore(t *testing.T) {
	must := require.New(t)
	_, err := Resolve(context.Background(), mapSecrets{}, nil)
	must.Error(err)
	_, err = Resolve(context.Background(), nil, &types.HTTPSPutConfig{URLRef: "secret://URL"})
	must.Error(err)
}
