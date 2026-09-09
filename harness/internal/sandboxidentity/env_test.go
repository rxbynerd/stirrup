package sandboxidentity

import (
	"reflect"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

const testTokenPath = "/run/stirrup/sandbox-identity/token"

const testHelper = `!f() { echo username=x-access-token; echo "password=$(cat /run/stirrup/sandbox-identity/token)"; }; f`

// TestComposeEnv_Canonical pins the composed sandbox env byte-for-byte for
// the canonical git-proxy configuration: gitProxy.url
// "http://haybale.internal:8466", hosts ["github.com"], rewriteSsh true,
// envVar "HAYBALE_TOKEN". The credential helper reads the token file, not
// the env var, so a refreshed token is presented on the next git operation.
func TestComposeEnv_Canonical(t *testing.T) {
	gp := &types.GitProxyConfig{
		URL:        "http://haybale.internal:8466",
		Hosts:      []string{"github.com"},
		RewriteSsh: true,
	}

	got, err := ComposeEnv("HAYBALE_TOKEN", "the-token", testTokenPath, gp)
	if err != nil {
		t.Fatalf("ComposeEnv() error: %v", err)
	}

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

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ComposeEnv canonical mismatch:\ngot:  %#v\nwant: %#v", got, want)
	}
}

// TestComposeEnv_NoGitProxy asserts a bare sandbox identity token (no
// GitProxy consumer) composes to the token variable and its file pointer.
func TestComposeEnv_NoGitProxy(t *testing.T) {
	got, err := ComposeEnv("HAYBALE_TOKEN", "the-token", testTokenPath, nil)
	if err != nil {
		t.Fatalf("ComposeEnv() error: %v", err)
	}
	want := []EnvVar{
		{Name: "HAYBALE_TOKEN", Value: "the-token"},
		{Name: "HAYBALE_TOKEN_FILE", Value: testTokenPath},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ComposeEnv(nil gitProxy) = %#v, want %#v", got, want)
	}
}

// TestComposeEnv_RewriteSshFalse asserts only the https insteadOf form is
// emitted per host when RewriteSsh is false: 1 insteadOf + 1 credential
// helper = 2 GIT_CONFIG entries.
func TestComposeEnv_RewriteSshFalse(t *testing.T) {
	gp := &types.GitProxyConfig{
		URL:   "http://haybale.internal:8466",
		Hosts: []string{"github.com"},
	}

	got, err := ComposeEnv("HAYBALE_TOKEN", "tok", testTokenPath, gp)
	if err != nil {
		t.Fatalf("ComposeEnv() error: %v", err)
	}

	want := []EnvVar{
		{Name: "HAYBALE_TOKEN", Value: "tok"},
		{Name: "HAYBALE_TOKEN_FILE", Value: testTokenPath},
		{Name: "GIT_CONFIG_COUNT", Value: "2"},
		{Name: "GIT_CONFIG_KEY_0", Value: "url.http://haybale.internal:8466/github.com/.insteadOf"},
		{Name: "GIT_CONFIG_VALUE_0", Value: "https://github.com/"},
		{Name: "GIT_CONFIG_KEY_1", Value: "credential.http://haybale.internal:8466/.helper"},
		{Name: "GIT_CONFIG_VALUE_1", Value: testHelper},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ComposeEnv(rewriteSsh=false) mismatch:\ngot:  %#v\nwant: %#v", got, want)
	}
}

// TestComposeEnv_MultiHost asserts each host in Hosts gets its own repeated
// insteadOf block, and exactly one credential helper covers all of them.
func TestComposeEnv_MultiHost(t *testing.T) {
	gp := &types.GitProxyConfig{
		URL:        "http://haybale.internal:8466",
		Hosts:      []string{"github.com", "gitlab.example.com"},
		RewriteSsh: true,
	}

	got, err := ComposeEnv("HAYBALE_TOKEN", "tok", testTokenPath, gp)
	if err != nil {
		t.Fatalf("ComposeEnv() error: %v", err)
	}

	// 3 insteadOf entries per host * 2 hosts + 1 credential helper = 7.
	wantCount := "7"
	if got[2].Name != "GIT_CONFIG_COUNT" || got[2].Value != wantCount {
		t.Fatalf("GIT_CONFIG_COUNT = %+v, want value %q", got[2], wantCount)
	}

	// The final numbered entry must be the single credential helper.
	last := got[len(got)-2]
	lastVal := got[len(got)-1]
	if last.Name != "GIT_CONFIG_KEY_6" || last.Value != "credential.http://haybale.internal:8466/.helper" {
		t.Errorf("final key entry = %+v, want credential helper at index 6", last)
	}
	if lastVal.Name != "GIT_CONFIG_VALUE_6" {
		t.Errorf("final value entry name = %q, want GIT_CONFIG_VALUE_6", lastVal.Name)
	}

	// Exactly one credential.*.helper entry across the whole output.
	credCount := 0
	for _, e := range got {
		if e.Value == "credential.http://haybale.internal:8466/.helper" {
			credCount++
		}
	}
	if credCount != 1 {
		t.Errorf("expected exactly one credential helper key, got %d", credCount)
	}

	// Both hosts' insteadOf keys must be present.
	for _, host := range gp.Hosts {
		wantKey := "url.http://haybale.internal:8466/" + host + "/.insteadOf"
		found := 0
		for _, e := range got {
			if e.Value == wantKey {
				found++
			}
		}
		if found != 3 {
			t.Errorf("host %s: expected 3 insteadOf key occurrences (rewriteSsh=true), got %d", host, found)
		}
	}
}

// TestComposeEnv_CustomEnvVarName asserts the token variable and its _FILE
// companion follow the caller-supplied name, not a hardcoded
// "HAYBALE_TOKEN", while the credential helper reads the token file and so
// carries no variable name at all.
func TestComposeEnv_CustomEnvVarName(t *testing.T) {
	gp := &types.GitProxyConfig{
		URL:   "http://proxy.internal:9000",
		Hosts: []string{"github.com"},
	}

	got, err := ComposeEnv("MY_CUSTOM_TOKEN", "tok", testTokenPath, gp)
	if err != nil {
		t.Fatalf("ComposeEnv() error: %v", err)
	}

	if got[0].Name != "MY_CUSTOM_TOKEN" || got[0].Value != "tok" {
		t.Errorf("first entry = %+v, want {MY_CUSTOM_TOKEN tok}", got[0])
	}
	if got[1].Name != "MY_CUSTOM_TOKEN_FILE" || got[1].Value != testTokenPath {
		t.Errorf("second entry = %+v, want {MY_CUSTOM_TOKEN_FILE %s}", got[1], testTokenPath)
	}

	var helperValue string
	for _, e := range got {
		if e.Name == "GIT_CONFIG_VALUE_1" {
			helperValue = e.Value
		}
	}
	if helperValue != testHelper {
		t.Errorf("credential helper = %q, want %q", helperValue, testHelper)
	}
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
			got, err := ComposeEnv(tc.envVar, "tok", testTokenPath, gp)
			if err == nil {
				t.Fatalf("ComposeEnv(%q) = %#v, nil; want an error", tc.envVar, got)
			}
			if got != nil {
				t.Errorf("ComposeEnv(%q) returned non-nil output %#v alongside an error", tc.envVar, got)
			}
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
			got, err := ComposeEnv("HAYBALE_TOKEN", "tok", tc.path, gp)
			if err == nil {
				t.Fatalf("ComposeEnv(tokenPath=%q) = %#v, nil; want an error", tc.path, got)
			}
			if got != nil {
				t.Errorf("ComposeEnv(tokenPath=%q) returned non-nil output %#v alongside an error", tc.path, got)
			}
		})
	}
}

// TestComposeEnv_ReservedEnvVarRejected asserts S-COMPOSEENV-DEFENSE: an
// envVar colliding with one of the egress-proxy variables the container/k8s
// executors set — either spelling of HTTP_PROXY, HTTPS_PROXY, NO_PROXY — is
// rejected rather than silently appended after, and likely overriding, the
// proxy URL.
func TestComposeEnv_ReservedEnvVarRejected(t *testing.T) {
	for _, envVar := range []string{
		"HTTP_PROXY", "http_proxy",
		"HTTPS_PROXY", "https_proxy",
		"NO_PROXY", "no_proxy",
	} {
		t.Run(envVar, func(t *testing.T) {
			got, err := ComposeEnv(envVar, "tok", testTokenPath, nil)
			if err == nil {
				t.Fatalf("ComposeEnv(%q) = %#v, nil; want an error", envVar, got)
			}
			if got != nil {
				t.Errorf("ComposeEnv(%q) returned non-nil output %#v alongside an error", envVar, got)
			}
		})
	}
}

// TestComposeEnv_MixedCaseProxyNameAllowed pins the scope of the
// reserved-name guard: it matches the exact names proxyEnvFor and the
// container executor inject, not any case-folded variant.
func TestComposeEnv_MixedCaseProxyNameAllowed(t *testing.T) {
	got, err := ComposeEnv("Http_Proxy", "tok", testTokenPath, nil)
	if err != nil {
		t.Fatalf("ComposeEnv(\"Http_Proxy\") unexpected error: %v", err)
	}
	want := []EnvVar{
		{Name: "Http_Proxy", Value: "tok"},
		{Name: "Http_Proxy_FILE", Value: testTokenPath},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ComposeEnv(\"Http_Proxy\") = %#v, want %#v", got, want)
	}
}
