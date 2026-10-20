package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/prompt"
)

func newTestPromptRenderCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "render", Args: cobra.NoArgs, RunE: runPromptRender}
	f := cmd.Flags()
	f.StringP("mode", "m", "planning", "")
	f.String("prompt-model", "", "")
	f.String("template", "", "")
	return cmd
}

func TestPromptRender_ShippedTemplate(t *testing.T) {
	must := require.New(t)
	cmd := newTestPromptRenderCommand()
	must.NoError(cmd.Flags().Set("mode", "execution"))
	must.NoError(cmd.Flags().Set("prompt-model", "claude-fable-5"))

	var stdout, stderr bytes.Buffer
	must.NoError(runPromptRenderWithIO(cmd, &stdout, &stderr))

	want, err := prompt.RenderModePrompt("execution", prompt.PromptContext{Mode: "execution", Model: "claude-fable-5"})
	must.NoError(err)
	must.Equal(want, strings.TrimSpace(stdout.String()))
	must.Contains(stderr.String(), "tier: frontier")
}

func TestPromptRender_RejectsUnknownMode(t *testing.T) {
	must := require.New(t)
	cmd := newTestPromptRenderCommand()
	must.NoError(cmd.Flags().Set("mode", "no-such-mode"))

	var stdout, stderr bytes.Buffer
	must.Error(runPromptRenderWithIO(cmd, &stdout, &stderr))
}

func TestPromptRender_OperatorTemplateRendersTierBranch(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "tuned.tmpl")
	content := `Tuned preamble.{{if eq .Tier "open-weight"}} Follow the loop.{{end}}`
	must.NoError(os.WriteFile(path, []byte(content), 0o600))

	cmd := newTestPromptRenderCommand()
	for k, v := range map[string]string{"mode": "execution", "prompt-model": "gemma-4", "template": path} {
		must.NoError(cmd.Flags().Set(k, v))
	}

	var stdout, stderr bytes.Buffer
	must.NoError(runPromptRenderWithIO(cmd, &stdout, &stderr))
	must.Contains(stdout.String(), "Tuned preamble. Follow the loop.")
	must.Contains(stderr.String(), "tier: open-weight")
}

func TestPromptRender_RejectsTemplateWithUnknownFunction(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "broken.tmpl")
	must.NoError(os.WriteFile(path, []byte("{{criticlevel}}"), 0o600))

	cmd := newTestPromptRenderCommand()
	must.NoError(cmd.Flags().Set("template", path))

	var stdout, stderr bytes.Buffer
	must.Error(runPromptRenderWithIO(cmd, &stdout, &stderr))
}
