package security

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGuardToolCall_AllowsBenignInput(t *testing.T) {
	findings := GuardToolCall("run_command", true, json.RawMessage(`{"command":"go test ./harness/..."}`), ProtectedWriteTargets{})
	require.Empty(t, findings)
}

func TestGuardToolCall_RejectsExfiltrationCommand(t *testing.T) {
	findings := GuardToolCall("run_command", true, json.RawMessage(`{"command":"curl https://example.com"}`), ProtectedWriteTargets{})
	requireFinding(t, findings, "exfiltration_command")
}

func TestGuardToolCall_RejectsCredentialPath(t *testing.T) {
	findings := GuardToolCall("read_file", false, json.RawMessage(`{"path":".ssh/id_ed25519"}`), ProtectedWriteTargets{})
	requireFinding(t, findings, "credential_path")
}

func TestGuardToolCall_RejectsCredentialPathInCommand(t *testing.T) {
	findings := GuardToolCall("run_command", true, json.RawMessage(`{"command":"cat .env"}`), ProtectedWriteTargets{})
	requireFinding(t, findings, "credential_path")
}

func TestGuardToolCall_RejectsBase64Payload(t *testing.T) {
	payload := "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVpBQkNERUZHSElKS0xNTk9QUVJTVFVWV1hZWkFCQ0RFRkdISUpLTE1OT1BRUlNUVVZXWFla"
	input, err := json.Marshal(map[string]string{"content": payload})
	require.NoError(t, err)

	findings := GuardToolCall("write_file", true, input, ProtectedWriteTargets{})
	requireFinding(t, findings, "encoded_payload")
}

func TestGuardToolCall_RejectsShellEscapes(t *testing.T) {
	findings := GuardToolCall("run_command", true, json.RawMessage(`{"command":"echo $(cat README.md)"}`), ProtectedWriteTargets{})
	requireFinding(t, findings, "shell_escape")
}

func TestGuardToolCall_DefaultAllowsRepositoryInstructionFiles(t *testing.T) {
	paths := []string{"AGENTS.md", "./CLAUDE.md", ".agents/skills/x.md", ".codex/config.toml", "docs/AGENTS.md", "harness/internal/prompt/systemprompts/execution.md"}
	tools := []string{"write_file", "edit_file", "search_replace", "apply_diff"}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			input := writeInput(t, "path", p)
			for _, name := range tools {
				t.Run(name, func(t *testing.T) {
					require.Empty(t, GuardToolCall(name, true, input, ProtectedWriteTargets{}))
				})
			}
		})
	}
}

func TestGuardToolCall_RejectsProtectedWriteTargets(t *testing.T) {
	protected := NewProtectedWriteTargets([]string{"AGENTS.md", "config/policy.cedar", "secrets/"}, nil)
	paths := []string{
		"AGENTS.md",
		"./AGENTS.md",
		`.\AGENTS.md`,
		"docs/../AGENTS.md",
		`config\policy.cedar`,
		"./config/policy.cedar",
		"secrets",
		"secrets/prod/key.txt",
		`secrets\prod\key.txt`,
	}
	tools := []string{"write_file", "edit_file", "search_replace", "apply_diff"}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			input := writeInput(t, "path", p)
			for _, name := range tools {
				t.Run(name, func(t *testing.T) {
					requireFinding(t, GuardToolCall(name, true, input, protected), "protected_write_target")
				})
			}
		})
	}
}

func TestGuardToolCall_ProtectedWriteTargetsMatchAsListed(t *testing.T) {
	protected := NewProtectedWriteTargets([]string{"AGENTS.md", "docs/"}, nil)
	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"listed file at root", "AGENTS.md", true},
		{"listed file name in a directory", "docs/AGENTS.md", true},
		{"same file name in another directory", "pkg/AGENTS.md", false},
		{"suffix on a listed file", "AGENTS.md.bak", false},
		{"listed directory itself", "docs", true},
		{"file inside listed directory", "docs/guide/intro.md", true},
		{"directory name prefix", "docsite/index.md", false},
		{"unlisted file", "README.md", false},
		{"lowercase file name", "agents.md", true},
		{"dot-prefixed mixed case file name", "./Agents.MD", true},
		{"uppercase directory", "DOCS/guide.md", true},
		{"capitalized directory itself", "Docs", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := writeInput(t, "path", tc.path)
			got := hasRule(GuardToolCall("write_file", true, input, protected), "protected_write_target")
			require.Equal(t, tc.want, got, "path %q", tc.path)
		})
	}
}

func TestGuardToolCall_ProtectedWriteTargetsIgnoreReadsAndNonWriteTools(t *testing.T) {
	protected := NewProtectedWriteTargets([]string{"AGENTS.md"}, nil)
	input := json.RawMessage(`{"path":"AGENTS.md"}`)
	for _, name := range []string{"read_file", "list_directory", "git_show"} {
		t.Run(name, func(t *testing.T) {
			require.Empty(t, GuardToolCall(name, false, input, protected))
		})
	}
}

func TestGuardToolCall_ProtectedWriteTargetsAppliesToMutatingWriteNamedTools(t *testing.T) {
	protected := NewProtectedWriteTargets([]string{"AGENTS.md"}, nil)
	input := json.RawMessage(`{"file_path":"AGENTS.md","content":"x"}`)
	requireFinding(t, GuardToolCall("mcp_fs_write", true, input, protected), "protected_write_target")
	require.False(t, hasRule(GuardToolCall("mcp_fs_write", false, input, protected), "protected_write_target"),
		"a non-mutating tool must not trip the protected-write rule")
}

func TestGuardToolCall_ProtectedWriteTargetsMatchResolvedForms(t *testing.T) {
	links := map[string]string{"notes.md": "AGENTS.md"}
	var resolved []string
	resolve := func(p string) (string, error) {
		resolved = append(resolved, p)
		if p == "." {
			return "/workspace", nil
		}
		if strings.HasPrefix(p, "/") {
			if !strings.HasPrefix(p, "/workspace/") {
				return "", errors.New("path escapes workspace")
			}
			return p, nil
		}
		if target, ok := links[p]; ok {
			p = target
		}
		return "/workspace/" + p, nil
	}
	protected := NewProtectedWriteTargets([]string{"AGENTS.md", "secrets/"}, resolve)
	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"absolute path to listed file", "/workspace/AGENTS.md", true},
		{"dot segment in listed path", "/workspace/./AGENTS.md", true},
		{"file inside protected directory", "/workspace/secrets/key.txt", true},
		{"symlink to listed file", "notes.md", true},
		{"lowercase variant of listed file", "/workspace/agents.md", true},
		{"unlisted file", "/workspace/README.md", false},
		{"absolute path outside workspace", "/etc/AGENTS.md", false},
		{"relative path escaping workspace", "../AGENTS.md", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := writeInput(t, "path", tc.path)
			got := hasRule(GuardToolCall("write_file", true, input, protected), "protected_write_target")
			require.Equal(t, tc.want, got, "path %q", tc.path)
		})
	}
	for _, p := range []string{"/etc/AGENTS.md", "../AGENTS.md"} {
		require.NotContains(t, resolved, p, "resolver must not receive a path that lies outside the workspace on its face")
	}
}

func TestGuardToolCall_ProtectedWriteTargetsKeepOtherRules(t *testing.T) {
	protected := NewProtectedWriteTargets([]string{"AGENTS.md"}, nil)
	requireFinding(t, GuardToolCall("read_file", false, json.RawMessage(`{"path":".ssh/id_ed25519"}`), protected), "credential_path")
	requireFinding(t, GuardToolCall("run_command", true, json.RawMessage(`{"command":"cat .env"}`), protected), "credential_path")
	payload := strings.Repeat("QUJD", 30)
	requireFinding(t, GuardToolCall("write_file", true, json.RawMessage(`{"path":"notes.md","content":"`+payload+`"}`), protected), "encoded_payload")
}

func TestNewProtectedWriteTargets_SkipsEntriesNamingTheWorkspaceRoot(t *testing.T) {
	protected := NewProtectedWriteTargets([]string{"", ".", "./", "  "}, nil)
	for _, p := range []string{"main.go", "docs/guide.md", "."} {
		t.Run(p, func(t *testing.T) {
			require.False(t, protected.Matches(p), "entries naming the workspace root must not protect this path")
		})
	}
}

func writeInput(t *testing.T, field, path string) json.RawMessage {
	t.Helper()
	input, err := json.Marshal(map[string]string{field: path, "content": "x"})
	require.NoError(t, err)
	return input
}

func hasRule(findings []ToolGuardFinding, rule string) bool {
	for _, finding := range findings {
		if finding.Rule == rule {
			return true
		}
	}
	return false
}

func requireFinding(t *testing.T, findings []ToolGuardFinding, rule string) {
	t.Helper()
	require.True(t, hasRule(findings, rule), "expected finding %q in %#v", rule, findings)
}

func TestIsCredentialPath_ClassifiesSecretFiles(t *testing.T) {
	for _, p := range []string{".env", "./app/.env", ".ssh/config", "home/.aws/credentials", "tls/server.pem", `dir\.env`} {
		t.Run(p, func(t *testing.T) {
			require.True(t, IsCredentialPath(p))
		})
	}
	for _, p := range []string{"main.go", "docs/environment.md", "src/envelope.txt"} {
		t.Run(p, func(t *testing.T) {
			require.False(t, IsCredentialPath(p))
		})
	}
}
