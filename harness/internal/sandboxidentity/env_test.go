package sandboxidentity

import (
	"reflect"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// TestComposeEnv_Canonical pins the issue's "Composed sandbox env" example
// byte-for-byte: gitProxy.url "http://haybale.internal:8466", hosts
// ["github.com"], rewriteSsh true, envVar "HAYBALE_TOKEN". This is the
// acceptance oracle for issue #516 Part B/C.
func TestComposeEnv_Canonical(t *testing.T) {
	gp := &types.GitProxyConfig{
		URL:        "http://haybale.internal:8466",
		Hosts:      []string{"github.com"},
		RewriteSsh: true,
	}

	got, err := ComposeEnv("HAYBALE_TOKEN", "the-token", gp)
	if err != nil {
		t.Fatalf("ComposeEnv() error: %v", err)
	}

	want := []EnvVar{
		{Name: "HAYBALE_TOKEN", Value: "the-token"},
		{Name: "GIT_CONFIG_COUNT", Value: "4"},
		{Name: "GIT_CONFIG_KEY_0", Value: "url.http://haybale.internal:8466/github.com/.insteadOf"},
		{Name: "GIT_CONFIG_VALUE_0", Value: "https://github.com/"},
		{Name: "GIT_CONFIG_KEY_1", Value: "url.http://haybale.internal:8466/github.com/.insteadOf"},
		{Name: "GIT_CONFIG_VALUE_1", Value: "git@github.com:"},
		{Name: "GIT_CONFIG_KEY_2", Value: "url.http://haybale.internal:8466/github.com/.insteadOf"},
		{Name: "GIT_CONFIG_VALUE_2", Value: "ssh://git@github.com/"},
		{Name: "GIT_CONFIG_KEY_3", Value: "credential.http://haybale.internal:8466/.helper"},
		{Name: "GIT_CONFIG_VALUE_3", Value: `!f() { echo username=x-access-token; echo "password=$HAYBALE_TOKEN"; }; f`},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ComposeEnv canonical mismatch:\ngot:  %#v\nwant: %#v", got, want)
	}
}

// TestComposeEnv_NoGitProxy asserts a bare sandbox identity token (no
// GitProxy consumer) composes to exactly one entry.
func TestComposeEnv_NoGitProxy(t *testing.T) {
	got, err := ComposeEnv("HAYBALE_TOKEN", "the-token", nil)
	if err != nil {
		t.Fatalf("ComposeEnv() error: %v", err)
	}
	want := []EnvVar{{Name: "HAYBALE_TOKEN", Value: "the-token"}}
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

	got, err := ComposeEnv("HAYBALE_TOKEN", "tok", gp)
	if err != nil {
		t.Fatalf("ComposeEnv() error: %v", err)
	}

	want := []EnvVar{
		{Name: "HAYBALE_TOKEN", Value: "tok"},
		{Name: "GIT_CONFIG_COUNT", Value: "2"},
		{Name: "GIT_CONFIG_KEY_0", Value: "url.http://haybale.internal:8466/github.com/.insteadOf"},
		{Name: "GIT_CONFIG_VALUE_0", Value: "https://github.com/"},
		{Name: "GIT_CONFIG_KEY_1", Value: "credential.http://haybale.internal:8466/.helper"},
		{Name: "GIT_CONFIG_VALUE_1", Value: `!f() { echo username=x-access-token; echo "password=$HAYBALE_TOKEN"; }; f`},
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

	got, err := ComposeEnv("HAYBALE_TOKEN", "tok", gp)
	if err != nil {
		t.Fatalf("ComposeEnv() error: %v", err)
	}

	// 3 insteadOf entries per host * 2 hosts + 1 credential helper = 7.
	wantCount := "7"
	if got[1].Name != "GIT_CONFIG_COUNT" || got[1].Value != wantCount {
		t.Fatalf("GIT_CONFIG_COUNT = %+v, want value %q", got[1], wantCount)
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

// TestComposeEnv_CustomEnvVarName asserts the credential helper references
// the caller-supplied variable name, not a hardcoded "HAYBALE_TOKEN" — the
// env-composition function must generalise beyond haybale's default.
func TestComposeEnv_CustomEnvVarName(t *testing.T) {
	gp := &types.GitProxyConfig{
		URL:   "http://proxy.internal:9000",
		Hosts: []string{"github.com"},
	}

	got, err := ComposeEnv("MY_CUSTOM_TOKEN", "tok", gp)
	if err != nil {
		t.Fatalf("ComposeEnv() error: %v", err)
	}

	firstEntry := got[0]
	if firstEntry.Name != "MY_CUSTOM_TOKEN" || firstEntry.Value != "tok" {
		t.Errorf("first entry = %+v, want {MY_CUSTOM_TOKEN tok}", firstEntry)
	}

	var helperValue string
	for _, e := range got {
		if e.Name == "GIT_CONFIG_VALUE_1" {
			helperValue = e.Value
		}
	}
	wantHelper := `!f() { echo username=x-access-token; echo "password=$MY_CUSTOM_TOKEN"; }; f`
	if helperValue != wantHelper {
		t.Errorf("credential helper = %q, want %q", helperValue, wantHelper)
	}
}

// TestComposeEnv_NonPosixEnvVarRejected asserts S-COMPOSEENV-DEFENSE:
// ComposeEnv must not trust its caller's envVar shape and interpolate a
// shell metacharacter into credentialHelperTemplate. This holds even
// though types.ValidateRunConfig already rejects the same shape upstream —
// the guard here is defense in depth, not redundant, per the doc comment
// on posixEnvVarNamePattern.
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
			got, err := ComposeEnv(tc.envVar, "tok", gp)
			if err == nil {
				t.Fatalf("ComposeEnv(%q) = %#v, nil; want an error", tc.envVar, got)
			}
			if got != nil {
				t.Errorf("ComposeEnv(%q) returned non-nil output %#v alongside an error", tc.envVar, got)
			}
		})
	}
}

// TestComposeEnv_ReservedEnvVarRejected asserts S-COMPOSEENV-DEFENSE: an
// envVar colliding with one of the egress-proxy variables the container/k8s
// executors set (HTTP_PROXY, HTTPS_PROXY, NO_PROXY) is rejected rather than
// silently appended after — and likely overriding — the proxy URL.
func TestComposeEnv_ReservedEnvVarRejected(t *testing.T) {
	for _, envVar := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"} {
		t.Run(envVar, func(t *testing.T) {
			got, err := ComposeEnv(envVar, "tok", nil)
			if err == nil {
				t.Fatalf("ComposeEnv(%q) = %#v, nil; want an error", envVar, got)
			}
			if got != nil {
				t.Errorf("ComposeEnv(%q) returned non-nil output %#v alongside an error", envVar, got)
			}
		})
	}
}

// TestComposeEnv_ReservedEnvVarLowercaseAllowed pins the scope of the
// reserved-name guard: only the exact upper-case forms the container/k8s
// executors set (see proxyEnvFor / container.go, which never set a
// lower-case http_proxy/https_proxy/no_proxy) are rejected. A lower-case
// name is a valid, non-colliding POSIX identifier.
func TestComposeEnv_ReservedEnvVarLowercaseAllowed(t *testing.T) {
	got, err := ComposeEnv("http_proxy", "tok", nil)
	if err != nil {
		t.Fatalf("ComposeEnv(\"http_proxy\") unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Name != "http_proxy" || got[0].Value != "tok" {
		t.Errorf("ComposeEnv(\"http_proxy\") = %#v, want [{http_proxy tok}]", got)
	}
}
