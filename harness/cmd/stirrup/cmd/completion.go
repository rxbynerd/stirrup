package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var completionCmd = &cobra.Command{
	Use:                   "completion [bash|zsh|fish|powershell]",
	Short:                 "Generate shell completion script",
	DisableFlagsInUseLine: true,
	ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
	Args:                  cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
	Long: `Generate the shell completion script for the named shell. The
output is the script body and is meant to be sourced; see the recipes
in docs/configuration.md ("Shell completions") for one-shot setup
incantations for each supported shell.

Examples:
  source <(stirrup completion bash)
  stirrup completion zsh > "${fpath[1]}/_stirrup"
  stirrup completion fish > ~/.config/fish/completions/stirrup.fish
  stirrup completion powershell | Out-String | Invoke-Expression`,
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()
		switch args[0] {
		case "bash":
			// GenBashCompletionV2 emits the modern (descriptions-aware)
			// bash script; the legacy generator (GenBashCompletion) does
			// not surface cobra's RegisterFlagCompletionFunc values.
			return cmd.Root().GenBashCompletionV2(out, true)
		case "zsh":
			return cmd.Root().GenZshCompletion(out)
		case "fish":
			return cmd.Root().GenFishCompletion(out, true)
		case "powershell":
			return cmd.Root().GenPowerShellCompletionWithDesc(out)
		default:
			// Unreachable: ValidArgs + OnlyValidArgs already constrains
			// args[0] to the four supported shells. The explicit error
			// preserves the symmetry of every case returning an error
			// for the type-checker and guards against a future edit
			// that drops the ValidArgs constraint.
			return fmt.Errorf("unsupported shell: %s", args[0])
		}
	},
}

func init() {
	rootCmd.AddCommand(completionCmd)
}
