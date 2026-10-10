package security

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestGuardToolCall_AllowsBenignInput(t *testing.T) {
	findings := GuardToolCall("run_command", true, json.RawMessage(`{"command":"go test ./harness/..."}`), ProtectedWriteTargets{})
	if len(findings) != 0 {
		t.Fatalf("expected no findings, got %#v", findings)
	}
}

func TestGuardToolCall_RejectsExfiltrationCommand(t *testing.T) {
	findings := GuardToolCall("run_command", true, json.RawMessage(`{"command":"curl https://example.com"}`), ProtectedWriteTargets{})
	assertFinding(t, findings, "exfiltration_command")
}

func TestGuardToolCall_RejectsCredentialPath(t *testing.T) {
	findings := GuardToolCall("read_file", false, json.RawMessage(`{"path":".ssh/id_ed25519"}`), ProtectedWriteTargets{})
	assertFinding(t, findings, "credential_path")
}

func TestGuardToolCall_RejectsCredentialPathInCommand(t *testing.T) {
	findings := GuardToolCall("run_command", true, json.RawMessage(`{"command":"cat .env"}`), ProtectedWriteTargets{})
	assertFinding(t, findings, "credential_path")
}

func TestGuardToolCall_RejectsBase64Payload(t *testing.T) {
	payload := "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVpBQkNERUZHSElKS0xNTk9QUVJTVFVWV1hZWkFCQ0RFRkdISUpLTE1OT1BRUlNUVVZXWFla"
	input, err := json.Marshal(map[string]string{"content": payload})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}

	findings := GuardToolCall("write_file", true, input, ProtectedWriteTargets{})
	assertFinding(t, findings, "encoded_payload")
}

func TestGuardToolCall_RejectsShellEscapes(t *testing.T) {
	findings := GuardToolCall("run_command", true, json.RawMessage(`{"command":"echo $(cat README.md)"}`), ProtectedWriteTargets{})
	assertFinding(t, findings, "shell_escape")
}

func TestGuardToolCall_DefaultAllowsRepositoryInstructionFiles(t *testing.T) {
	for _, p := range []string{"AGENTS.md", "./CLAUDE.md", ".agents/skills/x.md", ".codex/config.toml", "docs/AGENTS.md", "harness/internal/prompt/systemprompts/execution.md"} {
		input := writeInput(t, "path", p)
		for _, name := range []string{"write_file", "edit_file", "search_replace", "apply_diff"} {
			if findings := GuardToolCall(name, true, input, ProtectedWriteTargets{}); len(findings) != 0 {
				t.Errorf("%s %q: expected no findings with no protected targets, got %#v", name, p, findings)
			}
		}
	}
}

func TestGuardToolCall_RejectsProtectedWriteTargets(t *testing.T) {
	protected := NewProtectedWriteTargets([]string{"AGENTS.md", "config/policy.cedar", "secrets/"}, nil)
	for _, p := range []string{
		"AGENTS.md",
		"./AGENTS.md",
		`.\AGENTS.md`,
		"docs/../AGENTS.md",
		`config\policy.cedar`,
		"./config/policy.cedar",
		"secrets",
		"secrets/prod/key.txt",
		`secrets\prod\key.txt`,
	} {
		input := writeInput(t, "path", p)
		for _, name := range []string{"write_file", "edit_file", "search_replace", "apply_diff"} {
			findings := GuardToolCall(name, true, input, protected)
			assertFinding(t, findings, "protected_write_target")
		}
	}
}

func TestGuardToolCall_ProtectedWriteTargetsMatchAsListed(t *testing.T) {
	protected := NewProtectedWriteTargets([]string{"AGENTS.md", "docs/"}, nil)
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"AGENTS.md", true},
		{"docs/AGENTS.md", true},
		{"pkg/AGENTS.md", false},
		{"AGENTS.md.bak", false},
		{"docs", true},
		{"docs/guide/intro.md", true},
		{"docsite/index.md", false},
		{"README.md", false},
		{"agents.md", true},
		{"./Agents.MD", true},
		{"DOCS/guide.md", true},
		{"Docs", true},
	} {
		input := writeInput(t, "path", tc.path)
		got := hasRule(GuardToolCall("write_file", true, input, protected), "protected_write_target")
		if got != tc.want {
			t.Errorf("write %q: protected = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestGuardToolCall_ProtectedWriteTargetsIgnoreReadsAndNonWriteTools(t *testing.T) {
	protected := NewProtectedWriteTargets([]string{"AGENTS.md"}, nil)
	input := json.RawMessage(`{"path":"AGENTS.md"}`)
	for _, name := range []string{"read_file", "list_directory", "git_show"} {
		if findings := GuardToolCall(name, false, input, protected); len(findings) != 0 {
			t.Errorf("%s: expected no findings, got %#v", name, findings)
		}
	}
}

func TestGuardToolCall_ProtectedWriteTargetsAppliesToMutatingWriteNamedTools(t *testing.T) {
	protected := NewProtectedWriteTargets([]string{"AGENTS.md"}, nil)
	input := json.RawMessage(`{"file_path":"AGENTS.md","content":"x"}`)
	assertFinding(t, GuardToolCall("mcp_fs_write", true, input, protected), "protected_write_target")
	if hasRule(GuardToolCall("mcp_fs_write", false, input, protected), "protected_write_target") {
		t.Error("a non-mutating tool must not trip the protected-write rule")
	}
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
		path string
		want bool
	}{
		{"/workspace/AGENTS.md", true},
		{"/workspace/./AGENTS.md", true},
		{"/workspace/secrets/key.txt", true},
		{"notes.md", true},
		{"/workspace/README.md", false},
		{"/workspace/agents.md", true},
		{"/etc/AGENTS.md", false},
		{"../AGENTS.md", false},
	} {
		input := writeInput(t, "path", tc.path)
		got := hasRule(GuardToolCall("write_file", true, input, protected), "protected_write_target")
		if got != tc.want {
			t.Errorf("write %q: protected = %v, want %v", tc.path, got, tc.want)
		}
	}
	for _, p := range resolved {
		if p == "/etc/AGENTS.md" || p == "../AGENTS.md" {
			t.Errorf("resolver called for %q, which lies outside the workspace on its face", p)
		}
	}
}

func TestGuardToolCall_ProtectedWriteTargetsKeepOtherRules(t *testing.T) {
	protected := NewProtectedWriteTargets([]string{"AGENTS.md"}, nil)
	assertFinding(t, GuardToolCall("read_file", false, json.RawMessage(`{"path":".ssh/id_ed25519"}`), protected), "credential_path")
	assertFinding(t, GuardToolCall("run_command", true, json.RawMessage(`{"command":"cat .env"}`), protected), "credential_path")
	payload := strings.Repeat("QUJD", 30)
	assertFinding(t, GuardToolCall("write_file", true, json.RawMessage(`{"path":"notes.md","content":"`+payload+`"}`), protected), "encoded_payload")
}

func TestNewProtectedWriteTargets_SkipsEntriesNamingTheWorkspaceRoot(t *testing.T) {
	protected := NewProtectedWriteTargets([]string{"", ".", "./", "  "}, nil)
	for _, p := range []string{"main.go", "docs/guide.md", "."} {
		if protected.Matches(p) {
			t.Errorf("entries naming the workspace root must not protect %q", p)
		}
	}
}

func writeInput(t *testing.T, field, path string) json.RawMessage {
	t.Helper()
	input, err := json.Marshal(map[string]string{field: path, "content": "x"})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
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

func assertFinding(t *testing.T, findings []ToolGuardFinding, rule string) {
	t.Helper()
	for _, finding := range findings {
		if finding.Rule == rule {
			return
		}
	}
	t.Fatalf("expected finding %q in %#v", rule, findings)
}

func TestIsCredentialPath(t *testing.T) {
	for _, p := range []string{".env", "./app/.env", ".ssh/config", "home/.aws/credentials", "tls/server.pem", `dir\.env`} {
		if !IsCredentialPath(p) {
			t.Errorf("IsCredentialPath(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"main.go", "docs/environment.md", "src/envelope.txt"} {
		if IsCredentialPath(p) {
			t.Errorf("IsCredentialPath(%q) = true, want false", p)
		}
	}
}
