package sandboxidentity

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

const testTokenPath = "/run/stirrup/sandbox-identity/token"

const testHelper = `!f() { echo username=x-access-token; echo "password=$(cat /run/stirrup/sandbox-identity/token)"; }; f`

// countEnvValue returns how many entries in env carry exactly value.
func countEnvValue(env []EnvVar, value string) int {
	n := 0
	for _, e := range env {
		if e.Value == value {
			n++
		}
	}
	return n
}

// TestComposeEnv_Canonical pins the composed sandbox env byte-for-byte for
// the canonical git-proxy configuration: gitProxy.url
// "http://haybale.internal:8466", hosts ["github.com"], rewriteSsh true,
// envVar "HAYBALE_TOKEN". The credential helper reads the token file, not
// the env var, so a refreshed token is presented on the next git operation.
func TestComposeEnv_Canonical(t *testing.T) {
	must := require.New(t)
	gp := &types.GitProxyConfig{
		URL:        "http://haybale.internal:8466",
		Hosts:      []string{"github.com"},
		RewriteSsh: true,
	}

	got, err := ComposeEnv("HAYBALE_TOKEN", "the-token", testTokenPath, gp)
	must.NoError(err)

	want := []EnvVar{
		{Name: "HAYBALE_TOKEN", Value: "the-token"},
		{Name: "HAYBALE_TOKEN_FILE", Value: testTokenPath},
		{Name: "GIT_CONFIG_COUNT", Value: "4"},
		{Name: "GIT_CONFIG_KEY_0", Value: "url.http://haybale.internal:8466/github.com/.insteadOf"},
		{Name: "GIT_CONFIG_VALUE_0", Value: "https://github.com/"},
		{Name: "GIT_CONFIG_KEY_1", Value: "url.http://haybale.internal:8466/github.com/.insteadOf"},
		{Name: "GIT_CONFIG_VALUE_1", Value: "git@github.com:"},
		{Name: "GIT_CONFIG_KEY_2", Value: "url.http://haybale.internal:8466/github.com/.insteadOf"},
		{Name: "GIT_CONFIG_VALUE_2", Value: "ssh://git@github.com/"},
		{Name: "GIT_CONFIG_KEY_3", Value: "credential.http://haybale.internal:8466/.helper"},
		{Name: "GIT_CONFIG_VALUE_3", Value: testHelper},
	}
	must.Equal(want, got)
}

// TestComposeEnv_NoGitProxy asserts a bare sandbox identity token (no
// GitProxy consumer) composes to the token variable and its file pointer.
func TestComposeEnv_NoGitProxy(t *testing.T) {
	must := require.New(t)
	got, err := ComposeEnv("HAYBALE_TOKEN", "the-token", testTokenPath, nil)
	must.NoError(err)

	want := []EnvVar{
		{Name: "HAYBALE_TOKEN", Value: "the-token"},
		{Name: "HAYBALE_TOKEN_FILE", Value: testTokenPath},
	}
	must.Equal(want, got)
}

// TestComposeEnv_RewriteSshFalse asserts only the https insteadOf form is
// emitted per host when RewriteSsh is false: 1 insteadOf + 1 credential
// helper = 2 GIT_CONFIG entries.
func TestComposeEnv_RewriteSshFalse(t *testing.T) {
	must := require.New(t)
	gp := &types.GitProxyConfig{
		URL:   "http://haybale.internal:8466",
		Hosts: []string{"github.com"},
	}

	got, err := ComposeEnv("HAYBALE_TOKEN", "tok", testTokenPath, gp)
	must.NoError(err)

	want := []EnvVar{
		{Name: "HAYBALE_TOKEN", Value: "tok"},
		{Name: "HAYBALE_TOKEN_FILE", Value: testTokenPath},
		{Name: "GIT_CONFIG_COUNT", Value: "2"},
		{Name: "GIT_CONFIG_KEY_0", Value: "url.http://haybale.internal:8466/github.com/.insteadOf"},
		{Name: "GIT_CONFIG_VALUE_0", Value: "https://github.com/"},
		{Name: "GIT_CONFIG_KEY_1", Value: "credential.http://haybale.internal:8466/.helper"},
		{Name: "GIT_CONFIG_VALUE_1", Value: testHelper},
	}
	must.Equal(want, got)
}

// TestComposeEnv_MultiHost asserts each host in Hosts gets its own repeated
// insteadOf block, and exactly one credential helper covers all of them.
func TestComposeEnv_MultiHost(t *testing.T) {
	must := require.New(t)
	gp := &types.GitProxyConfig{
		URL:        "http://haybale.internal:8466",
		Hosts:      []string{"github.com", "gitlab.example.com"},
		RewriteSsh: true,
	}
	const credentialKey = "credential.http://haybale.internal:8466/.helper"

	got, err := ComposeEnv("HAYBALE_TOKEN", "tok", testTokenPath, gp)
	must.NoError(err)

	t.Run("config count", func(t *testing.T) {
		must := require.New(t)
		// 3 insteadOf entries per host * 2 hosts + 1 credential helper = 7.
		must.Equal(EnvVar{Name: "GIT_CONFIG_COUNT", Value: "7"}, got[2])
	})

	t.Run("credential helper is the final entry", func(t *testing.T) {
		must := require.New(t)
		must.Equal(EnvVar{Name: "GIT_CONFIG_KEY_6", Value: credentialKey}, got[len(got)-2])
		must.Equal("GIT_CONFIG_VALUE_6", got[len(got)-1].Name)
	})

	t.Run("credential helper appears once", func(t *testing.T) {
		must := require.New(t)
		must.Equal(1, countEnvValue(got, credentialKey))
	})

	for _, host := range gp.Hosts {
		t.Run("insteadOf for "+host, func(t *testing.T) {
			must := require.New(t)
			wantKey := "url.http://haybale.internal:8466/" + host + "/.insteadOf"
			must.Equal(3, countEnvValue(got, wantKey),
				"rewriteSsh=true emits three insteadOf entries per host")
		})
	}
}

// TestComposeEnv_CustomEnvVarName asserts the token variable and its _FILE
// companion follow the caller-supplied name, not a hardcoded
// "HAYBALE_TOKEN", while the credential helper reads the token file and so
// carries no variable name at all.
func TestComposeEnv_CustomEnvVarName(t *testing.T) {
	must := require.New(t)
	gp := &types.GitProxyConfig{
		URL:   "http://proxy.internal:9000",
		Hosts: []string{"github.com"},
	}

	got, err := ComposeEnv("MY_CUSTOM_TOKEN", "tok", testTokenPath, gp)
	must.NoError(err)

	must.Equal([]EnvVar{
		{Name: "MY_CUSTOM_TOKEN", Value: "tok"},
		{Name: "MY_CUSTOM_TOKEN_FILE", Value: testTokenPath},
	}, got[:2])

	var helperValue string
	for _, e := range got {
		if e.Name == "GIT_CONFIG_VALUE_1" {
			helperValue = e.Value
		}
	}
	must.Equal(testHelper, helperValue)
}

// TestComposeEnv_NonPosixEnvVarRejected asserts ComposeEnv does not trust
// its caller's envVar shape. This holds even though types.ValidateRunConfig
// already rejects the same shape upstream — the guard here is defense in
// depth, per the doc comment on posixEnvVarNamePattern.
func TestComposeEnv_NonPosixEnvVarRejected(t *testing.T) {
	gp := &types.GitProxyConfig{URL: "http://haybale.internal:8466", Hosts: []string{"github.com"}}

	cases := []struct {
		name   string
		envVar string
	}{
		{"shell metacharacter", `TOKEN"; rm -rf /; echo "`},
		{"embedded space", "TOKEN VAR"},
		{"leading digit", "1TOKEN"},
		{"empty", ""},
		{"hyphen", "TOKEN-VAR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got, err := ComposeEnv(tc.envVar, "tok", testTokenPath, gp)
			must.Error(err)
			must.Nil(got, "output returned alongside an error")
		})
	}
}

// TestComposeEnv_UnsafeTokenPathRejected asserts the token file path, which
// is interpolated unquoted into the shell credential helper, is bounded to
// an absolute path free of shell metacharacters.
func TestComposeEnv_UnsafeTokenPathRejected(t *testing.T) {
	gp := &types.GitProxyConfig{URL: "http://haybale.internal:8466", Hosts: []string{"github.com"}}

	cases := []struct {
		name string
		path string
	}{
		{"relative", "run/token"},
		{"command substitution", "/run/$(id)/token"},
		{"embedded space", "/run/stirrup token"},
		{"quote", `/run/"token`},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got, err := ComposeEnv("HAYBALE_TOKEN", "tok", tc.path, gp)
			must.Error(err)
			must.Nil(got, "output returned alongside an error")
		})
	}
}

// TestComposeEnv_ReservedEnvVarRejected asserts an envVar colliding with one
// of the egress-proxy variables the container/k8s executors set (either
// spelling of HTTP_PROXY, HTTPS_PROXY, NO_PROXY) is rejected rather than
// silently appended after, and likely overriding, the proxy URL.
func TestComposeEnv_ReservedEnvVarRejected(t *testing.T) {
	cases := []struct {
		name   string
		envVar string
	}{
		{"uppercase HTTP_PROXY", "HTTP_PROXY"},
		{"lowercase http_proxy", "http_proxy"},
		{"uppercase HTTPS_PROXY", "HTTPS_PROXY"},
		{"lowercase https_proxy", "https_proxy"},
		{"uppercase NO_PROXY", "NO_PROXY"},
		{"lowercase no_proxy", "no_proxy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got, err := ComposeEnv(tc.envVar, "tok", testTokenPath, nil)
			must.Error(err)
			must.Nil(got, "output returned alongside an error")
		})
	}
}

// TestComposeEnv_MixedCaseProxyNameAllowed pins the scope of the
// reserved-name guard: it matches the exact names proxyEnvFor and the
// container executor inject, not any case-folded variant.
func TestComposeEnv_MixedCaseProxyNameAllowed(t *testing.T) {
	must := require.New(t)
	got, err := ComposeEnv("Http_Proxy", "tok", testTokenPath, nil)
	must.NoError(err)

	want := []EnvVar{
		{Name: "Http_Proxy", Value: "tok"},
		{Name: "Http_Proxy_FILE", Value: testTokenPath},
	}
	must.Equal(want, got)
}
