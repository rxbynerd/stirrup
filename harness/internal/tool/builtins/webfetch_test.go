package builtins

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// credentialedLoopbackURL closes an httptest server to obtain a loopback URL
// whose port refuses connections, then rewrites it to embed userinfo and a
// secret query parameter — the CWE-532 leak scenario for web_fetch, where a
// user passes a presigned or otherwise credentialed URL that then fails to
// dial. Loopback keeps the SSRF guard satisfiable via allowPrivateHosts.
func credentialedLoopbackURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed := srv.URL
	srv.Close()

	u, err := url.Parse(closed)
	if err != nil {
		t.Fatalf("parse closed URL %q: %v", closed, err)
	}
	u.User = url.UserPassword("user", "pass")
	u.RawQuery = "api_key=supersecret"
	return u.String()
}

// TestWebFetch_TransportError_DoesNotLeakCredentials drives the dial-failure
// path: the returned error must not surface the userinfo or the secret query
// parameter embedded in the fetched URL.
func TestWebFetch_TransportError_DoesNotLeakCredentials(t *testing.T) {
	uri := credentialedLoopbackURL(t)

	tl := newWebFetchTool(webFetchOptions{allowPrivateHosts: true})
	input, err := json.Marshal(map[string]string{"url": uri})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}

	_, err = tl.Handler(context.Background(), input)
	if err == nil {
		t.Fatal("expected a transport error dialing a closed port")
	}
	msg := err.Error()
	if strings.Contains(msg, "supersecret") {
		t.Errorf("error leaked the query secret: %q", msg)
	}
	if strings.Contains(msg, "user:pass") {
		t.Errorf("error leaked userinfo: %q", msg)
	}
}
