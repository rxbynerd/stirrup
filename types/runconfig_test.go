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

// TestRedact_SessionNamePreserved pins that SessionName survives Redact().
// SessionName is not a secret — it's the operator's chosen label and it
// must appear in persisted traces so logs and traces can be cross-
// referenced. If Redact() ever starts stripping SessionName, downstream
// trace consumers (eval lakehouse, JSONL replay) lose the link.
func TestRedact_SessionNamePreserved(t *testing.T) {
	rc := RunConfig{
		SessionName: "nightly-eval",
		Provider:    ProviderConfig{APIKeyRef: "secret://k"},
	}
	redacted := rc.Redact()
	if redacted.SessionName != "nightly-eval" {
		t.Errorf("SessionName should be preserved, got %q", redacted.SessionName)
	}
}

func TestRedact_EmptyConfig(t *testing.T) {
	rc := RunConfig{}
	redacted := rc.Redact()
	if redacted.Provider.APIKeyRef != "" {
		t.Error("empty APIKeyRef should stay empty")
	}
}

// TestRedact_TraceEmitterHeaders pins that a "secret://" value in a
// trace-emitter header is rewritten to "secret://[REDACTED]" by Redact()
// while plaintext values pass through unchanged. Issue #100: when
// Stirrup ships traces directly to a cloud gateway (Grafana Cloud,
// Honeycomb, etc.) the auth token rides on the Authorization header. The
// resolved bearer must never enter a persisted RunTrace, but the secret
// reference itself shouldn't either — an operator rotating the env var
// expects no trace of the old reference to remain.
func TestRedact_TraceEmitterHeaders(t *testing.T) {
	rc := RunConfig{
		TraceEmitter: TraceEmitterConfig{
			Type: "otel",
			Headers: map[string]string{
				"Authorization": "secret://GRAFANA_CLOUD_AUTH",
				"X-Tenant":      "team-a",
			},
		},
	}
	redacted := rc.Redact()

	if got := redacted.TraceEmitter.Headers["Authorization"]; got != "secret://[REDACTED]" {
		t.Errorf("Authorization header = %q, want secret://[REDACTED]", got)
	}
	if got := redacted.TraceEmitter.Headers["X-Tenant"]; got != "team-a" {
		t.Errorf("X-Tenant plaintext header = %q, want team-a (plaintext should pass through)", got)
	}

	// Original unchanged.
	if rc.TraceEmitter.Headers["Authorization"] != "secret://GRAFANA_CLOUD_AUTH" {
		t.Error("Redact mutated original TraceEmitter.Headers")
	}
}

// TestRedact_ProviderRetryNotAliased pins that Redact() deep-copies
// ProviderConfig.Retry on both the top-level Provider and every entry
// in Providers. The shallow copy `redacted := rc` aliases the Retry
// pointer; without an explicit deep-copy, a downstream consumer
// mutating the redacted config's Retry struct would reach back into
// the live RunConfig. No code mutates Retry today, but every other
// pointer field touched by Redact() is deep-copied — matching the
// established pattern closes the aliasing window before Wave 2 lands
// retry-helper code that could exercise it.
func TestRedact_ProviderRetryNotAliased(t *testing.T) {
	rc := RunConfig{
		Provider: ProviderConfig{
			Type:  "openai-compatible",
			Retry: &ProviderRetryConfig{MaxAttempts: 3, InitialDelayMs: 500},
		},
		Providers: map[string]ProviderConfig{
			"secondary": {
				Type:  "openai-compatible",
				Retry: &ProviderRetryConfig{MaxAttempts: 4, InitialDelayMs: 250},
			},
		},
	}
	redacted := rc.Redact()

	if redacted.Provider.Retry == nil {
		t.Fatal("top-level Retry dropped by Redact")
	}
	if redacted.Provider.Retry == rc.Provider.Retry {
		t.Fatal("top-level Retry pointer aliased — Redact must deep-copy")
	}
	redacted.Provider.Retry.MaxAttempts = 99
	if rc.Provider.Retry.MaxAttempts != 3 {
		t.Errorf("mutating redacted Provider.Retry leaked to original: got %d, want 3", rc.Provider.Retry.MaxAttempts)
	}

	redactedSecondary := redacted.Providers["secondary"]
	originalSecondary := rc.Providers["secondary"]
	if redactedSecondary.Retry == nil {
		t.Fatal("named-provider Retry dropped by Redact")
	}
	if redactedSecondary.Retry == originalSecondary.Retry {
		t.Fatal("named-provider Retry pointer aliased — Redact must deep-copy")
	}
	redactedSecondary.Retry.MaxAttempts = 88
	if rc.Providers["secondary"].Retry.MaxAttempts != 4 {
		t.Errorf("mutating redacted named-provider Retry leaked to original: got %d, want 4", rc.Providers["secondary"].Retry.MaxAttempts)
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

func TestValidateRunConfig_SessionNameEmpty(t *testing.T) {
	c := validConfig()
	c.SessionName = ""
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("empty SessionName should pass validation, got: %v", err)
	}
}

func TestValidateRunConfig_SessionNameValid(t *testing.T) {
	cases := []string{
		"nightly-eval",
		"PR #123 sweep",
		"café-run",              // unicode letter with diacritic
		"文字列-test",              // CJK characters are printable
		"emoji-ok-\xe2\x9c\x85", // U+2705 white heavy check mark (valid printable symbol)
		strings.Repeat("a", 255),
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			c := validConfig()
			c.SessionName = name
			if err := ValidateRunConfig(c); err != nil {
				t.Fatalf("SessionName %q should pass validation, got: %v", name, err)
			}
		})
	}
}

func TestValidateRunConfig_SessionNameTooLong(t *testing.T) {
	c := validConfig()
	c.SessionName = strings.Repeat("a", 256)
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for SessionName > 255 bytes")
	}
	if !strings.Contains(err.Error(), "sessionName") || !strings.Contains(err.Error(), "255") {
		t.Errorf("error should describe the limit, got: %v", err)
	}
}

func TestValidateRunConfig_SessionNameRejectsControlChars(t *testing.T) {
	cases := map[string]string{
		"newline":  "bad\nname",
		"tab":      "bad\tname",
		"nul":      "bad\x00name",
		"del":      "bad\x7fname",
		"carriage": "bad\rname",
		"vtab":     "bad\vname",
		"escape":   "bad\x1bname",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			c := validConfig()
			c.SessionName = value
			err := ValidateRunConfig(c)
			if err == nil {
				t.Fatalf("expected error for SessionName containing %s", name)
			}
			if !strings.Contains(err.Error(), "sessionName") {
				t.Errorf("error should mention sessionName, got: %v", err)
			}
		})
	}
}

func TestValidateRunConfig_SessionNameRejectsInvalidUTF8(t *testing.T) {
	c := validConfig()
	// 0xff is never valid as a leading UTF-8 byte.
	c.SessionName = "bad\xffname"
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for invalid UTF-8 in SessionName")
	}
	if !strings.Contains(err.Error(), "sessionName") {
		t.Errorf("error should mention sessionName, got: %v", err)
	}
}

func TestValidateRunConfig_OpenAIResponsesProvider(t *testing.T) {
	// The new openai-responses provider type must be accepted by validation.
	// Before this case existed, callers who configured a Responses adapter
	// would be rejected at validation time.
	//
	// We pin Tools.BuiltIn to a side-effect-free set so this test stays
	// focused on provider-type validation; with the default (nil) tool
	// list every built-in is enabled, which combined with the secret
	// reference would trigger Rule of Two and obscure the actual
	// failure mode this test is asserting against.
	c := validConfig()
	c.Provider = ProviderConfig{Type: "openai-responses", APIKeyRef: "secret://OPENAI_KEY"}
	c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
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

func TestValidateRunConfig_AWSIRSATokenSourceValid(t *testing.T) {
	c := validConfig()
	c.Provider = ProviderConfig{
		Type:   "bedrock",
		Region: "us-east-1",
		Credential: &CredentialConfig{
			Type:    "web-identity",
			RoleARN: "arn:aws:iam::123456789012:role/test",
			TokenSource: &TokenSourceConfig{
				Type: "aws-irsa",
			},
		},
	}
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("aws-irsa should validate without extra fields, got: %v", err)
	}
}

func TestValidateRunConfig_AzureIMDSTokenSourceValid(t *testing.T) {
	c := validConfig()
	c.Provider = ProviderConfig{
		Type:   "bedrock",
		Region: "us-east-1",
		Credential: &CredentialConfig{
			Type:    "web-identity",
			RoleARN: "arn:aws:iam::123456789012:role/test",
			TokenSource: &TokenSourceConfig{
				Type:     "azure-imds",
				Resource: "https://management.azure.com/",
			},
		},
	}
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("azure-imds with resource should validate, got: %v", err)
	}
}

func TestValidateRunConfig_AzureIMDSTokenSourceMissingResource(t *testing.T) {
	c := validConfig()
	c.Provider = ProviderConfig{
		Type: "bedrock",
		Credential: &CredentialConfig{
			Type:    "web-identity",
			RoleARN: "arn:aws:iam::123456789012:role/test",
			TokenSource: &TokenSourceConfig{
				Type: "azure-imds",
			},
		},
	}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for missing resource")
	}
	if !strings.Contains(err.Error(), "resource") {
		t.Errorf("error should mention resource: %v", err)
	}
}

func TestValidateRunConfig_GitHubActionsOIDCTokenSourceValid(t *testing.T) {
	c := validConfig()
	c.Provider = ProviderConfig{
		Type:   "bedrock",
		Region: "us-east-1",
		Credential: &CredentialConfig{
			Type:    "web-identity",
			RoleARN: "arn:aws:iam::123456789012:role/test",
			TokenSource: &TokenSourceConfig{
				Type:     "github-actions-oidc",
				Audience: "sts.amazonaws.com",
			},
		},
	}
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("github-actions-oidc with audience should validate, got: %v", err)
	}
}

func TestValidateRunConfig_GitHubActionsOIDCTokenSourceMissingAudience(t *testing.T) {
	c := validConfig()
	c.Provider = ProviderConfig{
		Type: "bedrock",
		Credential: &CredentialConfig{
			Type:    "web-identity",
			RoleARN: "arn:aws:iam::123456789012:role/test",
			TokenSource: &TokenSourceConfig{
				Type: "github-actions-oidc",
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

// TestRedact_AnthropicWIFFieldsPreserved verifies that the four
// Anthropic federation identifiers ride through Redact() unchanged.
// Per issue #117 and Anthropic's WIF reference docs, these are
// non-secret values intended to be safe to commit to source control or
// bake into a container image; redacting them would needlessly hide
// information operators need to debug a federation failure from a
// stored trace.
func TestRedact_AnthropicWIFFieldsPreserved(t *testing.T) {
	rc := RunConfig{
		Provider: ProviderConfig{
			Type: "anthropic",
			Credential: &CredentialConfig{
				Type:             "anthropic-wif",
				FederationRuleID: "fdrl_abc123",
				OrganizationID:   "550e8400-e29b-41d4-a716-446655440000",
				ServiceAccountID: "svac_xyz789",
				WorkspaceID:      "wrkspc_def456",
				TokenSource: &TokenSourceConfig{
					Type:     "github-actions-oidc",
					Audience: "https://api.anthropic.com",
				},
			},
		},
	}
	redacted := rc.Redact()
	cred := redacted.Provider.Credential
	if cred == nil {
		t.Fatal("Credential should not be nil after redaction")
	}
	if cred.FederationRuleID != "fdrl_abc123" {
		t.Errorf("FederationRuleID should be preserved, got %q", cred.FederationRuleID)
	}
	if cred.OrganizationID != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("OrganizationID should be preserved, got %q", cred.OrganizationID)
	}
	if cred.ServiceAccountID != "svac_xyz789" {
		t.Errorf("ServiceAccountID should be preserved, got %q", cred.ServiceAccountID)
	}
	if cred.WorkspaceID != "wrkspc_def456" {
		t.Errorf("WorkspaceID should be preserved, got %q", cred.WorkspaceID)
	}
}

// validAnthropicWIFCredential builds a credential config that satisfies
// every required field for the anthropic-wif type. Negative-path tests
// mutate one field at a time off this baseline.
func validAnthropicWIFCredential() *CredentialConfig {
	return &CredentialConfig{
		Type:             "anthropic-wif",
		FederationRuleID: "fdrl_abc123",
		OrganizationID:   "550e8400-e29b-41d4-a716-446655440000",
		ServiceAccountID: "svac_xyz789",
		TokenSource: &TokenSourceConfig{
			Type:     "github-actions-oidc",
			Audience: "https://api.anthropic.com",
		},
	}
}

func TestValidateRunConfig_AnthropicWIF(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(c *RunConfig)
		wantErr   bool
		errSubstr string
	}{
		{
			name: "minimal anthropic-wif config passes",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = validAnthropicWIFCredential()
			},
			wantErr: false,
		},
		{
			name: "workspaceId default literal accepted",
			mutate: func(c *RunConfig) {
				cred := validAnthropicWIFCredential()
				cred.WorkspaceID = "default"
				c.Provider.Credential = cred
			},
			wantErr: false,
		},
		{
			name: "workspaceId structured wrkspc_ accepted",
			mutate: func(c *RunConfig) {
				cred := validAnthropicWIFCredential()
				cred.WorkspaceID = "wrkspc_def456"
				c.Provider.Credential = cred
			},
			wantErr: false,
		},
		{
			name: "missing federationRuleId fails",
			mutate: func(c *RunConfig) {
				cred := validAnthropicWIFCredential()
				cred.FederationRuleID = ""
				c.Provider.Credential = cred
			},
			wantErr:   true,
			errSubstr: "anthropic-wif requires federationRuleId",
		},
		{
			name: "missing organizationId fails",
			mutate: func(c *RunConfig) {
				cred := validAnthropicWIFCredential()
				cred.OrganizationID = ""
				c.Provider.Credential = cred
			},
			wantErr:   true,
			errSubstr: "anthropic-wif requires organizationId",
		},
		{
			name: "missing serviceAccountId fails",
			mutate: func(c *RunConfig) {
				cred := validAnthropicWIFCredential()
				cred.ServiceAccountID = ""
				c.Provider.Credential = cred
			},
			wantErr:   true,
			errSubstr: "anthropic-wif requires serviceAccountId",
		},
		{
			name: "missing tokenSource fails",
			mutate: func(c *RunConfig) {
				cred := validAnthropicWIFCredential()
				cred.TokenSource = nil
				c.Provider.Credential = cred
			},
			wantErr:   true,
			errSubstr: "anthropic-wif requires tokenSource",
		},
		{
			name: "federationRuleId without fdrl_ prefix rejected",
			mutate: func(c *RunConfig) {
				cred := validAnthropicWIFCredential()
				cred.FederationRuleID = "abc123"
				c.Provider.Credential = cred
			},
			wantErr:   true,
			errSubstr: "federationRuleId",
		},
		{
			name: "federationRuleId with empty suffix rejected",
			mutate: func(c *RunConfig) {
				cred := validAnthropicWIFCredential()
				cred.FederationRuleID = "fdrl_"
				c.Provider.Credential = cred
			},
			wantErr:   true,
			errSubstr: "federationRuleId",
		},
		{
			name: "organizationId uppercase rejected",
			mutate: func(c *RunConfig) {
				cred := validAnthropicWIFCredential()
				cred.OrganizationID = "550E8400-E29B-41D4-A716-446655440000"
				c.Provider.Credential = cred
			},
			wantErr:   true,
			errSubstr: "organizationId",
		},
		{
			name: "organizationId not a UUID rejected",
			mutate: func(c *RunConfig) {
				cred := validAnthropicWIFCredential()
				cred.OrganizationID = "not-a-uuid"
				c.Provider.Credential = cred
			},
			wantErr:   true,
			errSubstr: "organizationId",
		},
		{
			name: "serviceAccountId without svac_ prefix rejected",
			mutate: func(c *RunConfig) {
				cred := validAnthropicWIFCredential()
				cred.ServiceAccountID = "xyz789"
				c.Provider.Credential = cred
			},
			wantErr:   true,
			errSubstr: "serviceAccountId",
		},
		{
			name: "workspaceId other plain string rejected",
			mutate: func(c *RunConfig) {
				cred := validAnthropicWIFCredential()
				cred.WorkspaceID = "main"
				c.Provider.Credential = cred
			},
			wantErr:   true,
			errSubstr: "workspaceId",
		},
		{
			name: "workspaceId without wrkspc_ prefix rejected",
			mutate: func(c *RunConfig) {
				cred := validAnthropicWIFCredential()
				cred.WorkspaceID = "def456"
				c.Provider.Credential = cred
			},
			wantErr:   true,
			errSubstr: "workspaceId",
		},
		{
			name: "apiKeyRef set alongside anthropic-wif rejected",
			mutate: func(c *RunConfig) {
				c.Provider.APIKeyRef = "secret://ANTHROPIC_API_KEY"
				c.Provider.Credential = validAnthropicWIFCredential()
			},
			wantErr:   true,
			errSubstr: "apiKeyRef must not be set when credential.type is \"anthropic-wif\"",
		},
		{
			name: "roleArn on anthropic-wif rejected",
			mutate: func(c *RunConfig) {
				cred := validAnthropicWIFCredential()
				cred.RoleARN = "arn:aws:iam::123456789012:role/StirrupBedrock"
				c.Provider.Credential = cred
			},
			wantErr:   true,
			errSubstr: "roleArn is only valid for credential type",
		},
		{
			name: "audience on anthropic-wif rejected",
			mutate: func(c *RunConfig) {
				cred := validAnthropicWIFCredential()
				cred.Audience = "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/q"
				c.Provider.Credential = cred
			},
			wantErr:   true,
			errSubstr: "audience is only valid for credential type",
		},
		{
			name: "serviceAccount on anthropic-wif rejected",
			mutate: func(c *RunConfig) {
				cred := validAnthropicWIFCredential()
				cred.ServiceAccount = "vertex@my-project.iam.gserviceaccount.com"
				c.Provider.Credential = cred
			},
			wantErr:   true,
			errSubstr: "serviceAccount is only valid for credential type",
		},
		{
			name: "sessionName on anthropic-wif rejected",
			mutate: func(c *RunConfig) {
				cred := validAnthropicWIFCredential()
				cred.SessionName = "stirrup"
				c.Provider.Credential = cred
			},
			wantErr:   true,
			errSubstr: "sessionName is only valid for credential type",
		},
		{
			name: "federationRuleId on web-identity rejected",
			mutate: func(c *RunConfig) {
				c.Provider = ProviderConfig{
					Type:   "bedrock",
					Region: "us-east-1",
					Credential: &CredentialConfig{
						Type:             "web-identity",
						RoleARN:          "arn:aws:iam::123456789012:role/test",
						FederationRuleID: "fdrl_leak",
						TokenSource: &TokenSourceConfig{
							Type: "aws-irsa",
						},
					},
				}
			},
			wantErr:   true,
			errSubstr: "federationRuleId is only valid for credential type \"anthropic-wif\"",
		},
		{
			name: "organizationId on static rejected",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{
					Type:           "static",
					OrganizationID: "550e8400-e29b-41d4-a716-446655440000",
				}
			},
			wantErr:   true,
			errSubstr: "organizationId is only valid for credential type \"anthropic-wif\"",
		},
		{
			name: "serviceAccountId on gcp-default rejected",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{
					Type:             "gcp-default",
					ServiceAccountID: "svac_leak",
				}
			},
			wantErr:   true,
			errSubstr: "serviceAccountId is only valid for credential type \"anthropic-wif\"",
		},
		{
			name: "workspaceId on aws-default rejected",
			mutate: func(c *RunConfig) {
				c.Provider = ProviderConfig{
					Type:   "bedrock",
					Region: "us-east-1",
					Credential: &CredentialConfig{
						Type:        "aws-default",
						WorkspaceID: "default",
					},
				}
			},
			wantErr:   true,
			errSubstr: "workspaceId is only valid for credential type \"anthropic-wif\"",
		},
		{
			// Cross-provider validation (issue #117 N4 / important):
			// pairing credential.type=anthropic-wif with a non-Anthropic
			// provider type would result in stirrup exchanging a WIF
			// access token (sk-ant-oat01-...) and handing it to a
			// third-party endpoint. Fail closed at config-load time.
			name: "anthropic-wif paired with openai-compatible rejected",
			mutate: func(c *RunConfig) {
				c.Provider = ProviderConfig{
					Type:       "openai-compatible",
					BaseURL:    "https://example.invalid/v1",
					APIKeyRef:  "secret://OPENAI_KEY",
					Credential: validAnthropicWIFCredential(),
				}
			},
			wantErr:   true,
			errSubstr: "anthropic-wif",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(c)
			err := ValidateRunConfig(c)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.errSubstr)
				}
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("expected error to contain %q, got: %v", tc.errSubstr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
		})
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

// --- ExecutorConfig.Runtime ---

// TestValidateRunConfig_ExecutorRuntimeAcceptsClosedSet locks in the
// closed set of OCI runtimes. Adding a new runtime here without
// updating validExecutorRuntimes (or vice versa) fails loudly so the
// validator does not silently accept an unknown runtime string.
func TestValidateRunConfig_ExecutorRuntimeAcceptsClosedSet(t *testing.T) {
	for _, runtime := range []string{"", "runc", "runsc", "kata", "kata-qemu", "kata-fc"} {
		t.Run(fmt.Sprintf("runtime_%s", runtime), func(t *testing.T) {
			c := validConfig()
			c.Executor = ExecutorConfig{Type: "container", Runtime: runtime}
			if err := ValidateRunConfig(c); err != nil {
				t.Fatalf("expected runtime %q to validate, got: %v", runtime, err)
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

func TestValidateRunConfig_ExecutorRuntimeRejectsUnknown(t *testing.T) {
	c := validConfig()
	c.Executor = ExecutorConfig{Type: "container", Runtime: "foo"}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for unknown executor.runtime")
	}
	if !strings.Contains(err.Error(), "executor.runtime") || !strings.Contains(err.Error(), "foo") {
		t.Errorf("expected error to mention executor.runtime and the bad value, got: %v", err)
	}
}

// --- PermissionPolicyConfig.Type policy-engine ---

func TestValidateRunConfig_PolicyEngineRequiresPolicyFile(t *testing.T) {
	c := validConfig()
	c.PermissionPolicy = PermissionPolicyConfig{Type: "policy-engine"}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for policy-engine without policyFile")
	}
	if !strings.Contains(err.Error(), "policyFile") {
		t.Errorf("expected error to mention policyFile, got: %v", err)
	}
}

func TestValidateRunConfig_PolicyEngineWithPolicyFilePasses(t *testing.T) {
	c := validConfig()
	c.PermissionPolicy = PermissionPolicyConfig{Type: "policy-engine", PolicyFile: "/policies/main.cedar"}
	c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}} // skirt the Rule-of-Two trip
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("expected policy-engine + policyFile to validate, got: %v", err)
	}
}

func TestValidateRunConfig_FallbackAcceptsThreeNonEngineTypes(t *testing.T) {
	for _, fallback := range []string{"allow-all", "deny-side-effects", "ask-upstream"} {
		t.Run(fallback, func(t *testing.T) {
			c := validConfig()
			c.PermissionPolicy = PermissionPolicyConfig{
				Type:       "policy-engine",
				PolicyFile: "/policies/main.cedar",
				Fallback:   fallback,
			}
			c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
			if err := ValidateRunConfig(c); err != nil {
				t.Fatalf("expected fallback %q to validate, got: %v", fallback, err)
			}
		})
	}
}

func TestValidateRunConfig_FallbackRejectsPolicyEngine(t *testing.T) {
	c := validConfig()
	c.PermissionPolicy = PermissionPolicyConfig{
		Type:       "policy-engine",
		PolicyFile: "/policies/main.cedar",
		Fallback:   "policy-engine", // chained engines are not allowed
	}
	c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for fallback=policy-engine")
	}
	if !strings.Contains(err.Error(), "fallback") {
		t.Errorf("expected error to mention fallback, got: %v", err)
	}
}

func TestValidateRunConfig_FallbackRejectsUnknown(t *testing.T) {
	c := validConfig()
	c.PermissionPolicy = PermissionPolicyConfig{
		Type:       "policy-engine",
		PolicyFile: "/policies/main.cedar",
		Fallback:   "lasso",
	}
	c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for unknown fallback")
	}
	if !strings.Contains(err.Error(), "fallback") || !strings.Contains(err.Error(), "lasso") {
		t.Errorf("expected error to mention fallback and the bad value, got: %v", err)
	}
}

// TestValidateRunConfig_PolicyFile_PathTraversalRejected covers M6:
// a forged policyFile containing ".." must be rejected before any
// os.ReadFile happens. Without this, a malicious control plane could
// trick the harness into reading host files outside the workspace
// and leaking partial content via Cedar parser error messages.
func TestValidateRunConfig_PolicyFile_PathTraversalRejected(t *testing.T) {
	cases := []string{
		"../../etc/passwd",
		"policies/../../etc/passwd",
		"/policies/../../etc/passwd",
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			c := validConfig()
			c.PermissionPolicy = PermissionPolicyConfig{Type: "policy-engine", PolicyFile: p}
			c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
			err := ValidateRunConfig(c)
			if err == nil {
				t.Fatalf("expected error for traversal path %q", p)
			}
			if !strings.Contains(err.Error(), "policyFile") {
				t.Errorf("expected error to mention policyFile, got: %v", err)
			}
		})
	}
}

// TestValidateRunConfig_PolicyFile_RelativePathAllowed confirms that
// relative paths without traversal segments still validate. The shipped
// example RunConfig uses one (examples/policies/destructive-shell.cedar)
// and we don't want to break it for operators who run against the repo
// checkout. M6's stricter "absolute-only" alternative was rejected for
// this reason.
func TestValidateRunConfig_PolicyFile_RelativePathAllowed(t *testing.T) {
	c := validConfig()
	c.PermissionPolicy = PermissionPolicyConfig{
		Type:       "policy-engine",
		PolicyFile: "examples/policies/destructive-shell.cedar",
	}
	c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("expected workspace-relative policyFile to validate, got: %v", err)
	}
}

// TestValidateRunConfig_PolicyFile_IgnoredWithWrongTypeIsError covers
// S7: a policyFile set with a non-policy-engine type is silently
// dropped today, leaving the operator believing they have applied a
// Cedar policy. Reject the misconfiguration loudly.
func TestValidateRunConfig_PolicyFile_IgnoredWithWrongTypeIsError(t *testing.T) {
	c := validConfig()
	c.PermissionPolicy = PermissionPolicyConfig{
		Type:       "deny-side-effects",
		PolicyFile: "/policies/main.cedar",
	}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for policyFile set with deny-side-effects")
	}
	if !strings.Contains(err.Error(), "policyFile") || !strings.Contains(err.Error(), "policy-engine") {
		t.Errorf("expected error to mention policyFile and policy-engine, got: %v", err)
	}
}

// TestValidateRunConfig_RuntimeRequiresContainerExecutor covers S8:
// executor.runtime only changes behaviour for the container executor.
// A "local" run that sets runtime: "runsc" looks like gVisor isolation
// is enabled but the field is silently ignored — fail loudly instead.
func TestValidateRunConfig_RuntimeRequiresContainerExecutor(t *testing.T) {
	cases := []string{"local", "api"}
	for _, execType := range cases {
		t.Run(execType, func(t *testing.T) {
			c := validConfig()
			c.Executor = ExecutorConfig{Type: execType, Runtime: "runsc"}
			if execType == "api" {
				c.Executor.VcsBackend = &VcsBackendConfig{
					Type: "github", APIKeyRef: "secret://gh", Repo: "x/y", Ref: "main",
				}
				c.Mode = "research"
				c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
				c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
			}
			err := ValidateRunConfig(c)
			if err == nil {
				t.Fatalf("expected error for runtime=runsc with executor.type=%q", execType)
			}
			if !strings.Contains(err.Error(), "executor.runtime") || !strings.Contains(err.Error(), "container") {
				t.Errorf("expected error to mention executor.runtime and container, got: %v", err)
			}
		})
	}
}

// --- CodeScannerConfig ---

func TestValidateRunConfig_CodeScannerAcceptsClosedSet(t *testing.T) {
	for _, scanner := range []string{"none", "patterns", "semgrep"} {
		t.Run(scanner, func(t *testing.T) {
			c := validConfig()
			c.CodeScanner = &CodeScannerConfig{Type: scanner}
			if err := ValidateRunConfig(c); err != nil {
				t.Fatalf("expected scanner %q to validate, got: %v", scanner, err)
			}
		})
	}
}

func TestValidateRunConfig_CodeScannerCompositeRequiresScanners(t *testing.T) {
	c := validConfig()
	c.CodeScanner = &CodeScannerConfig{Type: "composite"} // no scanners set
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for composite scanner with no scanners list")
	}
	if !strings.Contains(err.Error(), "composite") || !strings.Contains(err.Error(), "scanners") {
		t.Errorf("expected error to mention composite and scanners, got: %v", err)
	}
}

func TestValidateRunConfig_CodeScannerCompositeAcceptsKnownScanners(t *testing.T) {
	c := validConfig()
	c.CodeScanner = &CodeScannerConfig{
		Type:     "composite",
		Scanners: []string{"patterns", "semgrep"},
	}
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("expected composite scanner with patterns+semgrep to validate, got: %v", err)
	}
}

func TestValidateRunConfig_CodeScannerCompositeRejectsRecursive(t *testing.T) {
	c := validConfig()
	c.CodeScanner = &CodeScannerConfig{
		Type:     "composite",
		Scanners: []string{"composite"},
	}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for composite scanner referencing composite")
	}
	if !strings.Contains(err.Error(), "scanners") {
		t.Errorf("expected error to mention scanners, got: %v", err)
	}
}

func TestValidateRunConfig_CodeScannerRejectsUnknown(t *testing.T) {
	c := validConfig()
	c.CodeScanner = &CodeScannerConfig{Type: "magic"}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for unknown scanner type")
	}
	if !strings.Contains(err.Error(), "codeScanner.type") || !strings.Contains(err.Error(), "magic") {
		t.Errorf("expected error to mention codeScanner.type and the bad value, got: %v", err)
	}
}

func TestValidateRunConfig_CodeScannerDefaultsExecutionToPatterns(t *testing.T) {
	c := validConfig()
	c.CodeScanner = nil
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("validation failed: %v", err)
	}
	if c.CodeScanner == nil {
		t.Fatal("expected ValidateRunConfig to populate a default CodeScanner for execution mode")
	}
	if c.CodeScanner.Type != "patterns" {
		t.Errorf("expected execution-mode default %q, got %q", "patterns", c.CodeScanner.Type)
	}
}

func TestValidateRunConfig_CodeScannerDefaultsReadOnlyToNone(t *testing.T) {
	for _, mode := range []string{"planning", "review", "research", "toil"} {
		t.Run(mode, func(t *testing.T) {
			c := validConfig()
			c.Mode = mode
			c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
			c.Tools = ToolsConfig{BuiltIn: []string{"read_file", "list_directory"}}
			c.CodeScanner = nil
			if err := ValidateRunConfig(c); err != nil {
				t.Fatalf("validation failed for mode %q: %v", mode, err)
			}
			if c.CodeScanner == nil {
				t.Fatalf("expected ValidateRunConfig to populate a default CodeScanner for mode %q", mode)
			}
			if c.CodeScanner.Type != "none" {
				t.Errorf("expected read-only-mode default %q for mode %q, got %q", "none", mode, c.CodeScanner.Type)
			}
		})
	}
}

func TestValidateRunConfig_CodeScannerExplicitOverridesDefault(t *testing.T) {
	c := validConfig()
	c.CodeScanner = &CodeScannerConfig{Type: "none"} // explicitly opt out
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("validation failed: %v", err)
	}
	if c.CodeScanner.Type != "none" {
		t.Errorf("expected explicit none to be preserved, got %q", c.CodeScanner.Type)
	}
}

// --- Rule of Two ---

// boolRef is a tiny helper for the *bool fields that gate a Rule-of-Two
// override. Inlining a takeAddress(false) at every call site would
// pollute the table-driven tests below.
func boolRef(b bool) *bool { return &b }

// ruleOfTwoConfig builds a RunConfig that exercises specific
// combinations of the three Rule-of-Two flags. Each leg can be
// turned on independently:
//
//   - holdsUntrusted is enabled by populating DynamicContext.
//   - holdsSensitive is enabled via the explicit RunConfig.SensitiveData
//     declaration. Operational secret references (provider/VCS/MCP API
//     keys) deliberately do NOT trip this leg — see ruleOfTwoSensitiveData
//     for rationale.
//   - canCommExternal is enabled by setting a non-"none" NetworkConfig
//     (so we don't have to drag in the Tools.BuiltIn semantics, which
//     are exercised separately in the dedicated table-driven test).
//
// The default tool list is constrained so isolated leg-flips don't
// trigger extra capabilities by accident.
func ruleOfTwoConfig(untrusted, sensitive, external bool, policy string) *RunConfig {
	timeout := 60
	c := &RunConfig{
		Mode:             "execution",
		Provider:         ProviderConfig{Type: "anthropic"},
		MaxTurns:         20,
		Timeout:          &timeout,
		PermissionPolicy: PermissionPolicyConfig{Type: policy},
		Tools:            ToolsConfig{BuiltIn: []string{"read_file"}},
	}
	if untrusted {
		c.DynamicContext = map[string]DynamicContextValue{"issue_body": {Value: "untrusted text"}}
	}
	if sensitive {
		c.SensitiveData = boolRef(true)
	}
	if external {
		c.Executor = ExecutorConfig{Network: &NetworkConfig{Mode: "allowlist", Allowlist: []string{"api.example.com"}}}
	}
	return c
}

func TestValidateRunConfig_RuleOfTwo_AllThreeWithoutAskUpstreamRejected(t *testing.T) {
	for _, policy := range []string{"allow-all", "deny-side-effects"} {
		t.Run(policy, func(t *testing.T) {
			c := ruleOfTwoConfig(true, true, true, policy)
			err := ValidateRunConfig(c)
			if err == nil {
				t.Fatalf("expected Rule-of-Two rejection for policy %q with all three flags", policy)
			}
			if !strings.Contains(err.Error(), "Rule of Two") {
				t.Errorf("expected error to mention Rule of Two, got: %v", err)
			}
			if !strings.Contains(err.Error(), "untrusted-input") {
				t.Errorf("expected error to mention untrusted-input, got: %v", err)
			}
			if !strings.Contains(err.Error(), "sensitive-data") {
				t.Errorf("expected error to mention sensitive-data, got: %v", err)
			}
			if !strings.Contains(err.Error(), "external-communication") {
				t.Errorf("expected error to mention external-communication, got: %v", err)
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

func TestValidateRunConfig_RuleOfTwo_AskUpstreamPasses(t *testing.T) {
	c := ruleOfTwoConfig(true, true, true, "ask-upstream")
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("ask-upstream should bypass Rule-of-Two rejection, got: %v", err)
	}
}

func TestValidateRunConfig_RuleOfTwo_OverridePasses(t *testing.T) {
	c := ruleOfTwoConfig(true, true, true, "deny-side-effects")
	c.RuleOfTwo = &RuleOfTwoConfig{Enforce: boolRef(false)}
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("explicit RuleOfTwo.Enforce: false should bypass rejection, got: %v", err)
	}
}

func TestValidateRunConfig_RuleOfTwo_ExplicitTrueStillEnforces(t *testing.T) {
	c := ruleOfTwoConfig(true, true, true, "deny-side-effects")
	c.RuleOfTwo = &RuleOfTwoConfig{Enforce: boolRef(true)}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("RuleOfTwo.Enforce: true must not bypass the invariant")
	}
	if !strings.Contains(err.Error(), "Rule of Two") {
		t.Errorf("expected Rule-of-Two error, got: %v", err)
	}
}

func TestValidateRunConfig_RuleOfTwo_TwoOfThreePasses(t *testing.T) {
	cases := []struct {
		name      string
		untrusted bool
		sensitive bool
		external  bool
	}{
		{"untrusted+sensitive", true, true, false},
		{"untrusted+external", true, false, true},
		{"sensitive+external", false, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ruleOfTwoConfig(tc.untrusted, tc.sensitive, tc.external, "deny-side-effects")
			if err := ValidateRunConfig(c); err != nil {
				t.Fatalf("two-of-three should pass, got: %v", err)
			}
		})
	}
}

// TestRuleOfTwoState_MatchesValidator pins the public RuleOfTwoState
// helper to the same booleans the internal validator reasons over.
// Factory wiring uses this helper to decide when to emit
// rule_of_two_disabled / rule_of_two_warning events.
func TestRuleOfTwoState_MatchesValidator(t *testing.T) {
	cases := []struct {
		name                                              string
		untrusted, sensitive, external                    bool
		wantUntrusted, wantSensitive, wantCanCommExternal bool
	}{
		{"all_off", false, false, false, false, false, false},
		{"untrusted_only", true, false, false, true, false, false},
		{"sensitive_only", false, true, false, false, true, false},
		{"external_only", false, false, true, false, false, true},
		{"all_on", true, true, true, true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ruleOfTwoConfig(tc.untrusted, tc.sensitive, tc.external, "deny-side-effects")
			gotU, gotS, gotE := RuleOfTwoState(c)
			if gotU != tc.wantUntrusted || gotS != tc.wantSensitive || gotE != tc.wantCanCommExternal {
				t.Errorf("RuleOfTwoState = (%v, %v, %v), want (%v, %v, %v)",
					gotU, gotS, gotE,
					tc.wantUntrusted, tc.wantSensitive, tc.wantCanCommExternal)
			}
		})
	}
}

// TestRuleOfTwoState_NilSafe documents the contract: passing nil
// returns the all-false state rather than panicking. Factory wiring
// relies on this for defensive emission paths.
func TestRuleOfTwoState_NilSafe(t *testing.T) {
	u, s, e := RuleOfTwoState(nil)
	if u || s || e {
		t.Errorf("nil config should return (false, false, false), got (%v, %v, %v)", u, s, e)
	}
}

func TestValidateRunConfig_RuleOfTwo_OneOrZeroPasses(t *testing.T) {
	cases := []struct {
		name      string
		untrusted bool
		sensitive bool
		external  bool
	}{
		{"none", false, false, false},
		{"untrusted_only", true, false, false},
		{"sensitive_only", false, true, false},
		{"external_only", false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ruleOfTwoConfig(tc.untrusted, tc.sensitive, tc.external, "deny-side-effects")
			if err := ValidateRunConfig(c); err != nil {
				t.Fatalf("one-or-zero flags should pass, got: %v", err)
			}
		})
	}
}

// TestValidateRunConfig_RuleOfTwo_OperationalSecretRefDoesNotTrigger pins
// the deliberate semantic that operational secret references — provider
// API keys, VCS backend keys, MCP server keys, including SSM-backed ones
// — do NOT trip the sensitive-data leg of the Rule of Two. The harness
// keeps these out of the agent's reach (run_command env-allowlist, log
// scrubbing, SecretStore deferred resolution), so they are not "data the
// agent has access to" in the rule's sense. The opposite would degrade
// the rule to "rule of one" because every working config has a provider
// API key.
//
// This test combines untrusted-input (DynamicContext) + external-comm
// (web_fetch) with a worst-case secret reference; the run is expected
// to validate cleanly because no sensitive-data signal is set.
func TestValidateRunConfig_RuleOfTwo_OperationalSecretRefDoesNotTrigger(t *testing.T) {
	timeout := 60
	cases := []struct {
		name      string
		apiKeyRef string
	}{
		{"secret-named env", "secret://ANTHROPIC_API_KEY"},
		{"secret-named ssm", "secret://ssm:///prod/anthropic"},
		{"non-secret-named env", "secret://CONFIG_PATH"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &RunConfig{
				Mode:             "execution",
				Provider:         ProviderConfig{Type: "anthropic", APIKeyRef: tc.apiKeyRef},
				MaxTurns:         20,
				Timeout:          &timeout,
				PermissionPolicy: PermissionPolicyConfig{Type: "deny-side-effects"},
				DynamicContext:   map[string]DynamicContextValue{"x": {Value: "y"}}, // untrusted
				Tools:            ToolsConfig{BuiltIn: []string{"web_fetch"}},       // external
			}
			if err := ValidateRunConfig(c); err != nil {
				t.Fatalf("operational secret reference must not trip the sensitive-data leg, got: %v", err)
			}
		})
	}
}

// TestValidateRunConfig_RuleOfTwo_ExplicitSensitiveDataTriggers verifies
// the operator-supplied RunConfig.SensitiveData flag trips the leg.
// Combined with DynamicContext (untrusted) and web_fetch (external),
// this should produce the all-three rejection.
func TestValidateRunConfig_RuleOfTwo_ExplicitSensitiveDataTriggers(t *testing.T) {
	timeout := 60
	c := &RunConfig{
		Mode:             "execution",
		Provider:         ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
		MaxTurns:         20,
		Timeout:          &timeout,
		PermissionPolicy: PermissionPolicyConfig{Type: "deny-side-effects"},
		DynamicContext:   map[string]DynamicContextValue{"x": {Value: "y"}}, // untrusted
		Tools:            ToolsConfig{BuiltIn: []string{"web_fetch"}},       // external
		SensitiveData:    boolRef(true),                                     // explicit
	}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("explicit SensitiveData must trip the Rule-of-Two rejection alongside untrusted+external")
	}
	if !strings.Contains(err.Error(), "Rule of Two") {
		t.Errorf("expected Rule-of-Two error, got: %v", err)
	}
}

// TestValidateRunConfig_RuleOfTwo_SensitiveDynamicContextEntryTriggers
// verifies a single per-entry Sensitive flag on a DynamicContext entry
// trips the leg. The motivating use case: a triage agent given a
// customer record block that's marked sensitive while other entries
// (issue body, repo metadata) remain non-sensitive.
func TestValidateRunConfig_RuleOfTwo_SensitiveDynamicContextEntryTriggers(t *testing.T) {
	timeout := 60
	c := &RunConfig{
		Mode:             "execution",
		Provider:         ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
		MaxTurns:         20,
		Timeout:          &timeout,
		PermissionPolicy: PermissionPolicyConfig{Type: "deny-side-effects"},
		DynamicContext: map[string]DynamicContextValue{
			"issue_body":      {Value: "non-sensitive"},
			"customer_record": {Value: "private", Sensitive: true},
		},
		Tools: ToolsConfig{BuiltIn: []string{"web_fetch"}},
	}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("a single sensitive DynamicContext entry must trip the Rule-of-Two rejection alongside untrusted+external")
	}
	if !strings.Contains(err.Error(), "Rule of Two") {
		t.Errorf("expected Rule-of-Two error, got: %v", err)
	}
}

// TestValidateRunConfig_RuleOfTwo_DefaultIsNotSensitive pins the
// out-of-the-box behavior: with neither RunConfig.SensitiveData nor any
// sensitive DynamicContext entry, the sensitive-data leg is false and
// a config with untrusted + external (which is what a bare
// `stirrup harness --prompt "x"` produces) validates cleanly. This is
// the regression guard for the original issue: PR #51's heuristic was
// always tripping sensitive-data on a bare invocation.
func TestValidateRunConfig_RuleOfTwo_DefaultIsNotSensitive(t *testing.T) {
	timeout := 60
	c := &RunConfig{
		Mode:             "execution",
		Provider:         ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
		MaxTurns:         20,
		Timeout:          &timeout,
		PermissionPolicy: PermissionPolicyConfig{Type: "allow-all"},
		DynamicContext:   map[string]DynamicContextValue{"issue_body": {Value: "y"}}, // untrusted
		Tools:            ToolsConfig{BuiltIn: []string{"web_fetch", "run_command"}}, // external
	}
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("default (no SensitiveData, no sensitive entry) must not trip the leg, got: %v", err)
	}
}

// TestValidateRunConfig_RuleOfTwo_ToolListReflectsActualBuiltIn verifies
// the issue brief's "tool-enabled checks must reflect the actual
// tools.builtIn list". A run-command-disabled config must not be
// flagged as canCommunicateExternally on that leg alone — even though
// "all tools enabled" (empty list) would.
func TestValidateRunConfig_RuleOfTwo_ToolListReflectsActualBuiltIn(t *testing.T) {
	timeout := 60
	c := &RunConfig{
		Mode:             "execution",
		Provider:         ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
		MaxTurns:         20,
		Timeout:          &timeout,
		PermissionPolicy: PermissionPolicyConfig{Type: "allow-all"},
		DynamicContext:   map[string]DynamicContextValue{"x": {Value: "y"}},
		SensitiveData:    boolRef(true),
		// Explicit list excludes web_fetch / run_command / mcp, and the
		// executor has no network config — no external-communication leg.
		Tools: ToolsConfig{BuiltIn: []string{"read_file", "list_directory"}},
	}
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("config with two flags only should pass, got: %v", err)
	}
}

// --- GuardRailConfig ---

// TestValidateGuardRailConfig is a table-driven exercise of every
// invariant validateGuardRailConfig enforces. Each case wraps a
// GuardRailConfig in an otherwise-valid RunConfig so the closed-set,
// range, and cross-field checks fire exactly as they would in
// production.
func TestValidateGuardRailConfig(t *testing.T) {
	think := true
	cases := []struct {
		name      string
		guard     *GuardRailConfig
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "nil",
			guard:   nil,
			wantErr: false,
		},
		{
			name:    "empty type with no fields",
			guard:   &GuardRailConfig{},
			wantErr: false,
		},
		{
			name:      "empty type with adapter fields",
			guard:     &GuardRailConfig{Endpoint: "http://x"},
			wantErr:   true,
			errSubstr: "guardRail.type is required",
		},
		{
			name:    "type none alone",
			guard:   &GuardRailConfig{Type: "none"},
			wantErr: false,
		},
		{
			name:      "type bogus",
			guard:     &GuardRailConfig{Type: "bogus"},
			wantErr:   true,
			errSubstr: "unsupported guardRail.type",
		},
		{
			name:      "granite-guardian without endpoint",
			guard:     &GuardRailConfig{Type: "granite-guardian"},
			wantErr:   true,
			errSubstr: "requires endpoint",
		},
		{
			name:    "granite-guardian with endpoint",
			guard:   &GuardRailConfig{Type: "granite-guardian", Endpoint: "http://vllm:8000/v1/chat/completions"},
			wantErr: false,
		},
		{
			name:      "granite-guardian endpoint not a url",
			guard:     &GuardRailConfig{Type: "granite-guardian", Endpoint: "not a url"},
			wantErr:   true,
			errSubstr: "guardRail.endpoint",
		},
		{
			name:      "granite-guardian endpoint ftp scheme",
			guard:     &GuardRailConfig{Type: "granite-guardian", Endpoint: "ftp://x/y"},
			wantErr:   true,
			errSubstr: "scheme http or https",
		},
		{
			name:      "granite-guardian endpoint missing host",
			guard:     &GuardRailConfig{Type: "granite-guardian", Endpoint: "http:///path"},
			wantErr:   true,
			errSubstr: "must include a host",
		},
		{
			name:      "composite empty stages",
			guard:     &GuardRailConfig{Type: "composite"},
			wantErr:   true,
			errSubstr: "non-empty stages",
		},
		{
			name: "composite of composite rejected",
			guard: &GuardRailConfig{
				Type: "composite",
				Stages: []GuardRailConfig{
					{Type: "composite", Stages: []GuardRailConfig{{Type: "none"}}},
				},
			},
			wantErr:   true,
			errSubstr: "stages[0].type",
		},
		{
			name: "composite with valid stages",
			guard: &GuardRailConfig{
				Type: "composite",
				Stages: []GuardRailConfig{
					{Type: "granite-guardian", Endpoint: "http://vllm:8000"},
					{Type: "none"},
				},
			},
			wantErr: false,
		},
		{
			name:      "composite with endpoint",
			guard:     &GuardRailConfig{Type: "composite", Endpoint: "http://x", Stages: []GuardRailConfig{{Type: "none"}}},
			wantErr:   true,
			errSubstr: "endpoint is not valid for type=composite",
		},
		{
			name:      "phases bogus",
			guard:     &GuardRailConfig{Type: "none", Phases: []string{"bogus"}},
			wantErr:   true,
			errSubstr: "is not a valid phase",
		},
		{
			name:      "phases duplicate",
			guard:     &GuardRailConfig{Type: "none", Phases: []string{"pre_turn", "pre_turn"}},
			wantErr:   true,
			errSubstr: "duplicate",
		},
		{
			name:    "phases all three",
			guard:   &GuardRailConfig{Type: "none", Phases: []string{"pre_turn", "pre_tool", "post_turn"}},
			wantErr: false,
		},
		{
			name:      "threshold below range",
			guard:     &GuardRailConfig{Type: "granite-guardian", Endpoint: "http://x", Threshold: -0.1},
			wantErr:   true,
			errSubstr: "threshold",
		},
		{
			name:      "threshold above range",
			guard:     &GuardRailConfig{Type: "granite-guardian", Endpoint: "http://x", Threshold: 1.5},
			wantErr:   true,
			errSubstr: "threshold",
		},
		{
			name:    "threshold zero",
			guard:   &GuardRailConfig{Type: "granite-guardian", Endpoint: "http://x", Threshold: 0},
			wantErr: false,
		},
		{
			name:    "threshold half",
			guard:   &GuardRailConfig{Type: "granite-guardian", Endpoint: "http://x", Threshold: 0.5},
			wantErr: false,
		},
		{
			name:    "threshold one",
			guard:   &GuardRailConfig{Type: "granite-guardian", Endpoint: "http://x", Threshold: 1.0},
			wantErr: false,
		},
		{
			name:      "timeoutMs below range",
			guard:     &GuardRailConfig{Type: "granite-guardian", Endpoint: "http://x", TimeoutMs: 49},
			wantErr:   true,
			errSubstr: "timeoutMs",
		},
		{
			name:      "timeoutMs above range",
			guard:     &GuardRailConfig{Type: "granite-guardian", Endpoint: "http://x", TimeoutMs: 30001},
			wantErr:   true,
			errSubstr: "timeoutMs",
		},
		{
			name:    "timeoutMs at lower bound",
			guard:   &GuardRailConfig{Type: "granite-guardian", Endpoint: "http://x", TimeoutMs: 50},
			wantErr: false,
		},
		{
			name:    "timeoutMs typical",
			guard:   &GuardRailConfig{Type: "granite-guardian", Endpoint: "http://x", TimeoutMs: 1500},
			wantErr: false,
		},
		{
			name:    "timeoutMs at upper bound",
			guard:   &GuardRailConfig{Type: "granite-guardian", Endpoint: "http://x", TimeoutMs: 30000},
			wantErr: false,
		},
		{
			name:      "minChunkChars negative",
			guard:     &GuardRailConfig{Type: "granite-guardian", Endpoint: "http://x", MinChunkChars: -1},
			wantErr:   true,
			errSubstr: "minChunkChars",
		},
		{
			name:      "minChunkChars above max",
			guard:     &GuardRailConfig{Type: "granite-guardian", Endpoint: "http://x", MinChunkChars: 4097},
			wantErr:   true,
			errSubstr: "minChunkChars",
		},
		{
			name:    "minChunkChars zero",
			guard:   &GuardRailConfig{Type: "granite-guardian", Endpoint: "http://x", MinChunkChars: 0},
			wantErr: false,
		},
		{
			name:    "minChunkChars typical",
			guard:   &GuardRailConfig{Type: "granite-guardian", Endpoint: "http://x", MinChunkChars: 256},
			wantErr: false,
		},
		{
			name:    "minChunkChars at max",
			guard:   &GuardRailConfig{Type: "granite-guardian", Endpoint: "http://x", MinChunkChars: 4096},
			wantErr: false,
		},
		{
			name: "customCriteria empty key",
			guard: &GuardRailConfig{
				Type:           "granite-guardian",
				Endpoint:       "http://x",
				CustomCriteria: map[string]string{"": "rule"},
			},
			wantErr:   true,
			errSubstr: "customCriteria contains an empty key",
		},
		{
			name: "customCriteria uppercase key",
			guard: &GuardRailConfig{
				Type:           "granite-guardian",
				Endpoint:       "http://x",
				CustomCriteria: map[string]string{"HARM": "rule"},
			},
			wantErr:   true,
			errSubstr: "customCriteria key",
		},
		{
			name: "customCriteria leading digit key",
			guard: &GuardRailConfig{
				Type:           "granite-guardian",
				Endpoint:       "http://x",
				CustomCriteria: map[string]string{"1bad": "rule"},
			},
			wantErr:   true,
			errSubstr: "customCriteria key",
		},
		{
			name: "customCriteria valid key",
			guard: &GuardRailConfig{
				Type:           "granite-guardian",
				Endpoint:       "http://x",
				CustomCriteria: map[string]string{"prompt_injection": "rule"},
			},
			wantErr: false,
		},
		{
			name: "criteria empty entry",
			guard: &GuardRailConfig{
				Type:     "granite-guardian",
				Endpoint: "http://x",
				Criteria: []string{"harm", ""},
			},
			wantErr:   true,
			errSubstr: "criteria[1] is empty",
		},
		{
			name:      "endpoint set on type none",
			guard:     &GuardRailConfig{Type: "none", Endpoint: "http://x"},
			wantErr:   true,
			errSubstr: "endpoint is not valid for type=none",
		},
		{
			name:    "think pointer accepted",
			guard:   &GuardRailConfig{Type: "granite-guardian", Endpoint: "http://x", Think: &think},
			wantErr: false,
		},
		{
			name:    "cloud-judge without endpoint allowed",
			guard:   &GuardRailConfig{Type: "cloud-judge", Model: "claude-haiku-4-5"},
			wantErr: false,
		},
		{
			name:    "cloud-judge with https endpoint",
			guard:   &GuardRailConfig{Type: "cloud-judge", Endpoint: "https://api.anthropic.com/v1/messages"},
			wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.GuardRail = tc.guard
			err := ValidateRunConfig(c)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.errSubstr)
				}
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("expected error to contain %q, got: %v", tc.errSubstr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
		})
	}
}

// geminiValidConfig is the smallest RunConfig that exercises a healthy
// gemini provider — wave-1 baseline used by the negative-path tests
// below to swap one field at a time.
func geminiValidConfig() *RunConfig {
	c := validConfig()
	c.Provider = ProviderConfig{
		Type:        "gemini",
		GCPProject:  "my-project",
		GCPLocation: "us-central1",
	}
	return c
}

func TestValidateRunConfig_GeminiProvider(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(c *RunConfig)
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "minimal gemini config passes",
			mutate:  func(c *RunConfig) {},
			wantErr: false,
		},
		{
			name: "global location passes",
			mutate: func(c *RunConfig) {
				c.Provider.GCPLocation = "global"
			},
			wantErr: false,
		},
		{
			name: "missing gcpProject fails",
			mutate: func(c *RunConfig) {
				c.Provider.GCPProject = ""
			},
			wantErr:   true,
			errSubstr: "gcpProject is required",
		},
		{
			name: "missing gcpLocation fails",
			mutate: func(c *RunConfig) {
				c.Provider.GCPLocation = ""
			},
			wantErr:   true,
			errSubstr: "gcpLocation is required",
		},
		{
			name: "apiKeyRef on gemini rejected with redirect",
			mutate: func(c *RunConfig) {
				c.Provider.APIKeyRef = "secret://GEMINI_KEY"
			},
			wantErr:   true,
			errSubstr: "configure provider.credential instead",
		},
		{
			name: "uppercase project ID fails",
			mutate: func(c *RunConfig) {
				c.Provider.GCPProject = "MyProject"
			},
			wantErr:   true,
			errSubstr: "gcpProject",
		},
		{
			name: "project ID with underscore fails",
			mutate: func(c *RunConfig) {
				c.Provider.GCPProject = "my_project"
			},
			wantErr:   true,
			errSubstr: "gcpProject",
		},
		{
			name: "project ID too short fails",
			mutate: func(c *RunConfig) {
				c.Provider.GCPProject = "abcde"
			},
			wantErr:   true,
			errSubstr: "gcpProject",
		},
		{
			name: "credentials file with traversal rejected",
			mutate: func(c *RunConfig) {
				c.Provider.GCPCredentialsFile = "../../etc/passwd"
				c.Provider.Credential = &CredentialConfig{Type: "gcp-service-account"}
			},
			wantErr:   true,
			errSubstr: "must not contain \"..\"",
		},
		{
			name: "gcp-service-account without credentials file fails",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{Type: "gcp-service-account"}
			},
			wantErr:   true,
			errSubstr: "gcpCredentialsFile is required",
		},
		{
			name: "gcp-default with credentials file fails",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{Type: "gcp-default"}
				c.Provider.GCPCredentialsFile = "/etc/sa.json"
			},
			wantErr:   true,
			errSubstr: "only valid when credential.type is",
		},
		{
			name: "gcp-service-account with credentials file passes",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{Type: "gcp-service-account"}
				c.Provider.GCPCredentialsFile = "/etc/sa.json"
			},
			wantErr: false,
		},
		{
			name: "gcp-default credential type accepted",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{Type: "gcp-default"}
			},
			wantErr: false,
		},
		{
			name: "gcp-workload-identity credential type accepted",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{Type: "gcp-workload-identity"}
			},
			wantErr: false,
		},
		{
			name: "gcp-workload-identity-federation with valid audience and tokenSource passes",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{
					Type:     "gcp-workload-identity-federation",
					Audience: "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/aws-pool/providers/aws-provider",
					TokenSource: &TokenSourceConfig{
						Type: "aws-irsa",
					},
				}
			},
			wantErr: false,
		},
		{
			name: "gcp-workload-identity-federation with serviceAccount impersonation passes",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{
					Type:           "gcp-workload-identity-federation",
					Audience:       "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/aws-pool/providers/aws-provider",
					ServiceAccount: "vertex@my-project.iam.gserviceaccount.com",
					TokenSource: &TokenSourceConfig{
						Type: "aws-irsa",
					},
				}
			},
			wantErr: false,
		},
		{
			name: "gcp-workload-identity-federation missing audience fails",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{
					Type: "gcp-workload-identity-federation",
					TokenSource: &TokenSourceConfig{
						Type: "aws-irsa",
					},
				}
			},
			wantErr:   true,
			errSubstr: "gcp-workload-identity-federation requires audience",
		},
		{
			name: "gcp-workload-identity-federation missing tokenSource fails",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{
					Type:     "gcp-workload-identity-federation",
					Audience: "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/aws-pool/providers/aws-provider",
				}
			},
			wantErr:   true,
			errSubstr: "gcp-workload-identity-federation requires tokenSource",
		},
		{
			name: "gcp-workload-identity-federation rejects plain-string audience",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{
					Type:     "gcp-workload-identity-federation",
					Audience: "not-an-audience",
					TokenSource: &TokenSourceConfig{
						Type: "aws-irsa",
					},
				}
			},
			wantErr:   true,
			errSubstr: "must match //iam.googleapis.com/projects/{N}/",
		},
		{
			name: "gcp-workload-identity-federation rejects wrong-host audience",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{
					Type:     "gcp-workload-identity-federation",
					Audience: "//example.com/projects/1/locations/global/workloadIdentityPools/p/providers/q",
					TokenSource: &TokenSourceConfig{
						Type: "aws-irsa",
					},
				}
			},
			wantErr:   true,
			errSubstr: "must match",
		},
		{
			name: "gcp-workload-identity-federation rejects non-numeric project",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{
					Type:     "gcp-workload-identity-federation",
					Audience: "//iam.googleapis.com/projects/abc/locations/global/workloadIdentityPools/aws-pool/providers/aws-provider",
					TokenSource: &TokenSourceConfig{
						Type: "aws-irsa",
					},
				}
			},
			wantErr:   true,
			errSubstr: "must match",
		},
		{
			name: "gcp-workload-identity-federation rejects malformed serviceAccount email",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{
					Type:           "gcp-workload-identity-federation",
					Audience:       "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/aws-pool/providers/aws-provider",
					ServiceAccount: "not-an-email",
					TokenSource: &TokenSourceConfig{
						Type: "aws-irsa",
					},
				}
			},
			wantErr:   true,
			errSubstr: "not a valid service account email",
		},
		{
			name: "gcp-workload-identity-federation rejects wrong-domain serviceAccount",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{
					Type:           "gcp-workload-identity-federation",
					Audience:       "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/aws-pool/providers/aws-provider",
					ServiceAccount: "vertex@my-project.gmail.com",
					TokenSource: &TokenSourceConfig{
						Type: "aws-irsa",
					},
				}
			},
			wantErr:   true,
			errSubstr: "not a valid service account email",
		},
		{
			name: "gcp-workload-identity-federation rejects too-short serviceAccount local part",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{
					Type:           "gcp-workload-identity-federation",
					Audience:       "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/aws-pool/providers/aws-provider",
					ServiceAccount: "vx@my-project.iam.gserviceaccount.com",
					TokenSource: &TokenSourceConfig{
						Type: "aws-irsa",
					},
				}
			},
			wantErr:   true,
			errSubstr: "not a valid service account email",
		},
		{
			name: "gcp-workload-identity-federation rejects uppercase serviceAccount",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{
					Type:           "gcp-workload-identity-federation",
					Audience:       "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/aws-pool/providers/aws-provider",
					ServiceAccount: "Vertex@my-project.iam.gserviceaccount.com",
					TokenSource: &TokenSourceConfig{
						Type: "aws-irsa",
					},
				}
			},
			wantErr:   true,
			errSubstr: "not a valid service account email",
		},
		{
			name: "gcp-workload-identity-federation accepts empty serviceAccount (federated identity used directly)",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{
					Type:     "gcp-workload-identity-federation",
					Audience: "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/aws-pool/providers/aws-provider",
					TokenSource: &TokenSourceConfig{
						Type: "aws-irsa",
					},
				}
			},
			wantErr: false,
		},
		{
			name: "valid safety setting passes",
			mutate: func(c *RunConfig) {
				c.Provider.GeminiSafetySettings = []GeminiSafetySetting{
					{Category: "HARM_CATEGORY_DANGEROUS_CONTENT", Threshold: "BLOCK_ONLY_HIGH"},
				}
			},
			wantErr: false,
		},
		{
			name: "all five categories pass",
			mutate: func(c *RunConfig) {
				c.Provider.GeminiSafetySettings = []GeminiSafetySetting{
					{Category: "HARM_CATEGORY_HATE_SPEECH", Threshold: "BLOCK_NONE"},
					{Category: "HARM_CATEGORY_HARASSMENT", Threshold: "BLOCK_LOW_AND_ABOVE"},
					{Category: "HARM_CATEGORY_DANGEROUS_CONTENT", Threshold: "BLOCK_MEDIUM_AND_ABOVE"},
					{Category: "HARM_CATEGORY_SEXUALLY_EXPLICIT", Threshold: "BLOCK_ONLY_HIGH"},
					{Category: "HARM_CATEGORY_CIVIC_INTEGRITY", Threshold: "BLOCK_NONE"},
				}
			},
			wantErr: false,
		},
		{
			name: "bogus safety category rejected",
			mutate: func(c *RunConfig) {
				c.Provider.GeminiSafetySettings = []GeminiSafetySetting{
					{Category: "HARM_CATEGORY_BOGUS", Threshold: "BLOCK_NONE"},
				}
			},
			wantErr:   true,
			errSubstr: "is not a valid HARM_CATEGORY_*",
		},
		{
			name: "bogus safety threshold rejected",
			mutate: func(c *RunConfig) {
				c.Provider.GeminiSafetySettings = []GeminiSafetySetting{
					{Category: "HARM_CATEGORY_DANGEROUS_CONTENT", Threshold: "BLOCK_ALMOST_NONE"},
				}
			},
			wantErr:   true,
			errSubstr: "is not a valid BLOCK_*",
		},
		{
			name: "empty category rejected",
			mutate: func(c *RunConfig) {
				c.Provider.GeminiSafetySettings = []GeminiSafetySetting{
					{Threshold: "BLOCK_NONE"},
				}
			},
			wantErr:   true,
			errSubstr: "category is required",
		},
		{
			name: "empty threshold rejected",
			mutate: func(c *RunConfig) {
				c.Provider.GeminiSafetySettings = []GeminiSafetySetting{
					{Category: "HARM_CATEGORY_DANGEROUS_CONTENT"},
				}
			},
			wantErr:   true,
			errSubstr: "threshold is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := geminiValidConfig()
			tc.mutate(c)
			err := ValidateRunConfig(c)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.errSubstr)
				}
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("expected error to contain %q, got: %v", tc.errSubstr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
		})
	}
}

// TestValidateRunConfig_AzureWorkloadIdentity covers the Azure WIF
// credential type's field-level rules (UUID format on tenant/client,
// required token source, optional but HTTPS-only scope) and the two
// cross-field invariants (apiKeyRef and apiKeyHeader="api-key" are
// mutually exclusive with the WIF type because the bearer is fetched
// via OAuth2 token-exchange and must travel on Authorization: Bearer).
//
// The cases mirror the structural shape of the GCP WIF table-driven
// tests above so a reviewer reading the two side by side can see the
// federation paths' invariants line up.
func TestValidateRunConfig_AzureWorkloadIdentity(t *testing.T) {
	const (
		validTenant = "11111111-2222-3333-4444-555555555555"
		validClient = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	)

	cases := []struct {
		name      string
		mutate    func(c *RunConfig)
		wantErr   bool
		errSubstr string
	}{
		{
			name: "azure-imds token source with valid UUIDs and default scope passes",
			mutate: func(c *RunConfig) {
				c.Provider = ProviderConfig{
					Type:    "openai-compatible",
					BaseURL: "https://example.openai.azure.com/openai/v1",
					Credential: &CredentialConfig{
						Type:          "azure-workload-identity",
						AzureTenantID: validTenant,
						AzureClientID: validClient,
						TokenSource: &TokenSourceConfig{
							Type:     "azure-imds",
							Resource: "api://AzureADTokenExchange",
						},
					},
				}
			},
			wantErr: false,
		},
		{
			name: "file token source with valid UUIDs passes",
			mutate: func(c *RunConfig) {
				c.Provider = ProviderConfig{
					Type:    "openai-compatible",
					BaseURL: "https://example.openai.azure.com/openai/v1",
					Credential: &CredentialConfig{
						Type:          "azure-workload-identity",
						AzureTenantID: validTenant,
						AzureClientID: validClient,
						TokenSource: &TokenSourceConfig{
							Type: "file",
							Path: "/var/run/secrets/azure/tokens/azure-identity-token",
						},
					},
				}
			},
			wantErr: false,
		},
		{
			name: "explicit https scope passes",
			mutate: func(c *RunConfig) {
				c.Provider = ProviderConfig{
					Type:    "openai-compatible",
					BaseURL: "https://example.openai.azure.com/openai/v1",
					Credential: &CredentialConfig{
						Type:          "azure-workload-identity",
						AzureTenantID: validTenant,
						AzureClientID: validClient,
						AzureScope:    "https://cognitiveservices.azure.com/.default",
						TokenSource: &TokenSourceConfig{
							Type: "file",
							Path: "/var/run/secrets/azure/tokens/azure-identity-token",
						},
					},
				}
			},
			wantErr: false,
		},
		{
			name: "missing azureTenantId fails",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{
					Type:          "azure-workload-identity",
					AzureClientID: validClient,
					TokenSource: &TokenSourceConfig{
						Type: "file",
						Path: "/var/run/secrets/azure/tokens/azure-identity-token",
					},
				}
			},
			wantErr:   true,
			errSubstr: "azure-workload-identity requires azureTenantId",
		},
		{
			name: "missing azureClientId fails",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{
					Type:          "azure-workload-identity",
					AzureTenantID: validTenant,
					TokenSource: &TokenSourceConfig{
						Type: "file",
						Path: "/var/run/secrets/azure/tokens/azure-identity-token",
					},
				}
			},
			wantErr:   true,
			errSubstr: "azure-workload-identity requires azureClientId",
		},
		{
			name: "missing tokenSource fails",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{
					Type:          "azure-workload-identity",
					AzureTenantID: validTenant,
					AzureClientID: validClient,
				}
			},
			wantErr:   true,
			errSubstr: "azure-workload-identity requires tokenSource",
		},
		{
			name: "malformed azureTenantId rejected",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{
					Type:          "azure-workload-identity",
					AzureTenantID: "not-a-uuid",
					AzureClientID: validClient,
					TokenSource: &TokenSourceConfig{
						Type: "file",
						Path: "/var/run/secrets/azure/tokens/azure-identity-token",
					},
				}
			},
			wantErr:   true,
			errSubstr: "azureTenantId",
		},
		{
			name: "uppercase azureTenantId rejected (canonical lowercase only)",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{
					Type:          "azure-workload-identity",
					AzureTenantID: "11111111-2222-3333-4444-555555555555",
					AzureClientID: "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE",
					TokenSource: &TokenSourceConfig{
						Type: "file",
						Path: "/var/run/secrets/azure/tokens/azure-identity-token",
					},
				}
			},
			wantErr:   true,
			errSubstr: "azureClientId",
		},
		{
			name: "malformed azureClientId rejected",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{
					Type:          "azure-workload-identity",
					AzureTenantID: validTenant,
					AzureClientID: "1234",
					TokenSource: &TokenSourceConfig{
						Type: "file",
						Path: "/var/run/secrets/azure/tokens/azure-identity-token",
					},
				}
			},
			wantErr:   true,
			errSubstr: "azureClientId",
		},
		{
			name: "azureScope plain string rejected",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{
					Type:          "azure-workload-identity",
					AzureTenantID: validTenant,
					AzureClientID: validClient,
					AzureScope:    "not-a-url",
					TokenSource: &TokenSourceConfig{
						Type: "file",
						Path: "/var/run/secrets/azure/tokens/azure-identity-token",
					},
				}
			},
			wantErr:   true,
			errSubstr: "azureScope",
		},
		{
			name: "azureScope http rejected (HTTPS-only)",
			mutate: func(c *RunConfig) {
				c.Provider.Credential = &CredentialConfig{
					Type:          "azure-workload-identity",
					AzureTenantID: validTenant,
					AzureClientID: validClient,
					AzureScope:    "http://cognitiveservices.azure.com/.default",
					TokenSource: &TokenSourceConfig{
						Type: "file",
						Path: "/var/run/secrets/azure/tokens/azure-identity-token",
					},
				}
			},
			wantErr:   true,
			errSubstr: "azureScope",
		},
		{
			name: "apiKeyRef alongside azure-workload-identity is mutually exclusive",
			mutate: func(c *RunConfig) {
				c.Provider = ProviderConfig{
					Type:      "openai-compatible",
					BaseURL:   "https://example.openai.azure.com/openai/v1",
					APIKeyRef: "secret://AZURE_OPENAI_KEY",
					Credential: &CredentialConfig{
						Type:          "azure-workload-identity",
						AzureTenantID: validTenant,
						AzureClientID: validClient,
						TokenSource: &TokenSourceConfig{
							Type: "file",
							Path: "/var/run/secrets/azure/tokens/azure-identity-token",
						},
					},
				}
			},
			wantErr:   true,
			errSubstr: "azure-workload-identity does not use apiKeyRef",
		},
		{
			name: "apiKeyHeader=api-key alongside azure-workload-identity is mutually exclusive",
			mutate: func(c *RunConfig) {
				c.Provider = ProviderConfig{
					Type:         "openai-compatible",
					BaseURL:      "https://example.openai.azure.com/openai/v1",
					APIKeyHeader: "api-key",
					Credential: &CredentialConfig{
						Type:          "azure-workload-identity",
						AzureTenantID: validTenant,
						AzureClientID: validClient,
						TokenSource: &TokenSourceConfig{
							Type: "file",
							Path: "/var/run/secrets/azure/tokens/azure-identity-token",
						},
					},
				}
			},
			wantErr:   true,
			errSubstr: "azure-workload-identity requires Authorization: Bearer",
		},
		{
			name: "explicit https azureTokenUrl (sovereign cloud) passes",
			mutate: func(c *RunConfig) {
				c.Provider = ProviderConfig{
					Type:    "openai-compatible",
					BaseURL: "https://example.openai.azure.us/openai/v1",
					Credential: &CredentialConfig{
						Type:          "azure-workload-identity",
						AzureTenantID: validTenant,
						AzureClientID: validClient,
						AzureTokenURL: "https://login.microsoftonline.us/" + validTenant + "/oauth2/v2.0/token",
						TokenSource: &TokenSourceConfig{
							Type: "file",
							Path: "/var/run/secrets/azure/tokens/azure-identity-token",
						},
					},
				}
			},
			wantErr: false,
		},
		{
			name: "azureTokenUrl http rejected (HTTPS-only)",
			mutate: func(c *RunConfig) {
				c.Provider = ProviderConfig{
					Type:    "openai-compatible",
					BaseURL: "https://example.openai.azure.com/openai/v1",
					Credential: &CredentialConfig{
						Type:          "azure-workload-identity",
						AzureTenantID: validTenant,
						AzureClientID: validClient,
						AzureTokenURL: "http://login.microsoftonline.com/" + validTenant + "/oauth2/v2.0/token",
						TokenSource: &TokenSourceConfig{
							Type: "file",
							Path: "/var/run/secrets/azure/tokens/azure-identity-token",
						},
					},
				}
			},
			wantErr:   true,
			errSubstr: "azureTokenUrl",
		},
		{
			name: "azureTokenUrl plain string rejected",
			mutate: func(c *RunConfig) {
				c.Provider = ProviderConfig{
					Type:    "openai-compatible",
					BaseURL: "https://example.openai.azure.com/openai/v1",
					Credential: &CredentialConfig{
						Type:          "azure-workload-identity",
						AzureTenantID: validTenant,
						AzureClientID: validClient,
						AzureTokenURL: "login.microsoftonline.us",
						TokenSource: &TokenSourceConfig{
							Type: "file",
							Path: "/var/run/secrets/azure/tokens/azure-identity-token",
						},
					},
				}
			},
			wantErr:   true,
			errSubstr: "azureTokenUrl",
		},
		{
			name: "azure-workload-identity rejected with anthropic provider type",
			mutate: func(c *RunConfig) {
				c.Provider = ProviderConfig{
					Type: "anthropic",
					Credential: &CredentialConfig{
						Type:          "azure-workload-identity",
						AzureTenantID: validTenant,
						AzureClientID: validClient,
						TokenSource: &TokenSourceConfig{
							Type: "file",
							Path: "/var/run/secrets/azure/tokens/azure-identity-token",
						},
					},
				}
				c.ModelRouter = ModelRouterConfig{
					Type:     "static",
					Provider: "anthropic",
					Model:    "claude-sonnet-4-6",
				}
			},
			wantErr:   true,
			errSubstr: "azure-workload-identity is only supported with openai-compatible or openai-responses",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(c)
			err := ValidateRunConfig(c)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.errSubstr)
				}
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("expected error to contain %q, got: %v", tc.errSubstr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
		})
	}
}

// TestValidateRunConfig_AzureWIFIncompatibleProvider is a focused
// regression check on the provider-type guard: a credential block of
// type azure-workload-identity must not pair with non-OpenAI provider
// types. The error message must name both the failing credential type
// and the accepted provider types so an operator can grep for the
// fix path. Defence-in-depth alongside the in-table coverage in
// TestValidateRunConfig_AzureWorkloadIdentity.
func TestValidateRunConfig_AzureWIFIncompatibleProvider(t *testing.T) {
	c := validConfig()
	c.Provider = ProviderConfig{
		Type: "anthropic",
		Credential: &CredentialConfig{
			Type:          "azure-workload-identity",
			AzureTenantID: "11111111-2222-3333-4444-555555555555",
			AzureClientID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			TokenSource: &TokenSourceConfig{
				Type: "file",
				Path: "/var/run/secrets/azure/tokens/azure-identity-token",
			},
		},
	}
	c.ModelRouter = ModelRouterConfig{
		Type:     "static",
		Provider: "anthropic",
		Model:    "claude-sonnet-4-6",
	}

	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "azure-workload-identity") {
		t.Errorf("error should name azure-workload-identity, got: %v", err)
	}
	if !strings.Contains(msg, "openai-compatible") {
		t.Errorf("error should name openai-compatible as accepted type, got: %v", err)
	}
}

// TestValidateRunConfig_GeminiModelNameWithSlash verifies B5: a Vertex
// model name containing a slash (or other URL-reserved bytes) is
// rejected at validation time. The adapter url.PathEscape's the name
// at the request-construction layer, but a model identifier with
// slashes is almost always a copy-paste error or malicious input —
// surface it loudly rather than letting a percent-encoded path land at
// a real (but unintended) Vertex endpoint.
func TestValidateRunConfig_GeminiModelNameWithSlash(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(c *RunConfig)
		errSubstr string
	}{
		{
			name: "modelRouter.model with traversal",
			mutate: func(c *RunConfig) {
				c.ModelRouter = ModelRouterConfig{
					Type:     "static",
					Provider: "gemini",
					Model:    "gemini-pro/../../evil",
				}
			},
			errSubstr: "modelRouter.model",
		},
		{
			name: "modelRouter.model with bare slash",
			mutate: func(c *RunConfig) {
				c.ModelRouter = ModelRouterConfig{
					Type:     "static",
					Provider: "gemini",
					Model:    "publishers/google/models/gemini-2.5-pro",
				}
			},
			errSubstr: "modelRouter.model",
		},
		{
			name: "modelRouter.model with percent",
			mutate: func(c *RunConfig) {
				c.ModelRouter = ModelRouterConfig{
					Type:     "static",
					Provider: "gemini",
					Model:    "gemini%2F../alt",
				}
			},
			errSubstr: "modelRouter.model",
		},
		{
			name: "default provider gemini, empty router provider",
			mutate: func(c *RunConfig) {
				// ModelRouter.Provider unset — falls back to top-level
				// Provider.Type which is gemini. Validation must still
				// fire for the model-name shape.
				c.ModelRouter = ModelRouterConfig{
					Type:  "static",
					Model: "gemini-pro/../../evil",
				}
			},
			errSubstr: "modelRouter.model",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := geminiValidConfig()
			tc.mutate(c)
			err := ValidateRunConfig(c)
			if err == nil {
				t.Fatalf("expected validation error containing %q, got nil", tc.errSubstr)
			}
			if !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("expected error to contain %q, got: %v", tc.errSubstr, err)
			}
		})
	}
}

// TestValidateRunConfig_GeminiModelNameValid pins that an ordinary
// Vertex model identifier passes through cleanly. Catches a regression
// where the new check accidentally rejects all gemini configs.
func TestValidateRunConfig_GeminiModelNameValid(t *testing.T) {
	for _, model := range []string{"gemini-2.5-pro", "gemini-2.0-flash", "gemini-1.5-pro_001"} {
		t.Run(model, func(t *testing.T) {
			c := geminiValidConfig()
			c.ModelRouter = ModelRouterConfig{
				Type:     "static",
				Provider: "gemini",
				Model:    model,
			}
			if err := ValidateRunConfig(c); err != nil {
				t.Fatalf("expected no error for valid model %q, got: %v", model, err)
			}
		})
	}
}

// TestValidateRunConfig_GeminiFieldsLeakRejected verifies that the four
// gemini-only ProviderConfig fields cannot ride along on a non-gemini
// provider. A stale value from an earlier provider-type choice would
// otherwise sit unused in the config and fool a future operator into
// thinking it was active. The check fires for both the default
// provider and named entries in the Providers map.
func TestValidateRunConfig_GeminiFieldsLeakRejected(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(p *ProviderConfig)
		errSubstr string
	}{
		{
			name:      "gcpProject on anthropic",
			mutate:    func(p *ProviderConfig) { p.GCPProject = "my-project" },
			errSubstr: "gcpProject is only valid",
		},
		{
			name:      "gcpLocation on anthropic",
			mutate:    func(p *ProviderConfig) { p.GCPLocation = "us-central1" },
			errSubstr: "gcpLocation is only valid",
		},
		{
			name:      "gcpCredentialsFile on anthropic",
			mutate:    func(p *ProviderConfig) { p.GCPCredentialsFile = "/etc/sa.json" },
			errSubstr: "gcpCredentialsFile is only valid",
		},
		{
			name: "geminiSafetySettings on anthropic",
			mutate: func(p *ProviderConfig) {
				p.GeminiSafetySettings = []GeminiSafetySetting{
					{Category: "HARM_CATEGORY_HATE_SPEECH", Threshold: "BLOCK_NONE"},
				}
			},
			errSubstr: "geminiSafetySettings is only valid",
		},
	}
	for _, tc := range cases {
		t.Run("default/"+tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(&c.Provider)
			err := ValidateRunConfig(c)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.errSubstr)
			}
			if !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("expected error to contain %q, got: %v", tc.errSubstr, err)
			}
		})
		t.Run("map/"+tc.name, func(t *testing.T) {
			c := validConfig()
			named := ProviderConfig{Type: "anthropic"}
			tc.mutate(&named)
			c.Providers = map[string]ProviderConfig{"alt": named}
			err := ValidateRunConfig(c)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.errSubstr)
			}
			if !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("expected error to contain %q, got: %v", tc.errSubstr, err)
			}
		})
	}
}

// TestValidateRunConfig_GeminiInProvidersMap pins that the gemini
// validation runs on the Providers map entries too, not just on the
// default Provider. Without this coverage a future refactor that
// touched validateProviderConfigs could silently skip the gemini
// gate for named providers.
func TestValidateRunConfig_GeminiInProvidersMap(t *testing.T) {
	c := validConfig()
	c.Providers = map[string]ProviderConfig{
		"vertex": {
			Type:        "gemini",
			GCPLocation: "us-central1",
			// missing GCPProject
		},
	}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for gemini provider in map missing gcpProject")
	}
	if !strings.Contains(err.Error(), "providers[vertex].gcpProject") {
		t.Errorf("expected error path providers[vertex].gcpProject, got: %v", err)
	}
}

// TestValidateRunConfig_ObservabilityValid pins that the typical
// non-empty Observability values used in production (deployment tier,
// org-scoped namespace) pass validation. Empty values are also valid —
// they fall through to env-var fallbacks at resource construction.
func TestValidateRunConfig_ObservabilityValid(t *testing.T) {
	cases := []ObservabilityConfig{
		{},
		{Environment: "production"},
		{ServiceNamespace: "stirrup-eval"},
		{Environment: "staging-eu", ServiceNamespace: "stirrup_team-a"},
		{Environment: strings.Repeat("a", 64), ServiceNamespace: strings.Repeat("b", 64)},
	}
	for _, obs := range cases {
		t.Run(fmt.Sprintf("env=%q ns=%q", obs.Environment, obs.ServiceNamespace), func(t *testing.T) {
			c := validConfig()
			c.Observability = obs
			if err := ValidateRunConfig(c); err != nil {
				t.Fatalf("expected nil error, got: %v", err)
			}
		})
	}
}

// TestValidateRunConfig_ObservabilityRejectsBadShape protects the OTel
// resource-attribute encoding from operator-supplied values that would
// not survive round-tripping through the wire format. CRLF and path
// separators are the immediately dangerous shapes; the regex also
// rejects spaces and the empty-after-trimming case (everything past
// the 64-char cap).
func TestValidateRunConfig_ObservabilityRejectsBadShape(t *testing.T) {
	cases := map[string]ObservabilityConfig{
		"newline in env":      {Environment: "prod\nuction"},
		"slash in env":        {Environment: "prod/uction"},
		"space in env":        {Environment: "prod uction"},
		"too long env":        {Environment: strings.Repeat("a", 65)},
		"newline in ns":       {ServiceNamespace: "stirrup\neval"},
		"colon in ns":         {ServiceNamespace: "stirrup:eval"},
		"too long ns":         {ServiceNamespace: strings.Repeat("a", 65)},
		"both fields invalid": {Environment: "x y", ServiceNamespace: "x y"},
	}
	for name, obs := range cases {
		t.Run(name, func(t *testing.T) {
			c := validConfig()
			c.Observability = obs
			err := ValidateRunConfig(c)
			if err == nil {
				t.Fatalf("expected error for %s", name)
			}
			if !strings.Contains(err.Error(), "observability.") {
				t.Errorf("expected error to mention observability.*, got: %v", err)
			}
		})
	}
}

// TestValidateRunConfig_InvalidTraceEmitterProtocol pins the closed-set
// rejection on TraceEmitterConfig.Protocol. A typo'd "http" or "grpcs"
// must surface at config-load time with a precise error rather than
// silently falling through to the default at exporter init. Without
// this coverage the closed-set check is invisible to the test suite —
// a regression that whitelisted "http" would pass every existing test.
func TestValidateRunConfig_InvalidTraceEmitterProtocol(t *testing.T) {
	c := validConfig()
	c.TraceEmitter = TraceEmitterConfig{Type: "otel", Protocol: "http"}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for unsupported protocol value")
	}
	if !strings.Contains(err.Error(), "unsupported traceEmitter.protocol") {
		t.Errorf("expected error to call out the bad protocol, got: %v", err)
	}
}

// TestValidateRunConfig_ProtocolOnNonOTelEmitter pins the cross-field
// invariant: Protocol only has meaning when Type=="otel". Carrying it
// on a jsonl emitter is almost certainly a stale-config artifact and
// must fail loudly. Without this test, dropping the type guard from
// validateTraceEmitterProtocolAndHeaders would go unnoticed.
func TestValidateRunConfig_ProtocolOnNonOTelEmitter(t *testing.T) {
	c := validConfig()
	c.TraceEmitter = TraceEmitterConfig{Type: "jsonl", Protocol: "grpc"}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for protocol on non-otel emitter")
	}
	if !strings.Contains(err.Error(), "only valid when traceEmitter.type is \"otel\"") {
		t.Errorf("expected error to call out the type mismatch, got: %v", err)
	}
}

// TestValidateRunConfig_HeadersOnNonOTelEmitter is the headers-side
// counterpart to TestValidateRunConfig_ProtocolOnNonOTelEmitter. The
// jsonl emitter does not send HTTP headers; carrying them must fail
// loudly so an operator does not assume their auth header is being
// honoured when it's silently dropped.
func TestValidateRunConfig_HeadersOnNonOTelEmitter(t *testing.T) {
	c := validConfig()
	c.TraceEmitter = TraceEmitterConfig{
		Type:    "jsonl",
		Headers: map[string]string{"X-Tenant": "team-a"},
	}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for headers on non-otel emitter")
	}
	if !strings.Contains(err.Error(), "traceEmitter.headers is only valid") {
		t.Errorf("expected error to call out the type mismatch, got: %v", err)
	}
}

// TestValidateRunConfig_HeadersOnGRPCProtocolRejected pins the
// MF-2 invariant: gRPC OTLP exporter paths in
// harness/internal/trace/otel.go and observability/metrics.go
// unconditionally call WithInsecure(), so any bearer/Basic credential
// supplied via Headers would be transmitted in plaintext. The
// validator must reject the combination at config-load time —
// catching it here means an operator never even attempts to ship
// credentials over an insecure gRPC channel.
//
// Empty Protocol defaults to gRPC at exporter construction, so the
// rejection covers both `""` and `"grpc"`. The accept cases (gRPC
// with empty headers, http/protobuf with non-empty headers) are
// covered as subtests so a future regression that overzealously
// rejects them would be caught.
func TestValidateRunConfig_HeadersOnGRPCProtocolRejected(t *testing.T) {
	t.Run("empty protocol with headers rejected", func(t *testing.T) {
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{
			Type:     "otel",
			Protocol: "",
			Headers:  map[string]string{"Authorization": "Basic abc"},
		}
		err := ValidateRunConfig(c)
		if err == nil {
			t.Fatal("expected error for headers on default-gRPC protocol")
		}
		if !strings.Contains(err.Error(), "headers requires protocol=http/protobuf") {
			t.Errorf("expected error to call out the gRPC plaintext footgun, got: %v", err)
		}
	})

	t.Run("explicit grpc protocol with headers rejected", func(t *testing.T) {
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{
			Type:     "otel",
			Protocol: "grpc",
			Headers:  map[string]string{"Authorization": "Bearer abc"},
		}
		err := ValidateRunConfig(c)
		if err == nil {
			t.Fatal("expected error for headers on gRPC protocol")
		}
		if !strings.Contains(err.Error(), "WithInsecure") {
			t.Errorf("expected error to mention WithInsecure plaintext path, got: %v", err)
		}
	})

	t.Run("grpc with empty headers accepted", func(t *testing.T) {
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{Type: "otel", Protocol: "grpc"}
		if err := ValidateRunConfig(c); err != nil {
			t.Fatalf("gRPC with no headers must remain valid (the local-collector flow): %v", err)
		}
	})

	t.Run("http/protobuf with headers accepted", func(t *testing.T) {
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{
			Type:     "otel",
			Protocol: "http/protobuf",
			Headers:  map[string]string{"Authorization": "Basic xxx"},
		}
		if err := ValidateRunConfig(c); err != nil {
			t.Fatalf("http/protobuf with headers must remain valid (the Grafana Cloud flow): %v", err)
		}
	})
}

// TestValidateRunConfig_TraceEmitterHeaders_CRLFRejected pins the MF-6
// hardening: a header name containing CRLF, or a value containing CRLF,
// must be rejected at config-load. Go 1.26's net/http panics on CRLF in
// header values; surfacing the misuse at validation time turns a
// process-crash into an "invalid config" error message. Mirrors the
// CRLF rejection on apiKeyHeader / queryParams in
// validateOpenAIAuthFields (runconfig.go:1862-1887).
func TestValidateRunConfig_TraceEmitterHeaders_CRLFRejected(t *testing.T) {
	t.Run("CR in header name rejected", func(t *testing.T) {
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{
			Type:     "otel",
			Protocol: "http/protobuf",
			Headers:  map[string]string{"X-Inj\rected": "v"},
		}
		err := ValidateRunConfig(c)
		if err == nil {
			t.Fatal("expected error for CR in header name")
		}
		if !strings.Contains(err.Error(), "alphanumeric") {
			t.Errorf("expected error to describe accepted character set, got: %v", err)
		}
	})

	t.Run("LF in header name rejected", func(t *testing.T) {
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{
			Type:     "otel",
			Protocol: "http/protobuf",
			Headers:  map[string]string{"X-Inj\nected": "v"},
		}
		err := ValidateRunConfig(c)
		if err == nil {
			t.Fatal("expected error for LF in header name")
		}
	})

	t.Run("CRLF in header value rejected", func(t *testing.T) {
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{
			Type:     "otel",
			Protocol: "http/protobuf",
			Headers:  map[string]string{"Authorization": "Bearer foo\r\nX-Injected: evil"},
		}
		err := ValidateRunConfig(c)
		if err == nil {
			t.Fatal("expected error for CRLF in header value")
		}
		if !strings.Contains(err.Error(), "must not contain CRLF") {
			t.Errorf("expected error to mention CRLF in value, got: %v", err)
		}
	})

	t.Run("colon in header name rejected", func(t *testing.T) {
		// Colon is the header separator; allowing it in the name
		// would let an operator concatenate two headers into one
		// validator-passing entry.
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{
			Type:     "otel",
			Protocol: "http/protobuf",
			Headers:  map[string]string{"X-Foo: X-Bar": "v"},
		}
		err := ValidateRunConfig(c)
		if err == nil {
			t.Fatal("expected error for colon in header name")
		}
	})
}

// --- ProviderRetryConfig tests ---

func TestValidateRunConfig_ProviderRetryDefaultsWhenNil(t *testing.T) {
	c := validConfig()
	c.Provider.Retry = nil
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if c.Provider.Retry == nil {
		t.Fatal("expected Provider.Retry to be populated after validation")
	}
	got := c.Provider.Retry
	if got.MaxAttempts != defaultProviderRetryMaxAttempts {
		t.Errorf("MaxAttempts = %d, want %d", got.MaxAttempts, defaultProviderRetryMaxAttempts)
	}
	if got.InitialDelayMs != defaultProviderRetryInitialDelayMs {
		t.Errorf("InitialDelayMs = %d, want %d", got.InitialDelayMs, defaultProviderRetryInitialDelayMs)
	}
	if got.MaxDelayMs != defaultProviderRetryMaxDelayMs {
		t.Errorf("MaxDelayMs = %d, want %d", got.MaxDelayMs, defaultProviderRetryMaxDelayMs)
	}
	if got.WallClockBudgetMs != defaultProviderRetryWallClockBudgetMs {
		t.Errorf("WallClockBudgetMs = %d, want %d", got.WallClockBudgetMs, defaultProviderRetryWallClockBudgetMs)
	}
}

func TestValidateRunConfig_ProviderRetryPartialDefaulting(t *testing.T) {
	c := validConfig()
	c.Provider.Retry = &ProviderRetryConfig{MaxAttempts: 2}
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	got := c.Provider.Retry
	if got.MaxAttempts != 2 {
		t.Errorf("MaxAttempts = %d, want 2 (caller-supplied)", got.MaxAttempts)
	}
	if got.InitialDelayMs != defaultProviderRetryInitialDelayMs {
		t.Errorf("InitialDelayMs = %d, want %d (default)", got.InitialDelayMs, defaultProviderRetryInitialDelayMs)
	}
	if got.MaxDelayMs != defaultProviderRetryMaxDelayMs {
		t.Errorf("MaxDelayMs = %d, want %d (default)", got.MaxDelayMs, defaultProviderRetryMaxDelayMs)
	}
	if got.WallClockBudgetMs != defaultProviderRetryWallClockBudgetMs {
		t.Errorf("WallClockBudgetMs = %d, want %d (default)", got.WallClockBudgetMs, defaultProviderRetryWallClockBudgetMs)
	}
}

// TestValidateRunConfig_ProviderRetryZeroMaxAttemptsTreatedAsUnset pins
// that callers who leave MaxAttempts zero (the JSON-omitempty default)
// receive the documented default, not a validation error.
func TestValidateRunConfig_ProviderRetryZeroMaxAttemptsTreatedAsUnset(t *testing.T) {
	c := validConfig()
	c.Provider.Retry = &ProviderRetryConfig{MaxAttempts: 0}
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("expected zero MaxAttempts to be defaulted, got: %v", err)
	}
	if c.Provider.Retry.MaxAttempts != defaultProviderRetryMaxAttempts {
		t.Errorf("MaxAttempts = %d, want %d (default after zero)", c.Provider.Retry.MaxAttempts, defaultProviderRetryMaxAttempts)
	}
}

func TestValidateRunConfig_ProviderRetryMaxAttemptsTooHigh(t *testing.T) {
	c := validConfig()
	c.Provider.Retry = &ProviderRetryConfig{MaxAttempts: maxProviderRetryMaxAttempts + 1}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for MaxAttempts above ceiling")
	}
	if !strings.Contains(err.Error(), "provider.retry") {
		t.Errorf("expected error to mention provider.retry path, got: %v", err)
	}
	if !strings.Contains(err.Error(), "maxAttempts") {
		t.Errorf("expected error to mention maxAttempts, got: %v", err)
	}
}

func TestValidateRunConfig_ProviderRetryMaxDelayTooHigh(t *testing.T) {
	c := validConfig()
	c.Provider.Retry = &ProviderRetryConfig{MaxDelayMs: maxProviderRetryMaxDelayMs + 1}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for MaxDelayMs above ceiling")
	}
	if !strings.Contains(err.Error(), "maxDelayMs") {
		t.Errorf("expected error to mention maxDelayMs, got: %v", err)
	}
}

func TestValidateRunConfig_ProviderRetryWallClockBudgetTooHigh(t *testing.T) {
	c := validConfig()
	c.Provider.Retry = &ProviderRetryConfig{WallClockBudgetMs: maxProviderRetryWallClockBudgetMs + 1}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for WallClockBudgetMs above ceiling")
	}
	if !strings.Contains(err.Error(), "wallClockBudgetMs") {
		t.Errorf("expected error to mention wallClockBudgetMs, got: %v", err)
	}
}

func TestValidateRunConfig_ProviderRetryInitialDelayExceedsMaxDelay(t *testing.T) {
	c := validConfig()
	// Caller pins InitialDelay > MaxDelay (both inside individual
	// ceilings). The cross-field check is what should reject this.
	c.Provider.Retry = &ProviderRetryConfig{
		InitialDelayMs: 5000,
		MaxDelayMs:     1000,
	}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for InitialDelayMs > MaxDelayMs")
	}
	if !strings.Contains(err.Error(), "initialDelayMs") {
		t.Errorf("expected error to mention initialDelayMs, got: %v", err)
	}
}

// TestValidateRunConfig_ProviderRetryDefaultedInitialDelayAnnotated pins
// the UX behaviour for the asymmetric case where the caller supplies
// maxDelayMs but leaves initialDelayMs at the JSON-omitempty zero.
// Defaulting fills initialDelayMs with 500 before the cross-field
// invariant runs, and historically the resulting error read
// "initialDelayMs (500) must be <= maxDelayMs (100)" — naming a value
// the caller never wrote. The "(default)" annotation makes it clear
// where the offending value came from so the operator can either
// raise maxDelayMs or pin a smaller initialDelayMs explicitly.
func TestValidateRunConfig_ProviderRetryDefaultedInitialDelayAnnotated(t *testing.T) {
	c := validConfig()
	c.Provider.Retry = &ProviderRetryConfig{
		MaxDelayMs: 100,
	}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error: defaulted initialDelayMs (500) exceeds caller-supplied maxDelayMs (100)")
	}
	msg := err.Error()
	if !strings.Contains(msg, "initialDelayMs") {
		t.Errorf("expected error to mention initialDelayMs, got: %v", err)
	}
	if !strings.Contains(msg, "default") {
		t.Errorf("expected error to annotate the defaulted initialDelayMs value with 'default'; got: %v", err)
	}
	if !strings.Contains(msg, "500") {
		t.Errorf("expected error to show the defaulted value 500; got: %v", err)
	}
	// The caller-supplied maxDelayMs should NOT be annotated as a default.
	// Match the exact substring the error renderer produces for a
	// caller-supplied value so a regression that flips the flag (and
	// labels maxDelayMs as a default) is caught.
	if !strings.Contains(msg, "maxDelayMs (100)") {
		t.Errorf("expected error to show caller-supplied maxDelayMs without 'default' annotation; got: %v", err)
	}
}

func TestValidateRunConfig_ProviderRetryWallClockBudgetBelowMaxDelay(t *testing.T) {
	c := validConfig()
	// WallClockBudget below MaxDelay would not give a single attempt
	// room to consume its backoff; reject at validation time.
	c.Provider.Retry = &ProviderRetryConfig{
		MaxDelayMs:        20000,
		WallClockBudgetMs: 10000,
	}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for WallClockBudgetMs < MaxDelayMs")
	}
	if !strings.Contains(err.Error(), "wallClockBudgetMs") {
		t.Errorf("expected error to mention wallClockBudgetMs, got: %v", err)
	}
}

// TestValidateRunConfig_ProviderRetryNegativeMaxAttempts pins that a
// negative MaxAttempts (e.g. {"maxAttempts": -1} in JSON) is rejected.
// The defaulter only fills on `== 0`, so a negative value reaches the
// validator unchanged. Without this test, a future inversion of the
// `< 0` guard on InitialDelayMs to `<= 0`, or a parallel guard added
// to the range check below, could pass undetected — and once Wave 2
// casts these fields to time.Duration, a negative value would silently
// flip retry semantics.
func TestValidateRunConfig_ProviderRetryNegativeMaxAttempts(t *testing.T) {
	c := validConfig()
	c.Provider.Retry = &ProviderRetryConfig{MaxAttempts: -1}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for negative MaxAttempts")
	}
	if !strings.Contains(err.Error(), "maxAttempts") {
		t.Errorf("expected error to mention maxAttempts, got: %v", err)
	}
}

// TestValidateRunConfig_ProviderRetryNegativeInitialDelay pins the
// `cfg.InitialDelayMs < 0` branch. The defaulter only fills the field
// when it is exactly zero, so an explicit `-1` passes through to
// validation. See the docstring on the MaxAttempts test above for the
// Wave-2 regression class this prevents.
func TestValidateRunConfig_ProviderRetryNegativeInitialDelay(t *testing.T) {
	c := validConfig()
	c.Provider.Retry = &ProviderRetryConfig{InitialDelayMs: -1}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for negative InitialDelayMs")
	}
	if !strings.Contains(err.Error(), "initialDelayMs") {
		t.Errorf("expected error to mention initialDelayMs, got: %v", err)
	}
}

// TestValidateRunConfig_ProviderRetryNegativeMaxDelay pins the range
// check `cfg.MaxDelayMs <= 0` against negative input. See the docstring
// on the MaxAttempts test above for context.
func TestValidateRunConfig_ProviderRetryNegativeMaxDelay(t *testing.T) {
	c := validConfig()
	c.Provider.Retry = &ProviderRetryConfig{MaxDelayMs: -1}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for negative MaxDelayMs")
	}
	if !strings.Contains(err.Error(), "maxDelayMs") {
		t.Errorf("expected error to mention maxDelayMs, got: %v", err)
	}
}

// TestValidateRunConfig_ProviderRetryNegativeWallClockBudget pins the
// range check `cfg.WallClockBudgetMs <= 0` against negative input. See
// the docstring on the MaxAttempts test above for context.
func TestValidateRunConfig_ProviderRetryNegativeWallClockBudget(t *testing.T) {
	c := validConfig()
	c.Provider.Retry = &ProviderRetryConfig{WallClockBudgetMs: -1}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for negative WallClockBudgetMs")
	}
	if !strings.Contains(err.Error(), "wallClockBudgetMs") {
		t.Errorf("expected error to mention wallClockBudgetMs, got: %v", err)
	}
}

// TestValidateRunConfig_ProviderRetryWallClockBudgetEqualsMaxDelay pins
// the strict-less-than boundary of the cross-field invariant. Equality
// is intentionally valid — a single attempt is allowed to consume the
// entire wall-clock budget on its backoff — and tightening the check to
// `<=` would reject a valid operator config at runtime. Pin equality as
// a passing case so the regression is caught at unit-test time.
func TestValidateRunConfig_ProviderRetryWallClockBudgetEqualsMaxDelay(t *testing.T) {
	c := validConfig()
	c.Provider.Retry = &ProviderRetryConfig{
		MaxDelayMs:        16000,
		WallClockBudgetMs: 16000,
	}
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("expected WallClockBudgetMs == MaxDelayMs to be accepted, got: %v", err)
	}
}

// TestValidateProviderRetryConfig_NilIsNoop exercises the `cfg == nil`
// guard at the top of validateProviderRetryConfig directly. The public
// ValidateRunConfig path always runs applyProviderRetryDefaults first,
// which allocates a non-nil ProviderRetryConfig before validation, so
// the nil guard is structurally unreachable through the public API.
// Without this direct call, the branch shows statement count=0 in the
// coverage profile and a future refactor that bypasses the defaulter
// would lose the safety net silently. Brings validateProviderRetryConfig
// to 100% coverage.
func TestValidateProviderRetryConfig_NilIsNoop(t *testing.T) {
	var errs []string
	validateProviderRetryConfig("provider.retry", nil, providerRetryDefaulted{}, &errs)
	if len(errs) != 0 {
		t.Errorf("expected nil ProviderRetryConfig to be a no-op, got errors: %v", errs)
	}
}

// TestValidateRunConfig_ProviderRetryNamedProviderRejected pins the
// "providers[<name>].retry" path string used in error messages for the
// named-provider rejection branch. The happy path is covered by
// TestValidateRunConfig_ProviderRetryNamedProviderDefaultsIndependently;
// without this negative-path test, a refactor of the
// fmt.Sprintf("providers[%s]", name) format string (or a typo in the
// ".retry" suffix) would silently regress the operator-facing
// diagnostics.
func TestValidateRunConfig_ProviderRetryNamedProviderRejected(t *testing.T) {
	c := validConfig()
	c.Providers = map[string]ProviderConfig{
		"secondary": {
			Type:      "openai-compatible",
			BaseURL:   "https://example.test/v1",
			APIKeyRef: "secret://SECONDARY_KEY",
			Retry:     &ProviderRetryConfig{MaxAttempts: 99},
		},
	}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for named-provider MaxAttempts above ceiling")
	}
	if !strings.Contains(err.Error(), "providers[secondary].retry") {
		t.Errorf("expected error to mention providers[secondary].retry path, got: %v", err)
	}
	if !strings.Contains(err.Error(), "maxAttempts") {
		t.Errorf("expected error to mention maxAttempts, got: %v", err)
	}
}

// TestValidateRunConfig_ProviderRetryNamedProviderNilRetryBlock pins
// the nil-allocation branch of defaultProviderRetry for an entry in
// the Providers map. The existing
// TestValidateRunConfig_ProviderRetryNamedProviderDefaultsIndependently
// supplies a partial (non-nil) ProviderRetryConfig, exercising only
// the "fill missing fields" branches. A refactor that split the
// nil-allocation path by call site (top-level vs map entry) would not
// be caught without this test.
func TestValidateRunConfig_ProviderRetryNamedProviderNilRetryBlock(t *testing.T) {
	c := validConfig()
	c.Providers = map[string]ProviderConfig{
		"secondary": {
			Type:      "openai-compatible",
			BaseURL:   "https://example.test/v1",
			APIKeyRef: "secret://SECONDARY_KEY",
			Retry:     nil,
		},
	}
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	got := c.Providers["secondary"].Retry
	if got == nil {
		t.Fatal("expected named-provider Retry to be allocated from nil")
	}
	if got.MaxAttempts != defaultProviderRetryMaxAttempts {
		t.Errorf("named-provider MaxAttempts = %d, want %d (default)", got.MaxAttempts, defaultProviderRetryMaxAttempts)
	}
	if got.InitialDelayMs != defaultProviderRetryInitialDelayMs {
		t.Errorf("named-provider InitialDelayMs = %d, want %d (default)", got.InitialDelayMs, defaultProviderRetryInitialDelayMs)
	}
	if got.MaxDelayMs != defaultProviderRetryMaxDelayMs {
		t.Errorf("named-provider MaxDelayMs = %d, want %d (default)", got.MaxDelayMs, defaultProviderRetryMaxDelayMs)
	}
	if got.WallClockBudgetMs != defaultProviderRetryWallClockBudgetMs {
		t.Errorf("named-provider WallClockBudgetMs = %d, want %d (default)", got.WallClockBudgetMs, defaultProviderRetryWallClockBudgetMs)
	}
}

func TestValidateRunConfig_ProviderRetryNamedProviderDefaultsIndependently(t *testing.T) {
	c := validConfig()
	c.Providers = map[string]ProviderConfig{
		"secondary": {
			Type:      "openai-compatible",
			BaseURL:   "https://example.test/v1",
			APIKeyRef: "secret://SECONDARY_KEY",
			Retry:     &ProviderRetryConfig{MaxAttempts: 4},
		},
	}
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	got := c.Providers["secondary"].Retry
	if got == nil {
		t.Fatal("expected secondary provider Retry to be populated")
	}
	if got.MaxAttempts != 4 {
		t.Errorf("secondary MaxAttempts = %d, want 4 (caller-supplied)", got.MaxAttempts)
	}
	if got.InitialDelayMs != defaultProviderRetryInitialDelayMs ||
		got.MaxDelayMs != defaultProviderRetryMaxDelayMs ||
		got.WallClockBudgetMs != defaultProviderRetryWallClockBudgetMs {
		t.Errorf("secondary provider should inherit defaults for unset fields; got %+v", got)
	}
	// Top-level provider continues to default independently.
	if c.Provider.Retry == nil || c.Provider.Retry.MaxAttempts != defaultProviderRetryMaxAttempts {
		t.Errorf("top-level provider Retry not defaulted independently; got %+v", c.Provider.Retry)
	}
}

// --- traceEmitter type=gcs ---

func TestValidateRunConfig_TraceEmitterGCS_Valid(t *testing.T) {
	c := validConfig()
	c.TraceEmitter = TraceEmitterConfig{
		Type:         "gcs",
		Bucket:       "stirrup-results",
		ObjectPrefix: "traces/",
		Credential:   &CredentialConfig{Type: "gcp-workload-identity"},
	}
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("expected valid gcs trace emitter, got: %v", err)
	}
}

func TestValidateRunConfig_TraceEmitterGCS_BucketRequired(t *testing.T) {
	c := validConfig()
	c.TraceEmitter = TraceEmitterConfig{Type: "gcs"}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for gcs trace emitter without bucket")
	}
	if !strings.Contains(err.Error(), "requires bucket") {
		t.Errorf("expected error to mention bucket is required, got: %v", err)
	}
}

func TestValidateRunConfig_TraceEmitterGCS_InvalidBucketName(t *testing.T) {
	cases := []struct {
		name   string
		bucket string
	}{
		{"uppercase", "Stirrup-Results"},
		{"slash", "stirrup/results"},
		{"too short", "ab"},
		{"contains space", "stirrup results"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.TraceEmitter = TraceEmitterConfig{Type: "gcs", Bucket: tc.bucket}
			err := ValidateRunConfig(c)
			if err == nil {
				t.Fatalf("expected error for invalid bucket %q", tc.bucket)
			}
			if !strings.Contains(err.Error(), "traceEmitter.bucket") {
				t.Errorf("expected error to mention traceEmitter.bucket, got: %v", err)
			}
		})
	}
}

// TestValidateRunConfig_TraceEmitter_ObjectPrefixDotDotRejected pins the
// M3 fix: traceEmitter.objectPrefix must reject ".." segments so an
// operator-supplied prefix cannot rewrite the produced GCS object path.
func TestValidateRunConfig_TraceEmitter_ObjectPrefixDotDotRejected(t *testing.T) {
	cases := []string{
		"../escape/",
		"traces/../escape/",
		"../../prod-traces/",
		"..",
	}
	for _, prefix := range cases {
		t.Run(prefix, func(t *testing.T) {
			c := validConfig()
			c.TraceEmitter = TraceEmitterConfig{
				Type:         "gcs",
				Bucket:       "stirrup-results",
				ObjectPrefix: prefix,
			}
			err := ValidateRunConfig(c)
			if err == nil {
				t.Fatalf("expected error for objectPrefix %q", prefix)
			}
			if !strings.Contains(err.Error(), `must not contain ".." path segments`) {
				t.Errorf("expected dot-dot rejection error, got: %v", err)
			}
		})
	}
}

// TestValidateRunConfig_TraceEmitter_ObjectPrefixTrailingSlashNormalised
// pins S3 option A: a missing trailing slash on objectPrefix is
// normalised in place by the validator so gcsObjectName produces a
// well-formed object path. Rejecting would be more pedantic but the
// api-design reviewer recommended ergonomics here.
func TestValidateRunConfig_TraceEmitter_ObjectPrefixTrailingSlashNormalised(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"missing slash", "traces", "traces/"},
		{"already slash", "traces/", "traces/"},
		{"nested missing", "tenant-a/traces", "tenant-a/traces/"},
		{"empty stays empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.TraceEmitter = TraceEmitterConfig{
				Type:         "gcs",
				Bucket:       "stirrup-results",
				ObjectPrefix: tc.in,
			}
			if err := ValidateRunConfig(c); err != nil {
				t.Fatalf("expected valid config, got: %v", err)
			}
			if got := c.TraceEmitter.ObjectPrefix; got != tc.want {
				t.Errorf("ObjectPrefix = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestValidateRunConfig_RunID_PatternEnforced pins the M3 fix: RunID is
// interpolated verbatim into the gcs trace emitter object name, so any
// slash, control byte, or path-traversal segment must be rejected at
// config-load time rather than reaching the GCS REST API.
func TestValidateRunConfig_RunID_PatternEnforced(t *testing.T) {
	t.Run("slash rejected", func(t *testing.T) {
		c := validConfig()
		c.RunID = "tenant-a/run-1"
		err := ValidateRunConfig(c)
		if err == nil {
			t.Fatal("expected error for runId containing a slash")
		}
		if !strings.Contains(err.Error(), "runId") {
			t.Errorf("expected error to mention runId, got: %v", err)
		}
	})
	t.Run("dotdot rejected", func(t *testing.T) {
		c := validConfig()
		c.RunID = ".."
		if err := ValidateRunConfig(c); err == nil {
			t.Fatal("expected error for runId \"..\"")
		}
	})
	t.Run("empty allowed", func(t *testing.T) {
		c := validConfig()
		c.RunID = ""
		if err := ValidateRunConfig(c); err != nil {
			t.Fatalf("expected empty runId to pass, got: %v", err)
		}
	})
	t.Run("uuid accepted", func(t *testing.T) {
		c := validConfig()
		c.RunID = "0ff0-4d1b-9c4e-1234567890ab"
		if err := ValidateRunConfig(c); err != nil {
			t.Fatalf("expected uuid-like runId to pass, got: %v", err)
		}
	})
}

func TestValidateRunConfig_TraceEmitterGCS_FieldsRejectedOnNonGCS(t *testing.T) {
	t.Run("bucket on jsonl", func(t *testing.T) {
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{Type: "jsonl", Bucket: "leftover"}
		err := ValidateRunConfig(c)
		if err == nil || !strings.Contains(err.Error(), "traceEmitter.bucket is only valid") {
			t.Errorf("expected bucket-only-for-gcs error, got: %v", err)
		}
	})
	t.Run("objectPrefix on otel", func(t *testing.T) {
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{Type: "otel", ObjectPrefix: "traces/"}
		err := ValidateRunConfig(c)
		if err == nil || !strings.Contains(err.Error(), "traceEmitter.objectPrefix is only valid") {
			t.Errorf("expected objectPrefix-only-for-gcs error, got: %v", err)
		}
	})
	t.Run("credential on jsonl", func(t *testing.T) {
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{
			Type:       "jsonl",
			Credential: &CredentialConfig{Type: "gcp-workload-identity"},
		}
		err := ValidateRunConfig(c)
		if err == nil || !strings.Contains(err.Error(), "traceEmitter.credential is only valid") {
			t.Errorf("expected credential-only-for-gcs error, got: %v", err)
		}
	})
}

// --- resultSink ---

func TestValidateRunConfig_ResultSink_NoneAndStdoutJSON(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		c := validConfig()
		c.ResultSink = &ResultSinkConfig{Type: "none"}
		if err := ValidateRunConfig(c); err != nil {
			t.Fatalf("expected resultSink=none to pass, got: %v", err)
		}
	})
	t.Run("stdout-json", func(t *testing.T) {
		c := validConfig()
		c.ResultSink = &ResultSinkConfig{Type: "stdout-json"}
		if err := ValidateRunConfig(c); err != nil {
			t.Fatalf("expected resultSink=stdout-json to pass, got: %v", err)
		}
	})
	t.Run("nil sink ok", func(t *testing.T) {
		c := validConfig()
		c.ResultSink = nil
		if err := ValidateRunConfig(c); err != nil {
			t.Fatalf("expected nil resultSink to pass, got: %v", err)
		}
	})
}

func TestValidateRunConfig_ResultSink_GCPPubsubReserved(t *testing.T) {
	c := validConfig()
	c.ResultSink = &ResultSinkConfig{Type: "gcp-pubsub", Topic: "stirrup-results"}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for reserved resultSink type gcp-pubsub")
	}
	if !strings.Contains(err.Error(), "reserved but not yet implemented") {
		t.Errorf("expected reserved-but-not-implemented error, got: %v", err)
	}
}

// TestValidateRunConfig_ResultSink_BarePubsubRejected pins S1's
// discriminator rename: the bare "pubsub" string is no longer in
// validResultSinkTypes, so an operator who somehow ships it gets the
// unsupported-type error rather than the reserved-but-unimplemented
// path. No deprecation cycle is needed because "pubsub" has never
// shipped in a released binary.
func TestValidateRunConfig_ResultSink_BarePubsubRejected(t *testing.T) {
	c := validConfig()
	c.ResultSink = &ResultSinkConfig{Type: "pubsub"}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for unrecognised resultSink type pubsub")
	}
	if !strings.Contains(err.Error(), "unsupported resultSink type") {
		t.Errorf("expected unsupported-resultSink-type error, got: %v", err)
	}
}

// TestValidateRunConfig_ResultSink_TopicRejectedForNonPubSub pins S2:
// resultSink.topic is meaningful only for the gcp-pubsub adapter, so
// carrying it on a stdout-json sink fails loudly rather than being
// silently ignored.
func TestValidateRunConfig_ResultSink_TopicRejectedForNonPubSub(t *testing.T) {
	t.Run("topic on stdout-json", func(t *testing.T) {
		c := validConfig()
		c.ResultSink = &ResultSinkConfig{Type: "stdout-json", Topic: "leftover"}
		err := ValidateRunConfig(c)
		if err == nil || !strings.Contains(err.Error(), "resultSink.topic is only valid") {
			t.Errorf("expected topic-only-for-gcp-pubsub error, got: %v", err)
		}
	})
	t.Run("attributes on stdout-json", func(t *testing.T) {
		c := validConfig()
		c.ResultSink = &ResultSinkConfig{
			Type:       "stdout-json",
			Attributes: map[string]string{"env": "prod"},
		}
		err := ValidateRunConfig(c)
		if err == nil || !strings.Contains(err.Error(), "resultSink.attributes is only valid") {
			t.Errorf("expected attributes-only-for-gcp-pubsub error, got: %v", err)
		}
	})
}

func TestValidateRunConfig_ResultSink_GCSReserved(t *testing.T) {
	c := validConfig()
	c.ResultSink = &ResultSinkConfig{Type: "gcs"}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for reserved resultSink type gcs")
	}
	if !strings.Contains(err.Error(), "reserved but not yet implemented") {
		t.Errorf("expected reserved-but-not-implemented error, got: %v", err)
	}
}

func TestValidateRunConfig_ResultSink_InvalidType(t *testing.T) {
	c := validConfig()
	c.ResultSink = &ResultSinkConfig{Type: "carrier-pigeon"}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for unknown resultSink type")
	}
	if !strings.Contains(err.Error(), "unsupported resultSink type") {
		t.Errorf("expected unsupported-resultSink-type error, got: %v", err)
	}
}

// --- executor.workspaceExportTo ---

func TestValidateRunConfig_WorkspaceExportTo_ValidGS(t *testing.T) {
	c := validConfig()
	c.Executor = ExecutorConfig{
		Type:              "local",
		WorkspaceExportTo: "gs://stirrup-results/runs/run-1/workspace.tar.gz",
	}
	if err := ValidateRunConfig(c); err != nil {
		t.Fatalf("expected valid gs:// workspaceExportTo, got: %v", err)
	}
}

func TestValidateRunConfig_WorkspaceExportTo_RejectsNonGSScheme(t *testing.T) {
	c := validConfig()
	c.Executor = ExecutorConfig{
		Type:              "local",
		WorkspaceExportTo: "https://example.com/results.tar.gz",
	}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for non-gs:// workspaceExportTo")
	}
	if !strings.Contains(err.Error(), "gs://") {
		t.Errorf("expected error to mention gs:// scheme, got: %v", err)
	}
}

func TestValidateRunConfig_WorkspaceExportTo_RejectsEmptyBucketPath(t *testing.T) {
	cases := []string{"gs://", "gs:///object"}
	for _, val := range cases {
		t.Run(val, func(t *testing.T) {
			c := validConfig()
			c.Executor = ExecutorConfig{Type: "local", WorkspaceExportTo: val}
			err := ValidateRunConfig(c)
			if err == nil {
				t.Fatalf("expected error for %q", val)
			}
			if !strings.Contains(err.Error(), "non-empty bucket path") {
				t.Errorf("expected non-empty-bucket-path error, got: %v", err)
			}
		})
	}
}

func TestValidateRunConfig_WorkspaceExportTo_RejectsAPIExecutor(t *testing.T) {
	c := validConfig()
	c.Executor = ExecutorConfig{
		Type:              "api",
		WorkspaceExportTo: "gs://bucket/path",
	}
	// The api executor needs a VcsBackend to validate; supply one
	// so the test isolates the workspaceExportTo failure.
	c.Executor.VcsBackend = &VcsBackendConfig{Type: "github", Repo: "owner/repo", Ref: "main"}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for workspaceExportTo with executor.type=api")
	}
	if !strings.Contains(err.Error(), "not valid for executor.type=\"api\"") {
		t.Errorf("expected error to mention api executor, got: %v", err)
	}
}

func TestValidateRunConfig_WorkspaceExportTo_RequiresExplicitExecutorType(t *testing.T) {
	c := validConfig()
	// validConfig sets no Executor; assign just WorkspaceExportTo.
	c.Executor = ExecutorConfig{WorkspaceExportTo: "gs://bucket/path"}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected error for workspaceExportTo with empty executor.type")
	}
	if !strings.Contains(err.Error(), "requires an explicit executor.type") {
		t.Errorf("expected error to mention explicit executor.type, got: %v", err)
	}
}

// --- Redact() coverage for resultSink.attributes ---

func TestRedact_ResultSinkAttributes(t *testing.T) {
	rc := RunConfig{
		ResultSink: &ResultSinkConfig{
			Type: "gcp-pubsub",
			Attributes: map[string]string{
				"auth-token":     "secret://PUBSUB_TOKEN",
				"workload":       "classification",
				"another-secret": "secret://ANOTHER",
			},
		},
	}
	redacted := rc.Redact()

	if got := redacted.ResultSink.Attributes["auth-token"]; got != "secret://[REDACTED]" {
		t.Errorf("auth-token attribute = %q, want secret://[REDACTED]", got)
	}
	if got := redacted.ResultSink.Attributes["another-secret"]; got != "secret://[REDACTED]" {
		t.Errorf("another-secret attribute = %q, want secret://[REDACTED]", got)
	}
	if got := redacted.ResultSink.Attributes["workload"]; got != "classification" {
		t.Errorf("plaintext attribute mutated: got %q", got)
	}
	// Original unchanged.
	if rc.ResultSink.Attributes["auth-token"] != "secret://PUBSUB_TOKEN" {
		t.Error("Redact mutated original ResultSink.Attributes")
	}
}
