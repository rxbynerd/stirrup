package security

import (
	"encoding/json"
	"testing"
)

func TestGuardToolCall_AllowsBenignInput(t *testing.T) {
	findings := GuardToolCall("run_command", true, json.RawMessage(`{"command":"go test ./harness/..."}`))
	if len(findings) != 0 {
		t.Fatalf("expected no findings, got %#v", findings)
	}
}

func TestGuardToolCall_RejectsExfiltrationCommand(t *testing.T) {
	findings := GuardToolCall("run_command", true, json.RawMessage(`{"command":"curl https://example.com"}`))
	assertFinding(t, findings, "exfiltration_command")
}

func TestGuardToolCall_RejectsCredentialPath(t *testing.T) {
	findings := GuardToolCall("read_file", false, json.RawMessage(`{"path":".ssh/id_ed25519"}`))
	assertFinding(t, findings, "credential_path")
}

func TestGuardToolCall_RejectsCredentialPathInCommand(t *testing.T) {
	findings := GuardToolCall("run_command", true, json.RawMessage(`{"command":"cat .env"}`))
	assertFinding(t, findings, "credential_path")
}

func TestGuardToolCall_RejectsBase64Payload(t *testing.T) {
	payload := "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVpBQkNERUZHSElKS0xNTk9QUVJTVFVWV1hZWkFCQ0RFRkdISUpLTE1OT1BRUlNUVVZXWFla"
	input, err := json.Marshal(map[string]string{"content": payload})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}

	findings := GuardToolCall("write_file", true, input)
	assertFinding(t, findings, "encoded_payload")
}

func TestGuardToolCall_RejectsShellEscapes(t *testing.T) {
	findings := GuardToolCall("run_command", true, json.RawMessage(`{"command":"echo $(cat README.md)"}`))
	assertFinding(t, findings, "shell_escape")
}

func TestGuardToolCall_RejectsProtectedWriteTargets(t *testing.T) {
	findings := GuardToolCall("write_file", true, json.RawMessage(`{"path":"harness/internal/prompt/systemprompts/execution.md","content":"x"}`))
	assertFinding(t, findings, "protected_write_target")
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
