package types

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedact_ProviderAPIKey(t *testing.T) {
	check := assert.New(t)
	rc := RunConfig{
		Provider: ProviderConfig{APIKeyRef: "secret://my-key"},
	}
	redacted := rc.Redact()
	check.Equal("secret://[REDACTED]", redacted.Provider.APIKeyRef)
	check.Equal("secret://my-key", rc.Provider.APIKeyRef, "Redact must not mutate the original config")
}

func TestRedact_VcsBackendAPIKey(t *testing.T) {
	check := assert.New(t)
	rc := RunConfig{
		Executor: ExecutorConfig{
			VcsBackend: &VcsBackendConfig{APIKeyRef: "secret://github-token"},
		},
	}
	redacted := rc.Redact()
	check.Equal("secret://[REDACTED]", redacted.Executor.VcsBackend.APIKeyRef)
	check.Equal("secret://github-token", rc.Executor.VcsBackend.APIKeyRef, "Redact must not mutate the original VcsBackend")
}

func TestRedact_ProvidersAPIKeys(t *testing.T) {
	check := assert.New(t)
	rc := RunConfig{
		Providers: map[string]ProviderConfig{
			"backup": {Type: "openai-compatible", APIKeyRef: "secret://backup-key"},
			"public": {Type: "openai-compatible"},
		},
	}
	redacted := rc.Redact()
	check.Equal("secret://[REDACTED]", redacted.Providers["backup"].APIKeyRef)
	check.Empty(redacted.Providers["public"].APIKeyRef)
	check.Equal("secret://backup-key", rc.Providers["backup"].APIKeyRef, "Redact must not mutate the original Providers map")
}

func TestRedact_ProviderConnectionFields(t *testing.T) {
	check := assert.New(t)
	rc := RunConfig{
		Provider: ProviderConfig{
			Type:         "openai-compatible",
			BaseURL:      "https://user:pass@llm-gateway.internal.corp/openai/v1?api-key=raw",
			APIKeyHeader: "x-api-key",
			QueryParams: map[string]string{
				"api-version": "2024-10-21",
				"api-key":     "raw-query-key",
				"tenant":      "internal-team",
			},
		},
		Providers: map[string]ProviderConfig{
			"backup": {
				Type:         "openai-responses",
				BaseURL:      "https://backup.internal.example/v1/responses?token=raw",
				APIKeyHeader: "api-key",
				QueryParams: map[string]string{
					"api-version": "preview",
					"token":       "raw-token",
				},
			},
		},
	}

	redacted := rc.Redact()

	check.Equal("https://llm-gateway.internal.corp", redacted.Provider.BaseURL, "Provider.BaseURL keeps the origin only")
	check.Equal("[REDACTED]", redacted.Provider.APIKeyHeader, "Provider.APIKeyHeader")
	check.Equal("2024-10-21", redacted.Provider.QueryParams["api-version"], "allowlisted query parameter")
	check.Equal("[REDACTED]", redacted.Provider.QueryParams["api-key"], "Provider.QueryParams[api-key]")
	check.Equal("[REDACTED]", redacted.Provider.QueryParams["tenant"], "Provider.QueryParams[tenant]")

	backup := redacted.Providers["backup"]
	check.Equal("https://backup.internal.example", backup.BaseURL, "Providers[backup].BaseURL keeps the origin only")
	check.Equal("[REDACTED]", backup.APIKeyHeader, "Providers[backup].APIKeyHeader")
	check.Equal("preview", backup.QueryParams["api-version"], "allowlisted query parameter")
	check.Equal("[REDACTED]", backup.QueryParams["token"], "Providers[backup].QueryParams[token]")

	redacted.Provider.QueryParams["tenant"] = "mutated"
	check.Equal("https://user:pass@llm-gateway.internal.corp/openai/v1?api-key=raw", rc.Provider.BaseURL, "Redact must not mutate the original Provider.BaseURL")
	check.Equal("x-api-key", rc.Provider.APIKeyHeader, "Redact must not mutate the original Provider.APIKeyHeader")
	check.Equal("internal-team", rc.Provider.QueryParams["tenant"], "Redact must not mutate the original Provider.QueryParams")
}

func TestRedact_MCPServersAPIKeys(t *testing.T) {
	check := assert.New(t)
	rc := RunConfig{
		Tools: ToolsConfig{
			MCPServers: []MCPServerConfig{
				{URI: "https://user:pass@mcp-a.internal.example/mcp?api_key=raw", APIKeyRef: "secret://key1"},
				{URI: "http://b", APIKeyRef: ""},
				{URI: "http://c/path?token=raw", APIKeyRef: "secret://key2"},
			},
		},
	}
	redacted := rc.Redact()
	check.Equal("https://mcp-a.internal.example", redacted.Tools.MCPServers[0].URI, "MCPServers[0].URI keeps the origin only")
	check.Equal("secret://[REDACTED]", redacted.Tools.MCPServers[0].APIKeyRef, "MCPServers[0].APIKeyRef")
	check.Equal("http://b", redacted.Tools.MCPServers[1].URI, "MCPServers[1].URI")
	check.Empty(redacted.Tools.MCPServers[1].APIKeyRef, "MCPServers[1].APIKeyRef")
	check.Equal("http://c", redacted.Tools.MCPServers[2].URI, "MCPServers[2].URI keeps the origin only")
	check.Equal("secret://[REDACTED]", redacted.Tools.MCPServers[2].APIKeyRef, "MCPServers[2].APIKeyRef")

	check.Equal("secret://key1", rc.Tools.MCPServers[0].APIKeyRef, "Redact must not mutate the original MCPServers APIKeyRef")
	check.Equal("https://user:pass@mcp-a.internal.example/mcp?api_key=raw", rc.Tools.MCPServers[0].URI, "Redact must not mutate the original MCPServers URI")
}

// TestRedact_SessionNamePreserved pins that SessionName survives Redact(): it is
// an operator label, not a secret, and must persist in traces for cross-referencing.
func TestRedact_SessionNamePreserved(t *testing.T) {
	must := require.New(t)
	rc := RunConfig{
		SessionName: "nightly-eval",
		Provider:    ProviderConfig{APIKeyRef: "secret://k"},
	}
	redacted := rc.Redact()
	must.Equal("nightly-eval", redacted.SessionName)
}

func TestRedact_EmptyConfig_LeavesAPIKeyEmpty(t *testing.T) {
	must := require.New(t)
	rc := RunConfig{}
	redacted := rc.Redact()
	must.Empty(redacted.Provider.APIKeyRef)
}

// TestRedact_TraceEmitterHeaders pins that a "secret://" trace-emitter header value
// is rewritten to "secret://[REDACTED]" by Redact() while plaintext headers pass
// through unchanged; the resolved bearer must never reach a persisted trace.
func TestRedact_TraceEmitterHeaders(t *testing.T) {
	check := assert.New(t)
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

	check.Equal("secret://[REDACTED]", redacted.TraceEmitter.Headers["Authorization"])
	check.Equal("team-a", redacted.TraceEmitter.Headers["X-Tenant"], "plaintext headers pass through")

	check.Equal("secret://GRAFANA_CLOUD_AUTH", rc.TraceEmitter.Headers["Authorization"], "Redact must not mutate the original TraceEmitter.Headers")
}

// TestRedact_TraceEmitterPlatformOptionsUnchanged pins that Redact() leaves
// the platform and its options untouched: options are validated never to be
// secrets, so a persisted trace shows exactly what the operator declared.
func TestRedact_TraceEmitterPlatformOptionsUnchanged(t *testing.T) {
	must := require.New(t)
	rc := RunConfig{
		TraceEmitter: TraceEmitterConfig{
			Type:            "otel",
			Platform:        "cloudwatch",
			PlatformOptions: map[string]string{"region": "us-east-1", "logGroup": "/stirrup/agents"},
		},
	}
	redacted := rc.Redact()
	must.Equal("cloudwatch", redacted.TraceEmitter.Platform)
	must.Equal(rc.TraceEmitter.PlatformOptions, redacted.TraceEmitter.PlatformOptions)
}

// TestRedact_ProviderRetryNotAliased pins that Redact() deep-copies
// ProviderConfig.Retry on both the top-level Provider and every Providers entry,
// so mutating the redacted config's Retry cannot reach the live one.
func TestRedact_ProviderRetryNotAliased(t *testing.T) {
	must := require.New(t)
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

	must.NotNil(redacted.Provider.Retry, "top-level Retry dropped by Redact")
	must.NotSame(rc.Provider.Retry, redacted.Provider.Retry, "top-level Retry must be deep-copied")
	redacted.Provider.Retry.MaxAttempts = 99
	must.Equal(3, rc.Provider.Retry.MaxAttempts, "mutating redacted Provider.Retry must not reach the original")

	redactedSecondary := redacted.Providers["secondary"]
	originalSecondary := rc.Providers["secondary"]
	must.NotNil(redactedSecondary.Retry, "named-provider Retry dropped by Redact")
	must.NotSame(originalSecondary.Retry, redactedSecondary.Retry, "named-provider Retry must be deep-copied")
	redactedSecondary.Retry.MaxAttempts = 88
	must.Equal(4, rc.Providers["secondary"].Retry.MaxAttempts, "mutating redacted named-provider Retry must not reach the original")
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

func TestValidateRunConfig_AcceptsValidConfig(t *testing.T) {
	must := require.New(t)
	must.NoError(ValidateRunConfig(validConfig()))
}

func TestValidateRunConfig_ReadOnlyModeWithAllowAll(t *testing.T) {
	readOnlyModes := []string{"planning", "review", "research", "toil"}
	for _, mode := range readOnlyModes {
		t.Run(mode, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Mode = mode
			c.PermissionPolicy = PermissionPolicyConfig{Type: "allow-all"}
			err := ValidateRunConfig(c)
			must.Error(err)
			must.ErrorContains(err, mode)
		})
	}
}

func TestValidateRunConfig_ReadOnlyModeWithDenySideEffects(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Mode = "review"
	c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
	c.Tools = ToolsConfig{BuiltIn: []string{"read_file", "list_directory", "grep_files", "find_files"}}
	must.NoError(ValidateRunConfig(c), "deny-side-effects should be accepted for read-only mode")
}

// TestValidateRunConfig_ReadOnlyModeDefaultsPermissionPolicy pins that an
// omitted permissionPolicy.type resolves to deny-side-effects for every
// read-only mode, so gRPC and embedding callers land where the CLI does.
func TestValidateRunConfig_ReadOnlyModeDefaultsPermissionPolicy(t *testing.T) {
	for _, mode := range []string{"planning", "review", "research", "toil"} {
		t.Run(mode, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Mode = mode
			c.PermissionPolicy = PermissionPolicyConfig{}
			c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
			must.NoError(ValidateRunConfig(c))
			must.Equal("deny-side-effects", c.PermissionPolicy.Type)
		})
	}
}

// TestValidateRunConfig_ReadOnlyModeDefaultRespectsExplicitPolicy confirms
// the default never overwrites a caller's stricter choice.
func TestValidateRunConfig_ReadOnlyModeDefaultRespectsExplicitPolicy(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Mode = "planning"
	c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-all"}
	c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
	must.NoError(ValidateRunConfig(c), "deny-all should be accepted for a read-only mode")
	must.Equal("deny-all", c.PermissionPolicy.Type)
}

// TestValidateRunConfig_EditableModeRequiresPermissionPolicy pins that an
// omitted permissionPolicy.type is rejected outside the read-only modes.
// The only default an editable run could take is allow-all, which must be
// an explicit choice rather than something inferred from a missing field.
func TestValidateRunConfig_EditableModeRequiresPermissionPolicy(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.PermissionPolicy = PermissionPolicyConfig{}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "permissionPolicy.type is required")
	for _, name := range []string{"allow-all", "deny-all", "deny-side-effects", "ask-upstream", "policy-engine"} {
		must.ErrorContains(err, name, "valid value %q should be named in the error", name)
	}
	must.Empty(c.PermissionPolicy.Type, "permissionPolicy.type should stay empty, not be defaulted")
}

// TestValidateRunConfig_UnknownModeSkipsPermissionPolicyRequirement keeps
// the required-policy error off configs whose mode is itself invalid, so
// the reported cause stays the mode.
func TestValidateRunConfig_UnknownModeSkipsPermissionPolicyRequirement(t *testing.T) {
	cases := []struct {
		name string
		mode string
	}{
		{name: "empty mode", mode: ""},
		{name: "misspelled mode", mode: "excution"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Mode = tc.mode
			c.PermissionPolicy = PermissionPolicyConfig{}
			err := ValidateRunConfig(c)
			must.Error(err)
			must.NotContains(err.Error(), "permissionPolicy.type is required")
		})
	}
}

func TestValidateRunConfig_UnknownPermissionPolicy(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effect"}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "permissionPolicy")
}

func TestValidateRunConfig_UnknownRouterProvider(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.ModelRouter = ModelRouterConfig{
		Type:     "static",
		Provider: "backup",
		Model:    "claude-sonnet-4-6",
	}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "unknown provider")
}

func TestValidateRunConfig_InvalidBuiltInTool(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Tools = ToolsConfig{BuiltIn: []string{"delete_everything"}}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "tools.builtIn")
}

func TestValidateRunConfig_MCPServerValidation(t *testing.T) {
	cases := []struct {
		name    string
		server  MCPServerConfig
		wantErr string // substring; "" means the config must validate
	}{
		{
			name:   "https_remote_valid",
			server: MCPServerConfig{Name: "ok", URI: "https://mcp.example.com/rpc"},
		},
		{
			name:   "http_localhost_valid",
			server: MCPServerConfig{Name: "local", URI: "http://localhost:8080"},
		},
		{
			name:   "http_loopback_ip_valid",
			server: MCPServerConfig{Name: "local", URI: "http://127.0.0.1:9000"},
		},
		{
			name:    "missing_name",
			server:  MCPServerConfig{URI: "https://mcp.example.com"},
			wantErr: "tools.mcpServers[0].name is required",
		},
		{
			name:    "missing_uri",
			server:  MCPServerConfig{Name: "x"},
			wantErr: "tools.mcpServers[0].uri is required",
		},
		{
			name:    "bad_scheme",
			server:  MCPServerConfig{Name: "x", URI: "file:///etc/passwd"},
			wantErr: "scheme",
		},
		{
			name:    "http_remote_rejected",
			server:  MCPServerConfig{Name: "x", URI: "http://mcp.example.com/rpc"},
			wantErr: "must use https",
		},
		{
			name:    "allowedhost_with_scheme",
			server:  MCPServerConfig{Name: "x", URI: "https://mcp.example.com", AllowedMCPHosts: []string{"https://evil.example.com"}},
			wantErr: "allowedMcpHosts[0]",
		},
		{
			name:    "allowedhost_empty",
			server:  MCPServerConfig{Name: "x", URI: "https://mcp.example.com", AllowedMCPHosts: []string{" "}},
			wantErr: "allowedMcpHosts[0]",
		},
		{
			name:   "allowedhost_ipv6_literal_valid",
			server: MCPServerConfig{Name: "x", URI: "https://mcp.example.com", AllowedMCPHosts: []string{"::1", "2001:db8::1"}},
		},
		{
			name:   "allowedhost_bare_name_valid",
			server: MCPServerConfig{Name: "x", URI: "https://mcp.example.com", AllowedMCPHosts: []string{"mcp.example.com"}},
		},
		{
			name:   "allowedtools_set_validates",
			server: MCPServerConfig{Name: "x", URI: "https://mcp.example.com", AllowedTools: []string{"search"}},
		},
		{
			name:    "allowedtools_empty_entry",
			server:  MCPServerConfig{Name: "x", URI: "https://mcp.example.com", AllowedTools: []string{"search", "  "}},
			wantErr: "allowedTools[1]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Tools = ToolsConfig{MCPServers: []MCPServerConfig{tc.server}}
			err := ValidateRunConfig(c)
			if tc.wantErr == "" {
				must.NoError(err)
				return
			}
			must.ErrorContains(err, tc.wantErr)
		})
	}
}

func TestValidateRunConfig_UnknownToolsProfile(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Tools.Profile = "provider-native-but-unimplemented"
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "tools.profile")
}

func TestValidateRunConfig_KnownToolsProfilesAccepted(t *testing.T) {
	// The empty profile is the default path and must keep validating.
	cases := []struct {
		name    string
		profile string
	}{
		{name: "empty", profile: ""},
		{name: "default", profile: "default"},
		{name: "coding-classic", profile: "coding-classic"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Tools.Profile = tc.profile
			must.NoError(ValidateRunConfig(c))
		})
	}
}

// TestToolsConfig_ProfileOmittedOnWire pins that an empty Profile is omitted from
// the wire (so pre-profile configs round-trip byte-identically), and a set Profile
// appears on the wire.
func TestToolsConfig_ProfileOmittedOnWire(t *testing.T) {
	must := require.New(t)
	b, err := json.Marshal(ToolsConfig{BuiltIn: []string{"read_file"}})
	must.NoError(err)
	must.NotContains(string(b), "profile", "empty Profile must be omitted from ToolsConfig")

	withProfile, err := json.Marshal(ToolsConfig{Profile: "coding-classic"})
	must.NoError(err)
	must.Contains(string(withProfile), `"profile":"coding-classic"`)
}

func TestValidateRunConfig_MaxTurnsExceedsLimit(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.MaxTurns = 101
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "maxTurns")
}

func TestValidateRunConfig_MaxTurnsZero(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.MaxTurns = 0
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "maxTurns")
}

func TestValidateRunConfig_MaxTurnsNegative(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.MaxTurns = -1
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "maxTurns")
}

func TestValidateRunConfig_NilTimeout(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Timeout = nil
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "timeout")
}

func TestValidateRunConfig_TimeoutExceedsMax(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	timeout := 3601
	c.Timeout = &timeout
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "timeout")
}

func TestValidateRunConfig_TimeoutMustBePositive(t *testing.T) {
	for _, timeout := range []int{0, -1} {
		t.Run(fmt.Sprintf("timeout_%d", timeout), func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Timeout = &timeout
			err := ValidateRunConfig(c)
			must.ErrorContains(err, "timeout")
		})
	}
}

func TestValidateRunConfig_ReportsEveryViolation(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Mode = "planning"
	c.PermissionPolicy = PermissionPolicyConfig{Type: "allow-all"}
	c.MaxTurns = 200
	c.Timeout = nil
	err := ValidateRunConfig(c)
	must.Error(err)

	must.ErrorContains(err, "planning")
	must.ErrorContains(err, "maxTurns")
	must.ErrorContains(err, "timeout")
}

func TestValidateRunConfig_ReadOnlyModeWithWriteToolInList(t *testing.T) {
	writeTools := []string{"write_file", "run_command", "edit_file", "search_replace", "apply_diff"}
	for _, mode := range []string{"planning", "review", "research", "toil"} {
		t.Run(mode, func(t *testing.T) {
			for _, tool := range writeTools {
				t.Run(tool, func(t *testing.T) {
					must := require.New(t)
					c := validConfig()
					c.Mode = mode
					c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
					c.Tools = ToolsConfig{BuiltIn: []string{"read_file", tool}}
					err := ValidateRunConfig(c)
					must.ErrorContains(err, "read-only mode")
					must.ErrorContains(err, tool)
				})
			}
		})
	}
}

func TestValidateRunConfig_ReadOnlyModeRejectsOffloadToFile(t *testing.T) {
	for _, mode := range []string{"planning", "review", "research", "toil"} {
		t.Run(mode, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Mode = mode
			c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
			c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
			c.ContextStrategy = ContextStrategyConfig{Type: "offload-to-file"}
			err := ValidateRunConfig(c)
			must.ErrorContains(err, mode)
			must.ErrorContains(err, "offload-to-file")
		})
	}
}

func TestValidateRunConfig_ReadOnlyModeAcceptsNonWritingStrategies(t *testing.T) {
	for _, mode := range []string{"planning", "review", "research", "toil"} {
		for _, strategy := range []string{"", "sliding-window", "summarise", "clear-tool-results"} {
			t.Run(mode+"/"+strategy, func(t *testing.T) {
				must := require.New(t)
				c := validConfig()
				c.Mode = mode
				c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
				c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
				c.ContextStrategy = ContextStrategyConfig{Type: strategy}
				must.NoError(ValidateRunConfig(c))
			})
		}
	}
}

func TestValidateRunConfig_ExecutionModeAcceptsOffloadToFile(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Mode = "execution"
	c.ContextStrategy = ContextStrategyConfig{Type: "offload-to-file"}
	must.NoError(ValidateRunConfig(c))
}

// A toolset profile must not loosen the read-only-mode write-tool exclusion: a
// read-only mode that lists a write tool is rejected regardless of profile.
func TestValidateRunConfig_ReadOnlyModeProfileDoesNotBypassWriteExclusion(t *testing.T) {
	for _, profile := range []string{"default", "coding-classic"} {
		t.Run(profile, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Mode = "research"
			c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
			c.Tools = ToolsConfig{
				BuiltIn: []string{"read_file", "run_command"},
				Profile: profile,
			}
			err := ValidateRunConfig(c)
			must.ErrorContains(err, "read-only mode")
			must.ErrorContains(err, "run_command")
		})
	}
}

func TestValidateRunConfig_ReadOnlyModeWithNoExplicitToolList(t *testing.T) {
	for _, mode := range []string{"planning", "review", "research", "toil"} {
		t.Run(mode, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Mode = mode
			c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
			// Tools.BuiltIn left nil (default: all tools enabled)
			err := ValidateRunConfig(c)
			must.ErrorContains(err, "requires an explicit tools.builtIn list")
		})
	}
}

func TestValidateRunConfig_ReadOnlyModeWithOnlyReadTools(t *testing.T) {
	for _, mode := range []string{"planning", "review", "research", "toil"} {
		t.Run(mode, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Mode = mode
			c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
			c.Tools = ToolsConfig{BuiltIn: []string{"read_file", "list_directory", "grep_files", "find_files"}}
			must.NoError(ValidateRunConfig(c))
		})
	}
}

// TestValidateRunConfig_ReadOnlyModeAcceptsGitTools pins that read-only VCS tools
// (git_status, git_changed_files, git_diff, git_show) are accepted in every
// read-only mode, while a config that also lists a write tool is still rejected.
func TestValidateRunConfig_ReadOnlyModeAcceptsGitTools(t *testing.T) {
	gitTools := []string{"git_status", "git_changed_files", "git_diff", "git_show"}
	for _, mode := range []string{"planning", "review", "research", "toil"} {
		t.Run(mode, func(t *testing.T) {
			t.Run("accepts git tools", func(t *testing.T) {
				must := require.New(t)
				c := validConfig()
				c.Mode = mode
				c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
				c.Tools = ToolsConfig{BuiltIn: append([]string{"read_file"}, gitTools...)}
				must.NoError(ValidateRunConfig(c))
			})
			t.Run("rejects write tool alongside git tools", func(t *testing.T) {
				must := require.New(t)
				c := validConfig()
				c.Mode = mode
				c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
				c.Tools = ToolsConfig{BuiltIn: append(append([]string{"read_file"}, gitTools...), "run_command")}
				err := ValidateRunConfig(c)
				must.ErrorContains(err, "read-only mode")
				must.ErrorContains(err, "run_command")
			})
		})
	}
}

// TestDefaultReadOnlyBuiltInTools_PassesValidation locks in that
// DefaultReadOnlyBuiltInTools() is always a valid Tools.BuiltIn list for every
// read-only mode, so adding a mode or a mutating tool without updating the
// defaults fails loudly here rather than at runtime.
func TestDefaultReadOnlyBuiltInTools_PassesValidation(t *testing.T) {
	must := require.New(t)
	defaults := DefaultReadOnlyBuiltInTools()
	must.NotEmpty(defaults)

	for _, tool := range defaults {
		t.Run("tool "+tool, func(t *testing.T) {
			must := require.New(t)
			must.False(mutatingTools[tool], "default read-only tool must not be mutating")
			must.True(validBuiltInToolNames[tool], "default read-only tool must be a known built-in")
		})
	}

	// Iterate over the actual readOnlyModes map so adding a new read-only mode
	// without updating the defaults (or vice versa) fails loudly.
	for mode := range readOnlyModes {
		t.Run(mode, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Mode = mode
			c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
			c.Tools = ToolsConfig{BuiltIn: DefaultReadOnlyBuiltInTools()}
			must.NoError(ValidateRunConfig(c))
		})
	}
}

func TestIsReadOnlyMode_ClassifiesModes(t *testing.T) {
	cases := map[string]bool{
		"planning": true, "review": true, "research": true, "toil": true,
		"execution": false, "": false, "unknown": false,
	}
	for mode, want := range cases {
		t.Run(fmt.Sprintf("%q", mode), func(t *testing.T) {
			must := require.New(t)
			must.Equal(want, IsReadOnlyMode(mode))
		})
	}
}

func TestIsBuiltInTool_ClassifiesNames(t *testing.T) {
	cases := map[string]bool{
		"read_file": true, "write_file": true, "run_command": true, "spawn_agent": true, "git_show": true,
		"": false, "mcp_docs_update": false, "ticket_lookup": false, "search_files": false,
	}
	for name, want := range cases {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			must := require.New(t)
			must.Equal(want, IsBuiltInTool(name))
		})
	}
}

func TestValidateRunConfig_ExecutionModeWithWriteTools(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Mode = "execution"
	c.Tools = ToolsConfig{BuiltIn: []string{"read_file", "write_file", "run_command"}}
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_FollowUpGraceBound(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	grace := 3601
	c.FollowUpGrace = &grace
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "followUpGrace")

	// At the boundary should pass.
	grace = 3600
	c.FollowUpGrace = &grace
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_FollowUpHistory(t *testing.T) {
	for _, value := range []string{"", FollowUpHistoryReset, FollowUpHistoryContinue} {
		t.Run(fmt.Sprintf("accepts %q", value), func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.FollowUpHistory = value
			must.NoError(ValidateRunConfig(c))
		})
	}

	t.Run("rejects unknown value", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.FollowUpHistory = "resume"
		err := ValidateRunConfig(c)
		must.ErrorContains(err, `followUpHistory "resume"`)
	})
}

func TestValidateRunConfig_FollowUpHistoryWithoutGraceIsAccepted(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.FollowUpGrace = nil
	c.FollowUpHistory = FollowUpHistoryContinue
	must.NoError(ValidateRunConfig(c))
}

func TestRunConfig_ContinuesFollowUpHistory(t *testing.T) {
	cases := map[string]bool{"": false, FollowUpHistoryReset: false, FollowUpHistoryContinue: true}
	for value, want := range cases {
		t.Run(fmt.Sprintf("followUpHistory %q", value), func(t *testing.T) {
			must := require.New(t)
			must.Equal(want, (&RunConfig{FollowUpHistory: value}).ContinuesFollowUpHistory())
		})
	}
	t.Run("nil config does not continue history", func(t *testing.T) {
		must := require.New(t)
		var nilConfig *RunConfig
		must.False(nilConfig.ContinuesFollowUpHistory())
	})
}

func TestValidateRunConfig_TokenBudgetBound(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	tokens := 50_000_001
	c.MaxTokenBudget = &tokens
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "maxTokenBudget")

	// At the boundary should pass.
	tokens = 50_000_000
	c.MaxTokenBudget = &tokens
	must.NoError(ValidateRunConfig(c))
}

// TestValidateRunConfig_CostBudgetBoundAndWarning pins both halves of the
// maxCostBudget contract: the $100 bound still rejects, and any value at
// all produces the warning that says the budget is not enforced, so the
// field can never look like a working spend cap.
func TestValidateRunConfig_CostBudgetBoundAndWarning(t *testing.T) {
	const wantMsg = "maxCostBudget is accepted and bounded but not enforced"
	for _, tc := range []struct {
		name    string
		set     bool
		budget  float64
		wantErr bool
	}{
		{"unset does not warn", false, 0, false},
		{"within cap warns", true, 10, false},
		{"at cap warns", true, 100, false},
		{"above cap errors and warns", true, 100.01, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var logBuf bytes.Buffer
			originalLogger := slog.Default()
			t.Cleanup(func() { slog.SetDefault(originalLogger) })
			slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))

			c := validConfig()
			if tc.set {
				budget := tc.budget
				c.MaxCostBudget = &budget
			}

			err := ValidateRunConfig(c)
			if tc.wantErr {
				must.ErrorContains(err, "maxCostBudget")
			} else {
				must.NoError(err)
			}

			logs := logBuf.String()
			if tc.set {
				must.Contains(logs, "level=WARN")
				must.Contains(logs, wantMsg)
				must.Contains(logs, fmt.Sprintf("maxCostBudget=%v", tc.budget))
			} else {
				must.NotContains(logs, wantMsg)
			}
		})
	}
}

func TestValidateRunConfig_NilBudgetsPass(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.FollowUpGrace = nil
	c.MaxTokenBudget = nil
	must.NoError(ValidateRunConfig(c))
}

// TestValidateRunConfig_ContextStrategyMaxTokens pins a deliberately wide
// accepted range so small-context local-model configs aren't rejected by a hard
// floor; only structurally-impossible budgets are rejected.
func TestValidateRunConfig_ContextStrategyMaxTokens(t *testing.T) {
	tests := []struct {
		name      string
		maxTokens int
		wantErr   bool
	}{
		{"unset uses default window", 0, false},
		{"negative rejected", -1, true},
		{"one token rejected", 1, true},
		{"just below floor rejected", minContextStrategyMaxTokens - 1, true},
		{"floor boundary accepted", minContextStrategyMaxTokens, false},
		// Matches the fixture value used by several harness/cmd/stirrup/cmd config-plumbing
		// tests that don't exercise a real run; must stay accepted so those don't regress.
		{"small-context fixture value accepted", 1000, false},
		{"small local-model window accepted", 32768, false},
		{"default window accepted", 200000, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.ContextStrategy = ContextStrategyConfig{Type: "sliding-window", MaxTokens: tt.maxTokens}
			err := ValidateRunConfig(c)
			if tt.wantErr {
				must.ErrorContains(err, "contextStrategy.maxTokens")
				return
			}
			must.NoError(err)
		})
	}
}

func TestValidateRunConfig_ClearToolResultsFields(t *testing.T) {
	intp := func(v int) *int { return &v }
	tests := []struct {
		name    string
		cfg     ContextStrategyConfig
		wantErr string
	}{
		{"defaults", ContextStrategyConfig{Type: "clear-tool-results"}, ""},
		{"keep zero accepted", ContextStrategyConfig{Type: "clear-tool-results", Keep: intp(0)}, ""},
		{"all fields", ContextStrategyConfig{
			Type: "clear-tool-results", MaxTokens: 100000, Keep: intp(5), ClearAtLeast: intp(20000),
			ExcludeTools: []string{"run_command", "web_fetch"},
		}, ""},
		{"negative keep", ContextStrategyConfig{Type: "clear-tool-results", Keep: intp(-1)}, "contextStrategy.keep must be >= 0"},
		{"zero clearAtLeast", ContextStrategyConfig{Type: "clear-tool-results", ClearAtLeast: intp(0)}, "contextStrategy.clearAtLeast must be > 0"},
		{"negative clearAtLeast", ContextStrategyConfig{Type: "clear-tool-results", ClearAtLeast: intp(-5)}, "contextStrategy.clearAtLeast must be > 0"},
		{"blank excluded tool", ContextStrategyConfig{Type: "clear-tool-results", ExcludeTools: []string{"read_file", " "}}, "contextStrategy.excludeTools[1]"},
		{"keep on another strategy", ContextStrategyConfig{Type: "summarise", Keep: intp(2)}, `contextStrategy.keep applies only to contextStrategy "clear-tool-results", not "summarise"`},
		{"clearAtLeast on the default strategy", ContextStrategyConfig{ClearAtLeast: intp(2)}, `contextStrategy.clearAtLeast applies only to contextStrategy "clear-tool-results", not "sliding-window"`},
		{"excludeTools on another strategy", ContextStrategyConfig{Type: "offload-to-file", ExcludeTools: []string{"read_file"}}, "contextStrategy.excludeTools applies only to"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Mode = "execution"
			c.ContextStrategy = tt.cfg
			err := ValidateRunConfig(c)
			if tt.wantErr == "" {
				must.NoError(err)
				return
			}
			must.ErrorContains(err, tt.wantErr)
		})
	}
}

func TestValidateRunConfig_TemperatureBounds(t *testing.T) {
	// Non-finite values must be rejected explicitly: IEEE 754 NaN compares false
	// against both bounds, so without a finite-number guard "--temperature=NaN"
	// would slip past the range checks and reach the provider.
	tests := []struct {
		name        string
		temperature float64
		wantErr     string
	}{
		{"negative rejected", -0.01, "temperature"},
		{"above 2.0 rejected", 2.01, "temperature"},
		{"NaN rejected", math.NaN(), "finite"},
		{"positive infinity rejected", math.Inf(1), "finite"},
		{"negative infinity rejected", math.Inf(-1), "finite"},
		// 0.0 is the greedy-decoding case; 1.0 is the Anthropic ceiling;
		// 2.0 is the OpenAI/Gemini ceiling.
		{"0.0 accepted", 0.0, ""},
		{"1.0 accepted", 1.0, ""},
		{"2.0 accepted", 2.0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			temperature := tt.temperature
			c.Temperature = &temperature
			err := ValidateRunConfig(c)
			if tt.wantErr == "" {
				must.NoError(err)
				return
			}
			must.ErrorContains(err, tt.wantErr)
		})
	}

	// Nil temperature falls back to the harness default and must validate cleanly.
	t.Run("nil temperature accepted", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Temperature = nil
		must.NoError(ValidateRunConfig(c))
	})
}

// TestValidateRunConfig_ReasoningEffort pins the closed enum on the
// provider-neutral reasoning knob. The accepted values are the union
// the targeted providers express natively; a wire-format spelling
// like Gemini's THINKING_LEVEL_HIGH must be rejected here so an adapter
// never has to guess which dialect a config was written in.
func TestValidateRunConfig_ReasoningEffort(t *testing.T) {
	for _, v := range []string{"", "minimal", "low", "medium", "high", "xhigh", "max"} {
		t.Run(fmt.Sprintf("accepts %q", v), func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.ReasoningEffort = v
			must.NoError(ValidateRunConfig(c))
		})
	}
	for _, v := range []string{"THINKING_LEVEL_HIGH", "maximum", "High"} {
		t.Run(fmt.Sprintf("rejects %q", v), func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.ReasoningEffort = v
			err := ValidateRunConfig(c)
			must.ErrorContains(err, "reasoningEffort")
		})
	}
}

func TestValidateRunConfig_ReasoningSummary(t *testing.T) {
	for _, v := range []string{"", "auto", "off"} {
		t.Run(fmt.Sprintf("accepts %q", v), func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.ReasoningSummary = v
			must.NoError(ValidateRunConfig(c))
		})
	}
	for _, v := range []string{"Auto", "none", "detailed", "concise"} {
		t.Run(fmt.Sprintf("rejects %q", v), func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.ReasoningSummary = v
			err := ValidateRunConfig(c)
			must.ErrorContains(err, "reasoningSummary")
		})
	}
}

func TestValidateRunConfig_EmptySessionNamePasses(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.SessionName = ""
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_SessionNameAcceptsPrintableText(t *testing.T) {
	cases := []struct {
		name        string
		sessionName string
	}{
		{"ascii with hyphens", "nightly-eval"},
		{"spaces and symbols", "PR #123 sweep"},
		{"diacritics", "café-run"},
		{"CJK characters", "文字列-test"},
		{"symbol emoji", "emoji-ok-\xe2\x9c\x85"}, // U+2705 white heavy check mark
		{"maximum length", strings.Repeat("a", 255)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.SessionName = tc.sessionName
			must.NoError(ValidateRunConfig(c))
		})
	}
}

func TestValidateRunConfig_SessionNameOver255BytesRejected(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.SessionName = strings.Repeat("a", 256)
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "sessionName")
	must.ErrorContains(err, "255")
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
			must := require.New(t)
			c := validConfig()
			c.SessionName = value
			must.ErrorContains(ValidateRunConfig(c), "sessionName")
		})
	}
}

func TestValidateRunConfig_SessionNameRejectsInvalidUTF8(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	// 0xff is never valid as a leading UTF-8 byte.
	c.SessionName = "bad\xffname"
	must.ErrorContains(ValidateRunConfig(c), "sessionName")
}

func TestValidateRunConfig_OpenAIResponsesProvider(t *testing.T) {
	must := require.New(t)
	// Tools.BuiltIn is pinned to a side-effect-free set so this test stays focused on
	// provider-type validation instead of also tripping Rule of Two.
	c := validConfig()
	c.Provider = ProviderConfig{Type: "openai-responses", APIKeyRef: "secret://OPENAI_KEY"}
	c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_CredentialNilPassesValidation(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Provider.Credential = nil
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_CredentialStaticPasses(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Provider.Credential = &CredentialConfig{Type: "static"}
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_CredentialAWSDefaultPasses(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Provider = ProviderConfig{
		Type:       "bedrock",
		Credential: &CredentialConfig{Type: "aws-default"},
	}
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_CredentialUnsupportedType(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Provider.Credential = &CredentialConfig{Type: "kerberos"}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "kerberos")
}

func TestValidateRunConfig_WebIdentityValid(t *testing.T) {
	must := require.New(t)
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
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_WebIdentityMissingRoleARN(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(ValidateRunConfig(c), "roleArn")
}

func TestValidateRunConfig_WebIdentityMissingTokenSource(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Provider = ProviderConfig{
		Type: "bedrock",
		Credential: &CredentialConfig{
			Type:    "web-identity",
			RoleARN: "arn:aws:iam::123456789012:role/test",
		},
	}
	must.ErrorContains(ValidateRunConfig(c), "tokenSource")
}

func TestValidateRunConfig_GKEMetadataMissingAudience(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(ValidateRunConfig(c), "audience")
}

func TestValidateRunConfig_FileTokenSourceMissingPath(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(ValidateRunConfig(c), "path")
}

func TestValidateRunConfig_EnvTokenSourceMissingEnvVar(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(ValidateRunConfig(c), "envVar")
}

func TestValidateRunConfig_AWSIRSATokenSourceValid(t *testing.T) {
	must := require.New(t)
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
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_AzureIMDSTokenSourceValid(t *testing.T) {
	must := require.New(t)
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
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_AzureIMDSTokenSourceMissingResource(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(ValidateRunConfig(c), "resource")
}

func TestValidateRunConfig_GitHubActionsOIDCTokenSourceValid(t *testing.T) {
	must := require.New(t)
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
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_GitHubActionsOIDCTokenSourceMissingAudience(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(ValidateRunConfig(c), "audience")
}

func TestValidateRunConfig_CredentialInProvidersMap(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(ValidateRunConfig(c), "providers[fallback].credential")
}

func TestRedact_CredentialConfigPreserved(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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

	check.Equal("secret://[REDACTED]", redacted.Provider.APIKeyRef)
	// Not sensitive, so Redact() must leave the credential config untouched.
	must.NotNil(redacted.Provider.Credential)
	check.Equal("arn:aws:iam::123456789012:role/test", redacted.Provider.Credential.RoleARN)
	check.Equal("sts.amazonaws.com", redacted.Provider.Credential.TokenSource.Audience)
}

// TestRedact_AnthropicWIFFieldsPreserved verifies the four Anthropic federation
// identifiers ride through Redact() unchanged: they are non-secret values safe to
// commit to source control, and redacting them would hide debugging information.
func TestRedact_AnthropicWIFFieldsPreserved(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NotNil(cred)
	check.Equal("fdrl_abc123", cred.FederationRuleID)
	check.Equal("550e8400-e29b-41d4-a716-446655440000", cred.OrganizationID)
	check.Equal("svac_xyz789", cred.ServiceAccountID)
	check.Equal("wrkspc_def456", cred.WorkspaceID)
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

func TestValidateRunConfig_AnthropicWIFCredentialRules(t *testing.T) {
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
			// Cross-provider validation: pairing credential.type=anthropic-wif with a
			// non-Anthropic provider would exchange a WIF access token and hand it to a
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
			must := require.New(t)
			c := validConfig()
			tc.mutate(c)
			err := ValidateRunConfig(c)
			if tc.wantErr {
				must.ErrorContains(err, tc.errSubstr)
				return
			}
			must.NoError(err)
		})
	}
}

// validOpenAIWIFCredential returns a baseline openai-wif credential with
// every required field set. Negative-path tests mutate one field at a time
// off this baseline.
func validOpenAIWIFCredential() *CredentialConfig {
	return &CredentialConfig{
		Type:                     "openai-wif",
		OpenAIIdentityProviderID: "idp_abc123",
		OpenAIServiceAccountID:   "sa_xyz789",
		TokenSource: &TokenSourceConfig{
			Type:     "github-actions-oidc",
			Audience: "https://api.openai.com/v1",
		},
	}
}

// validOpenAIWIFProvider returns an openai-compatible provider carrying the
// baseline openai-wif credential and no static apiKeyRef (the cross-field
// validator rejects a key alongside WIF).
func validOpenAIWIFProvider() ProviderConfig {
	return ProviderConfig{
		Type:       "openai-compatible",
		BaseURL:    "https://api.openai.com/v1",
		Credential: validOpenAIWIFCredential(),
	}
}

func TestValidateRunConfig_OpenAIWIFCredentialRules(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(c *RunConfig)
		wantErr   bool
		errSubstr string
	}{
		{
			name: "minimal openai-wif config passes",
			mutate: func(c *RunConfig) {
				c.Provider = validOpenAIWIFProvider()
			},
			wantErr: false,
		},
		{
			name: "subjectTokenType jwt URN override passes",
			mutate: func(c *RunConfig) {
				p := validOpenAIWIFProvider()
				p.Credential.OpenAISubjectTokenType = "urn:ietf:params:oauth:token-type:id_token"
				c.Provider = p
			},
			wantErr: false,
		},
		{
			name: "openai-responses provider passes",
			mutate: func(c *RunConfig) {
				p := validOpenAIWIFProvider()
				p.Type = "openai-responses"
				c.Provider = p
			},
			wantErr: false,
		},
		{
			name: "missing openaiIdentityProviderId fails",
			mutate: func(c *RunConfig) {
				p := validOpenAIWIFProvider()
				p.Credential.OpenAIIdentityProviderID = ""
				c.Provider = p
			},
			wantErr:   true,
			errSubstr: "openai-wif requires openaiIdentityProviderId",
		},
		{
			name: "missing openaiServiceAccountId fails",
			mutate: func(c *RunConfig) {
				p := validOpenAIWIFProvider()
				p.Credential.OpenAIServiceAccountID = ""
				c.Provider = p
			},
			wantErr:   true,
			errSubstr: "openai-wif requires openaiServiceAccountId",
		},
		{
			name: "missing tokenSource fails",
			mutate: func(c *RunConfig) {
				p := validOpenAIWIFProvider()
				p.Credential.TokenSource = nil
				c.Provider = p
			},
			wantErr:   true,
			errSubstr: "openai-wif requires tokenSource",
		},
		{
			name: "identity provider id with embedded whitespace rejected",
			mutate: func(c *RunConfig) {
				p := validOpenAIWIFProvider()
				p.Credential.OpenAIIdentityProviderID = "idp with space"
				c.Provider = p
			},
			wantErr:   true,
			errSubstr: "openaiIdentityProviderId",
		},
		{
			name: "service account id with embedded whitespace rejected",
			mutate: func(c *RunConfig) {
				p := validOpenAIWIFProvider()
				p.Credential.OpenAIServiceAccountID = "sa with space"
				c.Provider = p
			},
			wantErr:   true,
			errSubstr: "openaiServiceAccountId",
		},
		{
			name: "sessionName on openai-wif rejected",
			mutate: func(c *RunConfig) {
				p := validOpenAIWIFProvider()
				p.Credential.SessionName = "stirrup"
				c.Provider = p
			},
			wantErr:   true,
			errSubstr: "sessionName is only valid for credential type \"web-identity\"",
		},
		{
			name: "audience on openai-wif rejected",
			mutate: func(c *RunConfig) {
				p := validOpenAIWIFProvider()
				p.Credential.Audience = "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/pr"
				c.Provider = p
			},
			wantErr:   true,
			errSubstr: "audience is only valid for credential type \"gcp-workload-identity-federation\"",
		},
		{
			name: "serviceAccount on openai-wif rejected",
			mutate: func(c *RunConfig) {
				p := validOpenAIWIFProvider()
				p.Credential.ServiceAccount = "vertex@my-project.iam.gserviceaccount.com"
				c.Provider = p
			},
			wantErr:   true,
			errSubstr: "serviceAccount is only valid for credential type \"gcp-workload-identity-federation\"",
		},
		{
			name: "subjectTokenType non-URN rejected",
			mutate: func(c *RunConfig) {
				p := validOpenAIWIFProvider()
				p.Credential.OpenAISubjectTokenType = "jwt"
				c.Provider = p
			},
			wantErr:   true,
			errSubstr: "openaiSubjectTokenType",
		},
		{
			name: "apiKeyRef set alongside openai-wif rejected",
			mutate: func(c *RunConfig) {
				p := validOpenAIWIFProvider()
				p.APIKeyRef = "secret://OPENAI_KEY"
				c.Provider = p
			},
			wantErr:   true,
			errSubstr: "openai-wif does not use apiKeyRef",
		},
		{
			name: "apiKeyHeader api-key alongside openai-wif rejected",
			mutate: func(c *RunConfig) {
				p := validOpenAIWIFProvider()
				p.APIKeyHeader = "api-key"
				c.Provider = p
			},
			wantErr:   true,
			errSubstr: "openai-wif requires Authorization: Bearer",
		},
		{
			name: "roleArn on openai-wif rejected",
			mutate: func(c *RunConfig) {
				p := validOpenAIWIFProvider()
				p.Credential.RoleARN = "arn:aws:iam::123456789012:role/Stirrup"
				c.Provider = p
			},
			wantErr:   true,
			errSubstr: "roleArn is only valid for credential type \"web-identity\"",
		},
		{
			// Cross-provider validation: pairing credential.type=openai-wif
			// with a non-OpenAI provider would hand a federated access token
			// to a foreign endpoint. Fail closed at config-load time.
			name: "openai-wif paired with anthropic provider rejected",
			mutate: func(c *RunConfig) {
				c.Provider = ProviderConfig{
					Type:       "anthropic",
					Credential: validOpenAIWIFCredential(),
				}
			},
			wantErr:   true,
			errSubstr: "openai-wif is only supported with openai-compatible or openai-responses",
		},
		{
			// Reciprocal mutual-exclusion: the openai-wif fields are scoped
			// to type=openai-wif; a leftover value on another type is an
			// error so it does not silently linger across a type change.
			name: "openaiIdentityProviderId on static credential rejected",
			mutate: func(c *RunConfig) {
				c.Provider = ProviderConfig{
					Type:      "openai-compatible",
					APIKeyRef: "secret://OPENAI_KEY",
					Credential: &CredentialConfig{
						Type:                     "static",
						OpenAIIdentityProviderID: "idp_abc123",
					},
				}
			},
			wantErr:   true,
			errSubstr: "openaiIdentityProviderId is only valid for credential type \"openai-wif\"",
		},
		{
			name: "openaiServiceAccountId on static credential rejected",
			mutate: func(c *RunConfig) {
				c.Provider = ProviderConfig{
					Type:      "openai-compatible",
					APIKeyRef: "secret://OPENAI_KEY",
					Credential: &CredentialConfig{
						Type:                   "static",
						OpenAIServiceAccountID: "sa_abc123",
					},
				}
			},
			wantErr:   true,
			errSubstr: "openaiServiceAccountId is only valid for credential type \"openai-wif\"",
		},
		{
			name: "openaiSubjectTokenType on static credential rejected",
			mutate: func(c *RunConfig) {
				c.Provider = ProviderConfig{
					Type:      "openai-compatible",
					APIKeyRef: "secret://OPENAI_KEY",
					Credential: &CredentialConfig{
						Type:                   "static",
						OpenAISubjectTokenType: "urn:ietf:params:oauth:token-type:jwt",
					},
				}
			},
			wantErr:   true,
			errSubstr: "openaiSubjectTokenType is only valid for credential type \"openai-wif\"",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			tc.mutate(c)
			err := ValidateRunConfig(c)
			if tc.wantErr {
				must.Error(err)
				if tc.errSubstr != "" {
					must.ErrorContains(err, tc.errSubstr)
				}
				return
			}
			must.NoError(err)
		})
	}
}

// --- ValidateRunConfig: APIKeyHeader / QueryParams ---

func TestValidateRunConfig_APIKeyHeader_Valid(t *testing.T) {
	cases := []string{"", "api-key", "x-api-key", "Ocp-Apim-Subscription-Key", "Authorization"}
	for _, header := range cases {
		t.Run(header, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Provider = ProviderConfig{Type: "openai-responses", APIKeyHeader: header}
			must.NoError(ValidateRunConfig(c))
		})
	}
}

// --- ExecutorConfig.Runtime ---

// TestValidateRunConfig_ExecutorRuntimeAcceptsClosedSet locks in the
// closed set of container OCI runtimes. Adding a new runtime here without
// updating validContainerRuntimes (or vice versa) fails loudly so the
// validator does not silently accept an unknown runtime string.
func TestValidateRunConfig_ExecutorRuntimeAcceptsClosedSet(t *testing.T) {
	for _, runtime := range []string{"", "runc", "runsc", "kata", "kata-qemu", "kata-fc", "kata-clh"} {
		t.Run(fmt.Sprintf("runtime_%s", runtime), func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Executor = ExecutorConfig{Type: "container", Runtime: runtime}
			must.NoError(ValidateRunConfig(c))
		})
	}
}

// TestValidateRunConfig_RuntimeSetIsPerType pins that a runtime name valid for one
// executor is rejected on the other: naively sharing the closed set would let
// "container" accept "gvisor" (a k8s RuntimeClass, not a Docker OCI runtime) or
// "k8s" accept "runsc" (the container name for gVisor).
func TestValidateRunConfig_RuntimeSetIsPerType(t *testing.T) {
	cases := []struct {
		name        string
		execType    string
		runtime     string
		image       string
		k8sNS       string
		network     *NetworkConfig
		wantErr     bool
		errContains string
	}{
		{name: "container accepts runsc", execType: "container", runtime: "runsc", image: "img", wantErr: false},
		{name: "container accepts kata-clh", execType: "container", runtime: "kata-clh", image: "img", wantErr: false},
		{name: "container rejects gvisor", execType: "container", runtime: "gvisor", image: "img", wantErr: true, errContains: "gvisor"},
		{name: "k8s accepts gvisor", execType: "k8s", runtime: "gvisor", image: "img", k8sNS: "ns", network: &NetworkConfig{Mode: "none"}, wantErr: false},
		{name: "k8s accepts kata-clh", execType: "k8s", runtime: "kata-clh", image: "img", k8sNS: "ns", network: &NetworkConfig{Mode: "none"}, wantErr: false},
		{name: "k8s rejects runsc", execType: "k8s", runtime: "runsc", image: "img", k8sNS: "ns", network: &NetworkConfig{Mode: "none"}, wantErr: true, errContains: "runsc"},
		{name: "k8s rejects kata bare", execType: "k8s", runtime: "kata", image: "img", k8sNS: "ns", network: &NetworkConfig{Mode: "none"}, wantErr: true, errContains: "kata"},
		{name: "k8s-sandbox accepts gvisor", execType: "k8s-sandbox", runtime: "gvisor", image: "img", k8sNS: "ns", network: &NetworkConfig{Mode: "none"}, wantErr: false},
		{name: "k8s-sandbox rejects runc", execType: "k8s-sandbox", runtime: "runc", image: "img", k8sNS: "ns", network: &NetworkConfig{Mode: "none"}, wantErr: true, errContains: "gvisor"},
		{name: "k8s-sandbox rejects kata-clh", execType: "k8s-sandbox", runtime: "kata-clh", image: "img", k8sNS: "ns", network: &NetworkConfig{Mode: "none"}, wantErr: true, errContains: "gvisor"},
		{name: "local rejects any runtime", execType: "local", runtime: "runc", wantErr: true, errContains: "only valid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Executor = ExecutorConfig{
				Type:         tc.execType,
				Runtime:      tc.runtime,
				Image:        tc.image,
				K8sNamespace: tc.k8sNS,
				Network:      tc.network,
			}
			err := ValidateRunConfig(c)
			if tc.wantErr {
				must.Error(err)
				if tc.errContains != "" {
					must.ErrorContains(err, tc.errContains)
				}
				return
			}
			must.NoError(err)
		})
	}
}

// TestValidateRunConfig_K8sExecutorFields exercises the type-"k8s"
// cross-field arm: Image and K8sNamespace are required, and Workspace is
// rejected because the Pod workspace is fixed at /workspace.
func TestValidateRunConfig_K8sExecutorFields(t *testing.T) {
	cases := []struct {
		name        string
		exec        ExecutorConfig
		wantErr     bool
		errContains string
	}{
		{
			name: "valid minimal",
			exec: ExecutorConfig{Type: "k8s", Image: "img", K8sNamespace: "ns", Network: &NetworkConfig{Mode: "none"}},
		},
		{
			name: "valid with optional fields",
			exec: ExecutorConfig{
				Type:              "k8s",
				Image:             "img",
				K8sNamespace:      "ns",
				K8sKubeconfig:     "/home/u/.kube/config",
				K8sNodeSelector:   map[string]string{"disktype": "ssd"},
				K8sServiceAccount: "agent-sa",
				Runtime:           "gvisor",
				Network:           &NetworkConfig{Mode: "none"},
			},
		},
		{
			name:        "missing image",
			exec:        ExecutorConfig{Type: "k8s", K8sNamespace: "ns", Network: &NetworkConfig{Mode: "none"}},
			wantErr:     true,
			errContains: "executor.image is required",
		},
		{
			name:        "missing namespace",
			exec:        ExecutorConfig{Type: "k8s", Image: "img", Network: &NetworkConfig{Mode: "none"}},
			wantErr:     true,
			errContains: "executor.k8sNamespace is required",
		},
		{
			name:        "workspace rejected",
			exec:        ExecutorConfig{Type: "k8s", Image: "img", K8sNamespace: "ns", Workspace: "/ws", Network: &NetworkConfig{Mode: "none"}},
			wantErr:     true,
			errContains: "executor.workspace is not valid",
		},
		{
			name:        "nil network rejected",
			exec:        ExecutorConfig{Type: "k8s", Image: "img", K8sNamespace: "ns"},
			wantErr:     true,
			errContains: "executor.network is required for executor.type=\"k8s\"",
		},
		{
			name: "allowlist mode with proxy url is valid",
			exec: ExecutorConfig{
				Type:              "k8s",
				Image:             "img",
				K8sNamespace:      "ns",
				Network:           &NetworkConfig{Mode: "allowlist", Allowlist: []string{"api.example.com"}},
				K8sEgressProxyURL: "http://stirrup-egress-proxy.ns.svc:8080",
			},
		},
		{
			name: "allowlist mode without proxy url rejected",
			exec: ExecutorConfig{
				Type:         "k8s",
				Image:        "img",
				K8sNamespace: "ns",
				Network:      &NetworkConfig{Mode: "allowlist", Allowlist: []string{"api.example.com"}},
			},
			wantErr:     true,
			errContains: "executor.k8sEgressProxyUrl is required",
		},
		{
			name: "proxy url set with none mode rejected",
			exec: ExecutorConfig{
				Type:              "k8s",
				Image:             "img",
				K8sNamespace:      "ns",
				Network:           &NetworkConfig{Mode: "none"},
				K8sEgressProxyURL: "http://stirrup-egress-proxy.ns.svc:8080",
			},
			wantErr:     true,
			errContains: "executor.k8sEgressProxyUrl is only valid",
		},
		{
			// A nil network is the primary error; the egress-proxy cross-field
			// check is skipped (the nil-network error is the signal), so the
			// reported error is the network-required one, not the proxy one.
			name: "proxy url set with no network reports network-required",
			exec: ExecutorConfig{
				Type:              "k8s",
				Image:             "img",
				K8sNamespace:      "ns",
				K8sEgressProxyURL: "http://stirrup-egress-proxy.ns.svc:8080",
			},
			wantErr:     true,
			errContains: "executor.network is required for executor.type=\"k8s\"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Executor = tc.exec
			err := ValidateRunConfig(c)
			if tc.wantErr {
				must.Error(err)
				if tc.errContains != "" {
					must.ErrorContains(err, tc.errContains)
				}
				return
			}
			must.NoError(err)
		})
	}
}

// TestValidateRunConfig_K8sSandboxExecutorFields exercises the type="k8s-sandbox"
// arm, which reuses the K8s* cross-field requirements (Image, K8sNamespace,
// Network, egress-proxy coupling) but narrows the runtime set to gVisor only.
func TestValidateRunConfig_K8sSandboxExecutorFields(t *testing.T) {
	cases := []struct {
		name        string
		exec        ExecutorConfig
		wantErr     bool
		errContains string
	}{
		{
			name: "valid minimal",
			exec: ExecutorConfig{Type: "k8s-sandbox", Image: "img", K8sNamespace: "ns", Network: &NetworkConfig{Mode: "none"}},
		},
		{
			name: "valid with gvisor runtime and optional fields",
			exec: ExecutorConfig{
				Type:              "k8s-sandbox",
				Image:             "img",
				K8sNamespace:      "ns",
				K8sKubeconfig:     "/home/u/.kube/config",
				K8sNodeSelector:   map[string]string{"disktype": "ssd"},
				K8sServiceAccount: "agent-sa",
				Runtime:           "gvisor",
				Network:           &NetworkConfig{Mode: "none"},
			},
		},
		{
			name:        "missing image",
			exec:        ExecutorConfig{Type: "k8s-sandbox", K8sNamespace: "ns", Network: &NetworkConfig{Mode: "none"}},
			wantErr:     true,
			errContains: "executor.image is required for executor.type=\"k8s-sandbox\"",
		},
		{
			name:        "missing namespace",
			exec:        ExecutorConfig{Type: "k8s-sandbox", Image: "img", Network: &NetworkConfig{Mode: "none"}},
			wantErr:     true,
			errContains: "executor.k8sNamespace is required for executor.type=\"k8s-sandbox\"",
		},
		{
			name:        "workspace rejected",
			exec:        ExecutorConfig{Type: "k8s-sandbox", Image: "img", K8sNamespace: "ns", Workspace: "/ws", Network: &NetworkConfig{Mode: "none"}},
			wantErr:     true,
			errContains: "executor.workspace is not valid for executor.type=\"k8s-sandbox\"",
		},
		{
			name:        "nil network rejected",
			exec:        ExecutorConfig{Type: "k8s-sandbox", Image: "img", K8sNamespace: "ns"},
			wantErr:     true,
			errContains: "executor.network is required for executor.type=\"k8s-sandbox\"",
		},
		{
			name: "gvisor runtime accepted",
			exec: ExecutorConfig{Type: "k8s-sandbox", Image: "img", K8sNamespace: "ns", Runtime: "gvisor", Network: &NetworkConfig{Mode: "none"}},
		},
		{
			name:        "runc runtime rejected",
			exec:        ExecutorConfig{Type: "k8s-sandbox", Image: "img", K8sNamespace: "ns", Runtime: "runc", Network: &NetworkConfig{Mode: "none"}},
			wantErr:     true,
			errContains: "executor.runtime must be \"gvisor\" or empty for executor.type=\"k8s-sandbox\"",
		},
		{
			name:        "kata-qemu runtime rejected",
			exec:        ExecutorConfig{Type: "k8s-sandbox", Image: "img", K8sNamespace: "ns", Runtime: "kata-qemu", Network: &NetworkConfig{Mode: "none"}},
			wantErr:     true,
			errContains: "executor.runtime must be \"gvisor\" or empty for executor.type=\"k8s-sandbox\"",
		},
		{
			name: "allowlist mode with proxy url is valid",
			exec: ExecutorConfig{
				Type:              "k8s-sandbox",
				Image:             "img",
				K8sNamespace:      "ns",
				Network:           &NetworkConfig{Mode: "allowlist", Allowlist: []string{"api.example.com"}},
				K8sEgressProxyURL: "http://stirrup-egress-proxy.ns.svc:8080",
			},
		},
		{
			name: "allowlist mode without proxy url rejected",
			exec: ExecutorConfig{
				Type:         "k8s-sandbox",
				Image:        "img",
				K8sNamespace: "ns",
				Network:      &NetworkConfig{Mode: "allowlist", Allowlist: []string{"api.example.com"}},
			},
			wantErr:     true,
			errContains: "executor.k8sEgressProxyUrl is required",
		},
		{
			name: "proxy url set with none mode rejected",
			exec: ExecutorConfig{
				Type:              "k8s-sandbox",
				Image:             "img",
				K8sNamespace:      "ns",
				Network:           &NetworkConfig{Mode: "none"},
				K8sEgressProxyURL: "http://stirrup-egress-proxy.ns.svc:8080",
			},
			wantErr:     true,
			errContains: "executor.k8sEgressProxyUrl is only valid",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Executor = tc.exec
			err := ValidateRunConfig(c)
			if tc.wantErr {
				must.Error(err)
				if tc.errContains != "" {
					must.ErrorContains(err, tc.errContains)
				}
				return
			}
			must.NoError(err)
		})
	}
}

// TestValidateRunConfig_ResourceLimits covers the shared negative-bound
// rejection. A negative limit silently maps to "no limit" in both the
// container and k8s executors, so the validator must reject it on either
// type. The valid cases (nil, zero, positive) must pass through unchanged.
func TestValidateRunConfig_ResourceLimits(t *testing.T) {
	cases := []struct {
		name        string
		execType    string
		resources   *ResourceLimits
		image       string
		k8sNS       string
		wantErr     bool
		errContains string
	}{
		{
			name:      "nil resources valid",
			execType:  "container",
			resources: nil,
			image:     "img",
		},
		{
			name:      "all-zero resources valid",
			execType:  "container",
			resources: &ResourceLimits{},
			image:     "img",
		},
		{
			name:      "positive resources valid",
			execType:  "container",
			resources: &ResourceLimits{CPUs: 2, MemoryMB: 512, DiskMB: 1024, PIDs: 256},
			image:     "img",
		},
		{
			name:        "negative cpus rejected (container)",
			execType:    "container",
			resources:   &ResourceLimits{CPUs: -2},
			image:       "img",
			wantErr:     true,
			errContains: "executor.resources.cpus must not be negative",
		},
		{
			name:        "negative memory rejected (container)",
			execType:    "container",
			resources:   &ResourceLimits{MemoryMB: -512},
			image:       "img",
			wantErr:     true,
			errContains: "executor.resources.memoryMb must not be negative",
		},
		{
			name:        "negative disk rejected (k8s)",
			execType:    "k8s",
			resources:   &ResourceLimits{DiskMB: -1},
			image:       "img",
			k8sNS:       "ns",
			wantErr:     true,
			errContains: "executor.resources.diskMb must not be negative",
		},
		{
			name:        "negative pids rejected (k8s)",
			execType:    "k8s",
			resources:   &ResourceLimits{PIDs: -1},
			image:       "img",
			k8sNS:       "ns",
			wantErr:     true,
			errContains: "executor.resources.pids must not be negative",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Executor = ExecutorConfig{
				Type:         tc.execType,
				Image:        tc.image,
				K8sNamespace: tc.k8sNS,
				Resources:    tc.resources,
			}
			err := ValidateRunConfig(c)
			if tc.wantErr {
				must.Error(err)
				if tc.errContains != "" {
					must.ErrorContains(err, tc.errContains)
				}
				return
			}
			must.NoError(err)
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
			must := require.New(t)
			c := validConfig()
			c.Provider = ProviderConfig{Type: "openai-responses", APIKeyHeader: header}
			err := ValidateRunConfig(c)
			must.Error(err)
			must.ErrorContains(err, "apiKeyHeader")
		})
	}
}

func TestValidateRunConfig_ExecutorRuntimeRejectsUnknown(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Executor = ExecutorConfig{Type: "container", Runtime: "foo"}
	err := ValidateRunConfig(c)
	must.Error(err)
	must.ErrorContains(err, "executor.runtime")
	must.ErrorContains(err, "foo")
}

func TestValidateRunConfig_RegistryAllowlistAcceptsValidGlobs(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Executor = ExecutorConfig{
		Type:              "container",
		Image:             "ghcr.io/stirrup/base:latest",
		RegistryAllowlist: []string{"ghcr.io/stirrup/*", "docker.io/library/*", "registry.internal:5000/team/*"},
	}
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_RegistryAllowlistRejectsBadGlob(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Executor = ExecutorConfig{
		Type:              "container",
		Image:             "ghcr.io/stirrup/base:latest",
		RegistryAllowlist: []string{"ghcr.io/stirrup/["},
	}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "registryAllowlist")
}

func TestValidateRunConfig_RegistryAllowlistRejectsEmptyEntry(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Executor = ExecutorConfig{
		Type:              "container",
		Image:             "ghcr.io/stirrup/base:latest",
		RegistryAllowlist: []string{""},
	}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "registryAllowlist")
}

func TestValidateRunConfig_RegistryAllowlistRequiresContainerExecutor(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Executor = ExecutorConfig{
		Type:              "local",
		RegistryAllowlist: []string{"ghcr.io/stirrup/*"},
	}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "registryAllowlist")
	must.ErrorContains(err, "container")
}

// --- PermissionPolicyConfig.Type policy-engine ---

func TestValidateRunConfig_PolicyEngineRequiresPolicyFile(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.PermissionPolicy = PermissionPolicyConfig{Type: "policy-engine"}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "policyFile")
}

func TestValidateRunConfig_PolicyEngineWithPolicyFilePasses(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.PermissionPolicy = PermissionPolicyConfig{Type: "policy-engine", PolicyFile: "/policies/main.cedar"}
	c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}} // skirt the Rule-of-Two trip
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_FallbackAcceptsNonEngineTypes(t *testing.T) {
	for _, fallback := range []string{"allow-all", "deny-all", "deny-side-effects", "ask-upstream"} {
		t.Run(fallback, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.PermissionPolicy = PermissionPolicyConfig{
				Type:       "policy-engine",
				PolicyFile: "/policies/main.cedar",
				Fallback:   fallback,
			}
			c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
			must.NoError(ValidateRunConfig(c))
		})
	}
}

func TestValidateRunConfig_FallbackRejectsPolicyEngine(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.PermissionPolicy = PermissionPolicyConfig{
		Type:       "policy-engine",
		PolicyFile: "/policies/main.cedar",
		Fallback:   "policy-engine", // chained engines are not allowed
	}
	c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "fallback")
}

func TestValidateRunConfig_FallbackRejectsUnknown(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.PermissionPolicy = PermissionPolicyConfig{
		Type:       "policy-engine",
		PolicyFile: "/policies/main.cedar",
		Fallback:   "lasso",
	}
	c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "fallback")
	must.ErrorContains(err, "lasso")
}

// TestValidateRunConfig_ReadOnlyModeRejectsAllowAllFallback pins that the
// read-only-mode invariant covers the policy-engine's fallback too: an
// empty policy file with fallback allow-all is exactly allow-all, so
// permitting the combination would hollow out the invariant.
func TestValidateRunConfig_ReadOnlyModeRejectsAllowAllFallback(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Mode = "planning"
	c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
	c.PermissionPolicy = PermissionPolicyConfig{
		Type:       "policy-engine",
		PolicyFile: "/policies/main.cedar",
		Fallback:   "allow-all",
	}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "restrictive permission policy")

	// The same config with a deny-all fallback validates.
	c.PermissionPolicy.Fallback = "deny-all"
	must.NoError(ValidateRunConfig(c))
}

// TestValidateRunConfig_PolicyFile_PathTraversalRejected pins that a forged
// policyFile containing ".." is rejected before any os.ReadFile happens, so a
// malicious control plane cannot read host files outside the workspace.
func TestValidateRunConfig_PolicyFile_PathTraversalRejected(t *testing.T) {
	cases := []string{
		"../../etc/passwd",
		"policies/../../etc/passwd",
		"/policies/../../etc/passwd",
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.PermissionPolicy = PermissionPolicyConfig{Type: "policy-engine", PolicyFile: p}
			c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
			err := ValidateRunConfig(c)
			must.ErrorContains(err, "policyFile")
		})
	}
}

// TestValidateRunConfig_PolicyFile_RelativePathAllowed confirms relative paths
// without traversal segments still validate — the shipped example RunConfig uses
// one (examples/policies/destructive-shell.cedar).
func TestValidateRunConfig_PolicyFile_RelativePathAllowed(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.PermissionPolicy = PermissionPolicyConfig{
		Type:       "policy-engine",
		PolicyFile: "examples/policies/destructive-shell.cedar",
	}
	c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
	must.NoError(ValidateRunConfig(c))
}

// TestValidateRunConfig_PolicyFile_IgnoredWithWrongTypeIsError pins that a
// policyFile set with a non-policy-engine type is rejected rather than silently
// dropped, which would leave an operator believing a Cedar policy applies.
func TestValidateRunConfig_PolicyFile_IgnoredWithWrongTypeIsError(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.PermissionPolicy = PermissionPolicyConfig{
		Type:       "deny-side-effects",
		PolicyFile: "/policies/main.cedar",
	}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "policyFile")
	must.ErrorContains(err, "policy-engine")
}

// TestValidateRunConfig_RuntimeRequiresContainerExecutor pins that executor.runtime
// is rejected outside the container executor rather than silently ignored — a
// "local" run with runtime: "runsc" only looks isolated.
func TestValidateRunConfig_RuntimeRequiresContainerExecutor(t *testing.T) {
	cases := []string{"local", "api"}
	for _, execType := range cases {
		t.Run(execType, func(t *testing.T) {
			must := require.New(t)
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
			must.ErrorContains(err, "executor.runtime")
			must.ErrorContains(err, "container")
		})
	}
}

// TestValidateRunConfig_VcsBackendTypeIsClosedSet pins the closed set of forges
// the api executor dispatches to. The type decides which API the executor reads
// from, so an unset or unrecognised value has to fail at config load rather
// than resolve to whichever forge happens to be the default.
func TestValidateRunConfig_VcsBackendTypeIsClosedSet(t *testing.T) {
	tests := []struct {
		name        string
		backendType string
		apiKeyRef   string
		wantErr     string
	}{
		{name: "github", backendType: "github", apiKeyRef: "secret://gh"},
		{name: "gitlab", backendType: "gitlab", apiKeyRef: "secret://gl"},
		{name: "unauthenticated", backendType: "github"},
		{name: "unset", wantErr: "executor.vcsBackend type is required"},
		{name: "unknown", backendType: "bitbucket", wantErr: `unsupported executor.vcsBackend type "bitbucket"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Mode = "research"
			c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
			c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
			c.Executor = ExecutorConfig{
				Type: "api",
				VcsBackend: &VcsBackendConfig{
					Type:      tt.backendType,
					APIKeyRef: tt.apiKeyRef,
					Repo:      "owner/repo",
					Ref:       "main",
				},
			}

			err := ValidateRunConfig(c)
			if tt.wantErr == "" {
				must.NoError(err)
				return
			}
			must.ErrorContains(err, tt.wantErr)
		})
	}
}

// --- CodeScannerConfig ---

func TestValidateRunConfig_CodeScannerAcceptsClosedSet(t *testing.T) {
	for _, scanner := range []string{"none", "patterns", "semgrep"} {
		t.Run(scanner, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.CodeScanner = &CodeScannerConfig{Type: scanner}
			must.NoError(ValidateRunConfig(c))
		})
	}
}

func TestValidateRunConfig_CodeScannerCompositeRequiresScanners(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.CodeScanner = &CodeScannerConfig{Type: "composite"} // no scanners set
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "composite")
	must.ErrorContains(err, "scanners")
}

func TestValidateRunConfig_CodeScannerCompositeAcceptsKnownScanners(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.CodeScanner = &CodeScannerConfig{
		Type:     "composite",
		Scanners: []string{"patterns", "semgrep"},
	}
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_CodeScannerCompositeRejectsRecursive(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.CodeScanner = &CodeScannerConfig{
		Type:     "composite",
		Scanners: []string{"composite"},
	}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "scanners")
}

func TestValidateRunConfig_CodeScannerRejectsUnknown(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.CodeScanner = &CodeScannerConfig{Type: "magic"}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "codeScanner.type")
	must.ErrorContains(err, "magic")
}

func TestValidateRunConfig_CodeScannerDefaultsExecutionToPatterns(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.CodeScanner = nil
	must.NoError(ValidateRunConfig(c))
	must.NotNil(c.CodeScanner, "expected a default CodeScanner for execution mode")
	must.Equal("patterns", c.CodeScanner.Type)
}

func TestValidateRunConfig_CodeScannerDefaultsReadOnlyToNone(t *testing.T) {
	for _, mode := range []string{"planning", "review", "research", "toil"} {
		t.Run(mode, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Mode = mode
			c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
			c.Tools = ToolsConfig{BuiltIn: []string{"read_file", "list_directory"}}
			c.CodeScanner = nil
			must.NoError(ValidateRunConfig(c))
			must.NotNil(c.CodeScanner, "expected a default CodeScanner for read-only mode")
			must.Equal("none", c.CodeScanner.Type)
		})
	}
}

func TestValidateRunConfig_CodeScannerExplicitOverridesDefault(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.CodeScanner = &CodeScannerConfig{Type: "none"} // explicitly opt out
	must.NoError(ValidateRunConfig(c))
	must.Equal("none", c.CodeScanner.Type)
}

// --- EditStrategy default ---

// TestValidateRunConfig_EditStrategyDefaultsToMulti pins that a directly-embedded
// RunConfig with an empty EditStrategy.Type lands on "multi" after validation,
// matching the CLI and gRPC entrypoints.
func TestValidateRunConfig_EditStrategyDefaultsToMulti(t *testing.T) {
	must := require.New(t)
	c := validConfig() // EditStrategy.Type deliberately left empty
	must.NoError(ValidateRunConfig(c))
	must.Equal("multi", c.EditStrategy.Type)
}

// TestValidateRunConfig_EditStrategyExplicitPreserved confirms that an
// operator who selects whole-file, search-replace, or udiff explicitly
// is not silently rewritten to "multi" by the defaulting pass.
func TestValidateRunConfig_EditStrategyExplicitPreserved(t *testing.T) {
	for _, strategy := range []string{"whole-file", "search-replace", "udiff", "multi"} {
		t.Run(strategy, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.EditStrategy = EditStrategyConfig{Type: strategy}
			must.NoError(ValidateRunConfig(c))
			must.Equal(strategy, c.EditStrategy.Type)
		})
	}
}

// TestValidateRunConfig_FuzzyThresholdBounds pins that a fuzzyThreshold outside
// (0, 1] fails validation. At zero or below, the udiff fuzzy fallback accepts
// every candidate and splices at a -1 sentinel index.
func TestValidateRunConfig_FuzzyThresholdBounds(t *testing.T) {
	rejected := []struct {
		name      string
		threshold float64
	}{
		{name: "zero", threshold: 0},
		{name: "negative", threshold: -0.5},
		{name: "above one", threshold: 1.01},
	}
	for _, tt := range rejected {
		t.Run("rejects "+tt.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			threshold := tt.threshold
			c.EditStrategy = EditStrategyConfig{Type: "udiff", FuzzyThreshold: &threshold}
			must.ErrorContains(ValidateRunConfig(c), "fuzzyThreshold")
		})
	}

	// Boundary and mid-range values must validate cleanly. 1.0 is the
	// documented ceiling (exact-only fuzzy matches); 0.80 is the
	// documented default; a value just above 0 is the open lower bound.
	for _, v := range []float64{0.001, 0.5, 0.80, 1.0} {
		t.Run(fmt.Sprintf("accepts %v", v), func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.EditStrategy = EditStrategyConfig{Type: "udiff", FuzzyThreshold: &v}
			must.NoError(ValidateRunConfig(c))
		})
	}

	t.Run("accepts unset", func(t *testing.T) {
		must := require.New(t)
		// An unset threshold falls back to the harness default.
		c := validConfig()
		c.EditStrategy = EditStrategyConfig{Type: "udiff", FuzzyThreshold: nil}
		must.NoError(ValidateRunConfig(c))
	})
}

// --- Rule of Two ---

// boolRef is a tiny helper for the *bool fields that gate a Rule-of-Two
// override. Inlining a takeAddress(false) at every call site would
// pollute the table-driven tests below.
func boolRef(b bool) *bool { return &b }

// ruleOfTwoConfig sets each Rule-of-Two leg independently: untrusted via
// DynamicContext, sensitive via SensitiveData (operational secret refs do not
// set it), and external via a non-"none" network. The tool list is read_file
// only, so no other capability trips.
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
			must := require.New(t)
			c := ruleOfTwoConfig(true, true, true, policy)
			err := ValidateRunConfig(c)
			must.ErrorContains(err, "Rule of Two")
			must.ErrorContains(err, "untrusted-input")
			must.ErrorContains(err, "sensitive-data")
			must.ErrorContains(err, "external-communication")
		})
	}
}

func TestValidateRunConfig_QueryParams_Valid(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Provider = ProviderConfig{
		Type: "openai-responses",
		QueryParams: map[string]string{
			"api-version":   "preview",
			"deployment.id": "gpt4_prod",
			"flag":          "value with spaces are fine in values",
		},
	}
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_QueryParams_RejectsBadKeyChars(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Provider = ProviderConfig{
		Type:        "openai-responses",
		QueryParams: map[string]string{"api version": "preview"},
	}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "queryParams")
}

func TestValidateRunConfig_QueryParams_RejectsCRLFInValue(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Provider = ProviderConfig{
		Type:        "openai-responses",
		QueryParams: map[string]string{"api-version": "preview\r\nset-cookie: foo=bar"},
	}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "CR/LF")
}

func TestValidateRunConfig_QueryParams_RejectsOversize(t *testing.T) {
	must := require.New(t)
	// Build a value just over the 2048-byte cap so the encoded form trips it.
	huge := strings.Repeat("x", 2050)
	c := validConfig()
	c.Provider = ProviderConfig{
		Type:        "openai-responses",
		QueryParams: map[string]string{"k": huge},
	}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "byte cap")
}

func TestValidateRunConfig_QueryParams_RejectsEmptyKey(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Provider = ProviderConfig{
		Type:        "openai-responses",
		QueryParams: map[string]string{"": "value"},
	}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "empty key")
}

// TestValidateRunConfig_NonOpenAIProviderMalformedHeaderRejected pins that shape
// validation applies to every provider: a malformed header on anthropic fails
// even though the runtime ignores it. Forward compatibility means ignored at
// runtime, not unvalidated.
func TestValidateRunConfig_NonOpenAIProviderMalformedHeaderRejected(t *testing.T) {
	must := require.New(t)
	c := validConfig() // Provider.Type == "anthropic"
	c.Provider.APIKeyHeader = "bad: header"
	must.Error(ValidateRunConfig(c))
}

// TestValidateRunConfig_NonOpenAIProviderForwardCompatibleFieldsAccepted pins that
// well-formed APIKeyHeader and QueryParams are accepted on a non-OpenAI provider,
// which ignores them at runtime.
func TestValidateRunConfig_NonOpenAIProviderForwardCompatibleFieldsAccepted(t *testing.T) {
	must := require.New(t)
	c := validConfig() // Provider.Type == "anthropic"
	c.Provider.APIKeyHeader = "x-api-key"
	c.Provider.QueryParams = map[string]string{"hint": "future"}
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_RuleOfTwo_AskUpstreamPasses(t *testing.T) {
	must := require.New(t)
	c := ruleOfTwoConfig(true, true, true, "ask-upstream")
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_RuleOfTwo_OverridePasses(t *testing.T) {
	must := require.New(t)
	c := ruleOfTwoConfig(true, true, true, "deny-side-effects")
	c.RuleOfTwo = &RuleOfTwoConfig{Enforce: boolRef(false)}
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_RuleOfTwo_ExplicitTrueStillEnforces(t *testing.T) {
	must := require.New(t)
	c := ruleOfTwoConfig(true, true, true, "deny-side-effects")
	c.RuleOfTwo = &RuleOfTwoConfig{Enforce: boolRef(true)}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "Rule of Two", "RuleOfTwo.Enforce: true must not bypass the invariant")
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
			must := require.New(t)
			c := ruleOfTwoConfig(tc.untrusted, tc.sensitive, tc.external, "deny-side-effects")
			must.NoError(ValidateRunConfig(c))
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
			must := require.New(t)
			c := ruleOfTwoConfig(tc.untrusted, tc.sensitive, tc.external, "deny-side-effects")
			gotUntrusted, gotSensitive, gotExternal := RuleOfTwoState(c)
			must.Equal(tc.wantUntrusted, gotUntrusted, "untrusted flag")
			must.Equal(tc.wantSensitive, gotSensitive, "sensitive flag")
			must.Equal(tc.wantCanCommExternal, gotExternal, "canCommunicateExternally flag")
		})
	}
}

// TestRuleOfTwoState_NilSafe documents the contract: passing nil
// returns the all-false state rather than panicking. Factory wiring
// relies on this for defensive emission paths.
func TestRuleOfTwoState_NilSafe(t *testing.T) {
	must := require.New(t)
	untrusted, sensitive, external := RuleOfTwoState(nil)
	must.False(untrusted, "untrusted flag for nil config")
	must.False(sensitive, "sensitive flag for nil config")
	must.False(external, "canCommunicateExternally flag for nil config")
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
			must := require.New(t)
			c := ruleOfTwoConfig(tc.untrusted, tc.sensitive, tc.external, "deny-side-effects")
			must.NoError(ValidateRunConfig(c))
		})
	}
}

// TestValidateRunConfig_RuleOfTwo_OperationalSecretRefDoesNotTrigger pins that
// operational secret references (provider/VCS/MCP API keys) do not trip the
// sensitive-data leg: the harness keeps them out of the agent's reach, so they
// aren't "data the agent has access to" in the rule's sense.
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
			must := require.New(t)
			c := &RunConfig{
				Mode:             "execution",
				Provider:         ProviderConfig{Type: "anthropic", APIKeyRef: tc.apiKeyRef},
				MaxTurns:         20,
				Timeout:          &timeout,
				PermissionPolicy: PermissionPolicyConfig{Type: "deny-side-effects"},
				DynamicContext:   map[string]DynamicContextValue{"x": {Value: "y"}}, // untrusted
				Tools:            ToolsConfig{BuiltIn: []string{"web_fetch"}},       // external
			}
			must.NoError(ValidateRunConfig(c), "operational secret reference must not trip the sensitive-data leg")
		})
	}
}

// TestValidateRunConfig_RuleOfTwo_ExplicitSensitiveDataTriggers verifies
// the operator-supplied RunConfig.SensitiveData flag trips the leg.
// Combined with DynamicContext (untrusted) and web_fetch (external),
// this should produce the all-three rejection.
func TestValidateRunConfig_RuleOfTwo_ExplicitSensitiveDataTriggers(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(err, "Rule of Two", "explicit SensitiveData must trip the Rule-of-Two rejection alongside untrusted+external")
}

// TestValidateRunConfig_RuleOfTwo_SensitiveDynamicContextEntryTriggers
// verifies a single per-entry Sensitive flag on a DynamicContext entry
// trips the leg. The motivating use case: a triage agent given a
// customer record block that's marked sensitive while other entries
// (issue body, repo metadata) remain non-sensitive.
func TestValidateRunConfig_RuleOfTwo_SensitiveDynamicContextEntryTriggers(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(err, "Rule of Two", "a single sensitive DynamicContext entry must trip the Rule-of-Two rejection alongside untrusted+external")
}

// TestValidateRunConfig_RuleOfTwo_DefaultIsNotSensitive pins the out-of-the-box
// behavior: with no SensitiveData flag and no sensitive DynamicContext entry, a
// config with untrusted + external input (a bare `stirrup harness --prompt "x"`)
// validates cleanly.
func TestValidateRunConfig_RuleOfTwo_DefaultIsNotSensitive(t *testing.T) {
	must := require.New(t)
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
	must.NoError(ValidateRunConfig(c), "default (no SensitiveData, no sensitive entry) must not trip the leg")
}

// TestValidateRunConfig_RuleOfTwo_ToolListReflectsActualBuiltIn pins that
// tool-enabled checks reflect the actual tools.builtIn list: a run-command-disabled
// config is not flagged as canCommunicateExternally on that leg alone.
func TestValidateRunConfig_RuleOfTwo_ToolListReflectsActualBuiltIn(t *testing.T) {
	must := require.New(t)
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
	must.NoError(ValidateRunConfig(c), "config with two flags only should pass")
}

// --- RuleOfTwoRuntimeConfig ---

func TestValidateRunConfig_RuleOfTwoRuntime_ClassifierClosedSet(t *testing.T) {
	cases := []struct {
		name       string
		classifier string
		wantErr    bool
	}{
		{"empty_is_auto", "", false},
		{"patterns", "patterns", false},
		{"none", "none", false},
		{"unknown", "regex", true},
		{"case_sensitive", "Patterns", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.RuleOfTwo = &RuleOfTwoConfig{Runtime: &RuleOfTwoRuntimeConfig{Classifier: tc.classifier}}
			err := ValidateRunConfig(c)
			if tc.wantErr {
				must.ErrorContains(err, "ruleOfTwo.runtime.classifier")
				return
			}
			must.NoError(err)
		})
	}
}

func TestValidateRunConfig_RuleOfTwoRuntime_OnDetectClosedSet(t *testing.T) {
	cases := []struct {
		name      string
		onDetect  string
		transport string
		wantErr   bool
	}{
		{"empty_defaults_to_block_external", "", "", false},
		{"block_external", "block-external", "", false},
		{"ask_upstream_valid_value", "ask-upstream", "grpc", false}, // cross-field check exercised separately
		{"redact", "redact", "", false},
		{"abort", "abort", "", false},
		{"warn", "warn", "", false},
		{"unknown", "deny", "", true},
		{"case_sensitive", "Warn", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Transport = TransportConfig{Type: tc.transport}
			c.RuleOfTwo = &RuleOfTwoConfig{Runtime: &RuleOfTwoRuntimeConfig{OnDetect: tc.onDetect}}
			err := ValidateRunConfig(c)
			if tc.wantErr {
				must.ErrorContains(err, "ruleOfTwo.runtime.onDetect")
				return
			}
			must.NoError(err)
		})
	}
}

// TestValidateRunConfig_RuleOfTwoRuntime_NoneClassifierCrossField pins
// the dead-config rejection: classifier "none" disables the detector,
// so a non-empty onDetect would be stored and validated but never fire
// — an operator who sets it believing they have a safety net has none.
func TestValidateRunConfig_RuleOfTwoRuntime_NoneClassifierCrossField(t *testing.T) {
	t.Run("none with onDetect rejected", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.RuleOfTwo = &RuleOfTwoConfig{Runtime: &RuleOfTwoRuntimeConfig{Classifier: "none", OnDetect: "abort"}}
		err := ValidateRunConfig(c)
		must.ErrorContains(err, "has no effect")
	})
	t.Run("none with empty onDetect passes", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.RuleOfTwo = &RuleOfTwoConfig{Runtime: &RuleOfTwoRuntimeConfig{Classifier: "none"}}
		must.NoError(ValidateRunConfig(c))
	})
}

// TestValidateRunConfig_RuleOfTwoRuntime_AbortWithStaticSensitiveCrossField pins
// the dead-config rejection: onDetect "abort" fires only on a runtime latch
// transition, but a statically-sensitive run pre-trips the latch before the first
// scan, so the abort would silently never happen.
func TestValidateRunConfig_RuleOfTwoRuntime_AbortWithStaticSensitiveCrossField(t *testing.T) {
	t.Run("abort with sensitiveData rejected", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		// Only the sensitive leg holds (read-only tools, no untrusted
		// ingress) so the static all-three check stays silent and this
		// case isolates the abort cross-field rejection.
		c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
		c.SensitiveData = boolRef(true)
		c.RuleOfTwo = &RuleOfTwoConfig{Runtime: &RuleOfTwoRuntimeConfig{OnDetect: "abort"}}
		err := ValidateRunConfig(c)
		must.ErrorContains(err, "abort")
		must.ErrorContains(err, "sensitiveData")
	})
	t.Run("abort with sensitive dynamicContext entry rejected", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
		c.DynamicContext = map[string]DynamicContextValue{"record": {Value: "x", Sensitive: true}}
		c.RuleOfTwo = &RuleOfTwoConfig{Runtime: &RuleOfTwoRuntimeConfig{OnDetect: "abort"}}
		err := ValidateRunConfig(c)
		must.ErrorContains(err, "abort")
	})
	t.Run("block-external with sensitiveData passes", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
		c.SensitiveData = boolRef(true)
		c.RuleOfTwo = &RuleOfTwoConfig{Runtime: &RuleOfTwoRuntimeConfig{OnDetect: "block-external"}}
		must.NoError(ValidateRunConfig(c))
	})
	t.Run("abort without static sensitivity passes", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
		c.RuleOfTwo = &RuleOfTwoConfig{Runtime: &RuleOfTwoRuntimeConfig{OnDetect: "abort"}}
		must.NoError(ValidateRunConfig(c))
	})
}

// TestValidateRunConfig_RuleOfTwoRuntime_AskUpstreamRequiresGRPC pins the
// cross-field invariant: onDetect "ask-upstream" routes external-comm
// permission requests upstream, which only exists on the gRPC transport.
// A stdio (or defaulted-empty) transport has no control plane to answer
// the request, so the combination must fail at validation time rather
// than as a 60-second timeout per tool call at runtime.
func TestValidateRunConfig_RuleOfTwoRuntime_AskUpstreamRequiresGRPC(t *testing.T) {
	cases := []struct {
		name      string
		transport string
		wantErr   bool
	}{
		{"grpc_passes", "grpc", false},
		{"stdio_fails", "stdio", true},
		{"empty_transport_fails", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Transport = TransportConfig{Type: tc.transport}
			c.RuleOfTwo = &RuleOfTwoConfig{Runtime: &RuleOfTwoRuntimeConfig{OnDetect: "ask-upstream"}}
			err := ValidateRunConfig(c)
			if tc.wantErr {
				must.ErrorContains(err, "requires transport=grpc")
				return
			}
			must.NoError(err)
		})
	}
}

func TestValidateRunConfig_RuleOfTwoRuntime_GuardCriteriaPattern(t *testing.T) {
	cases := []struct {
		name     string
		criteria []string
		wantErr  bool
	}{
		{"snake_case_passes", []string{"sensitive_data", "pii"}, false},
		{"uppercase_fails", []string{"PII"}, true},
		{"leading_digit_fails", []string{"4data"}, true},
		{"hyphen_fails", []string{"sensitive-data"}, true},
		{"empty_entry_fails", []string{""}, true},
		{"mixed_reports_offender", []string{"sensitive_data", "Bad Entry"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.RuleOfTwo = &RuleOfTwoConfig{Runtime: &RuleOfTwoRuntimeConfig{GuardCriteria: tc.criteria}}
			err := ValidateRunConfig(c)
			if tc.wantErr {
				must.ErrorContains(err, "ruleOfTwo.runtime.guardCriteria")
				return
			}
			must.NoError(err)
		})
	}
}

// TestValidateRunConfig_RuleOfTwoRuntime_NilPassthrough pins the
// no-injection invariant: validation must neither reject nor mutate a
// config with a nil RuleOfTwo or nil Runtime. Default arming is factory
// behaviour — an injected Runtime block would alter the
// Redact()-persisted config operators audit against (contrast
// applyCodeScannerDefault, which deliberately does mutate).
func TestValidateRunConfig_RuleOfTwoRuntime_NilPassthrough(t *testing.T) {
	t.Run("nil RuleOfTwo", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		must.NoError(ValidateRunConfig(c))
		must.Nil(c.RuleOfTwo, "validation must not inject a RuleOfTwo block")
	})
	t.Run("nil Runtime", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.RuleOfTwo = &RuleOfTwoConfig{Enforce: boolRef(true)}
		must.NoError(ValidateRunConfig(c))
		must.Nil(c.RuleOfTwo.Runtime, "validation must not inject a Runtime block")
	})
}

func TestValidateRunConfig_RuleOfTwoRuntime_ValidFullBlockPasses(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Transport = TransportConfig{Type: "grpc"}
	c.RuleOfTwo = &RuleOfTwoConfig{
		Enforce: boolRef(true),
		Runtime: &RuleOfTwoRuntimeConfig{
			Classifier:    "patterns",
			OnDetect:      "ask-upstream",
			GuardCriteria: []string{"sensitive_data", "pii"},
		},
	}
	must.NoError(ValidateRunConfig(c))
}

// --- GuardRailConfig ---

// TestValidateRunConfig_GuardRailRules is a table-driven exercise of every
// invariant validateGuardRailConfig enforces. Each case wraps a
// GuardRailConfig in an otherwise-valid RunConfig so the closed-set,
// range, and cross-field checks fire exactly as they would in
// production.
func TestValidateRunConfig_GuardRailRules(t *testing.T) {
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
			guard:   &GuardRailConfig{Type: "cloud-judge", Model: "claude-haiku-5-5"},
			wantErr: false,
		},
		{
			name:    "cloud-judge with https endpoint",
			guard:   &GuardRailConfig{Type: "cloud-judge", Endpoint: "https://api.anthropic.com/v1/messages"},
			wantErr: false,
		},
		{
			name:      "shieldstral without endpoint",
			guard:     &GuardRailConfig{Type: "shieldstral"},
			wantErr:   true,
			errSubstr: "requires endpoint",
		},
		{
			name:      "shieldstral without model",
			guard:     &GuardRailConfig{Type: "shieldstral", Endpoint: "http://127.0.0.1:1234"},
			wantErr:   true,
			errSubstr: "requires model",
		},
		{
			name: "composite with shieldstral stage without model",
			guard: &GuardRailConfig{
				Type: "composite",
				Stages: []GuardRailConfig{
					{Type: "shieldstral", Endpoint: "http://127.0.0.1:1234"},
				},
			},
			wantErr:   true,
			errSubstr: "stages[0].type \"shieldstral\" requires model",
		},
		{
			name:    "shieldstral with endpoint",
			guard:   &GuardRailConfig{Type: "shieldstral", Endpoint: "https://api.mistral.ai/v1/chat/completions", Model: "mistralai/Shieldstral-1.0-3B"},
			wantErr: false,
		},
		{
			name:    "shieldstral with endpoint and valid apiKeyRef",
			guard:   &GuardRailConfig{Type: "shieldstral", Endpoint: "https://api.mistral.ai/v1/chat/completions", Model: "mistralai/Shieldstral-1.0-3B", APIKeyRef: "secret://MISTRAL_API_KEY"},
			wantErr: false,
		},
		{
			name:      "shieldstral with literal apiKeyRef",
			guard:     &GuardRailConfig{Type: "shieldstral", Endpoint: "https://api.mistral.ai/v1/chat/completions", Model: "mistralai/Shieldstral-1.0-3B", APIKeyRef: "sk-literal-key"},
			wantErr:   true,
			errSubstr: "must be a secret reference",
		},
		{
			name: "composite with shieldstral stage",
			guard: &GuardRailConfig{
				Type: "composite",
				Stages: []GuardRailConfig{
					{Type: "shieldstral", Endpoint: "https://api.mistral.ai", Model: "mistralai/Shieldstral-1.0-3B", APIKeyRef: "secret://MISTRAL_API_KEY"},
					{Type: "none"},
				},
			},
			wantErr: false,
		},
		{
			name:      "composite with apiKeyRef on top level",
			guard:     &GuardRailConfig{Type: "composite", APIKeyRef: "secret://KEY", Stages: []GuardRailConfig{{Type: "none"}}},
			wantErr:   true,
			errSubstr: "apiKeyRef is not valid for type=composite",
		},
		{
			name:      "none with apiKeyRef",
			guard:     &GuardRailConfig{Type: "none", APIKeyRef: "secret://KEY"},
			wantErr:   true,
			errSubstr: "apiKeyRef is not valid for type=none",
		},
		{
			name:      "granite-guardian with apiKeyRef",
			guard:     &GuardRailConfig{Type: "granite-guardian", Endpoint: "http://localhost:8000", APIKeyRef: "secret://KEY"},
			wantErr:   true,
			errSubstr: "apiKeyRef is not valid for type=granite-guardian",
		},
		{
			name:      "cloud-judge with apiKeyRef",
			guard:     &GuardRailConfig{Type: "cloud-judge", APIKeyRef: "secret://KEY"},
			wantErr:   true,
			errSubstr: "apiKeyRef is not valid for type=cloud-judge",
		},
		{
			name:      "shieldstral with think",
			guard:     &GuardRailConfig{Type: "shieldstral", Endpoint: "https://api.mistral.ai", Think: &think},
			wantErr:   true,
			errSubstr: "think is not valid for type=shieldstral",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.GuardRail = tc.guard
			err := ValidateRunConfig(c)
			if !tc.wantErr {
				must.NoError(err)
				return
			}
			must.Error(err)
			if tc.errSubstr != "" {
				must.ErrorContains(err, tc.errSubstr)
			}
		})
	}
}

// geminiValidConfig is the smallest RunConfig that exercises a healthy gemini
// provider, used by the negative-path tests below to swap one field at a time.
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
			must := require.New(t)
			c := geminiValidConfig()
			tc.mutate(c)
			err := ValidateRunConfig(c)
			if !tc.wantErr {
				must.NoError(err)
				return
			}
			must.Error(err, "expected error containing %q", tc.errSubstr)
			must.ErrorContains(err, tc.errSubstr)
		})
	}
}

// TestValidateRunConfig_AzureWorkloadIdentityCredentialRules covers the Azure WIF credential
// type's field-level rules (UUID tenant/client, required token source, HTTPS-only
// scope) and the cross-field invariants: apiKeyRef and apiKeyHeader="api-key" are
// mutually exclusive with WIF since the bearer travels on Authorization: Bearer.
func TestValidateRunConfig_AzureWorkloadIdentityCredentialRules(t *testing.T) {
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
			name: "uppercase azureClientId rejected (canonical lowercase only)",
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
			must := require.New(t)
			c := validConfig()
			tc.mutate(c)
			err := ValidateRunConfig(c)
			if !tc.wantErr {
				must.NoError(err)
				return
			}
			must.ErrorContains(err, tc.errSubstr)
		})
	}
}

// TestValidateRunConfig_AzureWIFRejectsAnthropicProvider checks that an
// azure-workload-identity credential cannot pair with the anthropic provider
// type, and that the error names both the credential type and the accepted
// provider types.
func TestValidateRunConfig_AzureWIFRejectsAnthropicProvider(t *testing.T) {
	must := require.New(t)
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
	must.Error(err)
	must.ErrorContains(err, "azure-workload-identity", "error should name the credential type")
	must.ErrorContains(err, "openai-compatible", "error should name the accepted provider type")
}

// TestValidateRunConfig_GeminiModelNameWithSlash pins that a Vertex model name
// containing a slash is rejected at validation time — the adapter's
// url.PathEscape would otherwise let a malformed identifier reach a real but
// unintended Vertex endpoint.
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
			must := require.New(t)
			c := geminiValidConfig()
			tc.mutate(c)
			err := ValidateRunConfig(c)
			must.ErrorContains(err, tc.errSubstr)
		})
	}
}

// TestValidateRunConfig_GeminiModelNameValid pins that an ordinary
// Vertex model identifier passes through cleanly. Catches a regression
// where the new check accidentally rejects all gemini configs.
func TestValidateRunConfig_GeminiModelNameValid(t *testing.T) {
	for _, model := range []string{"gemini-2.5-pro", "gemini-2.0-flash", "gemini-1.5-pro_001"} {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			c := geminiValidConfig()
			c.ModelRouter = ModelRouterConfig{
				Type:     "static",
				Provider: "gemini",
				Model:    model,
			}
			must.NoError(ValidateRunConfig(c))
		})
	}
}

// TestValidateRunConfig_GeminiRejectsClaudeModel pins that an Anthropic
// model name cannot be paired with the gemini provider, for the default
// model and for a per-mode override, and that non-claude names still pass.
func TestValidateRunConfig_GeminiRejectsClaudeModel(t *testing.T) {
	t.Run("default model", func(t *testing.T) {
		must := require.New(t)
		c := geminiValidConfig()
		c.ModelRouter = ModelRouterConfig{Type: "static", Provider: "gemini", Model: "claude-sonnet-4-6"}
		err := ValidateRunConfig(c)
		must.ErrorContains(err, "modelRouter.model")
		must.ErrorContains(err, "Anthropic model name")
	})

	t.Run("per-mode override", func(t *testing.T) {
		must := require.New(t)
		c := geminiValidConfig()
		c.ModelRouter = ModelRouterConfig{
			Type:       "per-mode",
			Provider:   "gemini",
			Model:      "gemini-2.5-pro",
			ModeModels: map[string]string{"planning": "gemini/claude-opus-4-7"},
		}
		err := ValidateRunConfig(c)
		must.ErrorContains(err, "modelRouter.modeModels[planning]")
	})

	t.Run("name merely containing claude passes", func(t *testing.T) {
		must := require.New(t)
		c := geminiValidConfig()
		c.ModelRouter = ModelRouterConfig{Type: "static", Provider: "gemini", Model: "gemini-claude-lookalike"}
		must.NoError(ValidateRunConfig(c))
	})
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
			must := require.New(t)
			c := validConfig()
			tc.mutate(&c.Provider)
			err := ValidateRunConfig(c)
			must.ErrorContains(err, tc.errSubstr)
		})
		t.Run("map/"+tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			named := ProviderConfig{Type: "anthropic"}
			tc.mutate(&named)
			c.Providers = map[string]ProviderConfig{"alt": named}
			err := ValidateRunConfig(c)
			must.ErrorContains(err, tc.errSubstr)
		})
	}
}

// TestValidateRunConfig_GeminiValidationAppliesToProvidersMap pins that the
// gemini validation runs on the Providers map entries too, not just on the
// default Provider.
func TestValidateRunConfig_GeminiValidationAppliesToProvidersMap(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Providers = map[string]ProviderConfig{
		"vertex": {
			Type:        "gemini",
			GCPLocation: "us-central1",
			// missing GCPProject
		},
	}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "providers[vertex].gcpProject")
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
		{ServiceName: "triage-agent"},
		{Environment: "staging-eu", ServiceNamespace: "stirrup_team-a"},
		{Environment: strings.Repeat("a", 64), ServiceNamespace: strings.Repeat("b", 64)},
		{LogsExport: LogsExportConfig{Type: "none"}},
		{LogsExport: LogsExportConfig{Type: "otlp", Endpoint: "localhost:4317"}},
	}
	for _, obs := range cases {
		t.Run(fmt.Sprintf("name=%q env=%q ns=%q", obs.ServiceName, obs.Environment, obs.ServiceNamespace), func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Observability = obs
			must.NoError(ValidateRunConfig(c))
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
		"space in name":       {ServiceName: "triage agent"},
		"too long name":       {ServiceName: strings.Repeat("a", 65)},
		"both fields invalid": {Environment: "x y", ServiceNamespace: "x y"},
	}
	for name, obs := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Observability = obs
			err := ValidateRunConfig(c)
			must.ErrorContains(err, "observability.")
		})
	}
}

// TestValidateRunConfig_LogsExportTypeRejectsUnknown pins the closed-set
// rejection on ObservabilityConfig.LogsExport.Type. A typo'd "loki" or
// "grpc" must surface at config-load time rather than silently falling
// through to stderr-only at factory-build time, which would leave an
// operator wondering why their logs never reached the collector.
func TestValidateRunConfig_LogsExportTypeRejectsUnknown(t *testing.T) {
	for _, bad := range []string{"otel", "loki", "grpc", "stdout"} {
		t.Run(bad, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Observability = ObservabilityConfig{LogsExport: LogsExportConfig{Type: bad}}
			err := ValidateRunConfig(c)
			must.ErrorContains(err, "observability.logsExport.type")
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
	must := require.New(t)
	c := validConfig()
	c.TraceEmitter = TraceEmitterConfig{Type: "otel", Protocol: "http"}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "unsupported traceEmitter.protocol")
}

// TestValidateRunConfig_ProtocolOnNonOTelEmitter pins the cross-field
// invariant: Protocol only has meaning when Type=="otel". Carrying it
// on a jsonl emitter is almost certainly a stale-config artifact and
// must fail loudly. Without this test, dropping the type guard from
// validateTraceEmitterProtocolAndHeaders would go unnoticed.
func TestValidateRunConfig_ProtocolOnNonOTelEmitter(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.TraceEmitter = TraceEmitterConfig{Type: "jsonl", Protocol: "grpc"}
	must.ErrorContains(ValidateRunConfig(c), `only valid when traceEmitter.type is "otel"`)
}

// TestValidateRunConfig_HeadersOnNonOTelEmitter is the headers-side
// counterpart to TestValidateRunConfig_ProtocolOnNonOTelEmitter. The
// jsonl emitter does not send HTTP headers; carrying them must fail
// loudly so an operator does not assume their auth header is being
// honoured when it's silently dropped.
func TestValidateRunConfig_HeadersOnNonOTelEmitter(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.TraceEmitter = TraceEmitterConfig{
		Type:    "jsonl",
		Headers: map[string]string{"X-Tenant": "team-a"},
	}
	must.ErrorContains(ValidateRunConfig(c), "traceEmitter.headers is only valid")
}

// TestValidateRunConfig_CaptureContentOnNonOTelEmitter is the
// captureContent counterpart to TestValidateRunConfig_HeadersOnNonOTelEmitter.
// Only the otel emitter consults captureContent (the jsonl emitter
// already records full transcript content unconditionally); carrying
// the toggle on a non-otel emitter is a stale-config artifact and must
// fail loudly rather than silently meaning nothing.
func TestValidateRunConfig_CaptureContentOnNonOTelEmitter(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.TraceEmitter = TraceEmitterConfig{
		Type:           "jsonl",
		CaptureContent: true,
	}
	must.ErrorContains(ValidateRunConfig(c), "traceEmitter.captureContent is only valid")
}

// TestValidateRunConfig_CaptureContentOnOTelEmitter pins the accept
// case: captureContent on the otel emitter is valid with both wire
// protocols. Without this, an overzealous future rejection (e.g.
// coupling the toggle to a protocol) would go unnoticed.
func TestValidateRunConfig_CaptureContentOnOTelEmitter(t *testing.T) {
	tests := []struct {
		name     string
		protocol string
	}{
		{name: "default protocol", protocol: ""},
		{name: "grpc", protocol: "grpc"},
		{name: "http/protobuf", protocol: "http/protobuf"},
	}
	for _, tc := range tests {
		t.Run("accepts "+tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.TraceEmitter = TraceEmitterConfig{
				Type:           "otel",
				Protocol:       tc.protocol,
				CaptureContent: true,
			}
			must.NoError(ValidateRunConfig(c))
		})
	}
}

// TestValidateRunConfig_TraceEmitterPlatform pins the closed platform set,
// its otel-only scope, and the shape rules on platformOptions.
func TestValidateRunConfig_TraceEmitterPlatform(t *testing.T) {
	valid := map[string]TraceEmitterConfig{
		"no platform": {Type: "otel"},
		"cloudwatch":  {Type: "otel", Platform: "cloudwatch", PlatformOptions: map[string]string{"region": "us-east-1", "logGroup": "/stirrup/agents"}},
		"phoenix":     {Type: "otel", Platform: "phoenix"},
		"langfuse":    {Type: "otel", Protocol: "http/protobuf", Platform: "langfuse", PlatformOptions: map[string]string{"host": "https://langfuse.example.com"}},
		"headers with the platform's required protocol": {Type: "otel", Platform: "langfuse", Headers: map[string]string{"Authorization": "secret://LANGFUSE_AUTH"}},
		"8 options":     {Type: "otel", Platform: "phoenix", PlatformOptions: map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5", "f": "6", "g": "7", "h": "8"}},
		"max value len": {Type: "otel", Platform: "phoenix", PlatformOptions: map[string]string{"project": strings.Repeat("p", 256)}},
		"max key len":   {Type: "otel", Platform: "phoenix", PlatformOptions: map[string]string{"k" + strings.Repeat("A", 31): "v"}},
		"empty value":   {Type: "otel", Platform: "phoenix", PlatformOptions: map[string]string{"project": ""}},
	}
	for name, te := range valid {
		t.Run("accepts "+name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.TraceEmitter = te
			must.NoError(ValidateRunConfig(c))
		})
	}

	invalid := map[string]struct {
		te   TraceEmitterConfig
		want string
	}{
		"unknown platform":           {TraceEmitterConfig{Type: "otel", Platform: "datadog"}, `unsupported traceEmitter.platform "datadog"`},
		"platform on jsonl":          {TraceEmitterConfig{Type: "jsonl", Platform: "phoenix"}, "traceEmitter.platform is only valid when traceEmitter.type is \"otel\""},
		"options on gcs":             {TraceEmitterConfig{Type: "gcs", Bucket: "b-123", Platform: "", PlatformOptions: map[string]string{"project": "p"}}, "traceEmitter.platformOptions is only valid when traceEmitter.type is \"otel\""},
		"options without name":       {TraceEmitterConfig{Type: "otel", PlatformOptions: map[string]string{"project": "p"}}, "traceEmitter.platformOptions requires traceEmitter.platform"},
		"too many options":           {TraceEmitterConfig{Type: "otel", Platform: "phoenix", PlatformOptions: map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5", "f": "6", "g": "7", "h": "8", "i": "9"}}, "has 9 entries (max 8)"},
		"uppercase key start":        {TraceEmitterConfig{Type: "otel", Platform: "phoenix", PlatformOptions: map[string]string{"Project": "p"}}, `key "Project" must match`},
		"key with dash":              {TraceEmitterConfig{Type: "otel", Platform: "phoenix", PlatformOptions: map[string]string{"log-group": "p"}}, `key "log-group" must match`},
		"key too long":               {TraceEmitterConfig{Type: "otel", Platform: "phoenix", PlatformOptions: map[string]string{"k" + strings.Repeat("a", 32): "v"}}, "must match"},
		"value too long":             {TraceEmitterConfig{Type: "otel", Platform: "phoenix", PlatformOptions: map[string]string{"project": strings.Repeat("p", 257)}}, "is 257 bytes (max 256)"},
		"value with LF":              {TraceEmitterConfig{Type: "otel", Platform: "phoenix", PlatformOptions: map[string]string{"project": "a\nb"}}, "must be printable text without CR or LF"},
		"value with CR":              {TraceEmitterConfig{Type: "otel", Platform: "phoenix", PlatformOptions: map[string]string{"project": "a\rb"}}, "must be printable text without CR or LF"},
		"value with control":         {TraceEmitterConfig{Type: "otel", Platform: "phoenix", PlatformOptions: map[string]string{"project": "a\x00b"}}, "must be printable text without CR or LF"},
		"value invalid utf8":         {TraceEmitterConfig{Type: "otel", Platform: "phoenix", PlatformOptions: map[string]string{"project": "a\xffb"}}, "must be printable text without CR or LF"},
		"secret reference value":     {TraceEmitterConfig{Type: "otel", Platform: "phoenix", PlatformOptions: map[string]string{"project": "secret://PROJECT"}}, "must not be a secret:// reference"},
		"grpc with an http platform": {TraceEmitterConfig{Type: "otel", Protocol: "grpc", Platform: "langfuse"}, `traceEmitter.platform "langfuse" requires traceEmitter.protocol http/protobuf (got "grpc")`},
		"headers on a platform without a required protocol": {TraceEmitterConfig{Type: "otel", Platform: "phoenix", Headers: map[string]string{"Authorization": "secret://PHOENIX_AUTH"}}, "traceEmitter.headers requires protocol=http/protobuf"},
	}
	for name, tc := range invalid {
		t.Run("rejects "+name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.TraceEmitter = tc.te
			must.ErrorContains(ValidateRunConfig(c), tc.want)
		})
	}
}

// TestValidateRunConfig_MetricsTemporality pins the closed set of
// traceEmitter.metricsTemporality values and its otel-only scope.
func TestValidateRunConfig_MetricsTemporality(t *testing.T) {
	t.Run("accepts supported values on otel", func(t *testing.T) {
		must := require.New(t)
		for _, temporality := range []string{"", "cumulative", "delta"} {
			c := validConfig()
			c.TraceEmitter = TraceEmitterConfig{Type: "otel", MetricsTemporality: temporality}
			must.NoErrorf(ValidateRunConfig(c), "metricsTemporality %q", temporality)
		}
	})

	t.Run("rejects an unsupported value", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{Type: "otel", MetricsTemporality: "lowmemory"}
		must.ErrorContains(ValidateRunConfig(c), "unsupported traceEmitter.metricsTemporality")
	})

	t.Run("rejects a value on a non-otel emitter", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{Type: "jsonl", MetricsTemporality: "delta"}
		must.ErrorContains(ValidateRunConfig(c), "traceEmitter.metricsTemporality is only valid")
	})
}

// TestValidateRunConfig_OTLPEndpoints pins the endpoint forms accepted on
// every OTLP endpoint field and that rejections never echo the value.
func TestValidateRunConfig_OTLPEndpoints(t *testing.T) {
	set := map[string]func(c *RunConfig, v string){
		"traceEmitter.endpoint":             func(c *RunConfig, v string) { c.TraceEmitter.Endpoint = v },
		"traceEmitter.metricsEndpoint":      func(c *RunConfig, v string) { c.TraceEmitter.MetricsEndpoint = v },
		"observability.logsExport.endpoint": func(c *RunConfig, v string) { c.Observability.LogsExport.Endpoint = v },
	}
	valid := []string{
		"localhost:4317",
		"collector:4318/prefix",
		"http://collector:4318",
		"https://gateway.example/otlp",
		"HTTPS://gateway.example/otlp",
		"https://collector:4318/v1/logs",
	}
	invalid := map[string]string{
		"ftp://collector:4318":                "must use scheme http or https",
		"https://":                            "must include a host",
		"https://user:SEKRIT@collector":       "must not embed user:password credentials",
		"https://collector/otlp?token=SEKRIT": "must not include a query string or fragment",
		"https://collector/otlp#SEKRIT":       "must not include a query string or fragment",
		"http://collector/%zzSEKRIT":          "must be host:port or an http(s) URL",
	}
	for field, apply := range set {
		t.Run(field, func(t *testing.T) {
			for _, v := range valid {
				t.Run("accepts "+v, func(t *testing.T) {
					must := require.New(t)
					c := validConfig()
					c.TraceEmitter.Type = "otel"
					apply(c, v)
					must.NoError(ValidateRunConfig(c))
				})
			}
			for v, want := range invalid {
				t.Run("rejects "+v, func(t *testing.T) {
					must := require.New(t)
					c := validConfig()
					c.TraceEmitter.Type = "otel"
					apply(c, v)
					err := ValidateRunConfig(c)
					must.ErrorContains(err, field+" "+want)
					must.NotContains(err.Error(), "SEKRIT", "error must not echo the value")
				})
			}
		})
	}
}

// TestValidateRunConfig_CaptureContentMaxBytes pins the cap's range and
// its dependency on captureContent.
func TestValidateRunConfig_CaptureContentMaxBytes(t *testing.T) {
	for _, maxBytes := range []int{0, MinCaptureContentMaxBytes, 2048} {
		t.Run(fmt.Sprintf("accepts %d", maxBytes), func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.TraceEmitter = TraceEmitterConfig{Type: "otel", CaptureContent: true, CaptureContentMaxBytes: maxBytes}
			must.NoError(ValidateRunConfig(c))
		})
	}

	for _, maxBytes := range []int{-1, 1, MinCaptureContentMaxBytes - 1} {
		t.Run(fmt.Sprintf("rejects %d", maxBytes), func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.TraceEmitter = TraceEmitterConfig{Type: "otel", CaptureContent: true, CaptureContentMaxBytes: maxBytes}
			must.ErrorContains(ValidateRunConfig(c), "captureContentMaxBytes must be 0 (no cap) or at least")
		})
	}

	t.Run("requires captureContent to be true", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{Type: "otel", CaptureContentMaxBytes: 2048}
		must.ErrorContains(ValidateRunConfig(c), "captureContentMaxBytes is only valid when traceEmitter.captureContent is true")
	})

	// A non-otel emitter names the emitter type as the cause, not
	// captureContent, so fixing the first error does not surface a second.
	for _, emitterType := range []string{"jsonl", "gcs"} {
		t.Run("rejects on "+emitterType+" emitter", func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.TraceEmitter = TraceEmitterConfig{Type: emitterType, Bucket: "trace-bucket", CaptureContentMaxBytes: 2048}
			if emitterType == "jsonl" {
				c.TraceEmitter.Bucket = ""
			}
			err := ValidateRunConfig(c)
			must.ErrorContains(err, `traceEmitter.captureContentMaxBytes is only valid when traceEmitter.type is "otel"`)
			must.NotContains(err.Error(), "captureContent is true")
		})
	}
}

// TestValidateRunConfig_HeadersOnGRPCProtocolRejected pins that the gRPC OTLP
// exporters dial host:port endpoints in plaintext, so a bearer/Basic credential
// in Headers would transmit unencrypted; the validator rejects the combination
// at config-load time. Empty Protocol defaults to gRPC, so the rejection covers both
// `""` and `"grpc"`.
func TestValidateRunConfig_HeadersOnGRPCProtocolRejected(t *testing.T) {
	t.Run("empty protocol with headers rejected", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{
			Type:     "otel",
			Protocol: "",
			Headers:  map[string]string{"Authorization": "Basic abc"},
		}
		must.ErrorContains(ValidateRunConfig(c), "headers requires protocol=http/protobuf")
	})

	t.Run("explicit grpc protocol with headers rejected", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{
			Type:     "otel",
			Protocol: "grpc",
			Headers:  map[string]string{"Authorization": "Bearer abc"},
		}
		must.ErrorContains(ValidateRunConfig(c), "plaintext")
	})

	t.Run("grpc with empty headers accepted", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{Type: "otel", Protocol: "grpc"}
		must.NoError(ValidateRunConfig(c), "gRPC with no headers must remain valid (the local-collector flow)")
	})

	t.Run("http/protobuf with headers accepted", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{
			Type:     "otel",
			Protocol: "http/protobuf",
			Headers:  map[string]string{"Authorization": "Basic xxx"},
		}
		must.NoError(ValidateRunConfig(c), "http/protobuf with headers must remain valid (the Grafana Cloud flow)")
	})
}

// TestValidateRunConfig_TraceEmitterHeaders_CRLFRejected pins that a header name
// or value containing CRLF is rejected at config-load: Go's net/http panics on
// CRLF in header values, so surfacing the misuse here turns a process crash into
// a config error.
func TestValidateRunConfig_TraceEmitterHeaders_CRLFRejected(t *testing.T) {
	t.Run("CR in header name rejected", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{
			Type:     "otel",
			Protocol: "http/protobuf",
			Headers:  map[string]string{"X-Inj\rected": "v"},
		}
		must.ErrorContains(ValidateRunConfig(c), "alphanumeric")
	})

	t.Run("LF in header name rejected", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{
			Type:     "otel",
			Protocol: "http/protobuf",
			Headers:  map[string]string{"X-Inj\nected": "v"},
		}
		must.Error(ValidateRunConfig(c))
	})

	t.Run("CRLF in header value rejected", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{
			Type:     "otel",
			Protocol: "http/protobuf",
			Headers:  map[string]string{"Authorization": "Bearer foo\r\nX-Injected: evil"},
		}
		must.ErrorContains(ValidateRunConfig(c), "must not contain CRLF")
	})

	t.Run("colon in header name rejected", func(t *testing.T) {
		must := require.New(t)
		// Colon is the header separator; allowing it in the name
		// would let an operator concatenate two headers into one
		// validator-passing entry.
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{
			Type:     "otel",
			Protocol: "http/protobuf",
			Headers:  map[string]string{"X-Foo: X-Bar": "v"},
		}
		must.Error(ValidateRunConfig(c))
	})
}

// --- ProviderRetryConfig tests ---

func TestValidateRunConfig_ProviderRetryDefaultsWhenNil(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	c := validConfig()
	c.Provider.Retry = nil
	must.NoError(ValidateRunConfig(c))
	must.NotNil(c.Provider.Retry, "Provider.Retry must be populated after validation")
	got := c.Provider.Retry
	check.Equal(defaultProviderRetryMaxAttempts, got.MaxAttempts, "MaxAttempts")
	check.Equal(defaultProviderRetryInitialDelayMs, got.InitialDelayMs, "InitialDelayMs")
	check.Equal(defaultProviderRetryMaxDelayMs, got.MaxDelayMs, "MaxDelayMs")
	check.Equal(defaultProviderRetryWallClockBudgetMs, got.WallClockBudgetMs, "WallClockBudgetMs")
}

func TestValidateRunConfig_ProviderRetryPartialDefaulting(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	c := validConfig()
	c.Provider.Retry = &ProviderRetryConfig{MaxAttempts: 2}
	must.NoError(ValidateRunConfig(c))
	got := c.Provider.Retry
	check.Equal(2, got.MaxAttempts, "MaxAttempts (caller-supplied)")
	check.Equal(defaultProviderRetryInitialDelayMs, got.InitialDelayMs, "InitialDelayMs (default)")
	check.Equal(defaultProviderRetryMaxDelayMs, got.MaxDelayMs, "MaxDelayMs (default)")
	check.Equal(defaultProviderRetryWallClockBudgetMs, got.WallClockBudgetMs, "WallClockBudgetMs (default)")
}

// TestValidateRunConfig_ProviderRetryZeroMaxAttemptsTreatedAsUnset pins
// that callers who leave MaxAttempts zero (the JSON-omitempty default)
// receive the documented default, not a validation error.
func TestValidateRunConfig_ProviderRetryZeroMaxAttemptsTreatedAsUnset(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Provider.Retry = &ProviderRetryConfig{MaxAttempts: 0}
	must.NoError(ValidateRunConfig(c))
	must.Equal(defaultProviderRetryMaxAttempts, c.Provider.Retry.MaxAttempts)
}

func TestValidateRunConfig_ProviderRetryMaxAttemptsTooHigh(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Provider.Retry = &ProviderRetryConfig{MaxAttempts: maxProviderRetryMaxAttempts + 1}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "provider.retry")
	must.ErrorContains(err, "maxAttempts")
}

func TestValidateRunConfig_ProviderRetryMaxDelayTooHigh(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Provider.Retry = &ProviderRetryConfig{MaxDelayMs: maxProviderRetryMaxDelayMs + 1}
	must.ErrorContains(ValidateRunConfig(c), "maxDelayMs")
}

func TestValidateRunConfig_ProviderRetryWallClockBudgetTooHigh(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Provider.Retry = &ProviderRetryConfig{WallClockBudgetMs: maxProviderRetryWallClockBudgetMs + 1}
	must.ErrorContains(ValidateRunConfig(c), "wallClockBudgetMs")
}

func TestValidateRunConfig_ProviderRetryInitialDelayExceedsMaxDelay(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	// Caller pins InitialDelay > MaxDelay (both inside individual
	// ceilings). The cross-field check is what should reject this.
	c.Provider.Retry = &ProviderRetryConfig{
		InitialDelayMs: 5000,
		MaxDelayMs:     1000,
	}
	must.ErrorContains(ValidateRunConfig(c), "initialDelayMs")
}

// TestValidateRunConfig_ProviderRetryDefaultedInitialDelayAnnotated pins the UX
// for the asymmetric case where the caller sets maxDelayMs but leaves
// initialDelayMs at zero: the defaulter fills initialDelayMs to 500 before the
// cross-field check runs, so the error must annotate it as "(default)" rather
// than naming a value the caller never wrote.
func TestValidateRunConfig_ProviderRetryDefaultedInitialDelayAnnotated(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Provider.Retry = &ProviderRetryConfig{
		MaxDelayMs: 100,
	}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "initialDelayMs")
	must.ErrorContains(err, "default")
	must.ErrorContains(err, "500")
	// The caller-supplied maxDelayMs must render without a "default" annotation.
	must.ErrorContains(err, "maxDelayMs (100)")
}

func TestValidateRunConfig_ProviderRetryWallClockBudgetBelowMaxDelay(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	// WallClockBudget below MaxDelay would not give a single attempt
	// room to consume its backoff; reject at validation time.
	c.Provider.Retry = &ProviderRetryConfig{
		MaxDelayMs:        20000,
		WallClockBudgetMs: 10000,
	}
	must.ErrorContains(ValidateRunConfig(c), "wallClockBudgetMs")
}

// TestValidateRunConfig_ProviderRetryNegativeMaxAttempts pins that a negative
// MaxAttempts is rejected: the defaulter only fills on `== 0`, so a negative
// value reaches the validator unchanged.
func TestValidateRunConfig_ProviderRetryNegativeMaxAttempts(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Provider.Retry = &ProviderRetryConfig{MaxAttempts: -1}
	must.ErrorContains(ValidateRunConfig(c), "maxAttempts")
}

// TestValidateRunConfig_ProviderRetryNegativeInitialDelay pins the
// `cfg.InitialDelayMs < 0` branch: the defaulter only fills the field when it is
// exactly zero, so an explicit -1 passes through to validation.
func TestValidateRunConfig_ProviderRetryNegativeInitialDelay(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Provider.Retry = &ProviderRetryConfig{InitialDelayMs: -1}
	must.ErrorContains(ValidateRunConfig(c), "initialDelayMs")
}

// TestValidateRunConfig_ProviderRetryNegativeMaxDelay pins the range
// check `cfg.MaxDelayMs <= 0` against negative input. See the docstring
// on the MaxAttempts test above for context.
func TestValidateRunConfig_ProviderRetryNegativeMaxDelay(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Provider.Retry = &ProviderRetryConfig{MaxDelayMs: -1}
	must.ErrorContains(ValidateRunConfig(c), "maxDelayMs")
}

// TestValidateRunConfig_ProviderRetryNegativeWallClockBudget pins the
// range check `cfg.WallClockBudgetMs <= 0` against negative input. See
// the docstring on the MaxAttempts test above for context.
func TestValidateRunConfig_ProviderRetryNegativeWallClockBudget(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Provider.Retry = &ProviderRetryConfig{WallClockBudgetMs: -1}
	must.ErrorContains(ValidateRunConfig(c), "wallClockBudgetMs")
}

// TestValidateRunConfig_ProviderRetryWallClockBudgetEqualsMaxDelay pins the
// strict-less-than boundary: equality is intentionally valid, since a single
// attempt may consume the entire wall-clock budget on its backoff.
func TestValidateRunConfig_ProviderRetryWallClockBudgetEqualsMaxDelay(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Provider.Retry = &ProviderRetryConfig{
		MaxDelayMs:        16000,
		WallClockBudgetMs: 16000,
	}
	must.NoError(ValidateRunConfig(c))
}

// TestValidateProviderRetryConfig_NilIsNoop exercises the `cfg == nil` guard
// directly. The public ValidateRunConfig path always runs
// applyProviderRetryDefaults first, which allocates a non-nil config, so the nil
// guard is structurally unreachable through the public API.
func TestValidateProviderRetryConfig_NilIsNoop(t *testing.T) {
	must := require.New(t)
	var errs []string
	validateProviderRetryConfig("provider.retry", nil, providerRetryDefaulted{}, &errs)
	must.Empty(errs, "nil ProviderRetryConfig must not add errors")
}

// TestValidateRunConfig_ProviderRetryNamedProviderRejected pins the
// "providers[<name>].retry" path string used in the named-provider rejection
// error, so a refactor of the format string can't silently regress it.
func TestValidateRunConfig_ProviderRetryNamedProviderRejected(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(err, "providers[secondary].retry")
	must.ErrorContains(err, "maxAttempts")
}

// TestValidateRunConfig_ProviderRetryNamedProviderNilRetryBlock pins the
// nil-allocation branch of defaultProviderRetry for a Providers map entry,
// distinct from the partial-block "fill missing fields" branches covered
// elsewhere.
func TestValidateRunConfig_ProviderRetryNamedProviderNilRetryBlock(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	c := validConfig()
	c.Providers = map[string]ProviderConfig{
		"secondary": {
			Type:      "openai-compatible",
			BaseURL:   "https://example.test/v1",
			APIKeyRef: "secret://SECONDARY_KEY",
			Retry:     nil,
		},
	}
	must.NoError(ValidateRunConfig(c))
	got := c.Providers["secondary"].Retry
	must.NotNil(got, "named-provider Retry must be allocated from nil")
	check.Equal(defaultProviderRetryMaxAttempts, got.MaxAttempts, "named-provider MaxAttempts (default)")
	check.Equal(defaultProviderRetryInitialDelayMs, got.InitialDelayMs, "named-provider InitialDelayMs (default)")
	check.Equal(defaultProviderRetryMaxDelayMs, got.MaxDelayMs, "named-provider MaxDelayMs (default)")
	check.Equal(defaultProviderRetryWallClockBudgetMs, got.WallClockBudgetMs, "named-provider WallClockBudgetMs (default)")
}

func TestValidateRunConfig_ProviderRetryNamedProviderDefaultsIndependently(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Providers = map[string]ProviderConfig{
		"secondary": {
			Type:      "openai-compatible",
			BaseURL:   "https://example.test/v1",
			APIKeyRef: "secret://SECONDARY_KEY",
			Retry:     &ProviderRetryConfig{MaxAttempts: 4},
		},
	}
	must.NoError(ValidateRunConfig(c))
	got := c.Providers["secondary"].Retry
	must.NotNil(got, "secondary provider Retry should be populated")
	must.Equal(4, got.MaxAttempts, "caller-supplied MaxAttempts")
	must.Equal(defaultProviderRetryInitialDelayMs, got.InitialDelayMs, "unset InitialDelayMs inherits the default")
	must.Equal(defaultProviderRetryMaxDelayMs, got.MaxDelayMs, "unset MaxDelayMs inherits the default")
	must.Equal(defaultProviderRetryWallClockBudgetMs, got.WallClockBudgetMs, "unset WallClockBudgetMs inherits the default")

	// Top-level provider continues to default independently.
	must.NotNil(c.Provider.Retry)
	must.Equal(defaultProviderRetryMaxAttempts, c.Provider.Retry.MaxAttempts, "top-level MaxAttempts defaults independently")
}

// --- traceEmitter type=gcs ---

func TestValidateRunConfig_TraceEmitterGCS_Valid(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.TraceEmitter = TraceEmitterConfig{
		Type:         "gcs",
		Bucket:       "stirrup-results",
		ObjectPrefix: "traces/",
		Credential:   &CredentialConfig{Type: "gcp-workload-identity"},
	}
	must.NoError(ValidateRunConfig(c))
}

func TestCommandOutput_DefaultsAndHardCaps(t *testing.T) {
	must := require.New(t)
	got := (ToolsConfig{}).EffectiveCommandOutput()
	must.EqualValues(32<<10, got.InlineMaxBytes, "inline max bytes default")
	must.EqualValues(4<<10, got.PreviewBytesPerStream, "preview bytes per stream default")
	must.EqualValues(50<<20, got.MaxBytesPerStream, "max bytes per stream default")
	must.EqualValues(500<<20, got.MaxBytesPerRun, "max bytes per run default")

	c := validConfig()
	c.Tools.CommandOutput.MaxBytesPerStream = MaxCommandOutputBytesPerStream + 1
	must.ErrorContains(ValidateRunConfig(c), "maxBytesPerStream")
}

func TestCommandOutput_CaptureEnabled(t *testing.T) {
	on, off := true, false

	t.Run("unset enabled defaults to capture on", func(t *testing.T) {
		must := require.New(t)
		must.True((ToolsConfig{}).CommandOutputCaptureEnabled())
	})
	t.Run("explicit true enables capture", func(t *testing.T) {
		must := require.New(t)
		must.True((ToolsConfig{CommandOutput: CommandOutputConfig{Enabled: &on}}).CommandOutputCaptureEnabled())
	})
	t.Run("explicit false disables capture", func(t *testing.T) {
		must := require.New(t)
		must.False((ToolsConfig{CommandOutput: CommandOutputConfig{Enabled: &off}}).CommandOutputCaptureEnabled())
	})
	t.Run("reader without capture is rejected", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Tools.BuiltIn = []string{"run_command", "read_command_output"}
		c.Tools.CommandOutput.Enabled = &off
		must.ErrorContains(ValidateRunConfig(c), "read_command_output")
	})
	t.Run("reader with capture validates", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Tools.BuiltIn = []string{"run_command", "read_command_output"}
		c.Tools.CommandOutput.Enabled = &on
		must.NoError(ValidateRunConfig(c))
	})
}

func TestCommandOutput_FailurePostureValidation(t *testing.T) {
	must := require.New(t)
	must.Equal(CommandOutputPostureStrict, (ToolsConfig{}).EffectiveCommandOutput().FailurePosture, "default posture")

	for _, valid := range []string{"", CommandOutputPostureStrict, CommandOutputPostureBestEffort} {
		c := validConfig()
		c.Tools.CommandOutput.FailurePosture = valid
		must.NoError(ValidateRunConfig(c), "posture %q should validate", valid)
	}

	c := validConfig()
	c.Tools.CommandOutput.FailurePosture = "lenient"
	must.ErrorContains(ValidateRunConfig(c), "failurePosture")
}

func TestValidateRunConfig_TraceArchive_RequiresBucketAndKeepsPrefix(t *testing.T) {
	t.Run("local archive validates", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.TraceEmitter.Archive = &TraceArchiveConfig{Type: "local", FilePath: "/tmp/run.command-output.tar.gz"}
		must.NoError(ValidateRunConfig(c))
	})
	t.Run("gcs archive requires bucket", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.TraceEmitter.Archive = &TraceArchiveConfig{Type: "gcs"}
		must.ErrorContains(ValidateRunConfig(c), "archive.bucket")
	})
	t.Run("gcs archive keeps objectPrefix", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.TraceEmitter.Archive = &TraceArchiveConfig{Type: "gcs", Bucket: "stirrup-archives", ObjectPrefix: "runs"}
		must.NoError(ValidateRunConfig(c))
		// The uploader owns slash normalisation, so validation must not rewrite the prefix.
		must.Equal("runs", c.TraceEmitter.Archive.ObjectPrefix)
	})
}

func TestValidateRunConfig_TraceEmitterGCS_BucketRequired(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.TraceEmitter = TraceEmitterConfig{Type: "gcs"}
	must.ErrorContains(ValidateRunConfig(c), "requires bucket")
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
			must := require.New(t)
			c := validConfig()
			c.TraceEmitter = TraceEmitterConfig{Type: "gcs", Bucket: tc.bucket}
			must.ErrorContains(ValidateRunConfig(c), "traceEmitter.bucket")
		})
	}
}

// TestValidateRunConfig_TraceEmitter_ObjectPrefixDotDotRejected pins that
// traceEmitter.objectPrefix rejects ".." segments so it cannot rewrite the
// produced GCS object path.
func TestValidateRunConfig_TraceEmitter_ObjectPrefixDotDotRejected(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
	}{
		{"leading dot-dot segment", "../escape/"},
		{"dot-dot segment in the middle", "traces/../escape/"},
		{"multiple dot-dot segments", "../../prod-traces/"},
		{"bare dot-dot", ".."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.TraceEmitter = TraceEmitterConfig{
				Type:         "gcs",
				Bucket:       "stirrup-results",
				ObjectPrefix: tc.prefix,
			}
			must.ErrorContains(ValidateRunConfig(c), `must not contain ".." path segments`)
		})
	}
}

// TestValidateRunConfig_TraceEmitter_ObjectPrefixTrailingSlashNormalised pins
// that a missing trailing slash on objectPrefix is normalised in place so
// gcsObjectName produces a well-formed object path.
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
			must := require.New(t)
			c := validConfig()
			c.TraceEmitter = TraceEmitterConfig{
				Type:         "gcs",
				Bucket:       "stirrup-results",
				ObjectPrefix: tc.in,
			}
			must.NoError(ValidateRunConfig(c))
			must.Equal(tc.want, c.TraceEmitter.ObjectPrefix)
		})
	}
}

// TestValidateRunConfig_RunID_PatternEnforced pins that RunID, interpolated
// verbatim into the gcs trace emitter object name, rejects any slash, control
// byte, or path-traversal segment at config-load time.
func TestValidateRunConfig_RunID_PatternEnforced(t *testing.T) {
	t.Run("slash rejected", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.RunID = "tenant-a/run-1"
		must.ErrorContains(ValidateRunConfig(c), "runId")
	})
	t.Run("dotdot rejected", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.RunID = ".."
		must.Error(ValidateRunConfig(c))
	})
	t.Run("empty allowed", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.RunID = ""
		must.NoError(ValidateRunConfig(c))
	})
	t.Run("uuid accepted", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.RunID = "0ff0-4d1b-9c4e-1234567890ab"
		must.NoError(ValidateRunConfig(c))
	})
}

func TestValidateRunConfig_TraceEmitterGCS_FieldsRejectedOnNonGCS(t *testing.T) {
	t.Run("bucket on jsonl", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{Type: "jsonl", Bucket: "leftover"}
		must.ErrorContains(ValidateRunConfig(c), "traceEmitter.bucket is only valid")
	})
	t.Run("objectPrefix on otel", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{Type: "otel", ObjectPrefix: "traces/"}
		must.ErrorContains(ValidateRunConfig(c), "traceEmitter.objectPrefix is only valid")
	})
	t.Run("credential on jsonl", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.TraceEmitter = TraceEmitterConfig{
			Type:       "jsonl",
			Credential: &CredentialConfig{Type: "gcp-workload-identity"},
		}
		must.ErrorContains(ValidateRunConfig(c), "traceEmitter.credential is only valid")
	})
}

// --- resultSink ---

func TestValidateRunConfig_ResultSink_NoneAndStdoutJSON(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.ResultSink = &ResultSinkConfig{Type: "none"}
		must.NoError(ValidateRunConfig(c))
	})
	t.Run("stdout-json", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.ResultSink = &ResultSinkConfig{Type: "stdout-json"}
		must.NoError(ValidateRunConfig(c))
	})
	t.Run("nil sink ok", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.ResultSink = nil
		must.NoError(ValidateRunConfig(c))
	})
}

func TestValidateRunConfig_ResultSink_GCPPubsubReserved(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.ResultSink = &ResultSinkConfig{Type: "gcp-pubsub", Topic: "stirrup-results"}
	must.ErrorContains(ValidateRunConfig(c), "reserved but not yet implemented")
}

// TestValidateRunConfig_ResultSink_BarePubsubRejected pins that the bare
// "pubsub" string is not a valid resultSink type; only "gcp-pubsub" names the
// Pub/Sub adapter.
func TestValidateRunConfig_ResultSink_BarePubsubRejected(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.ResultSink = &ResultSinkConfig{Type: "pubsub"}
	must.ErrorContains(ValidateRunConfig(c), "unsupported resultSink type")
}

// TestValidateRunConfig_ResultSink_TopicRejectedForNonPubSub pins that
// resultSink.topic is meaningful only for the gcp-pubsub adapter; carrying it on
// a stdout-json sink fails loudly rather than being silently ignored.
func TestValidateRunConfig_ResultSink_TopicRejectedForNonPubSub(t *testing.T) {
	t.Run("topic on stdout-json", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.ResultSink = &ResultSinkConfig{Type: "stdout-json", Topic: "leftover"}
		must.ErrorContains(ValidateRunConfig(c), "resultSink.topic is only valid")
	})
	t.Run("attributes on stdout-json", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.ResultSink = &ResultSinkConfig{
			Type:       "stdout-json",
			Attributes: map[string]string{"env": "prod"},
		}
		must.ErrorContains(ValidateRunConfig(c), "resultSink.attributes is only valid")
	})
}

func TestValidateRunConfig_ResultSink_GCSReserved(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.ResultSink = &ResultSinkConfig{Type: "gcs"}
	must.ErrorContains(ValidateRunConfig(c), "reserved but not yet implemented")
}

func TestValidateRunConfig_ResultSink_InvalidType(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.ResultSink = &ResultSinkConfig{Type: "carrier-pigeon"}
	must.ErrorContains(ValidateRunConfig(c), "unsupported resultSink type")
}

// TestValidateRunConfig_ResultSink_MaxFinalAssistantTextBytes pins that zero (the
// "use the default" sentinel) and any positive override pass, a negative value
// is rejected, and — unlike Topic/Attributes — the field is accepted on every
// implemented sink type since it bounds the RunResult field itself.
func TestValidateRunConfig_ResultSink_MaxFinalAssistantTextBytes(t *testing.T) {
	t.Run("zero is the default sentinel and passes", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.ResultSink = &ResultSinkConfig{Type: "stdout-json", MaxFinalAssistantTextBytes: 0}
		must.NoError(ValidateRunConfig(c))
	})
	t.Run("positive override passes", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.ResultSink = &ResultSinkConfig{Type: "stdout-json", MaxFinalAssistantTextBytes: 4096}
		must.NoError(ValidateRunConfig(c))
	})
	t.Run("negative is rejected", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.ResultSink = &ResultSinkConfig{Type: "stdout-json", MaxFinalAssistantTextBytes: -1}
		must.ErrorContains(ValidateRunConfig(c), "resultSink.maxFinalAssistantTextBytes must be non-negative")
	})
	t.Run("positive override passes on type=none too", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.ResultSink = &ResultSinkConfig{Type: "none", MaxFinalAssistantTextBytes: 4096}
		must.NoError(ValidateRunConfig(c))
	})
}

// --- executor.workspaceExportTo ---

func TestValidateRunConfig_WorkspaceExportTo_ValidGS(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Executor = ExecutorConfig{
		Type:              "local",
		WorkspaceExportTo: "gs://stirrup-results/runs/run-1/workspace.tar.gz",
	}
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_WorkspaceExportTo_RejectsNonGSScheme(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Executor = ExecutorConfig{
		Type:              "local",
		WorkspaceExportTo: "https://example.com/results.tar.gz",
	}
	must.ErrorContains(ValidateRunConfig(c), "gs://")
}

func TestValidateRunConfig_WorkspaceExportTo_RejectsEmptyBucketPath(t *testing.T) {
	cases := []struct {
		name string
		uri  string
	}{
		{"bare scheme", "gs://"},
		{"empty bucket with object", "gs:///object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Executor = ExecutorConfig{Type: "local", WorkspaceExportTo: tc.uri}
			must.ErrorContains(ValidateRunConfig(c), "non-empty bucket path")
		})
	}
}

func TestValidateRunConfig_WorkspaceExportTo_RejectsAPIExecutor(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Executor = ExecutorConfig{
		Type:              "api",
		WorkspaceExportTo: "gs://bucket/path",
	}
	// The api executor needs a VcsBackend to validate; supply one
	// so the test isolates the workspaceExportTo failure.
	c.Executor.VcsBackend = &VcsBackendConfig{Type: "github", Repo: "owner/repo", Ref: "main"}
	must.ErrorContains(ValidateRunConfig(c), `not valid for executor.type="api"`)
}

// TestValidateExecutorWorkspaceExportTo_ArchiverTable pins that the export
// destination is accepted exactly for the executor types that can archive
// their workspace, so a new executor type cannot silently pass validation
// and then export nothing.
func TestValidateExecutorWorkspaceExportTo_ArchiverTable(t *testing.T) {
	for typ := range validExecutorTypes {
		t.Run(typ, func(t *testing.T) {
			must := require.New(t)
			must.Contains(executorWorkspaceArchiver, typ, "executor type needs an executorWorkspaceArchiver entry")

			var errs []string
			validateExecutorWorkspaceExportTo(ExecutorConfig{Type: typ, WorkspaceExportTo: "gs://bucket/path"}, &errs)
			if ExecutorCanArchiveWorkspace(typ) {
				must.Empty(errs, "archiver executor should accept workspaceExportTo")
				return
			}
			must.Len(errs, 1, "non-archiver executor should get one rejection")
			must.Contains(errs[0], "cannot archive its workspace")
		})
	}
	for _, typ := range []string{"local", "container", "k8s", "k8s-sandbox"} {
		t.Run("archives "+typ, func(t *testing.T) {
			must := require.New(t)
			must.True(ExecutorCanArchiveWorkspace(typ))
		})
	}
}

func TestValidateRunConfig_WorkspaceExportTo_RequiresExplicitExecutorType(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Executor = ExecutorConfig{WorkspaceExportTo: "gs://bucket/path"}
	must.ErrorContains(ValidateRunConfig(c), "requires an explicit executor.type")
}

func TestValidateRunConfig_WorkspaceExportRequired_RejectedWithoutDestination(t *testing.T) {
	for _, v := range []bool{true, false} {
		t.Run(fmt.Sprintf("required %t", v), func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Executor = ExecutorConfig{Type: "local", WorkspaceExportRequired: &v}
			must.ErrorContains(ValidateRunConfig(c), "executor.workspaceExportRequired requires executor.workspaceExportTo")
		})
	}
}

func TestValidateRunConfig_WorkspaceExportRequired_AcceptedWithDestination(t *testing.T) {
	for _, v := range []bool{true, false} {
		t.Run(fmt.Sprintf("required %t", v), func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Executor = ExecutorConfig{Type: "local", WorkspaceExportTo: "gs://bucket/path", WorkspaceExportRequired: &v}
			must.NoError(ValidateRunConfig(c))
		})
	}
}

func TestExecutorConfig_ExportRequired(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name string
		cfg  ExecutorConfig
		want bool
	}{
		{"no destination", ExecutorConfig{}, false},
		{"destination defaults to required", ExecutorConfig{WorkspaceExportTo: "gs://b/p"}, true},
		{"explicit true", ExecutorConfig{WorkspaceExportTo: "gs://b/p", WorkspaceExportRequired: &yes}, true},
		{"explicit false", ExecutorConfig{WorkspaceExportTo: "gs://b/p", WorkspaceExportRequired: &no}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, tc.cfg.ExportRequired())
		})
	}
}

func TestExecutorConfig_WorkspaceExportRequired_JSONTriState(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want *bool
	}{
		{"omitted stays nil", `{"type":"local"}`, nil},
		{"false is kept", `{"type":"local","workspaceExportRequired":false}`, new(bool)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var ec ExecutorConfig
			must.NoError(json.Unmarshal([]byte(tc.doc), &ec))
			if tc.want == nil {
				must.Nil(ec.WorkspaceExportRequired)
				return
			}
			must.NotNil(ec.WorkspaceExportRequired)
			must.Equal(*tc.want, *ec.WorkspaceExportRequired)
		})
	}
}

// --- Redact() coverage for resultSink.attributes ---

func TestRedact_GuardRailAPIKeyRef(t *testing.T) {
	rc := RunConfig{
		GuardRail: &GuardRailConfig{
			Type:      "composite",
			Endpoint:  "",
			APIKeyRef: "",
			Stages: []GuardRailConfig{
				{
					Type:      "shieldstral",
					Endpoint:  "https://api.mistral.ai",
					APIKeyRef: "secret://MISTRAL_API_KEY",
				},
				{
					Type:     "granite-guardian",
					Endpoint: "http://localhost:8000",
				},
			},
		},
	}
	t.Run("composite stage keys are redacted", func(t *testing.T) {
		must := require.New(t)
		redacted := rc.Redact()
		must.NotNil(redacted.GuardRail, "GuardRail should not be nil after redact")
		must.Len(redacted.GuardRail.Stages, 2)
		must.Equal("secret://[REDACTED]", redacted.GuardRail.Stages[0].APIKeyRef)
		must.Equal("secret://MISTRAL_API_KEY", rc.GuardRail.Stages[0].APIKeyRef, "Redact must not mutate the original")
	})

	t.Run("top-level guard key is redacted", func(t *testing.T) {
		must := require.New(t)
		singleRC := RunConfig{
			GuardRail: &GuardRailConfig{
				Type:      "shieldstral",
				Endpoint:  "https://api.mistral.ai",
				APIKeyRef: "secret://MISTRAL_KEY",
			},
		}
		redacted := singleRC.Redact()
		must.Equal("secret://[REDACTED]", redacted.GuardRail.APIKeyRef)
		must.Equal("secret://MISTRAL_KEY", singleRC.GuardRail.APIKeyRef, "Redact must not mutate the original")
	})
}

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

	t.Run("secret attributes are redacted", func(t *testing.T) {
		must := require.New(t)
		must.Equal("secret://[REDACTED]", redacted.ResultSink.Attributes["auth-token"])
		must.Equal("secret://[REDACTED]", redacted.ResultSink.Attributes["another-secret"])
	})
	t.Run("plaintext attributes are kept", func(t *testing.T) {
		must := require.New(t)
		must.Equal("classification", redacted.ResultSink.Attributes["workload"])
	})
	t.Run("original config is not mutated", func(t *testing.T) {
		must := require.New(t)
		must.Equal("secret://PUBSUB_TOKEN", rc.ResultSink.Attributes["auth-token"])
	})
}

// TestValidateRunConfig_BedrockModelIDShape pins the fail-fast guard: the CLI's
// default --model is an Anthropic-API alias that Bedrock rejects only after
// IAM/SigV4 setup and a network round-trip, so the validator catches the shape
// before any provider call.
func TestValidateRunConfig_BedrockModelIDShape(t *testing.T) {
	bedrockWithModel := func(model string) *RunConfig {
		c := validConfig()
		c.Provider = ProviderConfig{
			Type:       "bedrock",
			Region:     "us-east-1",
			Credential: &CredentialConfig{Type: "aws-default"},
		}
		c.ModelRouter = ModelRouterConfig{Type: "static", Provider: "bedrock", Model: model}
		return c
	}

	t.Run("valid_model_ids", func(t *testing.T) {
		cases := []string{
			"anthropic.claude-sonnet-4-6",
			"eu.anthropic.claude-sonnet-4-6",
			"global.anthropic.claude-sonnet-4-6",
			"anthropic.claude-sonnet-4-5-20250929-v1:0",
			"arn:aws:bedrock:eu-west-1:123456789012:inference-profile/eu.anthropic.claude-sonnet-4-6",
			"arn:",                         // bare arn: prefix — deliberately permissive; Bedrock will reject at API layer
			"meta.llama3-8b-instruct-v1:0", // non-Anthropic vendor — dot rule is vendor-agnostic
		}
		for _, model := range cases {
			t.Run(model, func(t *testing.T) {
				must := require.New(t)
				must.NoError(ValidateRunConfig(bedrockWithModel(model)))
			})
		}
	})

	t.Run("invalid_anthropic_alias_shapes", func(t *testing.T) {
		cases := []string{
			"claude-sonnet-4-6",
			"claude-opus-4-1",
			"gpt-4",
			" ", // whitespace-only — never a valid bedrock id
		}
		for _, model := range cases {
			t.Run(model, func(t *testing.T) {
				must := require.New(t)
				err := ValidateRunConfig(bedrockWithModel(model))
				must.ErrorContains(err, "bedrock")
				// Several anchors so the wording can change without losing the remediation hint.
				msg := err.Error()
				must.True(strings.Contains(msg, "inference-profile") ||
					strings.Contains(msg, "inference profile") ||
					strings.Contains(msg, "list-inference-profiles"),
					"error should point at the inference-profile remediation path, got: %v", err)
			})
		}
	})

	t.Run("empty_model_defers_to_required_field_handling", func(t *testing.T) {
		must := require.New(t)
		// An empty model is left to the router's required-field checks;
		// the bedrock shape check must not add its own error.
		err := ValidateRunConfig(bedrockWithModel(""))
		if err != nil {
			must.NotContains(err.Error(), "Bedrock requires either")
		}
	})

	t.Run("anthropic_provider_with_alias_still_valid", func(t *testing.T) {
		must := require.New(t)
		// The bedrock-scoped check must not bleed into the anthropic path.
		// "claude-sonnet-4-6" is the CLI default and the canonical Anthropic-API alias.
		c := validConfig()
		c.Provider = ProviderConfig{Type: "anthropic"}
		c.ModelRouter = ModelRouterConfig{Type: "static", Provider: "anthropic", Model: "claude-sonnet-4-6"}
		must.NoError(ValidateRunConfig(c))
	})

	t.Run("named_bedrock_provider_via_mode_models", func(t *testing.T) {
		must := require.New(t)
		// The check resolves through Providers[name] entries too, so
		// a per-mode override that points at a bedrock-typed named
		// provider with an Anthropic-shaped alias is rejected.
		c := validConfig()
		c.Provider = ProviderConfig{Type: "anthropic"}
		c.Providers = map[string]ProviderConfig{
			"bdrk": {
				Type:       "bedrock",
				Region:     "us-east-1",
				Credential: &CredentialConfig{Type: "aws-default"},
			},
		}
		c.ModelRouter = ModelRouterConfig{
			Type:       "per-mode",
			Provider:   "anthropic",
			Model:      "claude-sonnet-4-6",
			ModeModels: map[string]string{"execution": "bdrk/claude-sonnet-4-6"},
		}
		must.ErrorContains(ValidateRunConfig(c), "bedrock")
	})
}

// TestValidateRunConfig_ProviderModelLabel_IsBounded pins the cardinality guard: the
// router-resolved model string rides on the provider.model OTel metric label, so
// for anthropic, openai-compatible, and openai-responses it must match
// modelLabelPattern (^[A-Za-z0-9./_-]{1,64}$), which allows forward slashes for
// OpenRouter-style "provider/model" naming.
func TestValidateRunConfig_ProviderModelLabel_IsBounded(t *testing.T) {
	// A 65-char model string overruns the 64-char label cap.
	tooLong := strings.Repeat("a", 65)

	t.Run("rejected", func(t *testing.T) {
		cases := []struct {
			name     string
			provider string
			model    string
		}{
			{"anthropic with space", "anthropic", "claude sonnet"},
			{"anthropic too long", "anthropic", tooLong},
			{"anthropic with newline", "anthropic", "claude\nsonnet"},
			{"openai-compatible with colon", "openai-compatible", "gpt-4:turbo"},
			{"openai-responses with percent", "openai-responses", "gpt%2F4o"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				must := require.New(t)
				c := validConfig()
				c.Provider = ProviderConfig{Type: tc.provider}
				c.ModelRouter = ModelRouterConfig{Type: "static", Provider: tc.provider, Model: tc.model}
				err := ValidateRunConfig(c)
				must.ErrorContains(err, "modelRouter.model")
				must.ErrorContains(err, "provider.model")
			})
		}
	})

	t.Run("accepted", func(t *testing.T) {
		cases := []struct {
			provider string
			model    string
		}{
			{"anthropic", "claude-sonnet-4-6"},
			{"anthropic", "claude-opus-4-1"},
			{"openai-compatible", "gpt-4o"},
			{"openai-responses", "gpt-4.1"},
			// OpenRouter-style model names with forward slashes
			{"openai-compatible", "deepseek/deepseek-v4-flash"},
			{"openai-compatible", "anthropic/claude-sonnet-4.6"},
			// exactly at the cap
			{"anthropic", strings.Repeat("a", 64)},
		}
		for _, tc := range cases {
			t.Run(tc.provider+"/"+tc.model, func(t *testing.T) {
				must := require.New(t)
				c := validConfig()
				c.Provider = ProviderConfig{Type: tc.provider}
				c.ModelRouter = ModelRouterConfig{Type: "static", Provider: tc.provider, Model: tc.model}
				must.NoError(ValidateRunConfig(c))
			})
		}
	})

	t.Run("per_mode_override_bound", func(t *testing.T) {
		must := require.New(t)
		// The label-bound check resolves through ModeModels "provider/model"
		// entries too, mirroring the gemini and bedrock validators.
		c := validConfig()
		c.Provider = ProviderConfig{Type: "anthropic"}
		c.ModelRouter = ModelRouterConfig{
			Type:       "per-mode",
			Provider:   "anthropic",
			Model:      "claude-sonnet-4-6",
			ModeModels: map[string]string{"execution": "anthropic/claude is bad"},
		}
		must.ErrorContains(ValidateRunConfig(c), "modelRouter.modeModels[execution]")
	})

	t.Run("bedrock_colon_and_arn_exempt_from_strict_class", func(t *testing.T) {
		// Bedrock model ids legitimately carry colons (version suffix) and
		// ARNs carry colons + slashes; the label-bound check must not
		// reject the shapes validateBedrockModelID already accepts. Only
		// the length + printability bound applies to bedrock.
		valid := []string{
			"anthropic.claude-sonnet-4-5-20250929-v1:0",
			"meta.llama3-8b-instruct-v1:0",
			"arn:aws:bedrock:eu-west-1:123456789012:inference-profile/eu.anthropic.claude-sonnet-4-6",
		}
		for _, model := range valid {
			t.Run(model, func(t *testing.T) {
				must := require.New(t)
				c := validConfig()
				c.Provider = ProviderConfig{
					Type:       "bedrock",
					Region:     "us-east-1",
					Credential: &CredentialConfig{Type: "aws-default"},
				}
				c.ModelRouter = ModelRouterConfig{Type: "static", Provider: "bedrock", Model: model}
				must.NoError(ValidateRunConfig(c))
			})
		}
	})

	t.Run("bedrock_non_printable_rejected", func(t *testing.T) {
		must := require.New(t)
		// The bedrock branch drops the strict character class but still
		// rejects non-printable bytes, which would corrupt the metric label.
		c := validConfig()
		c.Provider = ProviderConfig{
			Type:       "bedrock",
			Region:     "us-east-1",
			Credential: &CredentialConfig{Type: "aws-default"},
		}
		c.ModelRouter = ModelRouterConfig{Type: "static", Provider: "bedrock", Model: "anthropic.claude\x00sonnet"}
		must.ErrorContains(ValidateRunConfig(c), "non-printable")
	})
}

// TestValidateRunConfig_APIKeyRefMustBeSecretReference asserts that
// every secret-bearing apiKeyRef field is required to use the
// "secret://" scheme. A literal API key landing in RunConfig is a
// configuration mistake the validator should surface clearly rather
// than letting SecretStore fail at first provider call with a
// generic "no such secret" error.
func TestValidateRunConfig_APIKeyRefMustBeSecretReference(t *testing.T) {
	t.Run("provider.apiKeyRef literal rejected", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Provider = ProviderConfig{Type: "anthropic", APIKeyRef: "sk-ant-literal-key"}
		err := ValidateRunConfig(c)
		must.ErrorContains(err, "secret://")
		must.ErrorContains(err, "provider.apiKeyRef")
	})

	t.Run("provider.apiKeyRef secret reference accepted", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Provider = ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_KEY"}
		must.NoError(ValidateRunConfig(c))
	})

	t.Run("provider.apiKeyRef empty accepted", func(t *testing.T) {
		must := require.New(t)
		// Empty is the unset form — credential federation may leave
		// apiKeyRef blank and resolve auth via Credential instead.
		c := validConfig()
		c.Provider = ProviderConfig{Type: "anthropic"}
		must.NoError(ValidateRunConfig(c))
	})

	t.Run("executor.vcsBackend.apiKeyRef literal rejected", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Executor.VcsBackend = &VcsBackendConfig{Type: "github", APIKeyRef: "ghp_literal"}
		must.ErrorContains(ValidateRunConfig(c), "executor.vcsBackend.apiKeyRef")
	})

	t.Run("tools.mcpServers literal rejected", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Tools.MCPServers = []MCPServerConfig{{Name: "x", URI: "http://localhost", APIKeyRef: "literal-token"}}
		must.ErrorContains(ValidateRunConfig(c), "tools.mcpServers[0].apiKeyRef")
	})
}

func TestValidateRunConfig_ToolDispatchRejectsOutOfRange(t *testing.T) {
	for _, n := range []int{-1, MaxToolDispatchMaxParallel + 1, 17_000} {
		t.Run(fmt.Sprintf("max_parallel=%d", n), func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.ToolDispatch = &ToolDispatchConfig{MaxParallel: n}
			err := ValidateRunConfig(c)
			must.ErrorContains(err, "toolDispatch.maxParallel")
			// The message must name zero as the accepted default sentinel
			// so an operator knows it is a legal value.
			must.ErrorContains(err, "0 (use default)")
		})
	}
}

// TestValidateRunConfig_ToolChoiceEscalationRejectsOutOfRange pins the MaxRetries
// bound: values outside [1, MaxToolChoiceEscalationMaxRetries] are rejected with
// the "0 (use default)" sentinel called out. The check fires regardless of
// Enabled so a disabled config carrying a bad value still fails loudly.
func TestValidateRunConfig_ToolChoiceEscalationRejectsOutOfRange(t *testing.T) {
	for _, n := range []int{-1, MaxToolChoiceEscalationMaxRetries + 1, 9_000} {
		t.Run(fmt.Sprintf("max_retries=%d", n), func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.ToolChoiceEscalation = &ToolChoiceEscalationConfig{Enabled: true, MaxRetries: n}
			err := ValidateRunConfig(c)
			must.ErrorContains(err, "toolChoiceEscalation.maxRetries")
			must.ErrorContains(err, "0 (use default)")
		})
	}
}

// TestValidateRunConfig_ToolChoiceEscalationAcceptsValid pins the accepted
// shapes: nil (feature off), an explicit zero (defaults), and a bounded
// positive cap all pass validation, and the Effective accessor resolves
// each to the documented value.
func TestValidateRunConfig_ToolChoiceEscalationAcceptsValid(t *testing.T) {
	t.Run("nil_disabled", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.ToolChoiceEscalation = nil
		must.NoError(ValidateRunConfig(c))
		must.Equal(0, c.EffectiveToolChoiceEscalationMaxRetries())
	})
	t.Run("enabled_default", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.ToolChoiceEscalation = &ToolChoiceEscalationConfig{Enabled: true}
		must.NoError(ValidateRunConfig(c))
		must.Equal(DefaultToolChoiceEscalationMaxRetries, c.EffectiveToolChoiceEscalationMaxRetries())
	})
	t.Run("disabled_with_retries_stays_disabled", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.ToolChoiceEscalation = &ToolChoiceEscalationConfig{Enabled: false, MaxRetries: 2}
		must.NoError(ValidateRunConfig(c))
		must.Equal(0, c.EffectiveToolChoiceEscalationMaxRetries())
	})
	t.Run("explicit_cap_2", func(t *testing.T) {
		must := require.New(t)
		// An explicit positive cap is returned verbatim.
		c := validConfig()
		c.ToolChoiceEscalation = &ToolChoiceEscalationConfig{Enabled: true, MaxRetries: 2}
		must.NoError(ValidateRunConfig(c))
		must.Equal(2, c.EffectiveToolChoiceEscalationMaxRetries())
	})
}

// --- BatchProviderConfig ---

// batchValidConfig is the baseline for the batch suite: a non-execution
// mode that accepts batch (research) with stdio transport plus the
// harness-side polling flag set, no DynamicContext (so the Rule of Two
// stays satisfied), and a read-only tool list. Each sub-test mutates a
// single field to exercise one invariant in isolation.
func batchValidConfig() *RunConfig {
	timeout := 60
	return &RunConfig{
		Mode:             "research",
		Provider:         ProviderConfig{Type: "anthropic"},
		MaxTurns:         3,
		Timeout:          &timeout,
		PermissionPolicy: PermissionPolicyConfig{Type: "deny-side-effects"},
		Transport:        TransportConfig{Type: "stdio"},
		Tools:            ToolsConfig{BuiltIn: []string{"read_file"}},
	}
}

func TestValidateRunConfig_Batch_NilLeavesConfigUntouched(t *testing.T) {
	must := require.New(t)
	c := batchValidConfig()
	must.NoError(ValidateRunConfig(c))
	must.Nil(c.Provider.Batch, "validation must not materialise a nil Batch")
}

func TestValidateRunConfig_Batch_UnsupportedProviderType(t *testing.T) {
	for _, tc := range []struct {
		providerType string
		wantErr      bool
	}{
		{"anthropic", false},
		{"openai-compatible", false},
		{"openai-responses", false},
		{"bedrock", true},
		{"gemini", true},
	} {
		t.Run(tc.providerType, func(t *testing.T) {
			must := require.New(t)
			c := batchValidConfig()
			c.Provider.Type = tc.providerType
			// Bedrock and gemini need extra fields to clear unrelated
			// validation; supply just enough to isolate the batch check.
			if tc.providerType == "bedrock" {
				c.Provider.Region = "us-east-1"
				c.Provider.Credential = &CredentialConfig{Type: "aws-default"}
			}
			if tc.providerType == "gemini" {
				c.Provider.GCPProject = "example-project"
				c.Provider.GCPLocation = "global"
				c.Provider.Credential = &CredentialConfig{Type: "gcp-default"}
			}
			c.Provider.Batch = &BatchProviderConfig{Enabled: true, HarnessSidePolling: true}
			err := ValidateRunConfig(c)
			if tc.wantErr {
				must.ErrorContains(err, "batch is not supported for provider type")
			} else {
				must.NoError(err)
			}
		})
	}
}

func TestValidateRunConfig_Batch_RejectsExecutionMode(t *testing.T) {
	must := require.New(t)
	c := batchValidConfig()
	c.Mode = "execution"
	c.PermissionPolicy = PermissionPolicyConfig{Type: "allow-all"}
	c.Provider.Batch = &BatchProviderConfig{
		Enabled:               true,
		HarnessSidePolling:    true,
		AllowInteractiveModes: true, // must not bypass the execution check
	}
	must.ErrorContains(ValidateRunConfig(c), "batch cannot be used with mode=execution")
}

func TestValidateRunConfig_Batch_InteractiveModesRequireOptIn(t *testing.T) {
	for _, mode := range []string{"planning", "review"} {
		t.Run(mode+"_without_opt_in", func(t *testing.T) {
			must := require.New(t)
			c := batchValidConfig()
			c.Mode = mode
			c.Provider.Batch = &BatchProviderConfig{Enabled: true, HarnessSidePolling: true}
			must.ErrorContains(ValidateRunConfig(c),
				"batch requires allowInteractiveModes=true for mode="+mode)
		})
		t.Run(mode+"_with_opt_in_passes", func(t *testing.T) {
			must := require.New(t)
			c := batchValidConfig()
			c.Mode = mode
			c.Provider.Batch = &BatchProviderConfig{
				Enabled:               true,
				HarnessSidePolling:    true,
				AllowInteractiveModes: true,
			}
			must.NoError(ValidateRunConfig(c))
		})
	}
	t.Run("research_does_not_require_opt_in", func(t *testing.T) {
		must := require.New(t)
		// Sanity check: the opt-in only gates "planning" and "review".
		// Other read-only modes ("research", "toil") accept batch without it.
		c := batchValidConfig()
		c.Mode = "research"
		c.Provider.Batch = &BatchProviderConfig{Enabled: true, HarnessSidePolling: true}
		must.NoError(ValidateRunConfig(c))
	})
}

func TestValidateRunConfig_Batch_StdioRequiresHarnessSidePolling(t *testing.T) {
	must := require.New(t)
	c := batchValidConfig()
	c.Transport = TransportConfig{Type: "stdio"}
	c.Provider.Batch = &BatchProviderConfig{Enabled: true, HarnessSidePolling: false}
	must.ErrorContains(ValidateRunConfig(c), "batch with transport=stdio requires harnessSidePolling=true")
}

func TestValidateRunConfig_Batch_HarnessSidePollingRejectedWithGRPC(t *testing.T) {
	must := require.New(t)
	c := batchValidConfig()
	c.Transport = TransportConfig{Type: "grpc"}
	// HarnessSidePolling is rejected with grpc transport even when
	// Batch.Enabled is false — a stale flag combination must not be
	// silently retained.
	c.Provider.Batch = &BatchProviderConfig{Enabled: false, HarnessSidePolling: true}
	must.ErrorContains(ValidateRunConfig(c), "batch.harnessSidePolling must not be set with transport=grpc")
}

func TestValidateRunConfig_Batch_HarnessSidePollingRejectsAnthropicWIF(t *testing.T) {
	must := require.New(t)
	// harnessPollingBatchClient pins x-api-key auth; the anthropic-wif
	// credential type instead requires Authorization: Bearer (see the
	// AuthMode switch in anthropic.go). The combination is unsupported,
	// so the validator must reject it explicitly.
	c := batchValidConfig()
	c.Transport = TransportConfig{Type: "stdio"}
	c.Provider.Credential = validAnthropicWIFCredential()
	c.Provider.APIKeyRef = "" // mutually exclusive with anthropic-wif
	c.Provider.Batch = &BatchProviderConfig{Enabled: true, HarnessSidePolling: true}
	must.ErrorContains(ValidateRunConfig(c), "harnessSidePolling does not support anthropic-wif")
}

func TestValidateRunConfig_Batch_CancelBundleRejectedWithStdio(t *testing.T) {
	must := require.New(t)
	c := batchValidConfig()
	c.Transport = TransportConfig{Type: "stdio"}
	// As with HarnessSidePolling, CancelBundleOnRunCancel is rejected
	// even when Batch.Enabled is false.
	c.Provider.Batch = &BatchProviderConfig{Enabled: false, CancelBundleOnRunCancel: true}
	must.ErrorContains(ValidateRunConfig(c), "batch.cancelBundleOnRunCancel requires transport=grpc")
}

// TestValidateRunConfig_Batch_MaxWaitSecondsRange pins the batch wait to
// the run timeout: the run context is bound to Timeout, so a longer wait
// is unreachable and must be rejected rather than silently truncated.
func TestValidateRunConfig_Batch_MaxWaitSecondsRange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		seconds int
		wantErr bool
	}{
		{"one_passes", 1, false},
		{"equal_to_timeout_passes", 60, false},
		{"zero_fails", 0, true},
		{"one_over_timeout_fails", 61, true},
		{"far_over_timeout_fails", 86400, true},
		{"negative_fails", -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := batchValidConfig()
			seconds := tc.seconds
			c.Provider.Batch = &BatchProviderConfig{
				Enabled:            true,
				HarnessSidePolling: true,
				MaxWaitSeconds:     &seconds,
			}
			err := ValidateRunConfig(c)
			if tc.wantErr {
				must.ErrorContains(err, "batch.maxWaitSeconds must be in range (0, 60]")
				// The diagnostic must name both contradicting values.
				must.ErrorContains(err, "the run timeout of 60 seconds")
				must.ErrorContains(err, fmt.Sprintf("got %d", tc.seconds))
			} else {
				must.NoError(err)
			}
		})
	}
}

func TestValidateRunConfig_Batch_MaxWaitSecondsDefaultsWhenNil(t *testing.T) {
	must := require.New(t)
	c := batchValidConfig()
	c.Provider.Batch = &BatchProviderConfig{
		Enabled:            true,
		HarnessSidePolling: true,
		// MaxWaitSeconds intentionally nil.
	}
	must.NoError(ValidateRunConfig(c))
	must.NotNil(c.Provider.Batch.MaxWaitSeconds, "validation should populate MaxWaitSeconds when nil and enabled")
	must.Equal(*c.Timeout, *c.Provider.Batch.MaxWaitSeconds)
}

// TestValidateRunConfig_Batch_MaxWaitSecondsTracksTimeout pins that the
// default follows the configured timeout rather than a fixed constant.
func TestValidateRunConfig_Batch_MaxWaitSecondsTracksTimeout(t *testing.T) {
	for _, timeout := range []int{1, 600, MaxRunTimeoutSeconds} {
		t.Run(fmt.Sprintf("timeout_%d", timeout), func(t *testing.T) {
			must := require.New(t)
			c := batchValidConfig()
			seconds := timeout
			c.Timeout = &seconds
			c.Provider.Batch = &BatchProviderConfig{Enabled: true, HarnessSidePolling: true}
			must.NoError(ValidateRunConfig(c))
			must.Equal(timeout, *c.Provider.Batch.MaxWaitSeconds)
		})
	}
}

// TestValidateRunConfig_Batch_MaxWaitSecondsNotDefaultedWithInvalidTimeout
// pins that an unusable timeout leaves MaxWaitSeconds alone: the timeout
// error is the root cause and a derived default would be meaningless.
func TestValidateRunConfig_Batch_MaxWaitSecondsNotDefaultedWithInvalidTimeout(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	c := batchValidConfig()
	c.Timeout = nil
	c.Provider.Batch = &BatchProviderConfig{Enabled: true, HarnessSidePolling: true}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "timeout is required")
	check.NotContains(err.Error(), "batch.maxWaitSeconds", "a missing timeout must not produce a secondary batch error")
	check.Nil(c.Provider.Batch.MaxWaitSeconds, "MaxWaitSeconds must stay nil without a usable timeout")
}

// TestValidateRunConfig_Batch_MaxWaitSecondsRangeIgnoresEnabled pins that
// an explicit out-of-range maxWaitSeconds is rejected on a disabled batch
// block too, so the contradiction surfaces at authoring time rather than
// the first run that passes --batch.
func TestValidateRunConfig_Batch_MaxWaitSecondsRangeIgnoresEnabled(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	c := batchValidConfig()
	overLimit := 7200
	c.Provider.Batch = &BatchProviderConfig{Enabled: false, MaxWaitSeconds: &overLimit}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "batch.maxWaitSeconds must be in range (0, 60]")
	must.NotNil(c.Provider.Batch.MaxWaitSeconds, "validation must not clear an explicit maxWaitSeconds on a disabled block")
	check.Equal(overLimit, *c.Provider.Batch.MaxWaitSeconds, "validation must not rewrite an explicit maxWaitSeconds on a disabled block")
}

// TestValidateRunConfig_Batch_FallbackOnTimeoutNeedsHeadroom pins that
// fallbackOnTimeout is rejected whenever the batch wait resolves to the
// full run timeout. The run context is armed before the turn and the
// batch cap only starts once the wait blocks, so an equal wait can never
// expire first and the fallback could never fire.
func TestValidateRunConfig_Batch_FallbackOnTimeoutNeedsHeadroom(t *testing.T) {
	const wantErr = "batch.fallbackOnTimeout requires batch.maxWaitSeconds strictly below the run timeout"
	for _, tc := range []struct {
		name     string
		setWait  bool
		seconds  int
		fallback bool
		wantErr  bool
	}{
		{"unset defaults to timeout fails", false, 0, true, true},
		{"explicitly equal fails", true, 60, true, true},
		{"strictly below passes", true, 59, true, false},
		{"equal without fallback passes", true, 60, false, false},
		{"unset without fallback passes", false, 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := batchValidConfig()
			c.Provider.Batch = &BatchProviderConfig{
				Enabled:            true,
				HarnessSidePolling: true,
				FallbackOnTimeout:  tc.fallback,
			}
			if tc.setWait {
				seconds := tc.seconds
				c.Provider.Batch.MaxWaitSeconds = &seconds
			}
			err := ValidateRunConfig(c)
			if !tc.wantErr {
				must.NoError(err)
				return
			}
			must.ErrorContains(err, wantErr)
			// The diagnostic must name both contradicting values and the
			// budget an operator has to leave room for.
			for _, want := range []string{"maxWaitSeconds=60", "timeout=60", "90000 ms", "120 s"} {
				must.ErrorContains(err, want)
			}
		})
	}
}

func TestValidateRunConfig_Batch_MaxWaitSecondsNotDefaultedWhenDisabled(t *testing.T) {
	must := require.New(t)
	// The default applies only to Enabled=true configs; a disabled batch block keeps
	// MaxWaitSeconds nil so callers can still distinguish "operator did not set
	// this" from "default applied".
	c := batchValidConfig()
	c.Provider.Batch = &BatchProviderConfig{Enabled: false}
	must.NoError(ValidateRunConfig(c))
	must.Nil(c.Provider.Batch.MaxWaitSeconds, "MaxWaitSeconds must not be populated when Enabled=false")
}

func TestValidateRunConfig_Batch_MaxTurnsLatencyWarning(t *testing.T) {
	// Route slog through a buffer so we can assert the WARN line emitted by
	// validateBatchConfig appears above the threshold and is absent at or below it.
	//
	// The assertion pins the static message + structured attrs rather than
	// substring-matching a formatted figure, so consumers can parse the
	// threshold and worst-case values from structured fields.
	const wantMsg = "batch with maxTurns above the latency-warning threshold may exhaust the run timeout before the final turn completes"
	for _, tc := range []struct {
		name     string
		maxTurns int
		wantWarn bool
	}{
		{"at threshold no warn", 5, false},
		{"above threshold warns", 6, true},
		{"well above threshold warns", 50, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var logBuf bytes.Buffer
			originalLogger := slog.Default()
			t.Cleanup(func() { slog.SetDefault(originalLogger) })
			slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))

			c := batchValidConfig()
			c.MaxTurns = tc.maxTurns
			c.Provider.Batch = &BatchProviderConfig{Enabled: true, HarnessSidePolling: true}
			must.NoError(ValidateRunConfig(c), "batch warning path must not produce a hard error")

			logs := logBuf.String()
			if !tc.wantWarn {
				must.NotContains(logs, wantMsg)
				return
			}
			must.Contains(logs, "level=WARN")
			must.Contains(logs, wantMsg)
			must.Contains(logs, fmt.Sprintf("maxTurns=%d", tc.maxTurns))
			must.Contains(logs, fmt.Sprintf("thresholdTurns=%d", batchTurnsLatencyWarnThreshold))
			must.Contains(logs, fmt.Sprintf("maxWaitSeconds=%d", *c.Timeout))
			must.Contains(logs, fmt.Sprintf("worstCaseSeconds=%d", tc.maxTurns**c.Timeout))
		})
	}
}

func TestValidateRunConfig_Batch_HappyPathAllFieldsSet(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	// Exercise every BatchProviderConfig field at a non-zero value to
	// pin that the happy-path combination validates cleanly. Uses gRPC
	// transport so HarnessSidePolling stays false and
	// CancelBundleOnRunCancel can be true at the same time.
	c := batchValidConfig()
	c.Transport = TransportConfig{Type: "grpc"}
	timeout := MaxRunTimeoutSeconds
	c.Timeout = &timeout
	// Strictly below the timeout so FallbackOnTimeout has room to fire.
	maxWait := 3000
	c.Provider.Batch = &BatchProviderConfig{
		Enabled:                 true,
		MaxWaitSeconds:          &maxWait,
		HarnessSidePolling:      false,
		FallbackOnTimeout:       true,
		CancelBundleOnRunCancel: true,
		AllowInteractiveModes:   true,
	}
	must.NoError(ValidateRunConfig(c))
	must.NotNil(c.Provider.Batch.MaxWaitSeconds)
	check.Equal(3000, *c.Provider.Batch.MaxWaitSeconds, "a caller-supplied MaxWaitSeconds must not be overwritten")
}

// TestValidateRunConfig_Batch_ProvidersMapRejected pins that ValidateRunConfig
// rejects any non-nil Batch on a named providers map entry: Batch is a
// top-level-only concept in v1 and is otherwise silently ignored at runtime.
func TestValidateRunConfig_Batch_ProvidersMapRejected(t *testing.T) {
	t.Run("enabled_batch_on_named_entry_fails", func(t *testing.T) {
		must := require.New(t)
		c := batchValidConfig()
		c.Providers = map[string]ProviderConfig{
			"secondary": {
				Type:  "anthropic",
				Batch: &BatchProviderConfig{Enabled: true, HarnessSidePolling: true},
			},
		}
		err := ValidateRunConfig(c)
		want := `providers[secondary].batch is not supported in v1; batch applies only to the top-level provider`
		must.ErrorContains(err, want)
	})
	t.Run("disabled_but_present_batch_on_named_entry_also_fails", func(t *testing.T) {
		must := require.New(t)
		// The strict rule rejects any non-nil Batch (not just
		// Enabled=true). An operator who adds a batch block with
		// enabled=false has still tripped the foot-gun: they expect
		// the named-entry batch knobs to mean something, and v1
		// promises they don't.
		c := batchValidConfig()
		c.Providers = map[string]ProviderConfig{
			"secondary": {
				Type:  "anthropic",
				Batch: &BatchProviderConfig{Enabled: false},
			},
		}
		err := ValidateRunConfig(c)
		must.ErrorContains(err, "providers[secondary].batch is not supported in v1")
	})
}

// TestValidateRunConfig_Batch_EmptyProviderTypeSuppressesBatchTypeError pins that
// when batch is enabled but the provider type is empty, only the "provider type
// is required" error surfaces, not also a misleading batch-compatibility error.
func TestValidateRunConfig_Batch_EmptyProviderTypeSuppressesBatchTypeError(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	c := batchValidConfig()
	c.Provider.Type = ""
	c.Provider.Batch = &BatchProviderConfig{Enabled: true, HarnessSidePolling: true}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "provider type is required")
	check.NotContains(err.Error(), "batch is not supported for provider type", "batch-type error should be suppressed when provider type is invalid")
}

// TestBatchProviderConfig_JSONRoundTrip pins the *int with omitempty invariant on
// BatchProviderConfig.MaxWaitSeconds: callers rely on the nil/non-nil distinction
// to tell "not configured" from "default applied", so a marshal+unmarshal cycle
// must preserve both states, including a present-but-disabled block.
func TestBatchProviderConfig_JSONRoundTrip(t *testing.T) {
	t.Run("enabled with max wait seconds round trips", func(t *testing.T) {
		must := require.New(t)
		maxWait := 3600
		want := BatchProviderConfig{
			Enabled:                 true,
			MaxWaitSeconds:          &maxWait,
			HarnessSidePolling:      true,
			FallbackOnTimeout:       true,
			CancelBundleOnRunCancel: false,
			AllowInteractiveModes:   true,
		}
		pc := ProviderConfig{Type: "anthropic", Batch: &want}
		data, err := json.Marshal(pc)
		must.NoError(err, "marshal")
		var got ProviderConfig
		must.NoError(json.Unmarshal(data, &got), "unmarshal")
		must.NotNil(got.Batch, "Batch must survive JSON round-trip as non-nil")
		must.Equal(want, *got.Batch)
	})

	t.Run("disabled but present block survives round trip", func(t *testing.T) {
		check := assert.New(t)
		must := require.New(t)
		// `omitempty` is on ProviderConfig.Batch (the *pointer), not on the inner
		// BatchProviderConfig fields: marshaling a non-nil pointer to a zero-valued
		// struct must still emit a "batch" key so callers can distinguish "no batch key"
		// from "batch present but disabled".
		pc := ProviderConfig{
			Type:  "anthropic",
			Batch: &BatchProviderConfig{Enabled: false},
		}
		data, err := json.Marshal(pc)
		must.NoError(err, "marshal")
		must.Contains(string(data), `"batch"`)
		var got ProviderConfig
		must.NoError(json.Unmarshal(data, &got), "unmarshal")
		must.NotNil(got.Batch, "disabled-but-present batch must survive as non-nil pointer")
		check.False(got.Batch.Enabled)
		check.Nil(got.Batch.MaxWaitSeconds)
	})

	t.Run("absent batch unmarshals as nil", func(t *testing.T) {
		must := require.New(t)
		// Cross-check: the omitempty pointer on a nil Batch must drop the key entirely
		// on marshal, and an absent key must unmarshal to nil.
		pc := ProviderConfig{Type: "anthropic"}
		data, err := json.Marshal(pc)
		must.NoError(err, "marshal")
		must.NotContains(string(data), `"batch"`)
		var got ProviderConfig
		must.NoError(json.Unmarshal(data, &got), "unmarshal")
		must.Nil(got.Batch)
	})
}

// TestRedact_ProviderBatchNotAliased pins the Redact() deep-copy on
// Provider.Batch: validateBatchConfig mutates Batch.MaxWaitSeconds in place, so a
// shared pointer would let a later default-apply write reach the redacted
// snapshot through it. The same risk applies to entries in the Providers map.
func TestRedact_ProviderBatchNotAliased(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	maxWait := 3600
	otherMaxWait := 1800
	rc := RunConfig{
		Provider: ProviderConfig{
			Type:  "anthropic",
			Batch: &BatchProviderConfig{Enabled: true, MaxWaitSeconds: &maxWait, HarnessSidePolling: true},
		},
		Providers: map[string]ProviderConfig{
			"secondary": {
				Type:  "anthropic",
				Batch: &BatchProviderConfig{Enabled: true, MaxWaitSeconds: &otherMaxWait},
			},
		},
	}
	redacted := rc.Redact()

	must.NotNil(redacted.Provider.Batch, "top-level Batch dropped by Redact")
	must.NotSame(rc.Provider.Batch, redacted.Provider.Batch, "top-level Batch pointer aliased; Redact must deep-copy")
	redacted.Provider.Batch.Enabled = false
	check.True(rc.Provider.Batch.Enabled, "mutating redacted Provider.Batch.Enabled leaked to original")

	redactedSecondary := redacted.Providers["secondary"]
	originalSecondary := rc.Providers["secondary"]
	must.NotNil(redactedSecondary.Batch, "named-provider Batch dropped by Redact")
	must.NotSame(originalSecondary.Batch, redactedSecondary.Batch, "named-provider Batch pointer aliased; Redact must deep-copy")
	redactedSecondary.Batch.Enabled = false
	check.True(rc.Providers["secondary"].Batch.Enabled, "mutating redacted named-provider Batch.Enabled leaked to original")
}

// TestValidateRunConfig_CompatProfile_ClosedEnum pins the closed-enum behaviour for
// ProviderConfig.CompatProfile: empty and "zai-glm" validate; any other value
// fails at startup with an error mentioning the field path.
func TestValidateRunConfig_CompatProfile_ClosedEnum(t *testing.T) {
	t.Run("empty accepted", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Provider.CompatProfile = ""
		must.NoError(ValidateRunConfig(c))
	})
	t.Run("zai glm accepted", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Provider.CompatProfile = "zai-glm"
		must.NoError(ValidateRunConfig(c))
	})
	t.Run("unknown rejected", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Provider.CompatProfile = "unknown"
		err := ValidateRunConfig(c)
		must.ErrorContains(err, "compatProfile")
		must.ErrorContains(err, `"unknown"`)
	})
	t.Run("named provider rejected", func(t *testing.T) {
		must := require.New(t)
		// The named-provider loop runs the same validator; pin it so a
		// future refactor that drops the per-entry call surfaces here.
		c := validConfig()
		c.Providers = map[string]ProviderConfig{
			"secondary": {Type: "openai-compatible", CompatProfile: "not-real"},
		}
		err := ValidateRunConfig(c)
		must.ErrorContains(err, "providers[secondary].compatProfile")
	})
}

func TestValidateRunConfig_EndpointProfile_AcceptsEveryProfileOnOpenAIWireTypes(t *testing.T) {
	for _, providerType := range []string{"openai-compatible", "openai-responses"} {
		for _, profile := range append([]string{""}, ValidEndpointProfileValues()...) {
			t.Run(providerType+"/"+profile, func(t *testing.T) {
				must := require.New(t)
				c := validConfig()
				c.Provider = ProviderConfig{
					Type:            providerType,
					BaseURL:         "https://gateway.example.com/v1",
					EndpointProfile: profile,
				}
				must.NoError(ValidateRunConfig(c))
			})
		}
	}
}

func TestValidateRunConfig_EndpointProfile_RejectsUnknownValue(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Provider = ProviderConfig{Type: "openai-responses", EndpointProfile: "azure"}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, `provider.endpointProfile "azure" is not a recognised endpoint profile`)
	must.ErrorContains(err, "azure-openai, bedrock-mantle, generic, openai")
}

func TestValidateRunConfig_EndpointProfile_RejectsNonOpenAIWireType(t *testing.T) {
	for _, providerType := range []string{"anthropic", "bedrock", "gemini"} {
		t.Run(providerType, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Provider.Type = providerType
			c.Provider.EndpointProfile = EndpointProfileOpenAI
			err := ValidateRunConfig(c)
			must.ErrorContains(err, "provider.endpointProfile is only supported for openai-compatible and openai-responses providers")
		})
	}
}

func TestValidateRunConfig_EndpointProfile_RequiresBaseURLForNonOpenAIEndpoints(t *testing.T) {
	for _, profile := range []string{EndpointProfileAzureOpenAI, EndpointProfileBedrockMantle} {
		t.Run(profile, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Provider = ProviderConfig{Type: "openai-responses", EndpointProfile: profile}
			err := ValidateRunConfig(c)
			must.ErrorContains(err, fmt.Sprintf("provider.endpointProfile %q requires baseUrl", profile))
		})
	}
}

func TestValidateRunConfig_EndpointProfile_AcceptsOpenAIAndGenericWithoutBaseURL(t *testing.T) {
	for _, profile := range []string{EndpointProfileOpenAI, EndpointProfileGeneric} {
		t.Run(profile, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Provider = ProviderConfig{Type: "openai-responses", EndpointProfile: profile}
			must.NoError(ValidateRunConfig(c))
		})
	}
}

func TestValidateRunConfig_EndpointProfile_ValidatesNamedProviders(t *testing.T) {
	t.Run("unknown value", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Providers = map[string]ProviderConfig{
			"secondary": {Type: "openai-responses", EndpointProfile: "not-real"},
		}
		must.ErrorContains(ValidateRunConfig(c), `providers[secondary].endpointProfile "not-real"`)
	})
	t.Run("wrong type", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Providers = map[string]ProviderConfig{
			"secondary": {Type: "anthropic", EndpointProfile: EndpointProfileGeneric},
		}
		must.ErrorContains(ValidateRunConfig(c), "providers[secondary].endpointProfile is only supported")
	})
	t.Run("missing base url", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Providers = map[string]ProviderConfig{
			"secondary": {Type: "openai-compatible", EndpointProfile: EndpointProfileAzureOpenAI},
		}
		must.ErrorContains(ValidateRunConfig(c), `providers[secondary].endpointProfile "azure-openai" requires baseUrl`)
	})
}

// --- HooksConfig ---

func validHookConfig() HookConfig {
	return HookConfig{Command: "echo ok"}
}

func TestValidateRunConfig_Hooks_NilIsValid(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Hooks = nil
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_Hooks_MinimalAccepted(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Hooks = &HooksConfig{
		PreRun:  []HookConfig{{Command: "git clone https://example.com/repo ."}},
		PostRun: []HookConfig{{Command: "test -f marker", RunOn: "success"}},
	}
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_Hooks_RejectsAPIExecutorWithHooks(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Executor = ExecutorConfig{Type: "api"}
	c.Hooks = &HooksConfig{PreRun: []HookConfig{validHookConfig()}}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "exec-capable executor")
}

// TestValidateRunConfig_Hooks_RejectsNoneExecutorWithHooks closes the same
// exec-capability gap the api-executor check exists for: the none
// executor (see harness/internal/executor/none.go) has CanExec=false just
// like api, so hooks.preRun/postRun must be rejected for it too rather
// than passing validation and failing mid-run.
func TestValidateRunConfig_Hooks_RejectsNoneExecutorWithHooks(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Executor = ExecutorConfig{Type: "none"}
	c.Hooks = &HooksConfig{PostRun: []HookConfig{validHookConfig()}}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "exec-capable executor")
}

func TestValidateRunConfig_Hooks_EmptyHooksConfigAllowsAPIExecutor(t *testing.T) {
	must := require.New(t)
	// An explicit-but-empty HooksConfig{} (e.g. an unmarshalled wire
	// payload with an empty "hooks" sub-message) schedules nothing, so
	// it must not trip the exec-capable-executor requirement.
	c := validConfig()
	c.Executor = ExecutorConfig{Type: "api"}
	c.Hooks = &HooksConfig{}
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_Hooks_CommandRequired(t *testing.T) {
	for _, cmd := range []string{"", "   ", "\t\n"} {
		t.Run(fmt.Sprintf("blank command %q", cmd), func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Hooks = &HooksConfig{PreRun: []HookConfig{{Command: cmd}}}
			err := ValidateRunConfig(c)
			must.ErrorContains(err, "hooks.preRun[0].command is required")
		})
	}
}

func TestValidateRunConfig_Hooks_CommandMaxLength(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Hooks = &HooksConfig{PreRun: []HookConfig{{Command: strings.Repeat("a", maxHookCommandBytes+1)}}}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, "hooks.preRun[0].command must be <=")
}

func TestValidateRunConfig_Hooks_CommandRejectsSecretRef(t *testing.T) {
	cases := []struct {
		name    string
		command string
	}{
		{"bare reference", "secret://API_KEY"},
		{"inside command substitution", "curl -H \"Authorization: Bearer $(cat secret://API_KEY)\""},
		{"mid string", "echo secret://ANYWHERE_IN_STRING"},
		// Case-insensitive: a shell reads "secret://" and "SECRET://"
		// identically, so the rejection must too.
		{"upper case scheme", "SECRET://API_KEY"},
		{"title case scheme", "Secret://API_KEY"},
		{"mixed case scheme", "echo sEcReT://mixed_case"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Hooks = &HooksConfig{PreRun: []HookConfig{{Command: tc.command}}}
			err := ValidateRunConfig(c)
			must.ErrorContains(err, "secret://")
		})
	}
}

// TestValidateRunConfig_Hooks_AcceptedInReadOnlyMode pins the read-only-mode
// carve-out for lifecycle hooks: the invariant bounds only the model's tool
// surface, not operator-authored deterministic commands, so hooks — including a
// postRun hook that writes (git push) — validate cleanly in every read-only mode.
func TestValidateRunConfig_Hooks_AcceptedInReadOnlyMode(t *testing.T) {
	readOnlyModes := []string{"planning", "review", "research", "toil"}
	for _, mode := range readOnlyModes {
		t.Run(mode, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Mode = mode
			c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
			c.Tools = ToolsConfig{BuiltIn: []string{"read_file"}}
			c.Hooks = &HooksConfig{
				PreRun:  []HookConfig{{Name: "clone", Command: "git clone https://example.com/repo ."}},
				PostRun: []HookConfig{{Name: "push", Command: "git push", RunOn: "success"}},
			}
			must.NoError(ValidateRunConfig(c))
		})
	}
}

// TestRuleOfTwoState_HooksNeverAffectLegs pins that Hooks plays no part in any of
// the three Rule-of-Two legs: RuleOfTwoState must report identical flags for two
// configs differing only in Hooks. The base config already trips all three legs
// so the comparison is meaningful rather than trivially false==false.
func TestRuleOfTwoState_HooksNeverAffectLegs(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	base := validConfig()
	base.DynamicContext = map[string]DynamicContextValue{
		"issue": {Value: "untrusted issue body", Sensitive: true},
	}
	base.Tools = ToolsConfig{BuiltIn: []string{"web_fetch"}}

	withHooks := *base
	withHooks.Hooks = &HooksConfig{
		PreRun: []HookConfig{{Command: "curl -X POST https://example.com/webhook"}},
		PostRun: []HookConfig{
			{Command: "curl -X POST https://example.com/artifact", RunOn: "success"},
		},
	}

	baseUntrusted, baseSensitive, baseExternal := RuleOfTwoState(base)
	must.True(baseUntrusted && baseSensitive && baseExternal,
		"prerequisite: base config must trip all three legs, got (%v, %v, %v)", baseUntrusted, baseSensitive, baseExternal)

	hooksUntrusted, hooksSensitive, hooksExternal := RuleOfTwoState(&withHooks)
	check.Equal(baseUntrusted, hooksUntrusted, "untrusted leg changed when only Hooks was populated")
	check.Equal(baseSensitive, hooksSensitive, "sensitive leg changed when only Hooks was populated")
	check.Equal(baseExternal, hooksExternal, "external-communication leg changed when only Hooks was populated")
}

func TestValidateRunConfig_Hooks_TypeAllowlist(t *testing.T) {
	t.Run("empty accepted", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Hooks = &HooksConfig{PreRun: []HookConfig{{Type: "", Command: "true"}}}
		must.NoError(ValidateRunConfig(c))
	})
	t.Run("command accepted", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Hooks = &HooksConfig{PreRun: []HookConfig{{Type: "command", Command: "true"}}}
		must.NoError(ValidateRunConfig(c))
	})
	t.Run("unknown rejected", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Hooks = &HooksConfig{PreRun: []HookConfig{{Type: "gitSource", Command: "true"}}}
		err := ValidateRunConfig(c)
		must.ErrorContains(err, `unsupported hooks.preRun[0] type "gitSource"`)
	})
}

func TestValidateRunConfig_Hooks_TimeoutBounds(t *testing.T) {
	cases := []struct {
		name    string
		seconds int
		wantErr bool
	}{
		{"zero uses default", 0, false},
		{"one second", 1, false},
		{"max 1800", MaxHookTimeoutSeconds, false},
		{"over max rejected", MaxHookTimeoutSeconds + 1, true},
		{"negative rejected", -1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Hooks = &HooksConfig{PreRun: []HookConfig{{Command: "true", TimeoutSeconds: tc.seconds}}}
			err := ValidateRunConfig(c)
			if tc.wantErr {
				must.Error(err, "timeoutSeconds=%d must fail validation", tc.seconds)
			} else {
				must.NoError(err, "timeoutSeconds=%d must validate", tc.seconds)
			}
		})
	}
}

func TestValidateRunConfig_Hooks_NameBounds(t *testing.T) {
	t.Run("within bound accepted", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Hooks = &HooksConfig{PreRun: []HookConfig{{Command: "true", Name: strings.Repeat("a", maxHookNameBytes)}}}
		must.NoError(ValidateRunConfig(c), "64-byte name must validate")
	})
	t.Run("over bound rejected", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Hooks = &HooksConfig{PreRun: []HookConfig{{Command: "true", Name: strings.Repeat("a", maxHookNameBytes+1)}}}
		must.ErrorContains(ValidateRunConfig(c), "hooks.preRun[0].name must be <=")
	})
	t.Run("control byte rejected", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Hooks = &HooksConfig{PreRun: []HookConfig{{Command: "true", Name: "clone\x00repo"}}}
		must.ErrorContains(ValidateRunConfig(c), "non-printable character")
	})
}

func TestValidateRunConfig_Hooks_RunOnPreRunRejected(t *testing.T) {
	for _, runOn := range []string{"always", "success", "failure"} {
		t.Run(runOn, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Hooks = &HooksConfig{PreRun: []HookConfig{{Command: "true", RunOn: runOn}}}
			must.ErrorContains(ValidateRunConfig(c), "hooks.preRun[0].runOn must be empty on a preRun hook")
		})
	}
}

func TestValidateRunConfig_Hooks_RunOnPostRunClosedSet(t *testing.T) {
	t.Run("valid values accepted", func(t *testing.T) {
		for _, runOn := range []string{"", "always", "success", "failure"} {
			t.Run(runOn, func(t *testing.T) {
				must := require.New(t)
				c := validConfig()
				c.Hooks = &HooksConfig{PostRun: []HookConfig{{Command: "true", RunOn: runOn}}}
				must.NoError(ValidateRunConfig(c), "runOn=%q on a postRun hook must validate", runOn)
			})
		}
	})
	t.Run("unknown rejected", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Hooks = &HooksConfig{PostRun: []HookConfig{{Command: "true", RunOn: "sometimes"}}}
		must.ErrorContains(ValidateRunConfig(c), "hooks.postRun[0].runOn")
	})
}

func TestValidateRunConfig_Hooks_MaxHooksPerPhase(t *testing.T) {
	makeHooks := func(n int) []HookConfig {
		hooks := make([]HookConfig, n)
		for i := range hooks {
			hooks[i] = HookConfig{Command: "true"}
		}
		return hooks
	}
	t.Run("at limit accepted", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Hooks = &HooksConfig{PreRun: makeHooks(maxHooksPerPhase)}
		must.NoError(ValidateRunConfig(c))
	})
	t.Run("over limit rejected", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Hooks = &HooksConfig{PreRun: makeHooks(maxHooksPerPhase + 1)}
		must.ErrorContains(ValidateRunConfig(c), "hooks.preRun must have <=")
	})
	t.Run("postRun bounded independently", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Hooks = &HooksConfig{PostRun: makeHooks(maxHooksPerPhase + 1)}
		must.ErrorContains(ValidateRunConfig(c), "hooks.postRun must have <=")
	})
}

func TestValidateRunConfig_Hooks_PostRunTimeoutSumBounded(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Hooks = &HooksConfig{
		PostRun: []HookConfig{
			{Command: "true", TimeoutSeconds: 1000},
			{Command: "true", TimeoutSeconds: 900},
		},
	}
	must.ErrorContains(ValidateRunConfig(c), "sum of hooks.postRun effective timeouts")
}

func TestValidateRunConfig_Hooks_PostRunTimeoutSumAtBoundAccepted(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Hooks = &HooksConfig{
		PostRun: []HookConfig{
			{Command: "true", TimeoutSeconds: MaxHookTimeoutSeconds},
		},
	}
	must.NoError(ValidateRunConfig(c))
}

// The preRun timeout sum is only checked against the run's wall-clock
// Timeout with a slog.Warn, not a validation error. A run that will
// exceed its timeout is a runtime concern, not a config shape the
// harness should refuse to start.
func TestValidateRunConfig_Hooks_PreRunTimeoutSumWarnsOnly(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	timeout := 60
	c.Timeout = &timeout
	c.Hooks = &HooksConfig{
		PreRun: []HookConfig{{Command: "true", TimeoutSeconds: 300}},
	}
	must.NoError(ValidateRunConfig(c))
}

func TestEffectiveHookTimeout_DefaultsZeroAndKeepsExplicitValues(t *testing.T) {
	cases := []struct {
		name string
		cfg  HookConfig
		want int
	}{
		{"zero resolves to default", HookConfig{TimeoutSeconds: 0}, DefaultHookTimeoutSeconds},
		{"explicit value verbatim", HookConfig{TimeoutSeconds: 120}, 120},
		{"max value verbatim", HookConfig{TimeoutSeconds: MaxHookTimeoutSeconds}, MaxHookTimeoutSeconds},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, EffectiveHookTimeout(tc.cfg))
		})
	}
}

// Hook commands are operator config, like VerifierConfig.Command, so
// Redact leaves HooksConfig untouched. validateHooksConfig rejects
// "secret://" in a hook Command, so there is nothing to strip.
func TestRedact_HooksPassThrough(t *testing.T) {
	must := require.New(t)
	rc := RunConfig{
		Hooks: &HooksConfig{
			PreRun: []HookConfig{
				{Type: "command", Name: "clone", Command: "git clone https://example.com/repo .", TimeoutSeconds: 120},
			},
			PostRun: []HookConfig{
				{Command: "curl -X POST https://example.com/artifacts", RunOn: "success", ContinueOnError: true},
			},
		},
	}
	redacted := rc.Redact()

	must.NotNil(redacted.Hooks, "Redact must not drop Hooks")
	must.Len(redacted.Hooks.PreRun, 1)
	must.Equal("git clone https://example.com/repo .", redacted.Hooks.PreRun[0].Command, "PreRun must pass through unchanged")
	must.Len(redacted.Hooks.PostRun, 1)
	must.Equal("curl -X POST https://example.com/artifacts", redacted.Hooks.PostRun[0].Command, "PostRun must pass through unchanged")
	must.True(redacted.Hooks.PostRun[0].ContinueOnError, "ContinueOnError must pass through unchanged")
}

// Redact must leave the sandbox identity and git proxy blocks unchanged.
// Neither block holds a "secret://" reference, so there is nothing to
// strip, and the identity token itself never appears in RunConfig.
func TestRedact_SandboxIdentityAndGitProxyPassThrough(t *testing.T) {
	must := require.New(t)
	rc := RunConfig{
		Executor: ExecutorConfig{
			SandboxIdentity: &SandboxIdentityConfig{
				Source:   "control-plane",
				Audience: "https://haybale.internal",
				EnvVar:   "HAYBALE_TOKEN",
			},
			GitProxy: &GitProxyConfig{
				URL:         "http://haybale.internal:8466",
				Hosts:       []string{"github.com"},
				RewriteSsh:  true,
				TokenEnvVar: "HAYBALE_TOKEN",
			},
		},
	}
	redacted := rc.Redact()

	must.NotNil(redacted.Executor.SandboxIdentity, "Redact must not drop SandboxIdentity")
	must.Equal(*rc.Executor.SandboxIdentity, *redacted.Executor.SandboxIdentity, "SandboxIdentity must pass through unchanged")

	must.NotNil(redacted.Executor.GitProxy, "Redact must not drop GitProxy")
	must.Equal(rc.Executor.GitProxy.URL, redacted.Executor.GitProxy.URL)
	must.Equal(rc.Executor.GitProxy.RewriteSsh, redacted.Executor.GitProxy.RewriteSsh)
	must.Equal(rc.Executor.GitProxy.TokenEnvVar, redacted.Executor.GitProxy.TokenEnvVar)
	must.Equal([]string{"github.com"}, redacted.Executor.GitProxy.Hosts)
}

func TestHooksConfig_JSONRoundTrip(t *testing.T) {
	must := require.New(t)
	rc := RunConfig{
		Hooks: &HooksConfig{
			PreRun:  []HookConfig{{Command: "true"}},
			PostRun: []HookConfig{{Command: "true", RunOn: "failure"}},
		},
	}
	data, err := json.Marshal(rc)
	must.NoError(err)
	var got RunConfig
	must.NoError(json.Unmarshal(data, &got))
	must.NotNil(got.Hooks, "Hooks must survive JSON round-trip as non-nil")
	must.Len(got.Hooks.PreRun, 1)
	must.Equal("true", got.Hooks.PreRun[0].Command)
	must.Len(got.Hooks.PostRun, 1)
	must.Equal("failure", got.Hooks.PostRun[0].RunOn)
}

func TestRunConfig_HooksOmittedWhenNil(t *testing.T) {
	must := require.New(t)
	data, err := json.Marshal(RunConfig{})
	must.NoError(err)
	must.NotContains(string(data), `"hooks"`)
}

// TestBuiltInToolCapabilitySets_CoverCanonicalList guards against drift between
// the types-side capability mirrors (readCapabilityBuiltInTools, mutatingTools —
// hand-kept mirrors of harness/internal/core/factory.go's buildToolRegistry gates)
// and validBuiltInToolNames, the canonical closed set: their union must equal
// validBuiltInToolNames exactly, or a new tool could silently escape the
// none-executor fail-fast and the read-only-defaults filter.
func TestBuiltInToolCapabilitySets_CoverCanonicalList(t *testing.T) {
	check := assert.New(t)
	// Tools factory.go's buildToolRegistry registers without ever gating
	// on the executor's Capabilities() — see readCapabilityBuiltInTools's
	// doc comment. Kept in sync by hand alongside it.
	ungated := map[string]bool{"web_fetch": true, "spawn_agent": true}

	union := make(map[string]bool)
	addAll := func(set map[string]bool) {
		for name := range set {
			check.False(union[name], "tool %q appears in more than one of readCapabilityBuiltInTools/mutatingTools/the ungated list", name)
			union[name] = true
		}
	}
	addAll(readCapabilityBuiltInTools)
	addAll(mutatingTools)
	addAll(execCapabilityBuiltInTools)
	addAll(ungated)

	for name := range validBuiltInToolNames {
		check.True(union[name], "validBuiltInToolNames contains %q but none of readCapabilityBuiltInTools/mutatingTools/execCapabilityBuiltInTools/the ungated list accounts for it", name)
	}
	for name := range union {
		check.True(validBuiltInToolNames[name], "capability sets contain %q, which is not in validBuiltInToolNames", name)
	}
}

// TestDefaultReadOnlyBuiltInToolsForExecutor_None pins the exact
// capability-ungated subset the "none" branch returns, and proves it passes
// ValidateRunConfig for every read-only mode paired with executor.type="none".
func TestDefaultReadOnlyBuiltInToolsForExecutor_None(t *testing.T) {
	must := require.New(t)
	must.Equal([]string{"web_fetch", "spawn_agent"}, DefaultReadOnlyBuiltInToolsForExecutor("none"))

	for mode := range readOnlyModes {
		t.Run(mode, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Mode = mode
			c.Executor = ExecutorConfig{Type: "none"}
			c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
			c.Tools = ToolsConfig{BuiltIn: DefaultReadOnlyBuiltInToolsForExecutor("none")}
			must.NoError(ValidateRunConfig(c))
		})
	}
}

// TestDefaultReadOnlyBuiltInToolsForExecutor_OtherTypesUnchanged pins
// that every executor type besides "none" — including the empty string
// (defaults to "local") — gets the unfiltered DefaultReadOnlyBuiltInTools()
// list, so this helper only ever narrows behaviour for "none".
func TestDefaultReadOnlyBuiltInToolsForExecutor_OtherTypesUnchanged(t *testing.T) {
	for _, executorType := range []string{"", "local", "container", "k8s", "k8s-sandbox", "api"} {
		t.Run(executorType, func(t *testing.T) {
			must := require.New(t)
			must.Equal(DefaultReadOnlyBuiltInTools(), DefaultReadOnlyBuiltInToolsForExecutor(executorType))
		})
	}
}

// --- executor.type="none" ---

func TestValidateRunConfig_NoneExecutorAcceptedInExecutionMode(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Mode = "execution"
	c.Executor = ExecutorConfig{Type: "none"}
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_NoneExecutorAcceptedInReadOnlyMode(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Mode = "planning"
	c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
	c.Executor = ExecutorConfig{Type: "none"}
	c.Tools = ToolsConfig{BuiltIn: []string{"web_fetch"}}
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_NoneExecutorRejectsWorkspaceExportTo(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Executor = ExecutorConfig{Type: "none", WorkspaceExportTo: "gs://bucket/path"}
	must.ErrorContains(ValidateRunConfig(c), `not valid for executor.type="none"`)
}

// TestValidateRunConfig_NoneExecutorRejectsExecutionOnlyFields exercises the per-field
// rejection validateNoneExecutor enforces: every container/k8s/api-only
// field on ExecutorConfig is rejected outright when Type == "none", since
// the none executor has no execution surface to apply any of them to.
// Runtime and RegistryAllowlist are deliberately NOT in this table — they
// are covered by TestValidateRunConfig_NoneExecutorRuntimeAndRegistryAllowlistRejectedOnce,
// which pins that validateExecutorRuntime/validateExecutorRegistryAllowlist
// (not validateNoneExecutor) reject them for "none", exactly once each.
func TestValidateRunConfig_NoneExecutorRejectsExecutionOnlyFields(t *testing.T) {
	cases := []struct {
		name  string
		field string
		exec  ExecutorConfig
	}{
		{name: "workspace", field: "workspace", exec: ExecutorConfig{Type: "none", Workspace: "/ws"}},
		{name: "image", field: "image", exec: ExecutorConfig{Type: "none", Image: "img"}},
		{name: "vcs backend", field: "vcsBackend", exec: ExecutorConfig{Type: "none", VcsBackend: &VcsBackendConfig{Type: "github", Repo: "owner/repo"}}},
		{name: "network", field: "network", exec: ExecutorConfig{Type: "none", Network: &NetworkConfig{Mode: "none"}}},
		{name: "resources", field: "resources", exec: ExecutorConfig{Type: "none", Resources: &ResourceLimits{CPUs: 1}}},
		{name: "k8s namespace", field: "k8sNamespace", exec: ExecutorConfig{Type: "none", K8sNamespace: "ns"}},
		{name: "k8s kubeconfig", field: "k8sKubeconfig", exec: ExecutorConfig{Type: "none", K8sKubeconfig: "/home/u/.kube/config"}},
		{name: "k8s node selector", field: "k8sNodeSelector", exec: ExecutorConfig{Type: "none", K8sNodeSelector: map[string]string{"disktype": "ssd"}}},
		{name: "k8s service account", field: "k8sServiceAccount", exec: ExecutorConfig{Type: "none", K8sServiceAccount: "agent-sa"}},
		{name: "k8s egress proxy url", field: "k8sEgressProxyUrl", exec: ExecutorConfig{Type: "none", K8sEgressProxyURL: "http://stirrup-egress-proxy.ns.svc:8080"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Executor = tc.exec
			err := ValidateRunConfig(c)
			must.Error(err)
			want := fmt.Sprintf("executor.%s is not valid for executor.type=\"none\"", tc.field)
			must.Equal(1, strings.Count(err.Error(), want), "error: %v", err)
		})
	}
}

// TestValidateRunConfig_NoneExecutorRuntimeAndRegistryAllowlistRejectedOnce pins
// that Runtime and RegistryAllowlist are rejected for executor.type="none" by the
// generic validateExecutorRuntime / validateExecutorRegistryAllowlist validators,
// not validateNoneExecutor — so setting either produces exactly one error, not two.
func TestValidateRunConfig_NoneExecutorRuntimeAndRegistryAllowlistRejectedOnce(t *testing.T) {
	t.Run("runtime", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Executor = ExecutorConfig{Type: "none", Runtime: "runc"}
		err := ValidateRunConfig(c)
		must.Error(err)
		want := `executor.runtime is only valid when executor.type is "container", "k8s", or "k8s-sandbox"`
		must.Equal(1, strings.Count(err.Error(), want), "error: %v", err)
		must.NotContains(err.Error(), `executor.runtime is not valid for executor.type="none"`, "no duplicate none-specific error")
	})
	t.Run("registryAllowlist", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Executor = ExecutorConfig{Type: "none", RegistryAllowlist: []string{"ghcr.io/*"}}
		err := ValidateRunConfig(c)
		must.Error(err)
		want := `executor.registryAllowlist is only valid when executor.type is "container"`
		must.Equal(1, strings.Count(err.Error(), want), "error: %v", err)
		must.NotContains(err.Error(), `executor.registryAllowlist is not valid for executor.type="none"`, "no duplicate none-specific error")
	})
}

func TestValidateRunConfig_NoneExecutorRejectsDeterministicGit(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Executor = ExecutorConfig{Type: "none"}
	c.GitStrategy = GitStrategyConfig{Type: "deterministic"}
	must.ErrorContains(ValidateRunConfig(c), `gitStrategy.type="deterministic" is not valid with executor.type="none"`)
}

// TestValidateRunConfig_DeterministicGitRequiresExecCapableExecutor pins
// that the deterministic strategy, which runs git inside the executor, is
// accepted exactly for the executors that can run commands.
func TestValidateRunConfig_DeterministicGitRequiresExecCapableExecutor(t *testing.T) {
	cases := []struct {
		name   string
		exec   ExecutorConfig
		reject bool
	}{
		{"unset", ExecutorConfig{}, false},
		{"local", ExecutorConfig{Type: "local"}, false},
		{"container", ExecutorConfig{Type: "container", Image: "img", Network: &NetworkConfig{Mode: "none"}}, false},
		{"k8s", ExecutorConfig{Type: "k8s", Image: "img", K8sNamespace: "ns", Network: &NetworkConfig{Mode: "none"}}, false},
		{"k8s-sandbox", ExecutorConfig{Type: "k8s-sandbox", Image: "img", K8sNamespace: "ns", Network: &NetworkConfig{Mode: "none"}}, false},
		{"api", ExecutorConfig{Type: "api"}, true},
		{"none", ExecutorConfig{Type: "none"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Executor = tc.exec
			c.GitStrategy = GitStrategyConfig{Type: "deterministic"}
			err := ValidateRunConfig(c)
			if tc.reject {
				must.ErrorContains(err, fmt.Sprintf(`gitStrategy.type="deterministic" is not valid with executor.type=%q`, tc.exec.Type))
			} else {
				must.NoError(err)
			}
		})
	}
}

func TestValidateRunConfig_NoneExecutorAllowsNoneGitStrategy(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Executor = ExecutorConfig{Type: "none"}
	c.GitStrategy = GitStrategyConfig{Type: "none"}
	must.NoError(ValidateRunConfig(c))
}

// TestValidateRunConfig_NoneExecutorBuiltInToolFailFast exercises
// validateNoneExecutorTools: an explicit tools.builtIn entry that needs a
// filesystem or shell capability is rejected at config-load time instead
// of silently vanishing from the tool registry mid-run; web_fetch is not
// capability-gated so it must stay accepted.
func TestValidateRunConfig_NoneExecutorBuiltInToolFailFast(t *testing.T) {
	cases := []struct {
		tool    string
		wantErr bool
	}{
		{tool: "run_command", wantErr: true},
		{tool: "write_file", wantErr: true},
		{tool: "read_file", wantErr: true},
		{tool: "edit_file", wantErr: true},
		{tool: "web_fetch", wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Executor = ExecutorConfig{Type: "none"}
			c.Tools = ToolsConfig{BuiltIn: []string{tc.tool}}
			err := ValidateRunConfig(c)
			if tc.wantErr {
				must.ErrorContains(err, fmt.Sprintf("tools.builtIn entry %q requires an execution capability", tc.tool))
			} else {
				must.NoError(err)
			}
		})
	}
}

// --- SandboxIdentityConfig / GitProxyConfig (issue #516 Part B) ---

// sandboxIdentityValidConfig returns the minimal RunConfig the
// sandboxIdentity/gitProxy validation rules validate cleanly against:
// container executor, grpc transport, a matching envVar pair. Individual
// tests perturb one field at a time.
func sandboxIdentityValidConfig() *RunConfig {
	c := validConfig()
	c.Transport = TransportConfig{Type: "grpc"}
	c.Executor = ExecutorConfig{
		Type: "container",
		SandboxIdentity: &SandboxIdentityConfig{
			Source:   "control-plane",
			Audience: "https://haybale.internal",
			EnvVar:   "HAYBALE_TOKEN",
		},
		GitProxy: &GitProxyConfig{
			URL:         "http://haybale.internal:8466",
			Hosts:       []string{"github.com"},
			RewriteSsh:  true,
			TokenEnvVar: "HAYBALE_TOKEN",
		},
	}
	return c
}

// A RunConfig without SandboxIdentity or GitProxy must validate for any
// Transport or Executor.Type, and validation must not populate either block.
func TestValidateRunConfig_SandboxIdentityDefaultConfigStillValid(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Transport = TransportConfig{Type: "grpc"}
	c.Executor = ExecutorConfig{Type: "container"}
	must.NoError(ValidateRunConfig(c))
	must.Nil(c.Executor.SandboxIdentity, "ValidateRunConfig must not inject a SandboxIdentity block")
	must.Nil(c.Executor.GitProxy, "ValidateRunConfig must not inject a GitProxy block")

	// The bare default config (stdio transport, unset executor type) must
	// also validate, so the rules never fire on a missing transport or executor.
	must.NoError(ValidateRunConfig(validConfig()))
}

// A SandboxIdentity block without a GitProxy block is a valid shape,
// because the sandbox identity rules do not require GitProxy.
func TestValidateRunConfig_SandboxIdentityAloneStillValid(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Transport = TransportConfig{Type: "grpc"}
	c.Executor = ExecutorConfig{
		Type: "container",
		SandboxIdentity: &SandboxIdentityConfig{
			Source: "control-plane",
		},
	}
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_SandboxIdentityRequiresGRPC(t *testing.T) {
	cases := []struct {
		name      string
		transport string
		wantErr   bool
	}{
		{"grpc passes", "grpc", false},
		{"stdio fails", "stdio", true},
		{"empty transport fails", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := sandboxIdentityValidConfig()
			c.Transport = TransportConfig{Type: tc.transport}
			err := ValidateRunConfig(c)
			if tc.wantErr {
				must.ErrorContains(err, "requires transport=grpc")
				return
			}
			must.NoError(err)
		})
	}
}

// TestValidateRunConfig_GitProxyRequiresGRPC checks that a gitProxy block
// without sandboxIdentity still requires transport=grpc. Only the transport
// error is asserted, because the sandboxIdentity rule also fails here.
func TestValidateRunConfig_GitProxyRequiresGRPC(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Transport = TransportConfig{Type: "stdio"}
	c.Executor = ExecutorConfig{
		Type:     "container",
		GitProxy: &GitProxyConfig{URL: "http://haybale.internal:8466", TokenEnvVar: "HAYBALE_TOKEN"},
	}
	must.ErrorContains(ValidateRunConfig(c), "requires transport=grpc")
}

func TestValidateRunConfig_SandboxIdentityExecutorTypeGating(t *testing.T) {
	cases := []struct {
		name     string
		executor ExecutorConfig
		wantErr  bool
	}{
		{
			name:     "container passes",
			executor: ExecutorConfig{Type: "container"},
			wantErr:  false,
		},
		{
			name: "k8s passes",
			executor: ExecutorConfig{
				Type:         "k8s",
				Image:        "ghcr.io/rxbynerd/stirrup:latest",
				K8sNamespace: "default",
				Network:      &NetworkConfig{Mode: "none"},
			},
			wantErr: false,
		},
		{
			name: "k8s-sandbox passes",
			executor: ExecutorConfig{
				Type:         "k8s-sandbox",
				Image:        "ghcr.io/rxbynerd/stirrup:latest",
				K8sNamespace: "default",
				Network:      &NetworkConfig{Mode: "none"},
			},
			wantErr: false,
		},
		{
			name:     "local deferred fails",
			executor: ExecutorConfig{Type: "local"},
			wantErr:  true,
		},
		{
			name:     "unset type fails",
			executor: ExecutorConfig{},
			wantErr:  true,
		},
		{
			name:     "api fails",
			executor: ExecutorConfig{Type: "api", VcsBackend: &VcsBackendConfig{Type: "github", Repo: "rxbynerd/stirrup"}},
			wantErr:  true,
		},
		{
			name:     "none fails",
			executor: ExecutorConfig{Type: "none"},
			wantErr:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Transport = TransportConfig{Type: "grpc"}
			tc.executor.SandboxIdentity = &SandboxIdentityConfig{Source: "control-plane", EnvVar: "HAYBALE_TOKEN"}
			tc.executor.GitProxy = &GitProxyConfig{URL: "http://haybale.internal:8466", Hosts: []string{"github.com"}, TokenEnvVar: "HAYBALE_TOKEN"}
			c.Executor = tc.executor
			err := ValidateRunConfig(c)
			if tc.wantErr {
				must.ErrorContains(err, `is only valid for executor.type "container", "k8s", or "k8s-sandbox"`)
				return
			}
			must.NoError(err)
		})
	}
}

func TestValidateRunConfig_SandboxIdentitySourceClosedSet(t *testing.T) {
	cases := []struct {
		name    string
		source  string
		wantErr bool
	}{
		{"control plane passes", "control-plane", false},
		{"empty fails", "", true},
		{"unknown fails", "self-signed", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := sandboxIdentityValidConfig()
			c.Executor.SandboxIdentity.Source = tc.source
			err := ValidateRunConfig(c)
			if tc.wantErr {
				must.ErrorContains(err, "unsupported executor.sandboxIdentity.source")
				return
			}
			must.NoError(err)
		})
	}
}

// TestValidateRunConfig_SandboxIdentityEnvVarShape pins the injection-shape
// guard on SandboxIdentityConfig.EnvVar. The name, not the token value, is
// interpolated into a double-quoted shell credential-helper string, so a
// non-empty EnvVar must match posixEnvVarNamePattern. This is a charset check
// only; it does not reject valid but risky names such as PATH.
func TestValidateRunConfig_SandboxIdentityEnvVarShape(t *testing.T) {
	cases := []struct {
		name    string
		envVar  string
		wantErr bool
	}{
		{"valid", "HAYBALE_TOKEN", false},
		{"empty uses default", "", false},
		{"space", "HAYBALE TOKEN", true},
		{"equals sign", "TOKEN=x", true},
		{"leading digit", "1TOKEN", true},
		{"shell metacharacter", `HAYBALE"; rm -rf /; echo "`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := sandboxIdentityValidConfig()
			c.Executor.GitProxy = nil
			c.Executor.SandboxIdentity.EnvVar = tc.envVar
			err := ValidateRunConfig(c)
			if tc.wantErr {
				must.ErrorContains(err, "executor.sandboxIdentity.envVar")
				must.ErrorContains(err, "must be a valid POSIX environment variable name")
				return
			}
			must.NoError(err)
		})
	}
}

// TestValidateRunConfig_GitProxyRequiresSandboxIdentity pins the cross-field
// rule documented on GitProxyConfig. The only token source is control-plane
// issuance through SandboxIdentity, so a gitProxy block without it would name
// an env var that is never populated. Such configs are rejected, not ignored.
func TestValidateRunConfig_GitProxyRequiresSandboxIdentity(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Transport = TransportConfig{Type: "grpc"}
	c.Executor = ExecutorConfig{
		Type:     "container",
		GitProxy: &GitProxyConfig{URL: "http://haybale.internal:8466", TokenEnvVar: "HAYBALE_TOKEN"},
	}
	must.ErrorContains(ValidateRunConfig(c), "executor.gitProxy requires executor.sandboxIdentity to be set")
}

func TestValidateRunConfig_GitProxyTokenEnvVarConsistency(t *testing.T) {
	cases := []struct {
		name        string
		identityEnv string
		gitProxyEnv string
		wantErr     bool
	}{
		{"explicit match", "HAYBALE_TOKEN", "HAYBALE_TOKEN", false},
		{"both default match", "", "", false},
		{"identity explicit gitproxy default mismatch", "CUSTOM_TOKEN", "", true},
		{"explicit mismatch", "HAYBALE_TOKEN", "OTHER_TOKEN", true},
		{"identity default gitproxy explicit match", "", "HAYBALE_TOKEN", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := sandboxIdentityValidConfig()
			c.Executor.SandboxIdentity.EnvVar = tc.identityEnv
			c.Executor.GitProxy.TokenEnvVar = tc.gitProxyEnv
			err := ValidateRunConfig(c)
			if tc.wantErr {
				must.ErrorContains(err, "must match the effective executor.sandboxIdentity.envVar")
				return
			}
			must.NoError(err)
		})
	}
}

// TestValidateRunConfig_GitProxyURLValidation pins that gitProxy.url is
// required and must be an absolute http(s) URL with a host. The shape check
// mirrors validateGuardRailEndpoint for GuardRailConfig.Endpoint.
func TestValidateRunConfig_GitProxyURLValidation(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr string // substring; empty means the URL should pass
	}{
		{"empty url required", "", "executor.gitProxy.url is required when executor.gitProxy is set"},
		{"no scheme", "not-a-url", "must use scheme http or https"},
		{"unparseable url", "://bad", "must be a valid URL"},
		{"non http scheme", "ftp://host:21", "must use scheme http or https"},
		{"valid url", "http://haybale.internal:8466", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := sandboxIdentityValidConfig()
			c.Executor.GitProxy.URL = tc.url
			err := ValidateRunConfig(c)
			if tc.wantErr == "" {
				must.NoError(err)
				return
			}
			must.ErrorContains(err, tc.wantErr)
		})
	}
}

// TestValidateRunConfig_GitProxyTokenEnvVarShape applies the POSIX env var
// name guard from TestValidateRunConfig_SandboxIdentityEnvVarShape to
// GitProxyConfig.TokenEnvVar.
func TestValidateRunConfig_GitProxyTokenEnvVarShape(t *testing.T) {
	cases := []struct {
		name        string
		tokenEnvVar string
		wantErr     bool
	}{
		{"valid", "HAYBALE_TOKEN", false},
		{"empty uses default", "", false},
		{"space", "HAYBALE TOKEN", true},
		{"equals sign", "TOKEN=x", true},
		{"leading digit", "1TOKEN", true},
		{"shell metacharacter", `HAYBALE"; rm -rf /; echo "`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := sandboxIdentityValidConfig()
			c.Executor.GitProxy.TokenEnvVar = tc.tokenEnvVar
			err := ValidateRunConfig(c)
			if tc.wantErr {
				must.ErrorContains(err, "executor.gitProxy.tokenEnvVar")
				must.ErrorContains(err, "must be a valid POSIX environment variable name")
				return
			}
			must.NoError(err)
		})
	}
}

// TestValidateRunConfig_GitProxyHostsRequired pins that a configured gitProxy
// block names at least one host. Hosts drives how many rewrite rules are
// composed, so an empty list would validate cleanly while doing nothing.
func TestValidateRunConfig_GitProxyHostsRequired(t *testing.T) {
	cases := []struct {
		name  string
		hosts []string
	}{
		{"nil hosts", nil},
		{"empty hosts", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := sandboxIdentityValidConfig()
			c.Executor.GitProxy.Hosts = tc.hosts
			must.ErrorContains(ValidateRunConfig(c), "executor.gitProxy.hosts must contain at least one host when executor.gitProxy is set")
		})
	}
}

func TestValidateRunConfig_GitProxyAllowlistWarning(t *testing.T) {
	const wantMsg = "executor.gitProxy.url host:port is not present in executor.network.allowlist"
	cases := []struct {
		name      string
		mode      string
		allowlist []string
		// url overrides the default gitProxy.url from sandboxIdentityValidConfig
		// when non-empty, to exercise the port-defaulting branches.
		url      string
		wantWarn bool
	}{
		{"exact host and port no warning", "allowlist", []string{"haybale.internal:8466"}, "", false},
		{"missing from allowlist warns", "allowlist", []string{"other.example.com:8466"}, "", true},
		{"empty allowlist warns", "allowlist", nil, "", true},
		{"none mode no warning", "none", nil, "", false},
		// An http:// URL without a port defaults to port 80, which the allowlist entry matches.
		{"http default port matches port 80", "allowlist", []string{"haybale.internal:80"}, "http://haybale.internal", false},
		// An https:// URL without a port defaults to 443. A bare-host allowlist entry matches it.
		{"https default port matches bare host entry", "allowlist", []string{"haybale.internal"}, "https://haybale.internal", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var logBuf bytes.Buffer
			originalLogger := slog.Default()
			t.Cleanup(func() { slog.SetDefault(originalLogger) })
			slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))

			c := sandboxIdentityValidConfig()
			if tc.url != "" {
				c.Executor.GitProxy.URL = tc.url
			}
			c.Executor.Network = &NetworkConfig{Mode: tc.mode, Allowlist: tc.allowlist}
			must.NoError(ValidateRunConfig(c), "allowlist warning path must not produce a hard error")

			logs := logBuf.String()
			if tc.wantWarn {
				must.Contains(logs, "level=WARN")
				must.Contains(logs, wantMsg)
				return
			}
			must.NotContains(logs, wantMsg)
		})
	}
}

// TestSandboxIdentityAndGitProxyConfig_JSONRoundTrip pins that every field of
// SandboxIdentityConfig and GitProxyConfig survives a JSON round trip when
// populated.
func TestSandboxIdentityAndGitProxyConfig_JSONRoundTrip(t *testing.T) {
	must := require.New(t)
	rc := RunConfig{
		Executor: ExecutorConfig{
			SandboxIdentity: &SandboxIdentityConfig{
				Source:   "control-plane",
				Audience: "https://haybale.internal",
				EnvVar:   "HAYBALE_TOKEN",
			},
			GitProxy: &GitProxyConfig{
				URL:         "http://haybale.internal:8466",
				Hosts:       []string{"github.com"},
				RewriteSsh:  true,
				TokenEnvVar: "HAYBALE_TOKEN",
			},
		},
	}
	data, err := json.Marshal(rc)
	must.NoError(err)
	var got RunConfig
	must.NoError(json.Unmarshal(data, &got))

	must.NotNil(got.Executor.SandboxIdentity)
	must.Equal(*rc.Executor.SandboxIdentity, *got.Executor.SandboxIdentity)

	must.NotNil(got.Executor.GitProxy)
	must.Equal(rc.Executor.GitProxy.URL, got.Executor.GitProxy.URL)
	must.Equal(rc.Executor.GitProxy.Hosts, got.Executor.GitProxy.Hosts)
	must.Equal(rc.Executor.GitProxy.RewriteSsh, got.Executor.GitProxy.RewriteSsh)
	must.Equal(rc.Executor.GitProxy.TokenEnvVar, got.Executor.GitProxy.TokenEnvVar)
}

// TestRunConfig_SandboxIdentityGitProxyOmitEmptyFields pins the omitempty JSON
// tags on SandboxIdentityConfig.Audience and EnvVar and on GitProxyConfig.Hosts,
// RewriteSsh and TokenEnvVar. Source and URL have no omitempty tag, so they
// round-trip as empty-string keys.
func TestRunConfig_SandboxIdentityGitProxyOmitEmptyFields(t *testing.T) {
	must := require.New(t)
	rc := RunConfig{
		Executor: ExecutorConfig{
			SandboxIdentity: &SandboxIdentityConfig{},
			GitProxy:        &GitProxyConfig{},
		},
	}
	data, err := json.Marshal(rc)
	must.NoError(err)
	s := string(data)
	for _, absentKey := range []string{`"audience"`, `"envVar"`, `"hosts"`, `"rewriteSsh"`, `"tokenEnvVar"`} {
		must.NotContains(s, absentKey)
	}
	must.Contains(s, `"source":""`)
	must.Contains(s, `"url":""`)

	var got RunConfig
	must.NoError(json.Unmarshal(data, &got))
	must.NotNil(got.Executor.SandboxIdentity)
	must.Empty(got.Executor.SandboxIdentity.Source)
	must.NotNil(got.Executor.GitProxy)
	must.Empty(got.Executor.GitProxy.URL)
}

// TestSandboxIdentityConfig_EffectiveEnvVar_NilReceiver pins the documented
// contract on EffectiveEnvVar: a nil cfg returns the default so callers need
// not nil-check first.
func TestSandboxIdentityConfig_EffectiveEnvVar_NilReceiver(t *testing.T) {
	must := require.New(t)
	var cfg *SandboxIdentityConfig
	must.Equal(DefaultSandboxIdentityEnvVar, cfg.EffectiveEnvVar())
}

// TestGitProxyConfig_EffectiveTokenEnvVar_NilReceiver pins the documented
// contract on EffectiveTokenEnvVar: a nil cfg returns the default so callers
// need not nil-check first.
func TestGitProxyConfig_EffectiveTokenEnvVar_NilReceiver(t *testing.T) {
	must := require.New(t)
	var cfg *GitProxyConfig
	must.Equal(DefaultSandboxIdentityEnvVar, cfg.EffectiveTokenEnvVar())
}

// debugStyleTokens are the substrings a "debug"/"trace-wire"-shaped
// RunConfig field or JSON key would contain, in every casing convention
// this codebase's json tags use (camelCase, per the RunConfig field
// list) plus snake_case and kebab-case as a defensive catch-all against
// a future field named more loosely. Checked case-insensitively.
var debugStyleTokens = []string{"debug", "tracewire", "trace_wire", "trace-wire"}

// TestRunConfig_DoesNotExposeDebugFields guards the CLI-only posture of
// --debug and --trace-wire. A RunConfig is the wire shape a control-plane
// submission can construct, so it must never carry a field that requests
// either behaviour. The flags are registered on harnessCmd.Flags() outside
// BuildRunConfig, so this test catches a matching field added by mistake.
//
// Two checks run: a reflect walk over every exported field in the RunConfig
// type graph, and a JSON substring check over a populated instance.
func TestRunConfig_DoesNotExposeDebugFields(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	visited := map[reflect.Type]bool{}
	var walk func(reflect.Type, string)
	walk = func(rt reflect.Type, path string) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Array {
			rt = rt.Elem()
		}
		if rt.Kind() == reflect.Map {
			walk(rt.Key(), path+" (map key)")
			walk(rt.Elem(), path+" (map value)")
			return
		}
		if rt.Kind() != reflect.Struct {
			return
		}
		if visited[rt] {
			return
		}
		visited[rt] = true
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if f.PkgPath != "" {
				continue // unexported field: not part of the wire shape
			}
			fieldPath := path + "." + f.Name
			lower := strings.ToLower(f.Name)
			for _, tok := range debugStyleTokens {
				check.NotContains(lower, tok, "RunConfig field %s looks debug-related; --debug and --trace-wire must stay CLI-only", fieldPath)
			}
			walk(f.Type, fieldPath)
		}
	}
	walk(reflect.TypeOf(RunConfig{}), "RunConfig")

	// Catches a json tag set to a name the reflect walk would not match.
	rc := RunConfig{
		Mode:     "execution",
		Prompt:   "test",
		Provider: ProviderConfig{Type: "anthropic"},
	}
	data, err := json.Marshal(rc)
	must.NoError(err)
	lowerJSON := strings.ToLower(string(data))
	for _, tok := range debugStyleTokens {
		check.NotContains(lowerJSON, `"`+tok, "marshalled RunConfig JSON has a %q-shaped key", tok)
	}
}

func controlPlaneToolFixture(name string) ControlPlaneToolConfig {
	return ControlPlaneToolConfig{
		Name:        name,
		Description: "fixture",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
	}
}

func TestValidateRunConfig_ControlPlaneTools_RequiresGRPC(t *testing.T) {
	cases := []struct {
		name      string
		transport string
		wantErr   bool
	}{
		{"grpc passes", "grpc", false},
		{"stdio fails", "stdio", true},
		{"empty transport fails", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Transport = TransportConfig{Type: tc.transport}
			c.Tools.ControlPlane = []ControlPlaneToolConfig{controlPlaneToolFixture("search_memory")}
			err := ValidateRunConfig(c)
			if tc.wantErr {
				must.ErrorContains(err, "tools.controlPlane requires transport=grpc")
				return
			}
			must.NoError(err)
		})
	}
}

func TestValidateRunConfig_ControlPlaneTools_ReadOnlyModesAdmit(t *testing.T) {
	for _, mode := range []string{"planning", "review", "research", "toil"} {
		t.Run(mode, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Mode = mode
			c.Transport = TransportConfig{Type: "grpc"}
			c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
			c.Tools = ToolsConfig{
				BuiltIn:      []string{"read_file"},
				ControlPlane: []ControlPlaneToolConfig{controlPlaneToolFixture("save_memory")},
			}
			must.NoError(ValidateRunConfig(c))
		})
	}
}

func TestValidateRunConfig_ControlPlaneTools_EntryRules(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*ControlPlaneToolConfig)
		wantErr string
	}{
		{"missing name", func(ct *ControlPlaneToolConfig) { ct.Name = "" }, "tools.controlPlane[0].name is required"},
		{"bad name chars", func(ct *ControlPlaneToolConfig) { ct.Name = "has space" }, "must match"},
		{"leading digit", func(ct *ControlPlaneToolConfig) { ct.Name = "1tool" }, "must match"},
		{"too long", func(ct *ControlPlaneToolConfig) { ct.Name = strings.Repeat("a", 65) }, "must match"},
		{"builtin collision", func(ct *ControlPlaneToolConfig) { ct.Name = "read_file" }, "collides with a built-in tool"},
		{"mcp prefix", func(ct *ControlPlaneToolConfig) { ct.Name = "mcp_search_lookup" }, "mcp_ prefix"},
		{"missing description", func(ct *ControlPlaneToolConfig) { ct.Description = "  " }, "description is required"},
		{"missing schema", func(ct *ControlPlaneToolConfig) { ct.InputSchema = nil }, "inputSchema is required"},
		{"schema not object", func(ct *ControlPlaneToolConfig) { ct.InputSchema = json.RawMessage(`["x"]`) }, "must be a JSON Schema object"},
		{"schema invalid json", func(ct *ControlPlaneToolConfig) { ct.InputSchema = json.RawMessage(`{`) }, "must be a JSON Schema object"},
		{"schema wrong type", func(ct *ControlPlaneToolConfig) { ct.InputSchema = json.RawMessage(`{"type":"string"}`) }, `must declare "type": "object"`},
		{"negative timeout", func(ct *ControlPlaneToolConfig) { ct.TimeoutSeconds = -1 }, "timeoutSeconds must be between 0 and 3600"},
		{"timeout over cap", func(ct *ControlPlaneToolConfig) { ct.TimeoutSeconds = 3601 }, "timeoutSeconds must be between 0 and 3600"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Transport = TransportConfig{Type: "grpc"}
			ct := controlPlaneToolFixture("search_memory")
			tc.mutate(&ct)
			c.Tools.ControlPlane = []ControlPlaneToolConfig{ct}
			err := ValidateRunConfig(c)
			must.ErrorContains(err, tc.wantErr)
		})
	}

	t.Run("valid entries pass", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.Transport = TransportConfig{Type: "grpc"}
		full := controlPlaneToolFixture("save-memory_v2")
		full.TimeoutSeconds = 3600
		full.RequiresApproval = true
		c.Tools.ControlPlane = []ControlPlaneToolConfig{controlPlaneToolFixture("_search"), full}
		must.NoError(ValidateRunConfig(c))
	})
}

func TestValidateRunConfig_ControlPlaneTools_DuplicateNames(t *testing.T) {
	must := require.New(t)
	c := validConfig()
	c.Transport = TransportConfig{Type: "grpc"}
	c.Tools.ControlPlane = []ControlPlaneToolConfig{
		controlPlaneToolFixture("search_memory"),
		controlPlaneToolFixture("search_memory"),
	}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, `tools.controlPlane[1].name "search_memory" duplicates tools.controlPlane[0]`)
}

func TestRunConfig_ControlPlaneTools_JSONRoundTrip(t *testing.T) {
	must := require.New(t)
	raw := `{"tools":{"builtIn":["read_file"],"controlPlane":[{"name":"search_memory","description":"d","inputSchema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]},"timeoutSeconds":30,"requiresApproval":true}]}}`
	var c RunConfig
	must.NoError(json.Unmarshal([]byte(raw), &c))
	must.Len(c.Tools.ControlPlane, 1)
	ct := c.Tools.ControlPlane[0]
	must.Equal("search_memory", ct.Name)
	must.Equal(30, ct.TimeoutSeconds)
	must.True(ct.RequiresApproval)
	out, err := json.Marshal(c.Tools)
	must.NoError(err)
	must.Contains(string(out), `"inputSchema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`,
		"inputSchema must round-trip verbatim")
}

func TestToolsConfig_EffectiveMaxResultBytes(t *testing.T) {
	t.Run("unset uses the default", func(t *testing.T) {
		must := require.New(t)
		must.Equal(100<<10, (ToolsConfig{}).EffectiveMaxResultBytes())
	})
	t.Run("explicit value is returned", func(t *testing.T) {
		must := require.New(t)
		must.Equal(4096, (ToolsConfig{MaxResultBytes: 4096}).EffectiveMaxResultBytes())
	})
}

func TestValidateRunConfig_ToolsMaxResultBytes(t *testing.T) {
	tests := []struct {
		name    string
		value   int
		wantErr string
	}{
		{name: "unset selects default", value: 0},
		{name: "minimum", value: MinToolMaxResultBytes},
		{name: "maximum", value: MaxToolMaxResultBytes},
		{name: "negative", value: -1, wantErr: "tools.maxResultBytes must not be negative"},
		{name: "below minimum", value: MinToolMaxResultBytes - 1, wantErr: "tools.maxResultBytes must be at least 1024"},
		{name: "above maximum", value: MaxToolMaxResultBytes + 1, wantErr: "tools.maxResultBytes exceeds maximum of 1048576"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			c := validConfig()
			c.Tools.MaxResultBytes = tt.value
			err := ValidateRunConfig(c)
			if tt.wantErr == "" {
				must.NoError(err)
				return
			}
			must.ErrorContains(err, tt.wantErr)
		})
	}
}

func TestRedact_MCPServerOptionalPreserved(t *testing.T) {
	must := require.New(t)
	rc := RunConfig{
		Tools: ToolsConfig{
			MCPServers: []MCPServerConfig{
				{Name: "a", URI: "http://a", Optional: true},
				{Name: "b", URI: "http://b"},
			},
		},
	}
	redacted := rc.Redact()
	must.Equal([]bool{true, false}, []bool{
		redacted.Tools.MCPServers[0].Optional,
		redacted.Tools.MCPServers[1].Optional,
	})
}

func TestMCPServerConfig_OptionalJSON(t *testing.T) {
	must := require.New(t)
	raw, err := json.Marshal(MCPServerConfig{Name: "a", URI: "http://a"})
	must.NoError(err)
	must.NotContains(string(raw), "optional", "a required server must omit the optional key")

	var decoded MCPServerConfig
	must.NoError(json.Unmarshal([]byte(`{"name":"a","uri":"http://a","optional":true}`), &decoded))
	must.True(decoded.Optional, "optional:true must decode")
}

// TestRunConfig_WindDown_WireShapeAndAccessor pins the windDown block's wire
// shape and the WindDownEnabled accessor: absent and enabled:false are both
// off, and an enabled block validates.
func TestRunConfig_WindDown_WireShapeAndAccessor(t *testing.T) {
	cases := []struct {
		name string
		json string
		want bool
	}{
		{"absent", `{}`, false},
		{"empty block", `{"windDown":{}}`, false},
		{"disabled", `{"windDown":{"enabled":false}}`, false},
		{"enabled", `{"windDown":{"enabled":true}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var rc RunConfig
			must.NoError(json.Unmarshal([]byte(tc.json), &rc))
			must.Equal(tc.want, rc.WindDownEnabled())
		})
	}

	t.Run("nil config is off", func(t *testing.T) {
		must := require.New(t)
		must.False((*RunConfig)(nil).WindDownEnabled())
	})

	t.Run("enabled block validates", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.WindDown = &WindDownConfig{Enabled: true}
		must.NoError(ValidateRunConfig(c))
	})

	t.Run("nil block is omitted from JSON", func(t *testing.T) {
		must := require.New(t)
		c := validConfig()
		c.WindDown = nil
		data, err := json.Marshal(c)
		must.NoError(err)
		must.NotContains(string(data), "windDown")
	})
}

func TestResolveReasoningSummary_DefaultsPerProvider(t *testing.T) {
	cases := []struct {
		name         string
		providerType string
		configured   string
		want         string
	}{
		{"anthropic defaults to auto", "anthropic", "", ReasoningSummaryAuto},
		{"gemini defaults to auto", "gemini", "", ReasoningSummaryAuto},
		{"openai-compatible defaults to auto", "openai-compatible", "", ReasoningSummaryAuto},
		{"openai-responses defaults to off", "openai-responses", "", ReasoningSummaryOff},
		{"openai-responses keeps configured auto", "openai-responses", ReasoningSummaryAuto, ReasoningSummaryAuto},
		{"anthropic keeps configured off", "anthropic", ReasoningSummaryOff, ReasoningSummaryOff},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, ResolveReasoningSummary(tc.providerType, tc.configured))
		})
	}
}
