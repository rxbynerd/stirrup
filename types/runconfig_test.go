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

// TestDefaultReadOnlyBuiltInTools_PassesValidation locks in the invariant
// that DefaultReadOnlyBuiltInTools() is always a valid Tools.BuiltIn list
// for every read-only mode. Callers (notably the stirrup CLI) rely on
// this: if someone adds a new mode to readOnlyModes, or adds a new tool
// to mutatingTools that happens to also live in the default list,
// this test catches it before ValidateRunConfig starts rejecting every
// run booted in that mode.
func TestDefaultReadOnlyBuiltInTools_PassesValidation(t *testing.T) {
	defaults := DefaultReadOnlyBuiltInTools()
	if len(defaults) == 0 {
		t.Fatal("DefaultReadOnlyBuiltInTools returned an empty list")
	}

	// Sanity: none of the defaults should be a known mutating tool.
	for _, tool := range defaults {
		if mutatingTools[tool] {
			t.Errorf("DefaultReadOnlyBuiltInTools contains mutating tool %q", tool)
		}
		if !validBuiltInToolNames[tool] {
			t.Errorf("DefaultReadOnlyBuiltInTools contains unknown tool %q", tool)
		}
	}

	// Validation: the defaults must satisfy ValidateRunConfig for every
	// mode the validator treats as read-only. Iterate over the actual
	// readOnlyModes map so adding a new read-only mode without updating
	// the defaults (or vice versa) fails loudly.
	for mode := range readOnlyModes {
		t.Run(mode, func(t *testing.T) {
			c := validConfig()
			c.Mode = mode
			c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
			c.Tools = ToolsConfig{BuiltIn: DefaultReadOnlyBuiltInTools()}
			if err := ValidateRunConfig(c); err != nil {
				t.Fatalf("DefaultReadOnlyBuiltInTools should pass validation for mode %q, got: %v", mode, err)
			}
		})
	}
}

func TestIsReadOnlyMode(t *testing.T) {
	readOnly := []string{"planning", "review", "research", "toil"}
	for _, m := range readOnly {
		if !IsReadOnlyMode(m) {
			t.Errorf("IsReadOnlyMode(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"execution", "", "unknown"} {
		if IsReadOnlyMode(m) {
			t.Errorf("IsReadOnlyMode(%q) = true, want false", m)
		}
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
	c.MaxTokenBudget = nil
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("expected no error for nil budget fields, got: %v", err)
	}
}

func TestValidateRunConfig_OpenAIResponsesProvider(t *testing.T) {
	// The new openai-responses provider type must be accepted by validation.
	// Before this case existed, callers who configured a Responses adapter
	// would be rejected at validation time.
	c := validConfig()
	c.Provider = ProviderConfig{Type: "openai-responses", APIKeyRef: "secret://OPENAI_KEY"}
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("expected openai-responses to be accepted, got: %v", err)
	}
}

func TestValidateRunConfig_CredentialNilPassesValidation(t *testing.T) {
	c := validConfig()
	c.Provider.Credential = nil
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("nil credential should pass validation, got: %v", err)
	}
}

func TestValidateRunConfig_CredentialStaticPasses(t *testing.T) {
	c := validConfig()
	c.Provider.Credential = &CredentialConfig{Type: "static"}
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("static credential should pass validation, got: %v", err)
	}
}

func TestValidateRunConfig_CredentialAWSDefaultPasses(t *testing.T) {
	c := validConfig()
	c.Provider = ProviderConfig{
		Type:       "bedrock",
		Credential: &CredentialConfig{Type: "aws-default"},
	}
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("aws-default credential should pass validation, got: %v", err)
	}
}

func TestValidateRunConfig_CredentialUnsupportedType(t *testing.T) {
	c := validConfig()
	c.Provider.Credential = &CredentialConfig{Type: "kerberos"}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for unsupported credential type")
	}
	if !strings.Contains(err.Error(), "kerberos") {
		t.Errorf("error should mention unsupported type: %v", err)
	}
}

func TestValidateRunConfig_WebIdentityValid(t *testing.T) {
	c := validConfig()
	c.Provider = ProviderConfig{
		Type:   "bedrock",
		Region: "us-east-1",
		Credential: &CredentialConfig{
			Type:    "web-identity",
			RoleARN: "arn:aws:iam::123456789012:role/test",
			TokenSource: &TokenSourceConfig{
				Type:     "gke-metadata",
				Audience: "sts.amazonaws.com",
			},
		},
	}
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("valid web-identity config should pass, got: %v", err)
	}
}

func TestValidateRunConfig_WebIdentityMissingRoleARN(t *testing.T) {
	c := validConfig()
	c.Provider = ProviderConfig{
		Type: "bedrock",
		Credential: &CredentialConfig{
			Type: "web-identity",
			TokenSource: &TokenSourceConfig{
				Type:     "gke-metadata",
				Audience: "sts.amazonaws.com",
			},
		},
	}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for missing roleArn")
	}
	if !strings.Contains(err.Error(), "roleArn") {
		t.Errorf("error should mention roleArn: %v", err)
	}
}

func TestValidateRunConfig_WebIdentityMissingTokenSource(t *testing.T) {
	c := validConfig()
	c.Provider = ProviderConfig{
		Type: "bedrock",
		Credential: &CredentialConfig{
			Type:    "web-identity",
			RoleARN: "arn:aws:iam::123456789012:role/test",
		},
	}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for missing tokenSource")
	}
	if !strings.Contains(err.Error(), "tokenSource") {
		t.Errorf("error should mention tokenSource: %v", err)
	}
}

func TestValidateRunConfig_GKEMetadataMissingAudience(t *testing.T) {
	c := validConfig()
	c.Provider = ProviderConfig{
		Type: "bedrock",
		Credential: &CredentialConfig{
			Type:    "web-identity",
			RoleARN: "arn:aws:iam::123456789012:role/test",
			TokenSource: &TokenSourceConfig{
				Type: "gke-metadata",
			},
		},
	}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for missing audience")
	}
	if !strings.Contains(err.Error(), "audience") {
		t.Errorf("error should mention audience: %v", err)
	}
}

func TestValidateRunConfig_FileTokenSourceMissingPath(t *testing.T) {
	c := validConfig()
	c.Provider = ProviderConfig{
		Type: "bedrock",
		Credential: &CredentialConfig{
			Type:    "web-identity",
			RoleARN: "arn:aws:iam::123456789012:role/test",
			TokenSource: &TokenSourceConfig{
				Type: "file",
			},
		},
	}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for missing path")
	}
	if !strings.Contains(err.Error(), "path") {
		t.Errorf("error should mention path: %v", err)
	}
}

func TestValidateRunConfig_EnvTokenSourceMissingEnvVar(t *testing.T) {
	c := validConfig()
	c.Provider = ProviderConfig{
		Type: "bedrock",
		Credential: &CredentialConfig{
			Type:    "web-identity",
			RoleARN: "arn:aws:iam::123456789012:role/test",
			TokenSource: &TokenSourceConfig{
				Type: "env",
			},
		},
	}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for missing envVar")
	}
	if !strings.Contains(err.Error(), "envVar") {
		t.Errorf("error should mention envVar: %v", err)
	}
}

func TestValidateRunConfig_CredentialInProvidersMap(t *testing.T) {
	c := validConfig()
	c.Providers = map[string]ProviderConfig{
		"fallback": {
			Type: "bedrock",
			Credential: &CredentialConfig{
				Type: "web-identity",
				// Missing roleArn and tokenSource
			},
		},
	}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for invalid credential in providers map")
	}
	if !strings.Contains(err.Error(), "providers[fallback].credential") {
		t.Errorf("error should reference providers[fallback].credential path: %v", err)
	}
}

func TestRedact_CredentialConfigPreserved(t *testing.T) {
	rc := RunConfig{
		Provider: ProviderConfig{
			Type:      "bedrock",
			APIKeyRef: "secret://some-key",
			Credential: &CredentialConfig{
				Type:        "web-identity",
				RoleARN:     "arn:aws:iam::123456789012:role/test",
				SessionName: "stirrup",
				TokenSource: &TokenSourceConfig{
					Type:     "gke-metadata",
					Audience: "sts.amazonaws.com",
				},
			},
		},
	}
	redacted := rc.Redact()
	// APIKeyRef should be redacted
	if redacted.Provider.APIKeyRef != "secret://[REDACTED]" {
		t.Errorf("APIKeyRef should be redacted, got %q", redacted.Provider.APIKeyRef)
	}
	// Credential config should be preserved (not sensitive)
	if redacted.Provider.Credential == nil {
		t.Fatal("Credential should not be nil after redaction")
	}
	if redacted.Provider.Credential.RoleARN != "arn:aws:iam::123456789012:role/test" {
		t.Error("RoleARN should be preserved after redaction")
	}
	if redacted.Provider.Credential.TokenSource.Audience != "sts.amazonaws.com" {
		t.Error("TokenSource.Audience should be preserved after redaction")
	}
}

// --- ValidateRunConfig: APIKeyHeader / QueryParams (issue #48) ---

func TestValidateRunConfig_APIKeyHeader_Valid(t *testing.T) {
	cases := []string{"", "api-key", "x-api-key", "Ocp-Apim-Subscription-Key", "Authorization"}
	for _, header := range cases {
		t.Run(header, func(t *testing.T) {
			c := validConfig()
			c.Provider = ProviderConfig{Type: "openai-responses", APIKeyHeader: header}
			if err := ValidateRunConfig(c); err != nil {
				t.Errorf("expected nil error for valid header %q, got %v", header, err)
			}
		})
	}
}

func TestValidateRunConfig_APIKeyHeader_Rejected(t *testing.T) {
	cases := map[string]string{
		"contains colon":      "api-key:",
		"contains CR":         "api-key\r",
		"contains LF":         "api-key\nset-cookie: foo=bar",
		"contains tab":        "api\tkey",
		"contains space":      "api key",
		"contains underscore": "api_key",
		"contains slash":      "api/key",
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			c := validConfig()
			c.Provider = ProviderConfig{Type: "openai-responses", APIKeyHeader: header}
			err := ValidateRunConfig(c)
			if err == nil {
				t.Fatalf("expected error for invalid header %q, got nil", header)
			}
			if !strings.Contains(err.Error(), "apiKeyHeader") {
				t.Errorf("error should mention apiKeyHeader, got: %v", err)
			}
		})
	}
}

func TestValidateRunConfig_QueryParams_Valid(t *testing.T) {
	c := validConfig()
	c.Provider = ProviderConfig{
		Type: "openai-responses",
		QueryParams: map[string]string{
			"api-version":   "preview",
			"deployment.id": "gpt4_prod",
			"flag":          "value with spaces are fine in values",
		},
	}
	if err := ValidateRunConfig(c); err != nil {
		t.Errorf("expected nil error for valid query params, got %v", err)
	}
}

func TestValidateRunConfig_QueryParams_RejectsBadKeyChars(t *testing.T) {
	c := validConfig()
	c.Provider = ProviderConfig{
		Type:        "openai-responses",
		QueryParams: map[string]string{"api version": "preview"},
	}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for key with spaces, got nil")
	}
	if !strings.Contains(err.Error(), "queryParams") {
		t.Errorf("error should mention queryParams, got: %v", err)
	}
}

func TestValidateRunConfig_QueryParams_RejectsCRLFInValue(t *testing.T) {
	c := validConfig()
	c.Provider = ProviderConfig{
		Type:        "openai-responses",
		QueryParams: map[string]string{"api-version": "preview\r\nset-cookie: foo=bar"},
	}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for CRLF in value, got nil")
	}
	if !strings.Contains(err.Error(), "CR/LF") {
		t.Errorf("error should mention CR/LF, got: %v", err)
	}
}

func TestValidateRunConfig_QueryParams_RejectsOversize(t *testing.T) {
	// Build a value just over the 2048-byte cap so the encoded form trips it.
	huge := strings.Repeat("x", 2050)
	c := validConfig()
	c.Provider = ProviderConfig{
		Type:        "openai-responses",
		QueryParams: map[string]string{"k": huge},
	}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for oversize query string, got nil")
	}
	if !strings.Contains(err.Error(), "byte cap") {
		t.Errorf("error should mention byte cap, got: %v", err)
	}
}

func TestValidateRunConfig_QueryParams_RejectsEmptyKey(t *testing.T) {
	c := validConfig()
	c.Provider = ProviderConfig{
		Type:        "openai-responses",
		QueryParams: map[string]string{"": "value"},
	}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for empty key, got nil")
	}
	if !strings.Contains(err.Error(), "empty key") {
		t.Errorf("error should mention empty key, got: %v", err)
	}
}

// TestValidateRunConfig_AzureFieldsOnNonOpenAIProviderShapeStillEnforced
// pins the design choice that shape validation is universal: even if the
// fields will be ignored at runtime (because the provider is anthropic),
// keeping a malformed value in a stale config is a footgun. Forward
// compatibility means "ignore at runtime", not "skip validation".
func TestValidateRunConfig_AzureFieldsOnNonOpenAIProviderShapeStillEnforced(t *testing.T) {
	c := validConfig() // Provider.Type == "anthropic"
	c.Provider.APIKeyHeader = "bad: header"
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error even on anthropic with malformed header, got nil")
	}
}

// TestValidateRunConfig_AzureFieldsOnNonOpenAIProviderValidShape verifies
// the inverse of the above: well-formed APIKeyHeader / QueryParams on a
// non-OpenAI provider are tolerated (anthropic and bedrock will simply
// ignore them at runtime). This is forward compatibility.
func TestValidateRunConfig_AzureFieldsOnNonOpenAIProviderValidShape(t *testing.T) {
	c := validConfig() // Provider.Type == "anthropic"
	c.Provider.APIKeyHeader = "x-api-key"
	c.Provider.QueryParams = map[string]string{"hint": "future"}
	if err := ValidateRunConfig(c); err != nil {
		t.Errorf("expected nil error for forward-compatible fields on anthropic, got %v", err)
	}
}
