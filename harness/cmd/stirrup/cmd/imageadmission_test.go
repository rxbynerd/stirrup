package cmd

import (
	"os"
	"path/filepath"
	"testing"

	flag "github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

func imageAdmissionFlags(t *testing.T, args ...string) *flag.FlagSet {
	t.Helper()
	f := flag.NewFlagSet("test", flag.ContinueOnError)
	addImageAdmissionFlag(f)
	require.New(t).NoError(f.Parse(args))
	return f
}

func envOf(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestResolveImageAdmissionPolicy_NothingSetIsUnconfigured(t *testing.T) {
	must := require.New(t)
	policy, err := resolveImageAdmissionPolicy(imageAdmissionFlags(t), envOf(nil))
	must.NoError(err)
	must.False(policy.Configured())
}

func TestResolveImageAdmissionPolicy_ReadsEnvironment(t *testing.T) {
	must := require.New(t)
	policy, err := resolveImageAdmissionPolicy(nil, envOf(map[string]string{
		envImageAllowlist: "us-docker.pkg.dev/acme/, ghcr.io/acme/*",
	}))
	must.NoError(err)
	must.Equal([]string{"us-docker.pkg.dev/acme/", "ghcr.io/acme/*"}, policy.Patterns())
}

func TestResolveImageAdmissionPolicy_FlagReplacesEnvironment(t *testing.T) {
	must := require.New(t)
	f := imageAdmissionFlags(t, "--image-allowlist", "harbor.acme.internal/sandboxes/", "--image-allowlist", "ghcr.io/acme/,localhost/dev/")
	policy, err := resolveImageAdmissionPolicy(f, envOf(map[string]string{envImageAllowlist: "docker.io/library/*"}))
	must.NoError(err)
	must.Equal([]string{"harbor.acme.internal/sandboxes/", "ghcr.io/acme/", "localhost/dev/"}, policy.Patterns())
}

func TestResolveImageAdmissionPolicy_InvalidPatternNamesItsSource(t *testing.T) {
	t.Run("flag", func(t *testing.T) {
		must := require.New(t)
		_, err := resolveImageAdmissionPolicy(imageAdmissionFlags(t, "--image-allowlist", "ghcr.io"), envOf(nil))
		must.ErrorContains(err, "--image-allowlist")
		must.ErrorContains(err, `"ghcr.io/"`)
	})
	t.Run("environment", func(t *testing.T) {
		must := require.New(t)
		_, err := resolveImageAdmissionPolicy(nil, envOf(map[string]string{envImageAllowlist: "ghcr.io/acme/**"}))
		must.ErrorContains(err, envImageAllowlist)
		must.ErrorContains(err, `"**"`)
	})
}

func TestLoadRunConfigFile_RegistryAllowlistNamesReplacement(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "run.json")
	must.NoError(os.WriteFile(path, []byte(`{"executor":{"type":"container","image":"ubuntu","registryAllowlist":["ghcr.io/acme/*"]}}`), 0o600))

	_, err := loadRunConfigFile(path)
	must.ErrorContains(err, "executor.registryAllowlist has been removed")
	must.ErrorContains(err, "STIRRUP_IMAGE_ALLOWLIST")
	must.Equal(exitParse, classifyExitCode(err))
}
