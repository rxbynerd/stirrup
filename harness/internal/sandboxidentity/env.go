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
// `credential.<proxyURL>/.helper`; it echoes the token from the environment
// variable named by %s as the Basic-auth password. The username is a fixed
// placeholder because the proxy authenticates on the password alone.
const credentialHelperTemplate = `!f() { echo username=x-access-token; echo "password=$%s"; }; f`

// posixEnvVarNamePattern deliberately duplicates types.posixEnvVarNamePattern.
// ComposeEnv interpolates envVar unescaped into credentialHelperTemplate's
// shell string, so the shell-injection shape must not reopen if a refactor
// reorders validation or a new caller bypasses ValidateRunConfig.
var posixEnvVarNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedEnvVarNames are the egress-proxy variables the container and k8s
// executors set unconditionally in "allowlist" mode. A colliding envVar
// would silently append after — and likely override — the proxy URL rather
// than failing validation up front.
var reservedEnvVarNames = map[string]bool{
	"HTTP_PROXY":  true,
	"HTTPS_PROXY": true,
	"NO_PROXY":    true,
}

// ComposeEnv builds the ordered sandbox environment carrying a sandbox
// identity token, followed when gp is non-nil by the non-secret GIT_CONFIG_*
// pairs routing git through the proxy — see
// docs/configuration.md#sandbox-identity-and-git-proxy-wiring.
//
// Rejects an invalid or reserved envVar and composes nothing, so safety does
// not rest on types.ValidateRunConfig having already run.
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
