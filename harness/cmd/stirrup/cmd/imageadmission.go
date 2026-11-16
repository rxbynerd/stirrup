package cmd

import (
	"fmt"

	flag "github.com/spf13/pflag"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

const (
	flagImageAllowlist = "image-allowlist"
	envImageAllowlist  = "STIRRUP_IMAGE_ALLOWLIST"
)

func addImageAdmissionFlag(f *flag.FlagSet) {
	f.StringArray(flagImageAllowlist, nil, "Sandbox image admission policy: a registry or namespace prefix ending in / (admits every repository below it, at any depth) or a glob over the normalised image reference (* matches within one path segment). Repeatable; each value may hold comma-separated patterns. Applies to the container, k8s and k8s-sandbox executors. Replaces STIRRUP_IMAGE_ALLOWLIST when given. Deployment configuration, never part of the RunConfig. See docs/security.md#image-admission-policy.")
}

// resolveImageAdmissionPolicy reads the deployment's image admission policy:
// --image-allowlist when f is non-nil and the flag was given, otherwise
// STIRRUP_IMAGE_ALLOWLIST. Neither set yields the unconfigured policy.
func resolveImageAdmissionPolicy(f *flag.FlagSet, getenv func(string) string) (executor.ImageAdmissionPolicy, error) {
	if f != nil && f.Changed(flagImageAllowlist) {
		entries, _ := f.GetStringArray(flagImageAllowlist)
		policy, err := executor.ParseImageAdmissionPolicy(entries)
		if err != nil {
			return executor.ImageAdmissionPolicy{}, fmt.Errorf("--%s: %w", flagImageAllowlist, err)
		}
		return policy, nil
	}
	raw := getenv(envImageAllowlist)
	if raw == "" {
		return executor.ImageAdmissionPolicy{}, nil
	}
	policy, err := executor.ParseImageAdmissionPolicy([]string{raw})
	if err != nil {
		return executor.ImageAdmissionPolicy{}, fmt.Errorf("%s: %w", envImageAllowlist, err)
	}
	return policy, nil
}
