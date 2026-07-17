package sandboxidentity

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/rxbynerd/stirrup/types"
)

// EnvVar is a single sandbox environment variable entry. Composed env is
// returned as an ordered slice (not a map) because the GIT_CONFIG_KEY_n /
// GIT_CONFIG_VALUE_n encoding is positional: git reads the numbered pairs in
// order, and a repeated key (one per rewritten URL form) must appear at its
// own index to be honoured as a multi-valued config entry.
type EnvVar struct {
	Name  string
	Value string
}

// credentialHelperTemplate is the inline shell credential helper haybale's
// docs/stirrup-integration.md documents: git invokes it as
// `credential.<proxyURL>/.helper`, and it echoes the token from the sandbox
// environment variable named by %s as the Basic-auth password. username is
// a fixed placeholder ("x-access-token", the GitHub App convention) since
// haybale strips the username and authenticates solely on the password.
const credentialHelperTemplate = `!f() { echo username=x-access-token; echo "password=$%s"; }; f`

// posixEnvVarNamePattern duplicates types.posixEnvVarNamePattern. ComposeEnv
// interpolates envVar unescaped into credentialHelperTemplate's shell
// string, so this package must not rely solely on
// types.ValidateRunConfig having already run before it reaches this
// function — a future refactor that reorders validation, or a new caller
// that bypasses it, must not reopen the shell-injection shape. Duplicating
// the character class across the types/harness boundary mirrors the
// existing gitProxyAllowlistDefaultPort precedent in types/runconfig.go.
var posixEnvVarNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedEnvVarNames are the egress-proxy variables the container and k8s
// executors set unconditionally in "allowlist" network mode (see
// proxyEnvFor in harness/internal/executor/k8s_netpol.go and the
// equivalent literal block in container.go — both only ever set the
// upper-case forms). A sandbox identity envVar colliding with one of these
// would silently append after, and likely override, the proxy URL rather
// than failing validation up front.
var reservedEnvVarNames = map[string]bool{
	"HTTP_PROXY":  true,
	"HTTPS_PROXY": true,
	"NO_PROXY":    true,
}

// ComposeEnv builds the ordered sandbox environment variables that carry a
// sandbox identity token (and, when gp is non-nil, the non-secret git
// configuration that routes git operations through a proxy such as
// haybale). It is a pure function — no I/O — so it is directly
// unit-testable against the issue's "Composed sandbox env" canonical
// example.
//
// envVar is the sandbox environment variable name the token is injected as
// (SandboxIdentityConfig.EffectiveEnvVar() / GitProxyConfig.EffectiveTokenEnvVar()
// — ValidateRunConfig guarantees the two agree when both are set). token is
// the raw JWT from a successful Exchange.
//
// The first returned entry is always <envVar>=<token>. When gp is nil, that
// is the only entry (a bare sandbox identity token with no git-proxy
// consumer). When gp is non-nil, GIT_CONFIG_COUNT and the numbered
// GIT_CONFIG_KEY_n / GIT_CONFIG_VALUE_n pairs follow: for each host in
// gp.Hosts, an "https://" insteadOf rewrite, plus (when gp.RewriteSsh) the
// scp-style "git@host:" and "ssh://git@host/" forms — all three sharing the
// same key, since git appends repeated GIT_CONFIG_KEY_n entries as
// multi-valued config the way repeated lines in a config file would. A
// single credential.<gp.URL>/.helper entry follows every host's insteadOf
// rules — one proxy authenticates every rewritten host.
//
// Returns an error, composing nothing, when envVar fails the
// posixEnvVarNamePattern check or collides with a reservedEnvVarNames entry
// — defense in depth so this function's safety does not rest entirely on
// its caller (types.ValidateRunConfig) having already run.
func ComposeEnv(envVar, token string, gp *types.GitProxyConfig) ([]EnvVar, error) {
	if !posixEnvVarNamePattern.MatchString(envVar) {
		return nil, fmt.Errorf("sandboxidentity: envVar %q is not a valid POSIX environment variable name", envVar)
	}
	if reservedEnvVarNames[envVar] {
		return nil, fmt.Errorf("sandboxidentity: envVar %q collides with a reserved egress-proxy environment variable", envVar)
	}

	out := []EnvVar{{Name: envVar, Value: token}}
	if gp == nil {
		return out, nil
	}

	var keys, values []string
	for _, host := range gp.Hosts {
		insteadOfKey := fmt.Sprintf("url.%s/%s/.insteadOf", gp.URL, host)
		keys = append(keys, insteadOfKey)
		values = append(values, fmt.Sprintf("https://%s/", host))
		if gp.RewriteSsh {
			keys = append(keys, insteadOfKey)
			values = append(values, fmt.Sprintf("git@%s:", host))
			keys = append(keys, insteadOfKey)
			values = append(values, fmt.Sprintf("ssh://git@%s/", host))
		}
	}
	keys = append(keys, fmt.Sprintf("credential.%s/.helper", gp.URL))
	values = append(values, fmt.Sprintf(credentialHelperTemplate, envVar))

	out = append(out, EnvVar{Name: "GIT_CONFIG_COUNT", Value: strconv.Itoa(len(keys))})
	for i := range keys {
		out = append(out,
			EnvVar{Name: fmt.Sprintf("GIT_CONFIG_KEY_%d", i), Value: keys[i]},
			EnvVar{Name: fmt.Sprintf("GIT_CONFIG_VALUE_%d", i), Value: values[i]},
		)
	}
	return out, nil
}
