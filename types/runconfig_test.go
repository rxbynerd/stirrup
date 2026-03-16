package types

import (
	"strings"
	"testing"
)

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

// --- ValidateRunConfig tests ---

func validConfig() *RunConfig {
	timeout := 60
	return &RunConfig{
		Mode:             "execution",
		MaxTurns:         20,
		Timeout:          &timeout,
		PermissionPolicy: PermissionPolicyConfig{Type: "allow-all"},
	}
}

func TestValidateRunConfig_Valid(t *testing.T) {
	if err := ValidateRunConfig(validConfig()); err != nil {
		t.Fatalf("expected nil error for valid config, got: %v", err)
	}
}

func TestValidateRunConfig_ReadOnlyModeWithAllowAll(t *testing.T) {
	readOnlyModes := []string{"planning", "review", "research", "toil"}
	for _, mode := range readOnlyModes {
		t.Run(mode, func(t *testing.T) {
			c := validConfig()
			c.Mode = mode
			c.PermissionPolicy = PermissionPolicyConfig{Type: "allow-all"}
			err := ValidateRunConfig(c)
			if err == nil {
				t.Fatalf("expected error for %s with allow-all, got nil", mode)
			}
			if !strings.Contains(err.Error(), mode) {
				t.Errorf("expected error to mention mode %q, got: %v", mode, err)
			}
		})
	}
}

func TestValidateRunConfig_ReadOnlyModeWithDenySideEffects(t *testing.T) {
	c := validConfig()
	c.Mode = "review"
	c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("deny-side-effects should be accepted for read-only mode, got: %v", err)
	}
}

func TestValidateRunConfig_MaxTurnsExceedsLimit(t *testing.T) {
	c := validConfig()
	c.MaxTurns = 101
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for maxTurns > 100")
	}
	if !strings.Contains(err.Error(), "maxTurns") {
		t.Errorf("expected error to mention maxTurns, got: %v", err)
	}
}

func TestValidateRunConfig_MaxTurnsZero(t *testing.T) {
	c := validConfig()
	c.MaxTurns = 0
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for maxTurns=0")
	}
	if !strings.Contains(err.Error(), "maxTurns") {
		t.Errorf("expected error to mention maxTurns, got: %v", err)
	}
}

func TestValidateRunConfig_MaxTurnsNegative(t *testing.T) {
	c := validConfig()
	c.MaxTurns = -1
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for maxTurns=-1")
	}
	if !strings.Contains(err.Error(), "maxTurns") {
		t.Errorf("expected error to mention maxTurns, got: %v", err)
	}
}

func TestValidateRunConfig_NilTimeout(t *testing.T) {
	c := validConfig()
	c.Timeout = nil
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for nil timeout")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("expected error to mention timeout, got: %v", err)
	}
}

func TestValidateRunConfig_TimeoutExceedsMax(t *testing.T) {
	c := validConfig()
	timeout := 3601
	c.Timeout = &timeout
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for timeout > 3600")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("expected error to mention timeout, got: %v", err)
	}
}

func TestValidateRunConfig_MultipleErrors(t *testing.T) {
	c := validConfig()
	c.Mode = "planning"
	c.PermissionPolicy = PermissionPolicyConfig{Type: "allow-all"}
	c.MaxTurns = 200
	c.Timeout = nil
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for multiple violations")
	}
	errStr := err.Error()
	// Should contain all three errors.
	if !strings.Contains(errStr, "planning") {
		t.Error("expected error to mention planning mode violation")
	}
	if !strings.Contains(errStr, "maxTurns") {
		t.Error("expected error to mention maxTurns violation")
	}
	if !strings.Contains(errStr, "timeout") {
		t.Error("expected error to mention timeout violation")
	}
}
