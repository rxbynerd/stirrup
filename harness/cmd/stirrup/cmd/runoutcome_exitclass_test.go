package cmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

const (
	// replayReadMainTurn is one recorded turn that reads a file, so a
	// MaxTurns=1 run ends at max_turns after the tool result.
	replayReadMainTurn = `{"kind":"turn_record","turn":1,"modelOutput":[{"type":"tool_use","id":"c1","name":"read_file","input":{"path":"main.go"}}]}` + "\n"
	// replayAnswerTurn is one recorded text-only turn, which hands the
	// run to the verifier.
	replayAnswerTurn = `{"kind":"turn_record","turn":1,"modelOutput":[{"type":"text","text":"done"}]}` + "\n"
)

// writeReplayWorkspace returns a workspace holding main.go and the path
// of a replay recording with the given JSONL content.
func writeReplayWorkspace(t *testing.T, recording string) (workspace, replayFile string) {
	t.Helper()
	must := require.New(t)
	workspace = t.TempDir()
	must.NoError(os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n"), 0o600))
	replayFile = filepath.Join(t.TempDir(), "recording.jsonl")
	must.NoError(os.WriteFile(replayFile, []byte(recording), 0o600))
	return workspace, replayFile
}

// replayHarnessConfig is a valid single-turn RunConfig served by the
// replay provider. A failing test-runner verifier ends a text-only turn
// at verification_failed; without one the turn ends the run at success.
func replayHarnessConfig(t *testing.T, recording string, failingVerifier bool) *types.RunConfig {
	t.Helper()
	workspace, replayFile := writeReplayWorkspace(t, recording)
	timeout := 30
	cfg := &types.RunConfig{
		RunID:            "cmd-exitclass-test",
		Mode:             "execution",
		Prompt:           "Read main.go.",
		Provider:         types.ProviderConfig{Type: "replay", ReplayFile: replayFile},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "replay", Model: "replay"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: workspace},
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "deny-side-effects"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"read_file"}},
		MaxTurns:         1,
		Timeout:          &timeout,
	}
	if failingVerifier {
		cfg.Verifier = types.VerifierConfig{Type: "test-runner", Command: "exit 1"}
	}
	return cfg
}

// blockingProviderServer holds every request open until its client goes
// away, signalling arrived on the first one. It stands in for a model
// that is mid-turn when the run is cancelled.
func blockingProviderServer(t *testing.T) (srv *httptest.Server, arrived <-chan struct{}) {
	t.Helper()
	ch := make(chan struct{})
	var once sync.Once
	srv = httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(ch) })
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

// TestRunWithConfig_NonSuccessOutcome_ExitsRunOutcomeClass pins that a
// harness run that completes with max_turns or verification_failed
// exits with the run-outcome class, not the default failure class,
// while still emitting its RunResult.
func TestRunWithConfig_NonSuccessOutcome_ExitsRunOutcomeClass(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome string
		config  func(t *testing.T) *types.RunConfig
	}{
		{
			name:    "max_turns",
			outcome: "max_turns",
			config: func(t *testing.T) *types.RunConfig {
				return replayHarnessConfig(t, replayReadMainTurn, false)
			},
		},
		{
			name:    "verification_failed",
			outcome: "verification_failed",
			config: func(t *testing.T) *types.RunConfig {
				return replayHarnessConfig(t, replayAnswerTurn, true)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			config := tc.config(t)

			stdoutDone := captureStdout(t)
			runErr := runWithConfig(config, runOptions{outputMode: "json"})
			stdout := stdoutDone()

			must.Error(runErr)
			check.Equal(exitRunOutcome, classifyExitCode(runErr), "err=%v", runErr)
			check.Equal(tc.outcome, decodeStirrupResult(t, stdout).Outcome)
		})
	}
}

// interruptAfterProviderArrives replaces the signal handler so the first
// provider request triggers the interrupt a SIGTERM would deliver.
func interruptAfterProviderArrives(t *testing.T, arrived <-chan struct{}) {
	t.Helper()
	interrupts := make(chan func(), 1)
	prev := setupSignalHandler
	setupSignalHandler = func(interrupt, _ func()) { interrupts <- interrupt }
	t.Cleanup(func() { setupSignalHandler = prev })
	go func() {
		<-arrived
		(<-interrupts)()
	}()
}

// TestRunWithConfig_SignalCancel_KeepsDefaultExitClass pins that a run
// cancelled by SIGINT/SIGTERM exits with the default class, because an
// orchestrator that preempts or evicts a pod sends the same signal and
// expects the Job to retry.
func TestRunWithConfig_SignalCancel_KeepsDefaultExitClass(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv, arrived := blockingProviderServer(t)
	interruptAfterProviderArrives(t, arrived)
	config := runOutcomeTestConfig(t, srv.URL)

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout := stdoutDone()

	must.Error(runErr)
	check.Equal(1, classifyExitCode(runErr), "err=%v", runErr)
	check.Equal("cancelled", decodeStirrupResult(t, stdout).Outcome)
}

// TestRunWithConfig_BuildFailure_KeepsDefaultExitClass pins that a
// failure before any run starts is not reported as a run outcome.
func TestRunWithConfig_BuildFailure_KeepsDefaultExitClass(t *testing.T) {
	must := require.New(t)
	config := replayHarnessConfig(t, replayReadMainTurn, false)
	config.Provider.ReplayFile = filepath.Join(t.TempDir(), "absent.jsonl")

	runErr := runWithConfig(config, runOptions{outputMode: "json"})

	must.ErrorContains(runErr, "building harness")
	must.Equal(1, classifyExitCode(runErr), "err=%v", runErr)
}

// replayJobTask is the wire form of replayHarnessConfig.
func replayJobTask(t *testing.T, recording string, failingVerifier bool) *pb.RunConfig {
	t.Helper()
	workspace, replayFile := writeReplayWorkspace(t, recording)
	task := replayJobConfig(t, replayFile)
	task.RunId = "job-exitclass-test"
	task.Mode = "execution"
	task.MaxTurns = 1
	task.Executor = &pb.ExecutorConfig{Type: "local", Workspace: workspace}
	if failingVerifier {
		task.Verifier = &pb.VerifierConfig{Type: "test-runner", Command: "exit 1"}
	}
	return task
}

// awaitRunJobError returns the error runJob exits with, failing the
// test when it does not return in time.
func awaitRunJobError(t *testing.T, errCh <-chan error) error {
	t.Helper()
	select {
	case err := <-errCh:
		return err
	case <-time.After(terminalWaitBudget):
		require.FailNow(t, "runJob did not return")
		return nil
	}
}

// TestRunJob_NonSuccessOutcome_ExitsRunOutcomeClass pins that a job run
// that completes with max_turns, verification_failed or an operator
// cancel from the control plane reports its outcome on done and exits
// with the run-outcome class, so a Kubernetes Job can tell it from a
// harness failure.
func TestRunJob_NonSuccessOutcome_ExitsRunOutcomeClass(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome string
		// plane builds the control plane and, for cancelled, arranges
		// the mid-run cancel event.
		plane func(t *testing.T) *scriptedControlPlane
	}{
		{
			name:    "max_turns",
			outcome: "max_turns",
			plane: func(t *testing.T) *scriptedControlPlane {
				return newScriptedControlPlane(replayJobTask(t, replayReadMainTurn, false), func(int) *pb.ControlEvent { return nil })
			},
		},
		{
			name:    "verification_failed",
			outcome: "verification_failed",
			plane: func(t *testing.T) *scriptedControlPlane {
				return newScriptedControlPlane(replayJobTask(t, replayAnswerTurn, true), func(int) *pb.ControlEvent { return nil })
			},
		},
		{
			name:    "cancelled",
			outcome: "cancelled",
			plane: func(t *testing.T) *scriptedControlPlane {
				t.Setenv("TEST_OPENAI_KEY", "test-key")
				provider, arrived := blockingProviderServer(t)
				task := followUpJobConfig(t, provider.URL, 30, 0)
				srv := newScriptedControlPlane(task, func(int) *pb.ControlEvent { return nil })
				go func() {
					<-arrived
					_ = srv.send(&pb.ControlEvent{Type: "cancel"})
				}()
				return srv
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			useTempMarkerPaths(t)
			t.Setenv("CONTROL_PLANE_SESSION_ID", "")
			srv := tc.plane(t)
			serveControlPlane(t, srv)

			errCh := make(chan error, 1)
			go func() { errCh <- runJob(jobCmd, nil) }()

			select {
			case stopReason := <-srv.dones:
				check.Equal(tc.outcome, stopReason, "done stop_reason")
			case <-time.After(terminalWaitBudget):
				must.FailNow("control plane never received a done event", "got %v", srv.events())
			}
			runErr := awaitRunJobError(t, errCh)

			must.Error(runErr)
			check.Equal(exitRunOutcome, classifyExitCode(runErr), "err=%v", runErr)
		})
	}
}

// TestRunJob_SignalCancel_KeepsDefaultExitClass pins that a job
// cancelled by SIGTERM exits with the default class, unlike a cancel
// event from the control plane, which exits with the run-outcome class.
func TestRunJob_SignalCancel_KeepsDefaultExitClass(t *testing.T) {
	must := require.New(t)
	useTempMarkerPaths(t)
	t.Setenv("CONTROL_PLANE_SESSION_ID", "")
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	provider, arrived := blockingProviderServer(t)
	interruptAfterProviderArrives(t, arrived)
	srv := newScriptedControlPlane(followUpJobConfig(t, provider.URL, 30, 0), func(int) *pb.ControlEvent { return nil })
	serveControlPlane(t, srv)

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()
	runErr := awaitRunJobError(t, errCh)

	must.ErrorContains(runErr, `run outcome "cancelled" is not success`)
	must.Equal(1, classifyExitCode(runErr), "err=%v", runErr)
}

// TestRunJob_BuildFailure_KeepsDefaultExitClass pins that a task whose
// components cannot be built, which the control plane sees as an error
// then done, exits with the default class rather than the run-outcome
// class.
func TestRunJob_BuildFailure_KeepsDefaultExitClass(t *testing.T) {
	must := require.New(t)
	useTempMarkerPaths(t)
	t.Setenv("CONTROL_PLANE_SESSION_ID", "")
	task := replayJobTask(t, replayReadMainTurn, false)
	task.Provider.ReplayFile = filepath.Join(t.TempDir(), "absent.jsonl")
	srv := newRecordingControlPlane(task)
	startControlPlane(t, srv)

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()
	runErr := awaitRunJobError(t, errCh)

	must.ErrorContains(runErr, "building harness")
	must.Equal(1, classifyExitCode(runErr), "err=%v", runErr)
}

// TestRunJob_MissingControlPlaneAddr_KeepsDefaultExitClass pins that a
// job that never reaches a control plane exits with the default class.
func TestRunJob_MissingControlPlaneAddr_KeepsDefaultExitClass(t *testing.T) {
	must := require.New(t)
	t.Setenv("CONTROL_PLANE_ADDR", "")

	err := runJob(jobCmd, nil)

	must.Error(err)
	must.Equal(1, classifyExitCode(err), "err=%v", err)
}
