package cmd

import (
	"context"
	"fmt"
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
}

// Execute runs the root command. Called from main().
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// generateRunID creates a simple run identifier from the current timestamp.
func generateRunID() string {
	return fmt.Sprintf("run-%d", time.Now().UnixNano())
}

// printRunSummary writes a brief run summary to stderr.
func printRunSummary(runTrace *types.RunTrace) {
	fmt.Fprintf(os.Stderr, "\n--- Run complete ---\n")
	fmt.Fprintf(os.Stderr, "Outcome: %s\n", runTrace.Outcome)
	fmt.Fprintf(os.Stderr, "Turns: %d\n", runTrace.Turns)
	fmt.Fprintf(os.Stderr, "Tokens: %d in / %d out\n", runTrace.TokenUsage.Input, runTrace.TokenUsage.Output)
	fmt.Fprintf(os.Stderr, "Tool calls: %d\n", len(runTrace.ToolCalls))
	fmt.Fprintf(os.Stderr, "Duration: %s\n", runTrace.CompletedAt.Sub(runTrace.StartedAt).Round(time.Millisecond))
}

// buildRunResult constructs the small RunResult payload from the
// completed RunTrace. The mapping is straightforward; see
// types/result.go for the schema's stability rules.
//
// FinalAssistantText is left empty for v1: the loop computes it (see
// loop.go::lastAssistantText) but does not thread it onto RunTrace, and
// the brief is explicit about keeping schema additions to Chunk A.
// TODO(#164): expose the loop's last assistant text so the resultSink
// can carry the run's *answer* as well as its bookkeeping.
//
// VerifierVerdict is populated from the most recent VerificationResult
// when one exists. Treating an empty Feedback string as "no verifier
// ran" would conflate "verifier passed silently" with "verifier was
// not configured"; the wire shape uses presence of the optional
// VerifierResult pointer to disambiguate.
func buildRunResult(rt *types.RunTrace) types.RunResult {
	if rt == nil {
		return types.RunResult{SchemaVersion: 1}
	}
	res := types.RunResult{
		SchemaVersion: 1,
		RunID:         rt.ID,
		Outcome:       rt.Outcome,
		Turns:         rt.Turns,
		TokenUsage:    rt.TokenUsage,
		DurationMs:    rt.CompletedAt.Sub(rt.StartedAt).Milliseconds(),
	}
	if n := len(rt.VerificationResults); n > 0 {
		last := rt.VerificationResults[n-1]
		res.VerifierVerdict = &types.VerifierResult{
			Passed:   last.Passed,
			Feedback: last.Feedback,
		}
	}
	return res
}

// emitRunResult builds and emits a RunResult through the configured
// resultSink. Failures are logged but never fatal — the trace and the
// stderr summary already carry the run's outcome, so a transient sink
// error must not mask a successful run. Called from runWithConfig and
// runJob after the stderr summary so the structured line is the last
// thing on stdout (per the issue's Cloud Logging extraction contract).
func emitRunResult(ctx context.Context, cfg *types.RunConfig, rt *types.RunTrace) {
	sink, err := resultsink.NewResultSink(cfg.ResultSink)
	if err != nil {
		slog.Warn("build resultSink", "err", err)
		return
	}
	if err := sink.Emit(ctx, buildRunResult(rt)); err != nil {
		slog.Warn("emit resultSink", "err", err)
	}
}

// exportWorkspace tars + gzips the executor's workspace dir and
// uploads it to config.Executor.WorkspaceExportTo via the workspace
// exporter. No-op when the export field is empty.
//
// exportRequired controls error semantics: when true, the caller
// surfaces the export failure (a deployment that demands the artifact
// for downstream automation should exit non-zero so the operator
// notices); when false, the failure is logged with slog and the
// caller's exit code is unchanged (the trace and stderr summary still
// reflect the run's actual outcome).
//
// Returns nil when the workspace dir is missing or empty — the
// exporter treats those as silent skips, and that semantic is
// inherited here so a no-op executor (e.g. an api executor that
// reports nothing on disk) doesn't fail a run that opted into export.
func exportWorkspace(ctx context.Context, cfg *types.RunConfig, exportRequired bool) error {
	if cfg.Executor.WorkspaceExportTo == "" {
		return nil
	}
	// v1 supports only the GCS exporter; the field is validated at
	// config load to be a gs:// URI, so reaching this code path with
	// any other scheme indicates a logic regression in the validator.
	exp, err := workspaceexport.NewGCSExporter(workspaceexport.GCSExporterOptions{
		// Default credential source: gcp-workload-identity against
		// the runtime metadata server (the Cloud Run / GKE shape
		// this targets). Future work: thread an explicit
		// CredentialConfig through ExecutorConfig if non-GCP
		// runtimes need to export.
		CredentialSource: credential.NewGoogleWorkloadIdentitySource(),
	})
	if err != nil {
		if exportRequired {
			return fmt.Errorf("build workspace exporter: %w", err)
		}
		slog.Warn("build workspace exporter", "err", err)
		return nil
	}

	workspaceDir := cfg.Executor.Workspace
	if workspaceDir == "" {
		// An unset Workspace field means "current working directory" —
		// mirrors the local executor's defaulting in factory.go.
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
