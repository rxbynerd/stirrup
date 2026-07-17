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

	got := ComposeEnv("HAYBALE_TOKEN", "the-token", gp)

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
	got := ComposeEnv("HAYBALE_TOKEN", "the-token", nil)
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

	got := ComposeEnv("HAYBALE_TOKEN", "tok", gp)

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

	got := ComposeEnv("HAYBALE_TOKEN", "tok", gp)

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

	got := ComposeEnv("MY_CUSTOM_TOKEN", "tok", gp)

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
