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
		// A nil RunTrace means the loop produced no trace at all —
		// every other code path returns a structurally valid one,
		// even on cancellation. Returning RunResult{SchemaVersion: 1}
		// would surface an empty Outcome/RunID/Turns combination that
		// a downstream consumer cannot distinguish from a real run
		// that completed with an empty outcome. The "internal-error"
		// sentinel is documented on RunResult.Outcome and lets
		// consumers detect the no-trace case explicitly.
		return types.RunResult{SchemaVersion: 1, Outcome: "internal-error"}
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
// error must not mask a successful run. Called from runWithConfig (via
// emitRunOutput) and runJob after the stderr summary so the structured
// line is the last thing on stdout (per the issue's Cloud Logging
// extraction contract).
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

// emitRunOutput dispatches the post-run summary surfaces selected by
// --output (issue #242). Three modes are supported:
//
//   - "text" (or "" for backward compatibility with callers that bypass
//     the flag — only the job command today): print the human-readable
//     stderr summary AND emit through the configured resultSink. This
//     matches the pre-#242 behaviour exactly.
//   - "json": skip the stderr summary; emit a single STIRRUP_RESULT line
//     on stdout. When resultSink.type=stdout-json is also configured,
//     the line is emitted once — the flag wins because it is the more
//     explicit signal. When the configured sink targets a different
//     surface (a future gcp-pubsub or gcs adapter), that sink also fires
//     because it represents a separate channel with its own intent.
//   - "none": suppress the stderr summary AND any emission that would
//     write to stdout. The configured sink still fires when it targets a
//     non-stdout surface, on the same "different channel, different
//     intent" rationale.
//
// All emissions go through buildRunResult so a partial RunResult (e.g.
// a cancelled run) round-trips identically through every mode. Sink
// failures are logged via emitRunResult and never fatal.
func emitRunOutput(ctx context.Context, cfg *types.RunConfig, rt *types.RunTrace, mode string) {
	switch mode {
	case "json":
		// Always emit a STIRRUP_RESULT line to stdout. When the
		// configured sink is also stdout-json, swap it out for "none"
		// before delegating so emitRunResult does not produce a second
		// line. Any non-stdout sink stays untouched (different channel).
		stdoutSink := resultsink.NewStdoutJSONSink()
		if err := stdoutSink.Emit(ctx, buildRunResult(rt)); err != nil {
			slog.Warn("emit --output=json", "err", err)
		}
		if cfg.ResultSink == nil || cfg.ResultSink.Type == "stdout-json" {
			return
		}
		emitRunResult(ctx, cfg, rt)
	case "none":
		// Skip the stderr summary. The configured sink is invoked only
		// when it targets a non-stdout destination so --output=none
		// genuinely suppresses every stdout / stderr summary surface
		// regardless of which sink the run config selected.
		if cfg.ResultSink == nil || cfg.ResultSink.Type == "" || cfg.ResultSink.Type == "none" || cfg.ResultSink.Type == "stdout-json" {
			return
		}
		emitRunResult(ctx, cfg, rt)
	default:
		printRunSummary(rt)
		emitRunResult(ctx, cfg, rt)
	}
}

// newWorkspaceExporter is the seam tests use to inject a stub
// Exporter. Production code keeps the default factory; tests overwrite
// the variable for the duration of a test and restore it on cleanup.
// Returning an Exporter (not *GCSExporter) keeps the type usable by
// future S3 / Azure implementations without further indirection.
var newWorkspaceExporter = func() (workspaceexport.Exporter, error) {
	return workspaceexport.NewGCSExporter(workspaceexport.GCSExporterOptions{
		// Default credential source: gcp-workload-identity against
		// the runtime metadata server (the Cloud Run / GKE shape
		// this targets). Future work: thread an explicit
		// CredentialConfig through ExecutorConfig if non-GCP
		// runtimes need to export.
		CredentialSource: credential.NewGoogleWorkloadIdentitySource(),
	})
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
