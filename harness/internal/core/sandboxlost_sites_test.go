package core

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

// losingLoop returns a loop over an executor whose loss the test controls.
func losingLoop(prov provider.ProviderAdapter) (*AgenticLoop, *losableExecutor) {
	exec := &losableExecutor{Executor: executor.NewNoneExecutor()}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	loop.Executor = exec
	return loop, exec
}

func requireSandboxLost(t *testing.T, runTrace *types.RunTrace) {
	t.Helper()
	must := require.New(t)
	must.Equal("sandbox_lost", runTrace.Outcome)
	must.Equal(errTestSandboxLost.Error(), runTrace.Error)
}

func TestLoop_SandboxLost_FoundByThePreRunHookEndsSandboxLost(t *testing.T) {
	must := require.New(t)
	prov := &infiniteToolCallProvider{}
	loop, exec := losingLoop(prov)
	loop.Hooks = &fakeHookRunner{
		preErr: errors.New("clone failed"),
		onPre:  func() { exec.lost = errTestSandboxLost },
	}

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.Error(err)

	requireSandboxLost(t, runTrace)
	must.Equal(0, prov.callCount)
}

func TestLoop_SandboxLost_FoundByThePostRunHookEndsSandboxLost(t *testing.T) {
	must := require.New(t)
	loop, exec := losingLoop(endTurnProvider())
	loop.Hooks = &fakeHookRunner{
		postErr: errors.New("notify failed"),
		onPost:  func(context.Context) { exec.lost = errTestSandboxLost },
	}

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	requireSandboxLost(t, runTrace)
}

// losingFinaliseGit loses the sandbox while taking the final checkpoint.
type losingFinaliseGit struct {
	fakeFinaliseGit
	exec *losableExecutor
}

func (g *losingFinaliseGit) Finalise(context.Context, string) (*types.GitHandoff, error) {
	g.exec.lost = errTestSandboxLost
	return nil, errors.New("git commit failed")
}

func TestLoop_SandboxLost_FoundByTheFinalGitCheckpointEndsSandboxLost(t *testing.T) {
	must := require.New(t)
	loop, exec := losingLoop(endTurnProvider())
	loop.Git = &losingFinaliseGit{exec: exec}

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	requireSandboxLost(t, runTrace)
}

func TestLoop_SandboxLost_FoundByTheWorkspaceExportEndsSandboxLost(t *testing.T) {
	must := require.New(t)
	loop, exec := losingLoop(endTurnProvider())
	loop.Handoff = &fakeHandoff{
		err:       errors.New("tar failed"),
		onHandoff: func(context.Context) { exec.lost = errTestSandboxLost },
	}

	runTrace, err := loop.Run(context.Background(), exportConfig(nil))
	must.NoError(err)

	requireSandboxLost(t, runTrace)
	must.NotNil(runTrace.WorkspaceExport)
	must.Equal(types.ExportStatusFailed, runTrace.WorkspaceExport.Status)
}

func TestLoop_SandboxLost_FoundOnTheLastTurnEndsSandboxLost(t *testing.T) {
	must := require.New(t)
	loop, exec := losingLoop(&infiniteToolCallProvider{})
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
	config := buildTestConfig()
	config.MaxTurns = 1

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)

	requireSandboxLost(t, runTrace)
	must.Equal(1, runTrace.Turns)
}

func TestLoop_SandboxLost_FoundInAStalledBatchEndsSandboxLost(t *testing.T) {
	must := require.New(t)
	prov := &toolLoopProvider{input: func(int) map[string]any { return map[string]any{} }}
	loop, exec := losingLoop(prov)
	registry := tool.NewRegistry()
	loop.Tools = registry
	var calls int
	registry.Register(&tool.Tool{
		Name:        writeProbeTool,
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			calls++
			if calls == 2*stallRepeats {
				exec.lost = errTestSandboxLost
			}
			return "same result", nil
		},
	})

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	requireSandboxLost(t, runTrace)
	must.Equal(2*stallRepeats, calls, "the batch that tripped the stall detector found the loss")
}

func TestAttributeSandboxLoss_MapsEverySymptomToSandboxLost(t *testing.T) {
	for _, symptom := range []string{"error", "setup_failed", "verification_error", "hook_failed", "handoff_failed"} {
		t.Run(symptom, func(t *testing.T) {
			must := require.New(t)
			loop, exec := losingLoop(nil)
			exec.lost = errTestSandboxLost

			must.Equal("sandbox_lost", loop.attributeSandboxLoss(symptom))
			must.Equal(runFailure{outcome: "sandbox_lost", text: errTestSandboxLost.Error()}, loop.failure)
		})
	}
}

func TestAttributeSandboxLoss_KeepsOutcomesTheLoopDecided(t *testing.T) {
	for _, outcome := range []string{"success", "max_turns", "verification_failed", "timeout", "cancelled", "budget_exceeded"} {
		t.Run(outcome, func(t *testing.T) {
			must := require.New(t)
			loop, exec := losingLoop(nil)
			exec.lost = errTestSandboxLost
			loop.noteFailure(outcome, "the loop's own failure text")

			must.Equal(outcome, loop.attributeSandboxLoss(outcome))
			must.Equal(runFailure{outcome: outcome, text: "the loop's own failure text"}, loop.failure)
		})
	}
}

func TestAttributeSandboxLoss_KeepsSymptomWhenSandboxIsUsable(t *testing.T) {
	must := require.New(t)
	loop, _ := losingLoop(nil)

	must.Equal("hook_failed", loop.attributeSandboxLoss("hook_failed"))
}
