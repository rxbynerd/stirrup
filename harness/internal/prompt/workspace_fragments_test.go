package prompt

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/git"
)

func TestWorkspaceTreeFragment_ListsThroughExecutor(t *testing.T) {
	ws := newFakeWorkspace("/workspace")
	ws.entries = []string{"README.md", "main.go", "pkg/"}

	got, err := WorkspaceTreeFragment().Render(context.Background(), PromptContext{
		Mode:      "execution",
		Workspace: "/no/such/host/directory",
		Executor:  ws,
	})
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	if want := "Workspace files:\n  README.md\n  main.go\n  pkg/"; got != want {
		t.Errorf("Render() = %q, want %q", got, want)
	}
	if !slices.Equal(ws.listCalls, []string{"."}) {
		t.Errorf("ListDirectory calls = %q, want one call for the workspace root", ws.listCalls)
	}
	if len(ws.execCalls) != 0 {
		t.Errorf("tree fragment ran commands %q, want none", ws.execCalls)
	}
}

func TestWorkspaceTreeFragment_EmptyWorkspaceRendersNothing(t *testing.T) {
	ws := newFakeWorkspace("/workspace")

	got, err := WorkspaceTreeFragment().Render(context.Background(), PromptContext{Mode: "execution", Executor: ws})
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	if got != "" {
		t.Errorf("Render() = %q, want empty for an empty workspace", got)
	}
}

func TestWorkspaceTreeFragment_ListErrorFailsTheBuild(t *testing.T) {
	ws := newFakeWorkspace("/workspace")
	ws.listErr = errors.New("sandbox unreachable")

	_, err := WorkspaceTreeFragment().Render(context.Background(), PromptContext{Mode: "execution", Executor: ws})
	if err == nil || !strings.Contains(err.Error(), "sandbox unreachable") {
		t.Fatalf("Render() error = %v, want the listing error", err)
	}
}

func TestWorkspaceFragments_NilExecutorRendersNothing(t *testing.T) {
	dir := t.TempDir()
	for name, fragment := range map[string]PromptFragment{
		"tree":       WorkspaceTreeFragment(),
		"git status": GitStatusFragment(),
	} {
		got, err := fragment.Render(context.Background(), PromptContext{Mode: "execution", Workspace: dir})
		if err != nil {
			t.Fatalf("%s: Render() error: %v", name, err)
		}
		if got != "" {
			t.Errorf("%s: Render() = %q, want empty without an executor", name, got)
		}
	}
}

// Kubernetes executors require executor.workspace to be empty, so the
// listing and status must come from the executor alone.
func TestComposedPrompt_KubernetesWorkspaceRendersSandboxListing(t *testing.T) {
	ws := newFakeWorkspace("/workspace")
	ws.entries = []string{"go.mod", "main.go", "pkg/"}
	ws.toplevel = "/workspace"
	ws.status = "?? new.go\n"

	b := NewComposedPromptBuilder(WithFragments(DefaultComposedFragments()...))
	got, err := b.Build(context.Background(), PromptContext{Mode: "execution", Executor: ws})
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}
	for _, want := range []string{"Workspace files:\n  go.mod\n  main.go\n  pkg/", "Git status:\n?? new.go"} {
		if !strings.Contains(got, want) {
			t.Errorf("composed prompt missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Working directory:") {
		t.Errorf("composed prompt printed a working directory for an empty workspace path:\n%s", got)
	}
}

func TestGitStatusFragment_RunsHardenedCommandsInExecutor(t *testing.T) {
	ws := newFakeWorkspace("/workspace")
	ws.toplevel = "/workspace"
	ws.status = " M main.go\n?? new.go\n"

	got, err := GitStatusFragment().Render(context.Background(), PromptContext{Mode: "execution", Executor: ws})
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	if want := "Git status:\n M main.go\n?? new.go"; got != want {
		t.Errorf("Render() = %q, want %q", got, want)
	}

	want := []string{
		"pwd -P",
		git.CommandLine("-c", "safe.directory=/workspace", "rev-parse", "--show-toplevel"),
		git.CommandLine("-c", "safe.directory=/workspace", "--no-optional-locks", "status", "--porcelain", "--untracked-files=normal"),
	}
	if !slices.Equal(ws.execCalls, want) {
		t.Errorf("exec calls:\n got %q\nwant %q", ws.execCalls, want)
	}
	for _, flag := range []string{"'core.fsmonitor=false'", "'core.hooksPath=/dev/null'"} {
		if !strings.Contains(ws.execCalls[2], flag) {
			t.Errorf("status command %q lacks hardening flag %s", ws.execCalls[2], flag)
		}
	}
	if len(ws.listCalls) != 0 {
		t.Errorf("git status fragment listed directories %q, want none", ws.listCalls)
	}
}

func TestGitStatusFragment_CleanRepository(t *testing.T) {
	ws := newFakeWorkspace("/workspace")
	ws.toplevel = "/workspace"

	got, err := GitStatusFragment().Render(context.Background(), PromptContext{Mode: "execution", Executor: ws})
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	if got != "Git status: clean" {
		t.Errorf("Render() = %q, want clean status", got)
	}
}

func TestGitStatusFragment_EnclosingRepositoryIsOmitted(t *testing.T) {
	ws := newFakeWorkspace("/workspace/sub")
	ws.entries = []string{"local.go"}
	ws.toplevel = "/workspace"
	ws.status = "?? enclosing-secret.txt\n"

	b := NewComposedPromptBuilder(WithFragments(DefaultComposedFragments()...))
	got, err := b.Build(context.Background(), PromptContext{Mode: "execution", Executor: ws})
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}
	if strings.Contains(got, "Git status") || strings.Contains(got, "enclosing-secret") {
		t.Errorf("composed prompt carries the enclosing repository's status:\n%s", got)
	}
	if !strings.Contains(got, "Workspace files:\n  local.go") {
		t.Errorf("composed prompt lost the workspace listing:\n%s", got)
	}
	if cmds := ws.statusCommands(); len(cmds) != 0 {
		t.Errorf("git status ran for an enclosing repository: %q", cmds)
	}
}

func TestGitStatusFragment_RendersNothingWhenGitCannotReport(t *testing.T) {
	tests := []struct {
		name  string
		setup func(ws *fakeWorkspace)
	}{
		{name: "not a repository", setup: func(ws *fakeWorkspace) { ws.toplevel = "" }},
		{name: "status fails", setup: func(ws *fakeWorkspace) { ws.toplevel = "/workspace"; ws.statusExitCode = 128 }},
		{name: "workspace root unresolvable", setup: func(ws *fakeWorkspace) { ws.root = "relative" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := newFakeWorkspace("/workspace")
			tt.setup(ws)
			got, err := GitStatusFragment().Render(context.Background(), PromptContext{Mode: "execution", Executor: ws})
			if err != nil {
				t.Fatalf("Render() error: %v", err)
			}
			if got != "" {
				t.Errorf("Render() = %q, want empty", got)
			}
		})
	}
}

func TestWorkspaceFragments_ExecutorsWithoutExec(t *testing.T) {
	t.Run("none executor renders neither fragment", func(t *testing.T) {
		b := NewComposedPromptBuilder(WithFragments(WorkspaceTreeFragment(), GitStatusFragment()))
		got, err := b.Build(context.Background(), PromptContext{Mode: "execution", Executor: executor.NewNoneExecutor()})
		if err != nil {
			t.Fatalf("Build() error: %v", err)
		}
		if got != "" {
			t.Errorf("Build() = %q, want empty", got)
		}
	})

	t.Run("api executor lists but runs no git", func(t *testing.T) {
		ws := newFakeWorkspace("/workspace")
		ws.caps = executor.ExecutorCapabilities{CanRead: true, CanNetwork: true}
		ws.entries = []string{"README.md", "src/"}
		ws.toplevel = "/workspace"

		b := NewComposedPromptBuilder(WithFragments(WorkspaceTreeFragment(), GitStatusFragment()))
		got, err := b.Build(context.Background(), PromptContext{Mode: "execution", Executor: ws})
		if err != nil {
			t.Fatalf("Build() error: %v", err)
		}
		if want := "Workspace files:\n  README.md\n  src/"; got != want {
			t.Errorf("Build() = %q, want %q", got, want)
		}
		if len(ws.execCalls) != 0 {
			t.Errorf("exec calls = %q, want none for an executor without exec", ws.execCalls)
		}
	})
}

func TestGitStatusFragment_TruncatesOversizedStatus(t *testing.T) {
	ws := newFakeWorkspace("/workspace")
	ws.toplevel = "/workspace"
	ws.status = statusLines(gitStatusMaxEntries+50, "f")

	got, err := GitStatusFragment().Render(context.Background(), PromptContext{Mode: "execution", Executor: ws})
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	marker := fmt.Sprintf("[git status truncated: %d of %d entries shown]", gitStatusMaxEntries, gitStatusMaxEntries+50)
	if !strings.HasSuffix(got, "\n"+marker) {
		t.Errorf("Render() does not end with %q:\n%s", marker, got)
	}
	if strings.Contains(got, fmt.Sprintf("?? f%04d", gitStatusMaxEntries)) {
		t.Error("entry past the cap reached the prompt")
	}
}

func TestFormatGitStatus(t *testing.T) {
	longName := strings.Repeat("x", 1000)
	byteCapped := statusLines(20, longName)
	byteCappedShown := gitStatusMaxBytes / len(fmt.Sprintf("?? %s0000\n", longName))

	tests := []struct {
		name          string
		out           string
		execTruncated bool
		wantLines     int
		wantMarker    string
	}{
		{name: "clean", out: "", wantMarker: ""},
		{name: "within bounds", out: statusLines(3, "f"), wantLines: 3},
		{name: "at the entry cap", out: statusLines(gitStatusMaxEntries, "f"), wantLines: gitStatusMaxEntries},
		{
			name:       "over the entry cap",
			out:        statusLines(gitStatusMaxEntries+1, "f"),
			wantLines:  gitStatusMaxEntries,
			wantMarker: fmt.Sprintf("[git status truncated: %d of %d entries shown]", gitStatusMaxEntries, gitStatusMaxEntries+1),
		},
		{
			name:       "over the byte cap",
			out:        byteCapped,
			wantLines:  byteCappedShown,
			wantMarker: fmt.Sprintf("[git status truncated: %d of 20 entries shown]", byteCappedShown),
		},
		{
			name:          "truncated by the executor drops the partial last line",
			out:           statusLines(5, "f") + "?? partial-na",
			execTruncated: true,
			wantLines:     5,
			wantMarker:    "[git status truncated: 5 entries shown]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatGitStatus(tt.out, tt.execTruncated)
			if tt.out == "" {
				if got != "Git status: clean" {
					t.Errorf("formatGitStatus() = %q, want clean", got)
				}
				return
			}
			lines := strings.Split(got, "\n")
			if lines[0] != "Git status:" {
				t.Fatalf("first line = %q, want header", lines[0])
			}
			body := lines[1:]
			if tt.wantMarker != "" {
				if body[len(body)-1] != tt.wantMarker {
					t.Errorf("last line = %q, want marker %q", body[len(body)-1], tt.wantMarker)
				}
				body = body[:len(body)-1]
			}
			if len(body) != tt.wantLines {
				t.Errorf("rendered %d entries, want %d", len(body), tt.wantLines)
			}
			if size := len(strings.Join(body, "\n")) + 1; size > gitStatusMaxBytes {
				t.Errorf("rendered %d bytes of entries, cap is %d", size, gitStatusMaxBytes)
			}
			if strings.Contains(got, "partial-na") {
				t.Error("partial line from executor-truncated output reached the prompt")
			}
		})
	}
}

func TestFormatGitStatus_KeepsLeadingStatusColumn(t *testing.T) {
	if got, want := formatGitStatus(" M main.go\n", false), "Git status:\n M main.go"; got != want {
		t.Errorf("formatGitStatus() = %q, want %q", got, want)
	}
}

func statusLines(n int, prefix string) string {
	var sb strings.Builder
	for i := range n {
		fmt.Fprintf(&sb, "?? %s%04d\n", prefix, i)
	}
	return sb.String()
}
