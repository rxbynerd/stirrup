package sandboxidentity

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/rxbynerd/stirrup/types"
)

// EnvVar is a single sandbox environment variable entry. Composed env is an
// ordered slice, not a map, because the GIT_CONFIG_KEY_n / GIT_CONFIG_VALUE_n
// encoding is positional: a repeated key (one per rewritten URL form) must
// occupy its own index to be honoured as a multi-valued config entry.
type EnvVar struct {
	Name  string
	Value string
}

// credentialHelperTemplate is the inline shell helper git invokes as
// `credential.<proxyURL>/.helper`; it reads the token file named by %s on
// every invocation as the Basic-auth password, so a token refreshed after
// sandbox creation is presented on the next git operation. The username is
// a fixed placeholder because the proxy authenticates on the password alone.
const credentialHelperTemplate = `!f() { echo username=x-access-token; echo "password=$(cat %s)"; }; f`

// tokenFileEnvVarSuffix names the companion variable `<envVar>_FILE`
// carrying the token file path: the plain envVar holds the token as issued
// at sandbox creation and is not updated by refresh, so consumers other
// than git read the file instead.
const tokenFileEnvVarSuffix = "_FILE"

// posixEnvVarNamePattern deliberately duplicates types.posixEnvVarNamePattern.
// ComposeEnv interpolates envVar unescaped into the composed env, so the
// shell-injection shape must not reopen if a refactor reorders validation or
// a new caller bypasses ValidateRunConfig.
var posixEnvVarNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// tokenPathPattern bounds the token file path interpolated unquoted into
// credentialHelperTemplate to an absolute path free of shell metacharacters.
var tokenPathPattern = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

// reservedEnvVarNames are the egress-proxy variables the container and k8s
// executors set unconditionally in "allowlist" mode. A colliding envVar
// would silently append after — and likely override — the proxy URL rather
// than failing validation up front. Both spellings are injected, so both
// are reserved.
var reservedEnvVarNames = map[string]bool{
	"HTTP_PROXY":  true,
	"http_proxy":  true,
	"HTTPS_PROXY": true,
	"https_proxy": true,
	"NO_PROXY":    true,
	"no_proxy":    true,
}

// ComposeEnv builds the ordered sandbox environment carrying a sandbox
// identity token: envVar holds the token as issued, `<envVar>_FILE` names
// the in-sandbox file (tokenPath) that refreshes keep current, and when gp
// is non-nil the non-secret GIT_CONFIG_* pairs route git through the proxy
// with a credential helper that reads tokenPath — see
// docs/configuration.md#sandbox-identity-and-git-proxy-wiring.
//
// Rejects an invalid or reserved envVar, or a tokenPath unsafe to
// interpolate into the helper, and composes nothing, so safety does not
// rest on types.ValidateRunConfig having already run.
func ComposeEnv(envVar, token, tokenPath string, gp *types.GitProxyConfig) ([]EnvVar, error) {
	if !posixEnvVarNamePattern.MatchString(envVar) {
		return nil, fmt.Errorf("sandboxidentity: envVar %q is not a valid POSIX environment variable name", envVar)
	}
	if reservedEnvVarNames[envVar] {
		return nil, fmt.Errorf("sandboxidentity: envVar %q collides with a reserved egress-proxy environment variable", envVar)
	}
	if !tokenPathPattern.MatchString(tokenPath) {
		return nil, fmt.Errorf("sandboxidentity: tokenPath %q must be an absolute path of [A-Za-z0-9._/-]", tokenPath)
	}

	out := []EnvVar{
		{Name: envVar, Value: token},
		{Name: envVar + tokenFileEnvVarSuffix, Value: tokenPath},
	}
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
	values = append(values, fmt.Sprintf(credentialHelperTemplate, tokenPath))

	out = append(out, EnvVar{Name: "GIT_CONFIG_COUNT", Value: strconv.Itoa(len(keys))})
	for i := range keys {
		out = append(out,
			EnvVar{Name: fmt.Sprintf("GIT_CONFIG_KEY_%d", i), Value: keys[i]},
			EnvVar{Name: fmt.Sprintf("GIT_CONFIG_VALUE_%d", i), Value: values[i]},
		)
	}
	return out, nil
}
