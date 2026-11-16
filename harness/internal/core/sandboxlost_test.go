package core

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// losableExecutor is an executor whose sandbox loss the test controls.
type losableExecutor struct {
	executor.Executor
	lost error
}

func (e *losableExecutor) SandboxLost() error { return e.lost }

var errTestSandboxLost = &executor.SandboxLostError{
	State: "sandbox pod ns/pod has failed: container agent was OOMKilled (exit code 137)",
	Err:   errors.New(`unable to upgrade connection: container not found ("agent")`),
}

func TestLoop_SandboxLost_EndsRunAfterTheTurnThatFoundIt(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &infiniteToolCallProvider{}
	exec := &losableExecutor{Executor: executor.NewNoneExecutor()}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	loop.Executor = exec
	registry := tool.NewRegistry()
	loop.Tools = registry
	registry.Register(&tool.Tool{
		Name:        "test_tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			exec.lost = errTestSandboxLost
			return "", exec.lost
		},
	})

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	check.Equal("sandbox_lost", runTrace.Outcome)
	check.Equal(errTestSandboxLost.Error(), runTrace.Error)
	check.Equal(1, prov.callCount, "the model is not asked to call tools on a lost sandbox")
	check.Equal(1, runTrace.Turns)
}

func TestLoop_SandboxLost_BeforeFirstTurnSkipsTheModel(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &infiniteToolCallProvider{}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	loop.Executor = &losableExecutor{Executor: executor.NewNoneExecutor(), lost: errTestSandboxLost}

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	check.Equal("sandbox_lost", runTrace.Outcome)
	check.Contains(runTrace.Error, "OOMKilled (exit code 137)")
	check.Equal(0, prov.callCount)
}

func TestLoop_SandboxLost_UsableSandboxDoesNotEndRun(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &multiCallProvider{}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	loop.Executor = &losableExecutor{Executor: executor.NewNoneExecutor()}

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	check.Equal("success", runTrace.Outcome)
}

// losingVerifier finds the sandbox lost while verifying.
type losingVerifier struct{ exec *losableExecutor }

func (v *losingVerifier) Verify(context.Context, verifier.VerifyContext) (*types.VerificationResult, error) {
	v.exec.lost = errTestSandboxLost
	return nil, v.exec.lost
}

func TestLoop_SandboxLost_FoundByTheVerifierEndsSandboxLost(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	exec := &losableExecutor{Executor: executor.NewNoneExecutor()}
	loop := buildTestLoop(nil)
	loop.Provider = &multiCallProvider{}
	loop.Executor = exec
	loop.Verifier = &losingVerifier{exec: exec}

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	check.Equal("sandbox_lost", runTrace.Outcome)
	check.Equal(errTestSandboxLost.Error(), runTrace.Error)
}

func TestLoop_SandboxLost_FoundBySetupEndsSandboxLost(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &infiniteToolCallProvider{}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	loop.Executor = &losableExecutor{Executor: executor.NewNoneExecutor(), lost: errTestSandboxLost}
	loop.Prompt = &failingPromptBuilder{err: errTestSandboxLost}

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.Error(err)

	check.Equal("sandbox_lost", runTrace.Outcome)
	check.Equal(errTestSandboxLost.Error(), runTrace.Error)
	check.Equal(0, prov.callCount)
}

func TestLoop_SandboxLost_CancellationOutranksTheLoss(t *testing.T) {
	must := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	loop := buildTestLoop(nil)
	loop.Provider = &infiniteToolCallProvider{}
	loop.Executor = &losableExecutor{Executor: executor.NewNoneExecutor(), lost: errTestSandboxLost}

	runTrace, err := loop.Run(ctx, buildTestConfig())
	must.NoError(err)
	must.Equal("cancelled", runTrace.Outcome)
}

func TestLoop_SandboxLost_ProviderErrorOnALiveSandboxStaysError(t *testing.T) {
	must := require.New(t)
	loop := buildTestLoop(nil)
	loop.Provider = &errorProvider{err: errors.New("provider down")}
	loop.Executor = &losableExecutor{Executor: executor.NewNoneExecutor()}

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	must.Equal("error", runTrace.Outcome)
}
