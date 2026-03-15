package types

import "testing"

func TestRedact_ProviderAPIKey(t *testing.T) {
	rc := RunConfig{
		Provider: ProviderConfig{APIKeyRef: "secret://my-key"},
	}
	redacted := rc.Redact()
	if redacted.Provider.APIKeyRef != "secret://[REDACTED]" {
		t.Errorf("Provider.APIKeyRef = %q, want secret://[REDACTED]", redacted.Provider.APIKeyRef)
	}
	// Original unchanged.
	if rc.Provider.APIKeyRef != "secret://my-key" {
		t.Error("Redact mutated original")
	}
}

func TestRedact_VcsBackendAPIKey(t *testing.T) {
	rc := RunConfig{
		Executor: ExecutorConfig{
			VcsBackend: &VcsBackendConfig{APIKeyRef: "secret://github-token"},
		},
	}
	redacted := rc.Redact()
	if redacted.Executor.VcsBackend.APIKeyRef != "secret://[REDACTED]" {
		t.Errorf("VcsBackend.APIKeyRef = %q, want secret://[REDACTED]", redacted.Executor.VcsBackend.APIKeyRef)
	}
	// Original unchanged.
	if rc.Executor.VcsBackend.APIKeyRef != "secret://github-token" {
		t.Error("Redact mutated original VcsBackend")
	}
}

func TestRedact_MCPServersAPIKeys(t *testing.T) {
	rc := RunConfig{
		Tools: ToolsConfig{
			MCPServers: []MCPServerConfig{
				{URI: "http://a", APIKeyRef: "secret://key1"},
				{URI: "http://b", APIKeyRef: ""},
				{URI: "http://c", APIKeyRef: "secret://key2"},
			},
		},
	}
	redacted := rc.Redact()
	if redacted.Tools.MCPServers[0].APIKeyRef != "secret://[REDACTED]" {
		t.Errorf("MCPServers[0].APIKeyRef = %q, want redacted", redacted.Tools.MCPServers[0].APIKeyRef)
	}
	if redacted.Tools.MCPServers[1].APIKeyRef != "" {
		t.Errorf("MCPServers[1].APIKeyRef = %q, want empty", redacted.Tools.MCPServers[1].APIKeyRef)
	}
	if redacted.Tools.MCPServers[2].APIKeyRef != "secret://[REDACTED]" {
		t.Errorf("MCPServers[2].APIKeyRef = %q, want redacted", redacted.Tools.MCPServers[2].APIKeyRef)
	}
	// Original unchanged.
	if rc.Tools.MCPServers[0].APIKeyRef != "secret://key1" {
		t.Error("Redact mutated original MCPServers")
	}
}

func TestRedact_EmptyConfig(t *testing.T) {
	rc := RunConfig{}
	redacted := rc.Redact()
	if redacted.Provider.APIKeyRef != "" {
		t.Error("empty APIKeyRef should stay empty")
	}
}
