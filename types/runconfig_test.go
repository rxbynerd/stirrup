package types

import (
	"fmt"
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

func TestRedact_ProvidersAPIKeys(t *testing.T) {
	rc := RunConfig{
		Providers: map[string]ProviderConfig{
			"backup": {Type: "openai-compatible", APIKeyRef: "secret://backup-key"},
			"public": {Type: "openai-compatible"},
		},
	}
	redacted := rc.Redact()
	if redacted.Providers["backup"].APIKeyRef != "secret://[REDACTED]" {
		t.Errorf("Providers[backup].APIKeyRef = %q, want redacted", redacted.Providers["backup"].APIKeyRef)
	}
	if redacted.Providers["public"].APIKeyRef != "" {
		t.Errorf("Providers[public].APIKeyRef = %q, want empty", redacted.Providers["public"].APIKeyRef)
	}
	if rc.Providers["backup"].APIKeyRef != "secret://backup-key" {
		t.Error("Redact mutated original Providers map")
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
		Provider:         ProviderConfig{Type: "anthropic"},
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
	c.Tools = ToolsConfig{BuiltIn: []string{"read_file", "list_directory", "search_files"}}
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("deny-side-effects should be accepted for read-only mode, got: %v", err)
	}
}

func TestValidateRunConfig_UnknownPermissionPolicy(t *testing.T) {
	c := validConfig()
	c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effect"}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for unknown permission policy type")
	}
	if !strings.Contains(err.Error(), "permissionPolicy") {
		t.Errorf("expected error to mention permissionPolicy, got: %v", err)
	}
}

func TestValidateRunConfig_UnknownRouterProvider(t *testing.T) {
	c := validConfig()
	c.ModelRouter = ModelRouterConfig{
		Type:     "static",
		Provider: "backup",
		Model:    "claude-sonnet-4-6",
	}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for unknown router provider")
	}
	if !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("expected error to mention unknown provider, got: %v", err)
	}
}

func TestValidateRunConfig_InvalidBuiltInTool(t *testing.T) {
	c := validConfig()
	c.Tools = ToolsConfig{BuiltIn: []string{"delete_everything"}}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for invalid builtin tool")
	}
	if !strings.Contains(err.Error(), "tools.builtIn") {
		t.Errorf("expected error to mention tools.builtIn, got: %v", err)
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

func TestValidateRunConfig_TimeoutMustBePositive(t *testing.T) {
	for _, timeout := range []int{0, -1} {
		t.Run(fmt.Sprintf("timeout_%d", timeout), func(t *testing.T) {
			c := validConfig()
			c.Timeout = &timeout
			err := ValidateRunConfig(c)
			if err == nil {
				t.Fatal("expected error for non-positive timeout")
			}
			if !strings.Contains(err.Error(), "timeout") {
				t.Errorf("expected error to mention timeout, got: %v", err)
			}
		})
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

func TestValidateRunConfig_ReadOnlyModeWithWriteToolInList(t *testing.T) {
	writeTools := []string{"write_file", "run_command"}
	for _, mode := range []string{"planning", "review", "research", "toil"} {
		for _, tool := range writeTools {
			t.Run(fmt.Sprintf("%s/%s", mode, tool), func(t *testing.T) {
				c := validConfig()
				c.Mode = mode
				c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
				c.Tools = ToolsConfig{BuiltIn: []string{"read_file", tool}}
				err := ValidateRunConfig(c)
				if err == nil {
					t.Fatalf("expected error for %s mode with %s tool", mode, tool)
				}
				errStr := err.Error()
				if !strings.Contains(errStr, "read-only mode") || !strings.Contains(errStr, tool) {
					t.Errorf("expected error mentioning read-only mode and %q, got: %v", tool, err)
				}
			})
		}
	}
}

func TestValidateRunConfig_ReadOnlyModeWithNoExplicitToolList(t *testing.T) {
	for _, mode := range []string{"planning", "review", "research", "toil"} {
		t.Run(mode, func(t *testing.T) {
			c := validConfig()
			c.Mode = mode
			c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
			// Tools.BuiltIn left nil (default: all tools enabled)
			err := ValidateRunConfig(c)
			if err == nil {
				t.Fatalf("expected error for %s mode with no explicit tool list", mode)
			}
			if !strings.Contains(err.Error(), "requires an explicit tools.builtIn list") {
				t.Errorf("expected error about explicit tools.builtIn list, got: %v", err)
			}
		})
	}
}

func TestValidateRunConfig_ReadOnlyModeWithOnlyReadTools(t *testing.T) {
	for _, mode := range []string{"planning", "review", "research", "toil"} {
		t.Run(mode, func(t *testing.T) {
			c := validConfig()
			c.Mode = mode
			c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
			c.Tools = ToolsConfig{BuiltIn: []string{"read_file", "list_directory", "search_files"}}
			if err := ValidateRunConfig(c); err != nil {
				t.Fatalf("expected no error for %s mode with read-only tools, got: %v", mode, err)
			}
		})
	}
}

func TestValidateRunConfig_ExecutionModeWithWriteTools(t *testing.T) {
	c := validConfig()
	c.Mode = "execution"
	c.Tools = ToolsConfig{BuiltIn: []string{"read_file", "write_file", "run_command"}}
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("expected no error for execution mode with write tools, got: %v", err)
	}
}

func TestValidateRunConfig_FollowUpGraceBound(t *testing.T) {
	c := validConfig()
	grace := 3601
	c.FollowUpGrace = &grace
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for followUpGrace > 3600")
	}
	if !strings.Contains(err.Error(), "followUpGrace") {
		t.Errorf("expected error to mention followUpGrace, got: %v", err)
	}

	// At the boundary should pass.
	grace = 3600
	c.FollowUpGrace = &grace
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("expected no error for followUpGrace=3600, got: %v", err)
	}
}

func TestValidateRunConfig_CostBudgetBound(t *testing.T) {
	c := validConfig()
	cost := 100.01
	c.MaxCostBudget = &cost
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for maxCostBudget > 100")
	}
	if !strings.Contains(err.Error(), "maxCostBudget") {
		t.Errorf("expected error to mention maxCostBudget, got: %v", err)
	}

	// At the boundary should pass.
	cost = 100.0
	c.MaxCostBudget = &cost
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("expected no error for maxCostBudget=100, got: %v", err)
	}
}

func TestValidateRunConfig_TokenBudgetBound(t *testing.T) {
	c := validConfig()
	tokens := 50_000_001
	c.MaxTokenBudget = &tokens
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for maxTokenBudget > 50_000_000")
	}
	if !strings.Contains(err.Error(), "maxTokenBudget") {
		t.Errorf("expected error to mention maxTokenBudget, got: %v", err)
	}

	// At the boundary should pass.
	tokens = 50_000_000
	c.MaxTokenBudget = &tokens
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("expected no error for maxTokenBudget=50_000_000, got: %v", err)
	}
}

func TestValidateRunConfig_NilBudgetsPass(t *testing.T) {
	c := validConfig()
	c.FollowUpGrace = nil
	c.MaxCostBudget = nil
	c.MaxTokenBudget = nil
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("expected no error for nil budget fields, got: %v", err)
	}
}
