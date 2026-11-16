package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	flag "github.com/spf13/pflag"
	"github.com/stretchr/testify/require"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/harness/internal/core"
	"github.com/rxbynerd/stirrup/types"
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

func TestRunJob_ImageAdmissionPolicyFromEnvironmentRefusesTask(t *testing.T) {
	must := require.New(t)
	useTempMarkerPaths(t)
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	t.Setenv("CONTROL_PLANE_SESSION_ID", "")
	t.Setenv(envImageAllowlist, "ghcr.io/acme/")
	provider, requests := startOpenAIStub(t)

	task := followUpJobConfig(t, provider.URL, 30, 0)
	task.Executor = &pb.ExecutorConfig{
		Type:      "container",
		Image:     "docker.io/someuser/app:latest",
		Workspace: t.TempDir(),
		Network:   &pb.NetworkConfig{Mode: "none"},
	}
	srv := newRecordingControlPlane(task)
	startControlPlane(t, srv)

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()

	select {
	case <-srv.doneCh:
	case <-time.After(terminalWaitBudget):
		must.FailNow("control plane never received a done event for the refused image")
	}
	select {
	case runErr := <-errCh:
		must.ErrorContains(runErr, "not admitted by the image admission policy")
	case <-time.After(terminalWaitBudget):
		must.FailNow("runJob did not return after signalling the control plane")
	}
	errEvent := srv.findEvent("error")
	must.NotNil(errEvent, "no error event received; got %v", srv.events())
	must.Contains(errEvent.Message, "not admitted by the image admission policy")
	must.Equal(int32(0), requests.Load(), "no provider request may be made for a refused image")
}

func TestRunHarness_DryRunReceivesImageAdmissionPolicy(t *testing.T) {
	must := require.New(t)
	data, err := json.Marshal(baseFileConfig())
	must.NoError(err)
	path := filepath.Join(t.TempDir(), "run.json")
	must.NoError(os.WriteFile(path, data, 0o600))
	t.Setenv(envImageAllowlist, "docker.io/library/*")

	var got core.PreflightOptions
	orig := preflightFn
	preflightFn = func(_ context.Context, _ *types.RunConfig, opts core.PreflightOptions) (*core.PreflightReport, error) {
		got = opts
		return okReport(), nil
	}
	t.Cleanup(func() { preflightFn = orig })

	cmd, _, _ := dryRunTestCmd()
	must.NoError(cmd.Flags().Set("config", path))
	must.NoError(cmd.Flags().Set("dry-run", "true"))
	must.NoError(cmd.Flags().Set(flagImageAllowlist, "ghcr.io/acme/"))

	must.NoError(runHarness(cmd, nil))
	must.Equal([]string{"ghcr.io/acme/"}, got.ImageAdmission.Patterns())
}
