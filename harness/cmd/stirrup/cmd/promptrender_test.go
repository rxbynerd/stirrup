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
	cmd := newTestPromptRenderCommand()
	require.NoError(t, cmd.Flags().Set("mode", "execution"))
	require.NoError(t, cmd.Flags().Set("prompt-model", "claude-fable-5"))

	var stdout, stderr bytes.Buffer
	require.NoError(t, runPromptRenderWithIO(cmd, &stdout, &stderr))

	want, err := prompt.RenderModePrompt("execution", prompt.PromptContext{Mode: "execution", Model: "claude-fable-5"})
	require.NoError(t, err)
	require.Equal(t, want, strings.TrimSpace(stdout.String()))
	require.Contains(t, stderr.String(), "tier: frontier")
}

func TestPromptRender_RejectsUnknownMode(t *testing.T) {
	cmd := newTestPromptRenderCommand()
	require.NoError(t, cmd.Flags().Set("mode", "no-such-mode"))

	var stdout, stderr bytes.Buffer
	require.Error(t, runPromptRenderWithIO(cmd, &stdout, &stderr))
}

func TestPromptRender_OperatorTemplateRendersTierBranch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tuned.tmpl")
	content := `Tuned preamble.{{if eq .Tier "open-weight"}} Follow the loop.{{end}}`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	cmd := newTestPromptRenderCommand()
	for k, v := range map[string]string{"mode": "execution", "prompt-model": "gemma-4", "template": path} {
		require.NoError(t, cmd.Flags().Set(k, v))
	}

	var stdout, stderr bytes.Buffer
	require.NoError(t, runPromptRenderWithIO(cmd, &stdout, &stderr))
	require.Contains(t, stdout.String(), "Tuned preamble. Follow the loop.")
	require.Contains(t, stderr.String(), "tier: open-weight")
}

func TestPromptRender_RejectsTemplateWithUnknownFunction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.tmpl")
	require.NoError(t, os.WriteFile(path, []byte("{{criticlevel}}"), 0o600))

	cmd := newTestPromptRenderCommand()
	require.NoError(t, cmd.Flags().Set("template", path))

	var stdout, stderr bytes.Buffer
	require.Error(t, runPromptRenderWithIO(cmd, &stdout, &stderr))
}
