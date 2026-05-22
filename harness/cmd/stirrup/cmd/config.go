package cmd

import "github.com/spf13/cobra"

// configCmd is the parent of every introspection / static-analysis
// subcommand that does not run the agentic loop. Issue #247 adds the
// first leaf (`explain`); future siblings ("validate", "diff", etc.)
// belong here too. Kept separate from `run-config` so that subcommand's
// name accurately describes the produce-a-RunConfig-document role
// without growing a grab-bag of unrelated verbs.
var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Inspect RunConfig schema and documentation",
	Long: `Inspect the RunConfig schema without running the agentic loop.

Subcommands:
  explain    Print documentation for a RunConfig field path.

The documentation is sourced from the doc comments above every
RunConfig field in types/runconfig.go via a build-time generator;
the lookup table lives in types/runconfig_docs.go.`,
}

func init() {
	rootCmd.AddCommand(configCmd)
}
