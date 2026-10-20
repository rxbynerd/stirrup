package builtins

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// credentialedLoopbackURL closes an httptest server to obtain a loopback URL
// whose port refuses connections, then rewrites it to embed userinfo and a
// secret query parameter — the CWE-532 leak scenario for web_fetch, where a
// user passes a presigned or otherwise credentialed URL that then fails to
// dial. Loopback keeps the SSRF guard satisfiable via allowPrivateHosts.
func credentialedLoopbackURL(t *testing.T) string {
	t.Helper()
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed := srv.URL
	srv.Close()

	u, err := url.Parse(closed)
	must.NoError(err, "parse closed URL %q", closed)
	u.User = url.UserPassword("user", "pass")
	u.RawQuery = "api_key=supersecret"
	return u.String()
}

// TestWebFetch_TransportError_DoesNotLeakCredentials drives the dial-failure
// path: the returned error must not surface the userinfo or the secret query
// parameter embedded in the fetched URL.
func TestWebFetch_TransportError_DoesNotLeakCredentials(t *testing.T) {
	must := require.New(t)
	uri := credentialedLoopbackURL(t)

	tl := newWebFetchTool(webFetchOptions{allowPrivateHosts: true})
	input, err := json.Marshal(map[string]string{"url": uri})
	must.NoError(err)

	_, err = tl.Handler(context.Background(), input)
	must.Error(err, "expected a transport error dialing a closed port")
	msg := err.Error()

	t.Run("omits query secret", func(t *testing.T) {
		must := require.New(t)
		must.NotContains(msg, "supersecret", "error leaked the query secret")
	})
	t.Run("omits userinfo", func(t *testing.T) {
		must := require.New(t)
		must.NotContains(msg, "user:pass", "error leaked userinfo")
	})
}
