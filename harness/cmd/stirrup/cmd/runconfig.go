package cmd

import (
	"errors"
	"io"
	"os"

	"github.com/spf13/cobra"
)

var runConfigCmd = &cobra.Command{
	Use:   "run-config [flags]",
	Short: "Emit a resolved RunConfig as JSON",
	Long: `Produce a fully-resolved RunConfig JSON document and print it to stdout.
Does NOT run the agentic loop. Composable via UNIX pipes: stdin is
treated as a base RunConfig, then explicit flags override individual
fields, mirroring how ` + "`stirrup harness --config <file>`" + ` already works.

Resolution order (lowest -> highest precedence):
  1. Defaults (flag DefValues, mode-derived defaults when --validate)
  2. Base RunConfig from stdin OR --config <path> (mutually exclusive)
  3. Explicit flags (those whose Changed() bit is set)

Flags: identical to ` + "`stirrup harness`" + ` for every RunConfig-producing
flag. CLI-only behaviour flags (--export-workspace-required,
--output-runconfig) are excluded because they do not map to
RunConfig fields.

Output:
  - Default: pretty-printed JSON (2-space indent) to stdout
  - --compact: single-line JSON
  - --redact: apply RunConfig.Redact() before emit (rewrites
    secret:// references to secret://[REDACTED] — produces a copy
    safe to commit / share, but not runnable as-is)`,
	Args: cobra.NoArgs,
	RunE: runRunConfig,
}

func init() {
	rootCmd.AddCommand(runConfigCmd)

	addRunConfigFlags(runConfigCmd)

	// Subcommand-only flags. None of these map to RunConfig fields;
	// they control how the resolved document is processed before
	// emission, so they live on run-config rather than the shared
	// helper.
	f := runConfigCmd.Flags()
	f.Bool("validate", false, "Run types.ValidateRunConfig on the resolved RunConfig and exit non-zero on failure. Without this flag, partial / chained configs are emitted as-is so they can be completed downstream.")
	f.Bool("compact", false, "Emit single-line JSON instead of indented (2-space) JSON.")
	f.Bool("redact", false, "Apply RunConfig.Redact() before emit. Rewrites secret:// references to secret://[REDACTED] for share-safe artifacts; the result is no longer runnable as-is.")
}

func runRunConfig(cmd *cobra.Command, args []string) error {
	return runRunConfigWithIO(cmd, args, os.Stdin, os.Stdout)
}

// runRunConfigWithIO is the testable seam for runRunConfig: it accepts
// explicit stdin and stdout so tests can drive the cobra flag-reading
// wiring (validate, redact, compact) through the real entry point
// instead of replicating the function body. The cobra RunE signature
// reaches it via runRunConfig, which threads os.Stdin / os.Stdout for
// the production binary.
func runRunConfigWithIO(cmd *cobra.Command, args []string, stdin io.Reader, stdout io.Writer) error {
	f := cmd.Flags()
	configPath, _ := f.GetString("config")
	resolve := ResolveBase
	if validate, _ := f.GetBool("validate"); validate {
		resolve = ResolveAll
	}

	cfg, err := BuildRunConfig(RunConfigSources{
		Stdin:      stdin,
		ConfigPath: configPath,
		Cmd:        cmd,
		Args:       args,
		Resolve:    resolve,
	})
	if err != nil {
		// The interactive usage-hint sentinel is a harness-only affordance
		// (issue #249): a bare `stirrup harness` on a TTY gets the grouped
		// hint. run-config has no such hint, so reaching here with the
		// sentinel — `stirrup run-config --validate` on a TTY with no
		// prompt — must surface the plain, actionable prompt-required
		// error instead of leaking the sentinel's internal string to
		// cobra (which would print "Error: stirrup: interactive prompt
		// hint requested" and exit non-zero).
		if errors.Is(err, errPromptHintRequested) {
			// A missing prompt is a precondition / validation-class
			// failure (exit 1, issue #253): the config could not be
			// completed because a required field had no source. The plain
			// errPromptRequired message replaces the internal sentinel
			// string so the operator sees an actionable error.
			return validationError(errPromptRequired)
		}
		return err
	}

	if redact, _ := f.GetBool("redact"); redact {
		r := cfg.Redact()
		cfg = &r
	}

	compact, _ := f.GetBool("compact")
	return writeRunConfigJSON(stdout, cfg, compact)
}
