package core

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// requireBuildFailure asserts err is a BuildError with reason and step.
func requireBuildFailure(t *testing.T, err error, reason, step string) {
	t.Helper()
	must := require.New(t)
	var built *BuildError
	must.ErrorAs(err, &built)
	must.Equal(&types.BuildFailure{
		Reason: reason,
		Phase:  types.SessionPhase{Phase: types.SessionPhaseConstructing, Step: step},
	}, built.Failure())
}

func TestClassifyBuildError_HostFileWinsOverMarkedClass(t *testing.T) {
	must := require.New(t)
	err := markBuildFailure(types.BuildFailureCredential, &HostFileError{Field: "provider.gcpCredentialsFile", Err: errors.New("no such file")})

	classified := classifyBuildError(err, "")

	requireBuildFailure(t, classified, types.BuildFailureHostFile, "")
	must.Equal(err.Error(), classified.Error(), "classification does not change the text")
	must.Same(classified, classifyBuildError(classified, types.SessionStepSandboxCreate), "a classified error is not wrapped again")
}

func TestClassifyBuildError_UsesExecutorStartupClass(t *testing.T) {
	err := &executor.StartupError{Reason: types.BuildFailureAdmissionMismatch, Err: errors.New("admitted pod: spec.runtimeClassName")}

	requireBuildFailure(t, classifyBuildError(err, types.SessionStepSandboxReady), types.BuildFailureAdmissionMismatch, types.SessionStepSandboxReady)
	requireBuildFailure(t, classifyBuildError(errors.New("mcp: dial"), ""), types.BuildFailureUnclassified, "")
}

func TestBuildLoop_InvalidConfigIsConfigInvalid(t *testing.T) {
	_, err := BuildLoop(context.Background(), &types.RunConfig{Mode: "execution"})

	requireBuildFailure(t, err, types.BuildFailureConfigInvalid, "")
}

func TestBuildLoop_RefusedImageIsSandboxImage(t *testing.T) {
	config := buildTestConfig()
	config.Executor = types.ExecutorConfig{Type: "container", Image: "docker.io/someuser/app", Workspace: t.TempDir()}

	_, err := BuildLoop(context.Background(), config, WithImageAdmission(mustImagePolicy(t, "ghcr.io/acme/")))

	requireBuildFailure(t, err, types.BuildFailureSandboxImage, "")
}

func TestBuildLoop_MalformedImageIsConfigInvalid(t *testing.T) {
	config := buildTestConfig()
	config.Executor = types.ExecutorConfig{Type: "container", Image: "ghcr.io/acme/", Workspace: t.TempDir()}

	_, err := BuildLoop(context.Background(), config)

	requireBuildFailure(t, err, types.BuildFailureConfigInvalid, "")
}

func TestBuildLoop_UnresolvedAPIKeyIsCredential(t *testing.T) {
	config := buildTestConfig()
	t.Setenv("TEST", "")

	_, err := BuildLoop(context.Background(), config)

	requireBuildFailure(t, err, types.BuildFailureCredential, "")
}

func TestBuildLoop_MissingReplayFileIsHostFile(t *testing.T) {
	config := buildTestConfig()
	config.Provider = types.ProviderConfig{Type: "replay", ReplayFile: filepath.Join(t.TempDir(), "missing.jsonl")}
	config.ModelRouter.Provider = "replay"

	_, err := BuildLoop(context.Background(), config)

	requireBuildFailure(t, err, types.BuildFailureHostFile, "")
}

func TestBuildLoopWithTransport_DeclinedTokenExchangeNamesItsStep(t *testing.T) {
	sock, _, cleanupEngine := fakeDockerEngine(t)
	defer cleanupEngine()
	t.Setenv("DOCKER_HOST", "unix://"+sock)
	tp := &fakeControlPlaneTransport{respondIsError: true, respondReason: "no issuer configured"}

	_, err := BuildLoopWithTransport(context.Background(), sandboxIdentityFailClosedConfig(t, "typed-token-decline"), tp)

	requireBuildFailure(t, err, types.BuildFailureTokenExchange, types.SessionStepTokenExchange)
}

func TestBuildLoop_ExecutorStartupFailureNamesTheStepReached(t *testing.T) {
	prev := buildExecutorFn
	buildExecutorFn = func(_ context.Context, _ types.ExecutorConfig, _ security.SecretStore, _ *security.SecurityLogger, _ []executor.EnvPair, life executor.Lifecycle) (executor.Executor, error) {
		life.StartupProgress(types.SessionStepSandboxReady)
		return nil, &executor.StartupError{Reason: types.BuildFailureSandboxImage, Err: errors.New("pod not ready: ImagePullBackOff")}
	}
	t.Cleanup(func() { buildExecutorFn = prev })
	t.Setenv("TEST", "unused")
	config := buildTestConfig()
	config.Executor = types.ExecutorConfig{Type: "container", Image: "ghcr.io/x/missing:1", Workspace: t.TempDir(), Network: &types.NetworkConfig{Mode: "none"}}

	_, err := BuildLoop(context.Background(), config)

	requireBuildFailure(t, err, types.BuildFailureSandboxImage, types.SessionStepSandboxReady)
}
