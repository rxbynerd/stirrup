package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/harness/internal/resultsink"
	"github.com/rxbynerd/stirrup/harness/internal/workspaceexport"
	"github.com/rxbynerd/stirrup/types"
	"github.com/rxbynerd/stirrup/types/version"
)

var rootCmd = &cobra.Command{
	Use:     "stirrup",
	Short:   "A coding agent harness",
	Long:    "Stirrup is a coding agent harness with swappable components that can be composed via RunConfig.",
	Version: version.Full(),
	// NoArgs so a mistyped subcommand errors via Cobra's unknown-command
	// path rather than silently printing the orientation hint.
	Args: cobra.NoArgs,
	// Run (not RunE) so a bare invocation prints the short orientation
	// hint and exits 0; RunE would make Cobra append its full usage
	// block on error instead.
	Run: func(cmd *cobra.Command, _ []string) {
		printRootUsageHint(cmd.OutOrStdout())
	},
}

// Execute runs the root command. Called from main().
//
// SilenceErrors + SilenceUsage are set so a RunE failure prints only
// the error message below, not Cobra's full usage block. --help is
// unaffected since SilenceUsage only suppresses usage-on-error.
//
// Exit codes follow the scheme in docs/configuration.md#exit-codes;
// classifyExitCode encodes the mapping so it is unit-testable without
// reaching os.Exit.
func Execute() {
	rootCmd.SilenceErrors = true
	rootCmd.SilenceUsage = true
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(classifyExitCode(err))
	}
}

// generateRunID creates a simple run identifier from the current timestamp.
func generateRunID() string {
	return fmt.Sprintf("run-%d", time.Now().UnixNano())
}

// postRunEmitTimeout bounds the fresh context for emitRunOutput after
// the loop returns, since the loop's own context may already be
// cancelled by a stop signal. 10s mirrors bestEffortCancel in
// batchpoll.go: enough for one remote RPC, short enough to avoid an
// apparent hang.
const postRunEmitTimeout = 10 * time.Second

// postRunExportTimeout bounds the fresh context for exportWorkspace
// after the loop returns. Larger than postRunEmitTimeout to leave room
// for the GCS exporter's internal 5-minute upload ceiling plus setup.
const postRunExportTimeout = 6 * time.Minute

// printRunSummary writes a brief run summary to stderr. A nil RunTrace
// (loop produced no trace at all) prints a "no trace" line instead of
// dereferencing fields — the stderr counterpart to buildRunResult's
// "internal-error" sentinel.
func printRunSummary(runTrace *types.RunTrace) {
	if runTrace == nil {
		fmt.Fprintf(os.Stderr, "\n--- Run complete (no trace) ---\n")
		return
	}
	fmt.Fprintf(os.Stderr, "\n--- Run complete ---\n")
	fmt.Fprintf(os.Stderr, "Outcome: %s\n", runTrace.Outcome)
	fmt.Fprintf(os.Stderr, "Turns: %d\n", runTrace.Turns)
	fmt.Fprintf(os.Stderr, "Tokens: %d in / %d out\n", runTrace.TokenUsage.Input, runTrace.TokenUsage.Output)
	fmt.Fprintf(os.Stderr, "Tool calls: %d\n", len(runTrace.ToolCalls))
	fmt.Fprintf(os.Stderr, "Duration: %s\n", runTrace.CompletedAt.Sub(runTrace.StartedAt).Round(time.Millisecond))
	if len(runTrace.HookResults) > 0 {
		ran, failed := hookSummaryCounts(runTrace.HookResults)
		fmt.Fprintf(os.Stderr, "Hooks: %d run, %d failed\n", ran, failed)
	}
}

// hookSummaryCounts reports how many lifecycle hooks actually ran and
// how many of those failed. Entries with Skipped=true count toward
// neither: they never executed, and never set Error (see
// hook.ExecRunner), so counting Failed() alone is sufficient.
func hookSummaryCounts(results []types.HookExecution) (ran, failed int) {
	for _, r := range results {
		if r.Skipped {
			continue
		}
		ran++
		if r.Failed() {
			failed++
		}
	}
	return ran, failed
}

// buildRunResult constructs the small RunResult payload from the
// completed RunTrace; see types/result.go for the schema's stability
// rules. VerifierVerdict uses the optional pointer's presence to
// disambiguate "verifier passed silently" from "not configured".
// maxFinalAssistantTextBytes bounds only the returned RunResult copy —
// rt.FinalAssistantText and the trace emitters are untouched.
func buildRunResult(rt *types.RunTrace, maxFinalAssistantTextBytes int) types.RunResult {
	if rt == nil {
		// Every other code path returns a structurally valid trace, even
		// on cancellation, so nil means the loop produced none at all.
		// The "internal-error" sentinel (documented on RunResult.Outcome)
		// lets consumers detect this case explicitly rather than seeing
		// an empty Outcome/RunID/Turns combination.
		return types.RunResult{SchemaVersion: 1, Outcome: "internal-error"}
	}
	finalText, truncated := types.CapFinalAssistantText(rt.FinalAssistantText, maxFinalAssistantTextBytes)
	res := types.RunResult{
		SchemaVersion:               1,
		RunID:                       rt.ID,
		Outcome:                     rt.Outcome,
		Turns:                       rt.Turns,
		TokenUsage:                  rt.TokenUsage,
		DurationMs:                  rt.CompletedAt.Sub(rt.StartedAt).Milliseconds(),
		FinalAssistantText:          finalText,
		FinalAssistantTextTruncated: truncated,
	}
	if n := len(rt.VerificationResults); n > 0 {
		last := rt.VerificationResults[n-1]
		res.VerifierVerdict = &types.VerifierResult{
			Passed:   last.Passed,
			Feedback: last.Feedback,
		}
	}
	// hookSummaryCounts is nil-safe, resolving to 0 for a hookless run.
	_, res.HookFailures = hookSummaryCounts(rt.HookResults)
	return res
}

// runSuccessOutcomes is the closed set of RunTrace.Outcome values that
// count as a successful run for process exit-code purposes. See
// docs/configuration.md#exit-codes for why this is a binary
// success/non-success split, distinct from the eval suite's
// passed/failed/inconclusive taxonomy.
var runSuccessOutcomes = map[string]bool{
	"success": true,
}

// runOutcomeError reports whether rt's outcome falls outside
// runSuccessOutcomes, returning a descriptive error if so. Called at
// the tail of runWithConfig and runJob after every non-fatal post-run
// step has run, so a non-success outcome always yields a non-zero exit.
func runOutcomeError(rt *types.RunTrace) error {
	if rt == nil || runSuccessOutcomes[rt.Outcome] {
		return nil
	}
	return fmt.Errorf("run outcome %q is not success", rt.Outcome)
}

// newResultSink is the seam tests use to inject a stub ResultSink.
// Production code keeps the resultsink.NewResultSink factory.
var newResultSink = resultsink.NewResultSink

// emitRunResult builds and emits a RunResult through the configured
// resultSink. Failures are logged but never fatal — a transient sink
// error must not mask a successful run already reflected in the trace
// and stderr summary.
func emitRunResult(ctx context.Context, cfg *types.RunConfig, rt *types.RunTrace) {
	sink, err := newResultSink(cfg.ResultSink)
	if err != nil {
		slog.Warn("build resultSink", "err", err)
		return
	}
	if err := sink.Emit(ctx, buildRunResult(rt, cfg.ResultSink.ResolvedMaxFinalAssistantTextBytes())); err != nil {
		slog.Warn("emit resultSink", "err", err)
	}
}

// emitRunOutput dispatches the post-run summary surfaces selected by
// --output; see docs/configuration.md's `--output` flag entry for the
// text/json/none semantics. All emissions go through buildRunResult so
// a partial RunResult round-trips identically through every mode.
func emitRunOutput(ctx context.Context, cfg *types.RunConfig, rt *types.RunTrace, mode string) {
	if rt == nil {
		// Surfaces the no-trace case even when the output mode suppresses stderr.
		slog.Warn("emitRunOutput: nil RunTrace, RunResult will carry the internal-error sentinel", "mode", mode)
	}
	switch mode {
	case "json":
		// A configured stdout-json sink is swapped for "none" below so
		// emitRunResult does not produce a second STIRRUP_RESULT line.
		stdoutSink := resultsink.NewStdoutJSONSink()
		if err := stdoutSink.Emit(ctx, buildRunResult(rt, cfg.ResultSink.ResolvedMaxFinalAssistantTextBytes())); err != nil {
			slog.Warn("emit --output=json", "err", err)
		}
		if cfg.ResultSink == nil || cfg.ResultSink.Type == "stdout-json" {
			return
		}
		emitRunResult(ctx, cfg, rt)
	case "none":
		// The configured sink still fires for a non-stdout destination.
		if cfg.ResultSink == nil || cfg.ResultSink.Type == "" || cfg.ResultSink.Type == "none" || cfg.ResultSink.Type == "stdout-json" {
			return
		}
		emitRunResult(ctx, cfg, rt)
	case "text", "":
		printRunSummary(rt)
		emitRunResult(ctx, cfg, rt)
	default:
		slog.Warn("emitRunOutput: unrecognised mode, defaulting to text", "mode", mode)
		printRunSummary(rt)
		emitRunResult(ctx, cfg, rt)
	}
}

// newWorkspaceExporter is the seam tests use to inject a stub
// Exporter. Returning an Exporter (not *GCSExporter) keeps the type
// usable by future S3 / Azure implementations.
var newWorkspaceExporter = func() (workspaceexport.Exporter, error) {
	return workspaceexport.NewGCSExporter(workspaceexport.GCSExporterOptions{
		// gcp-workload-identity against the runtime metadata server.
		CredentialSource: credential.NewGoogleWorkloadIdentitySource(),
	})
}

// exportWorkspace tars + gzips the executor's workspace dir and
// uploads it to config.Executor.WorkspaceExportTo. No-op when the
// export field is empty. exportRequired controls error semantics: when
// true, an export failure is surfaced to the caller; when false, it is
// logged and the caller's exit code is unchanged.
func exportWorkspace(ctx context.Context, cfg *types.RunConfig, exportRequired bool) error {
	if cfg.Executor.WorkspaceExportTo == "" {
		return nil
	}
	// v1 supports only the GCS exporter.
	exp, err := newWorkspaceExporter()
	if err != nil {
		if exportRequired {
			return fmt.Errorf("build workspace exporter: %w", err)
		}
		slog.Warn("build workspace exporter", "err", err)
		return nil
	}

	workspaceDir := cfg.Executor.Workspace
	if workspaceDir == "" {
		// Mirrors the local executor's defaulting in factory.go.
		wd, _ := os.Getwd()
		workspaceDir = wd
	}

	if err := exp.Export(ctx, workspaceDir, cfg.Executor.WorkspaceExportTo); err != nil {
		if exportRequired {
			return fmt.Errorf("export workspace to %s: %w", cfg.Executor.WorkspaceExportTo, err)
		}
		slog.Warn("export workspace failed", "dest", cfg.Executor.WorkspaceExportTo, "err", err)
		return nil
	}
	return nil
}

// setupSignalHandler installs SIGINT/SIGTERM handlers that cancel the given context.
func setupSignalHandler(cancel context.CancelFunc) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\nReceived interrupt, shutting down...")
		cancel()
	}()
}

// shutdownCloseGrace bounds how long armShutdownWatchdog waits after a
// process shutdown signal before proactively closing the loop, even if
// Run() has not returned yet (e.g. a detached postRun lifecycle hook is
// still winding down). Chosen comfortably under the shortest documented
// orchestrator SIGKILL escalation (10s on Cloud Run and `docker stop`;
// see docs/cloud-run-jobs.md) so the executor's sandbox has a real
// chance to be torn down before the process is killed outright.
const shutdownCloseGrace = 5 * time.Second

// armShutdownWatchdog starts a background goroutine that proactively
// closes loop after shutdownCtx is done, unless the returned stop
// function is called first (Run() already returned; the caller's own
// `defer loop.Close()` handles teardown). Guards against the
// orchestrator's SIGKILL escalation firing before Run() returns, which
// would otherwise skip the deferred Close() entirely and orphan the
// executor's sandbox. Close() is safe to invoke more than once.
func armShutdownWatchdog(shutdownCtx context.Context, loop io.Closer, grace time.Duration) (stop func()) {
	runDone := make(chan struct{})
	go func() {
		select {
		case <-runDone:
			return
		case <-shutdownCtx.Done():
		}
		select {
		case <-runDone:
		case <-time.After(grace):
			if err := loop.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: proactive shutdown close: %v\n", err)
			}
		}
	}()
	return func() { close(runDone) }
}
