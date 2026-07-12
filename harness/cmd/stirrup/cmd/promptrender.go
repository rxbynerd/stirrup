package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/stirrup/harness/internal/prompt"
)

// promptCmd is the root of the `stirrup prompt …` subcommand family.
// The subcommands inspect prompt-builder output without booting the
// agentic loop or contacting a provider.
var promptCmd = &cobra.Command{
	Use:   "prompt",
	Short: "Inspect system prompt rendering",
	Long: `Inspect system prompt rendering without running a turn.

Subcommands:
  render  Render a mode's system prompt template for a model and print
          the result. Side-effect-free; needs no API key.`,
}

// promptRenderCmd renders the shipped mode prompt templates (#492)
// offline so prompt content can be iterated on and diffed across
// models without a provider call or credentials.
var promptRenderCmd = &cobra.Command{
	Use:   "render",
	Short: "Render a mode's system prompt template for a model",
	Long: `Render the system prompt preamble for the supplied (--mode,
--prompt-model) pair and print it to stdout. Only the templated
preamble is rendered: the structural sections the harness appends at
run time (working directory, turn budget, workspace tree, git status,
dynamic context) depend on a live workspace and are omitted.

By default the shipped, embedded mode template is rendered. Passing
--template <path> renders an operator template file instead — the same
content promptBuilder.template accepts — so tuned prompts can be
previewed against models before deployment.

The resolved tier is printed to stderr so a shell capturing stdout
still gets only prompt text. A model matching no tier table renders
the base prompt ("default" tier); this is the documented fall-through
for unrecognised models, not an error.`,
	Args: cobra.NoArgs,
	RunE: runPromptRender,
}

func init() {
	rootCmd.AddCommand(promptCmd)
	promptCmd.AddCommand(promptRenderCmd)

	f := promptRenderCmd.Flags()
	f.StringP("mode", "m", "planning", "Run mode whose prompt to render: execution, planning, review, research, toil.")
	f.String("prompt-model", "", "Model identity to render the template against (same semantics as the harness --prompt-model flag). Empty renders the base prompt only.")
	f.String("template", "", "Path to an operator Go text/template file to render instead of the shipped mode template (the same content promptBuilder.template accepts).")
}

func runPromptRender(cmd *cobra.Command, _ []string) error {
	return runPromptRenderWithIO(cmd, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

// runPromptRenderWithIO is the testable seam for runPromptRender: it
// accepts explicit stdout and stderr so tests can assert the prompt
// text and tier report separately through the real cobra wiring.
func runPromptRenderWithIO(cmd *cobra.Command, stdout, stderr io.Writer) error {
	f := cmd.Flags()
	mode, _ := f.GetString("mode")
	promptModel, _ := f.GetString("prompt-model")
	templatePath, _ := f.GetString("template")

	data := prompt.TemplateData{Model: promptModel, Mode: mode}

	var rendered string
	if templatePath != "" {
		raw, err := os.ReadFile(templatePath)
		if err != nil {
			return fmt.Errorf("read template file: %w", err)
		}
		builder, err := prompt.NewTemplatePromptBuilder(string(raw), data)
		if err != nil {
			return err
		}
		rendered, err = builder.Build(cmd.Context(), prompt.PromptContext{Mode: mode, Model: promptModel})
		if err != nil {
			return err
		}
	} else {
		var err error
		rendered, err = prompt.RenderModePrompt(mode, prompt.PromptContext{Mode: mode, Model: promptModel})
		if err != nil {
			return err
		}
	}

	_, _ = fmt.Fprintf(stderr, "prompt model: %q, tier: %s\n", promptModel, prompt.TierFor(promptModel))
	if _, err := fmt.Fprintln(stdout, rendered); err != nil {
		return fmt.Errorf("write rendered prompt: %w", err)
	}
	return nil
}
