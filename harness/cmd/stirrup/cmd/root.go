package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/stirrup/harness/internal/core"
	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/harness/internal/debugbuild"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/resultsink"
	"github.com/rxbynerd/stirrup/harness/internal/workspaceexport"
	"github.com/rxbynerd/stirrup/types"
	"github.com/rxbynerd/stirrup/types/version"
)

var rootCmd = &cobra.Command{
	Use:   "stirrup",
	Short: "A coding agent harness",
	Long:  "Stirrup is a coding agent harness with swappable components that can be composed via RunConfig.",
	// VersionSuffix is "+debug" only on a binary built with -tags
	// stirrupdebug, so `stirrup --version` is unmistakable on a debug
	// build. See docs/security.md#debug-builds.
	Version: version.Full() + debugbuild.VersionSuffix(),
	// NoArgs so a mistyped subcommand errors via Cobra's unknown-command
	// path rather than silently printing the orientation hint.
	Args: cobra.NoArgs,
	// Every subcommand inherits this hook, so the process-default slog
	// logger is scrubbed before any code path can use it.
	PersistentPreRun: func(cmd *cobra.Command, _ []string) {
		slog.SetDefault(newScrubbedTextLogger(cmd.ErrOrStderr(), slog.LevelInfo))
	},
	// Run (not RunE) so a bare invocation prints the short orientation
	// hint and exits 0; RunE would make Cobra append its full usage
	// block on error instead.
	Run: func(cmd *cobra.Command, _ []string) {
		printRootUsageHint(cmd.OutOrStdout())
	},
}

// newScrubbedTextLogger returns a text-format logger writing to w whose
// messages and attribute values pass through the secret scrubber.
func newScrubbedTextLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(observability.NewScrubHandler(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})))
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

// runTimeoutFor resolves RunConfig.Timeout as a duration; zero when
// unset or non-positive (the job path validates before this, the CLI
// path defaults it, so zero only means "no budget" for callers that
// deliberately omit it).
func runTimeoutFor(cfg *types.RunConfig) time.Duration {
	if cfg.Timeout == nil || *cfg.Timeout <= 0 {
		return 0
	}
	return time.Duration(*cfg.Timeout) * time.Second
}

// cliSessionFollowUps is the number of follow-up runs, each preceded by
// a full idle grace window, that `stirrup harness` budgets for a
// session on top of the primary run. `stirrup job` relies on the
// orchestrator's deadline instead; the CLI has none behind it, so the
// session must bound itself.
const cliSessionFollowUps = 10

// cliSessionBudget is the wall-clock bound on a `stirrup harness`
// session once the primary run has completed: zero (no bound) without
// a follow-up window, otherwise room for cliSessionFollowUps follow-up
// runs each waited for through a full grace window.
func cliSessionBudget(runTimeout time.Duration, graceSecs int) time.Duration {
	if graceSecs <= 0 {
		return 0
	}
	return cliSessionFollowUps * (runTimeout + time.Duration(graceSecs)*time.Second)
}

// withRunTimeout derives one run's context from the cancel-only
// parent: a fresh deadline when timeout is positive, plain
// cancellation otherwise.
func withRunTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(parent, timeout)
	}
	return context.WithCancel(parent)
}

// postRunEmitTimeout bounds the fresh context for emitRunOutput after
// the loop returns, since the loop's own context may already be
// cancelled by a stop signal. 10s mirrors bestEffortCancel in
// batchpoll.go: enough for one remote RPC, short enough to avoid an
// apparent hang.
const postRunEmitTimeout = 10 * time.Second

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
	if runTrace.Error != "" {
		fmt.Fprintf(os.Stderr, "Error: %s\n", runTrace.Error)
	}
	fmt.Fprintf(os.Stderr, "Turns: %d\n", runTrace.Turns)
	fmt.Fprintf(os.Stderr, "Tokens: %d in%s / %d out%s\n",
		runTrace.TokenUsage.Input, inputTokenDetail(runTrace.TokenUsage.CacheRead, runTrace.TokenUsage.CacheWrite),
		runTrace.TokenUsage.Output, outputTokenDetail(runTrace.TokenUsage.Reasoning))
	fmt.Fprintf(os.Stderr, "Tool calls: %d\n", len(runTrace.ToolCalls))
	fmt.Fprintf(os.Stderr, "Duration: %s\n", runTrace.CompletedAt.Sub(runTrace.StartedAt).Round(time.Millisecond))
	if len(runTrace.HookResults) > 0 {
		ran, failed := hookSummaryCounts(runTrace.HookResults)
		fmt.Fprintf(os.Stderr, "Hooks: %d run, %d failed\n", ran, failed)
	}
	if runTrace.CommandOutputArchive != "" {
		fmt.Fprintf(os.Stderr, "Command output archive: %s\n", runTrace.CommandOutputArchive)
	}
	if r := runTrace.WorkspaceExport; r != nil {
		fmt.Fprintf(os.Stderr, "Workspace export: %s (%s)\n", r.Status, r.URI)
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
		StructuredOutput:            rt.StructuredOutput,
		Error:                       rt.Error,
		CommandOutputArchive:        rt.CommandOutputArchive,
		Git:                         rt.Git,
		WorkspaceExport:             rt.WorkspaceExport,
		DegradedCapabilities:        rt.DegradedCapabilities,
	}
	if n := len(rt.VerificationResults); n > 0 {
		last := rt.VerificationResults[n-1]
		res.VerifierVerdict = &types.VerifierResult{
			Attempt:  last.Attempt,
			Passed:   last.Passed,
			Feedback: last.Feedback,
			Error:    last.Error,
		}
		res.Error = verificationOutcomeError(rt.Outcome, last)
	}
	// hookSummaryCounts is nil-safe, resolving to 0 for a hookless run.
	_, res.HookFailures = hookSummaryCounts(rt.HookResults)
	return res
}

// verificationOutcomeError is RunResult.Error for a run that the
// verifier ended: the last attempt's verifier error or feedback. Empty
// for any other outcome.
func verificationOutcomeError(outcome string, last types.VerificationResult) string {
	switch outcome {
	case "verification_error":
		if last.Error != "" {
			return last.Error
		}
		return "verifier error"
	case "verification_failed":
		if last.Feedback != "" {
			return last.Feedback
		}
		return "verification failed"
	}
	return ""
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

// exportWorkspace archives the run's workspace through exec, so the
// tarball reflects the sandbox rather than the harness host, uploads it to
// dest and returns the exporter's receipt.
func exportWorkspace(ctx context.Context, cfg *types.RunConfig, exec executor.Executor, dest string) (types.ExportReceipt, error) {
	failed := types.ExportReceipt{URI: dest, Status: types.ExportStatusFailed}
	executorType := cfg.Executor.Type
	if executorType == "" {
		executorType = "local"
	}
	archiver, ok := exec.(executor.WorkspaceArchiver)
	if !ok {
		return failed, fmt.Errorf("export workspace to %s: the %s executor cannot archive its workspace", dest, executorType)
	}
	// v1 supports only the GCS exporter.
	exp, err := newWorkspaceExporter()
	if err != nil {
		return failed, fmt.Errorf("build workspace exporter: %w", err)
	}

	receipt, err := exp.Export(ctx, archiver, dest)
	if err != nil {
		return receipt, fmt.Errorf("export workspace from %s executor to %s: %w", executorType, dest, err)
	}
	return receipt, nil
}

// workspaceHandoff is the core.WorkspaceHandoff `stirrup harness` and
// `stirrup job` inject: it exports each run's workspace to the
// configured destination, and each follow-up run to its own
// followUpExportURI so successive runs never overwrite one another.
type workspaceHandoff struct {
	primaryRunID string
}

var _ core.WorkspaceHandoff = workspaceHandoff{}

func (h workspaceHandoff) Handoff(ctx context.Context, cfg *types.RunConfig, exec executor.Executor) (*types.ExportReceipt, error) {
	dest := cfg.Executor.WorkspaceExportTo
	if cfg.RunID != h.primaryRunID {
		dest = followUpExportURI(dest, cfg.RunID)
	}
	if dest == "" {
		return nil, nil
	}
	receipt, err := exportWorkspace(ctx, cfg, exec, dest)
	return &receipt, err
}

// followUpExportURI derives a follow-up run's workspace export
// destination from the configured URI by inserting the run ID as the
// path segment before the object's final component, so successive
// runs in one process never overwrite each other's tarball:
//
//	gs://bucket/runs/r1/workspace.tar.gz → gs://bucket/runs/r1/<runID>/workspace.tar.gz
//
// The object's file name is preserved so consumers keyed on it keep
// working. A base with no object name (a bucket, or a prefix ending in
// "/") gains the run ID as its final segment instead. An empty base
// stays empty (export disabled).
func followUpExportURI(base, runID string) string {
	if base == "" {
		return ""
	}
	scheme, rest, ok := strings.Cut(base, "://")
	if !ok {
		return strings.TrimSuffix(base, "/") + "/" + runID
	}
	dir, file, ok := strings.Cut(rest, "/")
	if ok {
		if i := strings.LastIndex(file, "/"); i >= 0 {
			dir, file = dir+"/"+file[:i], file[i+1:]
		}
	}
	if !ok || file == "" {
		return strings.TrimSuffix(base, "/") + "/" + runID
	}
	return scheme + "://" + dir + "/" + runID + "/" + file
}

// postRunPolicy is the per-run finalisation `stirrup harness` and
// `stirrup job` apply to the primary run and to every follow-up run
// alike: emit the run's result. The workspace export has already run
// inside the loop, before "done".
type postRunPolicy struct {
	// emit publishes the run's RunResult (and any stderr summary).
	emit func(ctx context.Context, cfg *types.RunConfig, rt *types.RunTrace)
	// firstFollowUpErr is the first required-export failure or control
	// stream loss among the follow-up runs, surfaced as the process's
	// exit status once the follow-up window closes. A follow-up's other
	// run errors are not recorded: they are already on that run's "done"
	// and RunResult, and the exit status reports the assigned task (the
	// primary run).
	firstFollowUpErr error
}

// finaliseFollowUp applies the policy to one completed follow-up run
// and records the first required-export failure or control stream loss
// for followUpErr.
func (p *postRunPolicy) finaliseFollowUp(cfg *types.RunConfig, rt *types.RunTrace, runErr error) {
	_ = p.finalise(cfg, rt, runErr)
	if p.firstFollowUpErr != nil || rt == nil {
		return
	}
	if rt.Outcome == "control_stream_lost" {
		p.firstFollowUpErr = fmt.Errorf("follow-up run %s: %s", cfg.RunID, rt.Error)
		return
	}
	if r := rt.WorkspaceExport; r != nil && r.Required && !r.Delivered() {
		p.firstFollowUpErr = fmt.Errorf("export workspace to %s: %s", r.URI, r.Error)
	}
}

// followUpErr reports the first required-export failure or control
// stream loss among the follow-up runs, nil when there was none.
func (p *postRunPolicy) followUpErr() error {
	return p.firstFollowUpErr
}

// finalise applies the policy to one completed run. The emit gets a
// fresh, bounded context: the run's own context may already be
// cancelled by a SIGTERM or deadline, and a remote sink would otherwise
// silently drop the result on every cancelled run.
//
// Returns runErr wrapped when non-nil (the result is still emitted so
// the failure is observable); nil otherwise. A nil trace means the loop
// produced none at all; it has already emitted its terminal "done", so
// nothing is emitted here to avoid a second one.
func (p *postRunPolicy) finalise(cfg *types.RunConfig, rt *types.RunTrace, runErr error) error {
	if rt == nil {
		return fmt.Errorf("running harness: %w", runErr)
	}
	emitCtx, emitCancel := context.WithTimeout(context.Background(), postRunEmitTimeout)
	defer emitCancel()
	p.emit(emitCtx, cfg, rt)

	if runErr != nil {
		return fmt.Errorf("running harness: %w", runErr)
	}
	return nil
}

// setupSignalHandler installs SIGINT/SIGTERM handlers; see handleSignals.
// A variable so tests can deliver the signals without a real process
// signal.
var setupSignalHandler = func(interrupt, shutdown func()) {
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go handleSignals(sigCh, os.Stderr, interrupt, shutdown)
}

// handleSignals calls interrupt on the first signal, which ends the run
// while its bounded post-run work (git finalise, postRun hooks, trace
// finish) continues, and shutdown, when non-nil, on a second signal to
// abandon that work.
func handleSignals(sigCh <-chan os.Signal, out io.Writer, interrupt, shutdown func()) {
	<-sigCh
	_, _ = fmt.Fprintln(out, "\nReceived interrupt, shutting down...")
	interrupt()
	if shutdown == nil {
		return
	}
	<-sigCh
	_, _ = fmt.Fprintln(out, "Received second interrupt, abandoning post-run work...")
	shutdown()
}

// shutdownCloseGrace bounds how long armShutdownWatchdog waits after its
// signal context is done before closing the loop, even if Run() has not
// returned yet. It sits under Cloud Run's 10 s SIGTERM-to-SIGKILL
// escalation (see docs/cloud-run-jobs.md) so the executor's sandbox is
// torn down before the process is killed. A variable so tests can
// shorten it.
var shutdownCloseGrace = 5 * time.Second

// armShutdownWatchdog starts a background goroutine that closes loop
// grace after signalCtx is done, unless the returned stop function is
// called first (Run() already returned; the caller's own
// `defer loop.Close()` handles teardown). Closing tears down the
// executor, transport and trace emitter, so any post-run work still
// running is cut short; callers choose signalCtx accordingly. Close()
// is safe to invoke more than once.
func armShutdownWatchdog(signalCtx context.Context, loop io.Closer, grace time.Duration) (stop func()) {
	runDone := make(chan struct{})
	go func() {
		select {
		case <-runDone:
			return
		case <-signalCtx.Done():
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
