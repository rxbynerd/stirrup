package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

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
	if err := cmd.Flags().Set("mode", "execution"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("prompt-model", "claude-fable-5"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := runPromptRenderWithIO(cmd, &stdout, &stderr); err != nil {
		t.Fatalf("runPromptRenderWithIO: %v", err)
	}

	want, err := prompt.RenderModePrompt("execution", prompt.PromptContext{Mode: "execution", Model: "claude-fable-5"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(stdout.String()); got != want {
		t.Errorf("stdout differs from RenderModePrompt output:\n%s", got)
	}
	if !strings.Contains(stderr.String(), "tier: frontier") {
		t.Errorf("stderr should report the tier, got: %q", stderr.String())
	}
}

func TestPromptRender_UnknownModeFails(t *testing.T) {
	cmd := newTestPromptRenderCommand()
	if err := cmd.Flags().Set("mode", "no-such-mode"); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := runPromptRenderWithIO(cmd, &stdout, &stderr); err == nil {
		t.Fatal("expected error for unknown mode")
	}
}

func TestPromptRender_OperatorTemplateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tuned.tmpl")
	content := `Tuned preamble.{{if eq .Tier "open-weight"}} Follow the loop.{{end}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newTestPromptRenderCommand()
	for k, v := range map[string]string{"mode": "execution", "prompt-model": "gemma-4", "template": path} {
		if err := cmd.Flags().Set(k, v); err != nil {
			t.Fatal(err)
		}
	}

	var stdout, stderr bytes.Buffer
	if err := runPromptRenderWithIO(cmd, &stdout, &stderr); err != nil {
		t.Fatalf("runPromptRenderWithIO: %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "Tuned preamble. Follow the loop.") {
		t.Errorf("operator template not rendered with tier branch: %q", got)
	}
	if !strings.Contains(stderr.String(), "tier: open-weight") {
		t.Errorf("stderr should report the tier, got: %q", stderr.String())
	}
}

func TestPromptRender_BadTemplateFileFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.tmpl")
	if err := os.WriteFile(path, []byte("{{criticlevel}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newTestPromptRenderCommand()
	if err := cmd.Flags().Set("template", path); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := runPromptRenderWithIO(cmd, &stdout, &stderr); err == nil {
		t.Fatal("expected error for mustache-style placeholder in template file")
	}
}
