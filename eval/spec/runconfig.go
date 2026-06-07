package spec

import (
	"fmt"
	"strings"

	"github.com/rxbynerd/stirrup/types"
)

// runConfigSpec mirrors types.RunConfig for HCL decoding. Field names
// here use snake_case to match the codebase's convention; tag-based
// gohcl decoding rejects unknown attributes so typos surface at parse
// time rather than as silent zero values.
//
// Not every RunConfig field is exposed yet. The following are
// deliberately omitted from chunk 2 and tracked for follow-up:
//
//   - Providers map[string]ProviderConfig (named multi-provider lineup)
//   - DynamicContext map[string]DynamicContextValue
//   - GuardRailConfig.CustomCriteria map[string]string
//   - TransportConfig (the eval runner is stdio-only)
//   - ToolsConfig.MCPServers (slice of structs)
//
// Maps-of-structs and slices-of-structs are awkward to express in
// gohcl's attribute model; they need either an hcl block or a
// dedicated cty path. Adding them is purely additive.
type runConfigSpec struct {
	RunID                string                `hcl:"run_id,optional"`
	Mode                 string                `hcl:"mode,optional"`
	SessionName          string                `hcl:"session_name,optional"`
	Prompt               string                `hcl:"prompt,optional"`
	MaxTurns             *int                  `hcl:"max_turns,optional"`
	MaxTokenBudget       *int                  `hcl:"max_token_budget,optional"`
	MaxCostBudget        *float64              `hcl:"max_cost_budget,optional"`
	Timeout              *int                  `hcl:"timeout,optional"`
	FollowUpGrace        *int                  `hcl:"follow_up_grace,optional"`
	LogLevel             string                `hcl:"log_level,optional"`
	SystemPromptOverride string                `hcl:"system_prompt_override,optional"`
	SensitiveData        *bool                 `hcl:"sensitive_data,optional"`
	Provider             *providerSpec         `hcl:"provider,block"`
	ModelRouter          *modelRouterSpec      `hcl:"model_router,block"`
	PromptBuilder        *promptBuilderSpec    `hcl:"prompt_builder,block"`
	ContextStrategy      *contextStrategySpec  `hcl:"context_strategy,block"`
	Executor             *executorSpec         `hcl:"executor,block"`
	EditStrategy         *editStrategySpec     `hcl:"edit_strategy,block"`
	Verifier             *verifierSpec         `hcl:"verifier,block"`
	PermissionPolicy     *permissionPolicySpec `hcl:"permission_policy,block"`
	GitStrategy          *gitStrategySpec      `hcl:"git_strategy,block"`
	Transport            *transportSpec        `hcl:"transport,block"`
	TraceEmitter         *traceEmitterSpec     `hcl:"trace_emitter,block"`
	Tools                *toolsSpec            `hcl:"tools,block"`
	RuleOfTwo            *ruleOfTwoSpec        `hcl:"rule_of_two,block"`
	CodeScanner          *codeScannerSpec      `hcl:"code_scanner,block"`
	GuardRail            *guardRailSpec        `hcl:"guard_rail,block"`
	Observability        *observabilitySpec    `hcl:"observability,block"`
}

// runConfigOverridesSpec mirrors types.RunConfigOverrides. Every field
// is a pointer / optional so a sparse overlay is the natural shape.
//
// Note: Mode is intentionally absent from the HCL surface. Per-task mode
// is controlled by the task block's own `mode` attribute; the runner
// always passes that on as the harness's --mode flag, which would
// silently override anything written here. The Go type
// RunConfigOverrides retains Mode for the experiment runner path,
// which writes its own merged config and does not pass --mode on the
// command line.
type runConfigOverridesSpec struct {
	MaxTurns        *int                 `hcl:"max_turns,optional"`
	Provider        *providerSpec        `hcl:"provider,block"`
	ModelRouter     *modelRouterSpec     `hcl:"model_router,block"`
	ContextStrategy *contextStrategySpec `hcl:"context_strategy,block"`
	EditStrategy    *editStrategySpec    `hcl:"edit_strategy,block"`
	Verifier        *verifierSpec        `hcl:"verifier,block"`
}

type providerSpec struct {
	Type                 string                    `hcl:"type"`
	APIKeyRef            string                    `hcl:"api_key_ref,optional"`
	Region               string                    `hcl:"region,optional"`
	Profile              string                    `hcl:"profile,optional"`
	BaseURL              string                    `hcl:"base_url,optional"`
	APIKeyHeader         string                    `hcl:"api_key_header,optional"`
	QueryParams          map[string]string         `hcl:"query_params,optional"`
	GCPProject           string                    `hcl:"gcp_project,optional"`
	GCPLocation          string                    `hcl:"gcp_location,optional"`
	GCPCredentialsFile   string                    `hcl:"gcp_credentials_file,optional"`
	Credential           *credentialSpec           `hcl:"credential,block"`
	GeminiSafetySettings []geminiSafetySettingSpec `hcl:"gemini_safety_setting,block"`
}

type credentialSpec struct {
	Type             string           `hcl:"type"`
	RoleARN          string           `hcl:"role_arn,optional"`
	SessionName      string           `hcl:"session_name,optional"`
	Audience         string           `hcl:"audience,optional"`
	ServiceAccount   string           `hcl:"service_account,optional"`
	FederationRuleID string           `hcl:"federation_rule_id,optional"`
	OrganizationID   string           `hcl:"organization_id,optional"`
	ServiceAccountID string           `hcl:"service_account_id,optional"`
	WorkspaceID      string           `hcl:"workspace_id,optional"`
	AzureTenantID    string           `hcl:"azure_tenant_id,optional"`
	AzureClientID    string           `hcl:"azure_client_id,optional"`
	AzureScope       string           `hcl:"azure_scope,optional"`
	AzureTokenURL    string           `hcl:"azure_token_url,optional"`
	TokenSource      *tokenSourceSpec `hcl:"token_source,block"`
}

type tokenSourceSpec struct {
	Type     string `hcl:"type"`
	Audience string `hcl:"audience,optional"`
	Path     string `hcl:"path,optional"`
	EnvVar   string `hcl:"env_var,optional"`
	Resource string `hcl:"resource,optional"`
	ClientID string `hcl:"client_id,optional"`
}

type geminiSafetySettingSpec struct {
	Category  string `hcl:"category"`
	Threshold string `hcl:"threshold"`
}

type modelRouterSpec struct {
	Type                    string            `hcl:"type,optional"`
	Provider                string            `hcl:"provider,optional"`
	Model                   string            `hcl:"model,optional"`
	ModeModels              map[string]string `hcl:"mode_models,optional"`
	CheapProvider           string            `hcl:"cheap_provider,optional"`
	CheapModel              string            `hcl:"cheap_model,optional"`
	ExpensiveProvider       string            `hcl:"expensive_provider,optional"`
	ExpensiveModel          string            `hcl:"expensive_model,optional"`
	ExpensiveTurnThreshold  int               `hcl:"expensive_turn_threshold,optional"`
	ExpensiveTokenThreshold int               `hcl:"expensive_token_threshold,optional"`
	CheapStopReasons        []string          `hcl:"cheap_stop_reasons,optional"`
}

type promptBuilderSpec struct {
	Type     string `hcl:"type"`
	Template string `hcl:"template,optional"`
}

type contextStrategySpec struct {
	Type      string `hcl:"type"`
	MaxTokens int    `hcl:"max_tokens,optional"`
}

type executorSpec struct {
	Type       string              `hcl:"type"`
	Workspace  string              `hcl:"workspace,optional"`
	Image      string              `hcl:"image,optional"`
	Proxy      string              `hcl:"proxy,optional"`
	Runtime    string              `hcl:"runtime,optional"`
	VcsBackend *vcsBackendSpec     `hcl:"vcs_backend,block"`
	Network    *networkSpec        `hcl:"network,block"`
	Resources  *resourceLimitsSpec `hcl:"resources,block"`
}

type vcsBackendSpec struct {
	Type      string `hcl:"type"`
	APIKeyRef string `hcl:"api_key_ref,optional"`
	Repo      string `hcl:"repo,optional"`
	Ref       string `hcl:"ref,optional"`
}

type networkSpec struct {
	Mode      string   `hcl:"mode"`
	Allowlist []string `hcl:"allowlist,optional"`
}

type resourceLimitsSpec struct {
	CPUs     float64 `hcl:"cpus"`
	MemoryMB int     `hcl:"memory_mb"`
	DiskMB   int     `hcl:"disk_mb"`
	PIDs     int     `hcl:"pids"`
}

type editStrategySpec struct {
	Type           string   `hcl:"type"`
	FuzzyThreshold *float64 `hcl:"fuzzy_threshold,optional"`
}

type verifierSpec struct {
	Type      string         `hcl:"type"`
	Command   string         `hcl:"command,optional"`
	Timeout   int            `hcl:"timeout,optional"`
	Criteria  string         `hcl:"criteria,optional"`
	Model     string         `hcl:"model,optional"`
	Verifiers []verifierSpec `hcl:"verifier,block"`
}

type permissionPolicySpec struct {
	Type       string `hcl:"type"`
	Timeout    int    `hcl:"timeout,optional"`
	PolicyFile string `hcl:"policy_file,optional"`
	Fallback   string `hcl:"fallback,optional"`
}

type gitStrategySpec struct {
	Type string `hcl:"type"`
}

type transportSpec struct {
	Type    string `hcl:"type"`
	Address string `hcl:"address,optional"`
}

type traceEmitterSpec struct {
	Type            string            `hcl:"type"`
	FilePath        string            `hcl:"file_path,optional"`
	Endpoint        string            `hcl:"endpoint,optional"`
	MetricsEndpoint string            `hcl:"metrics_endpoint,optional"`
	Protocol        string            `hcl:"protocol,optional"`
	Headers         map[string]string `hcl:"headers,optional"`
}

type toolsSpec struct {
	BuiltIn []string `hcl:"built_in,optional"`
}

type ruleOfTwoSpec struct {
	Enforce *bool                 `hcl:"enforce,optional"`
	Runtime *ruleOfTwoRuntimeSpec `hcl:"runtime,block"`
}

type ruleOfTwoRuntimeSpec struct {
	Classifier    string   `hcl:"classifier,optional"`
	OnDetect      string   `hcl:"on_detect,optional"`
	GuardCriteria []string `hcl:"guard_criteria,optional"`
}

type codeScannerSpec struct {
	Type              string   `hcl:"type"`
	Scanners          []string `hcl:"scanners,optional"`
	BlockOnWarn       bool     `hcl:"block_on_warn,optional"`
	SemgrepConfigPath string   `hcl:"semgrep_config_path,optional"`
}

type guardRailSpec struct {
	Type          string          `hcl:"type"`
	Phases        []string        `hcl:"phases,optional"`
	Endpoint      string          `hcl:"endpoint,optional"`
	Model         string          `hcl:"model,optional"`
	Threshold     float64         `hcl:"threshold,optional"`
	Criteria      []string        `hcl:"criteria,optional"`
	Think         *bool           `hcl:"think,optional"`
	TimeoutMs     int             `hcl:"timeout_ms,optional"`
	FailOpen      bool            `hcl:"fail_open,optional"`
	MinChunkChars int             `hcl:"min_chunk_chars,optional"`
	Stages        []guardRailSpec `hcl:"stage,block"`
}

type observabilitySpec struct {
	Environment      string `hcl:"environment,optional"`
	ServiceNamespace string `hcl:"service_namespace,optional"`
}

// validateInlineAPIKeyRefs rejects any inline api_key_ref value that
// is not a secret:// reference. The check duplicates the rule already
// enforced by types.ValidateRunConfig so authors see the diagnostic
// at HCL parse time — with the field path named — rather than as a
// downstream validation error after the runner has already merged the
// config. Without this layer the parse path silently accepts a
// pasted-in literal API key; the merged run_config.json the harness
// receives would carry the raw value (and the redacted artifact would
// hide the misconfiguration from audit by rewriting it to
// "secret://[REDACTED]"). Apply to every secret-bearing field
// surfaced by the HCL grammar today: Provider.APIKeyRef on both the
// inline run_config and per-task run_config_overrides, and
// Executor.VcsBackend.APIKeyRef on the inline run_config.
//
// The set of fields here must stay in lockstep with the surface the
// HCL grammar accepts. Adding a new secret-bearing field to
// providerSpec / runConfigOverridesSpec / executorSpec without
// extending this validator would create a parse-time hole.
func validateInlineAPIKeyRefs(cfg *types.RunConfig, overrides *types.RunConfigOverrides) error {
	checkRef := func(path, ref string) error {
		if ref == "" {
			return nil
		}
		if !strings.HasPrefix(ref, "secret://") {
			return fmt.Errorf(
				"%s %q: raw credentials are not permitted; use a secret:// reference",
				path, ref,
			)
		}
		return nil
	}
	if cfg != nil {
		if err := checkRef("provider.api_key_ref", cfg.Provider.APIKeyRef); err != nil {
			return err
		}
		if cfg.Executor.VcsBackend != nil {
			if err := checkRef("executor.vcs_backend.api_key_ref", cfg.Executor.VcsBackend.APIKeyRef); err != nil {
				return err
			}
		}
	}
	if overrides != nil && overrides.Provider != nil {
		if err := checkRef("run_config_overrides.provider.api_key_ref", overrides.Provider.APIKeyRef); err != nil {
			return err
		}
	}
	return nil
}

// runConfigSpecToType materialises a parsed runConfigSpec into a
// *types.RunConfig. Nil input returns nil so callers can treat
// "absent" and "present-but-empty" identically.
func runConfigSpecToType(s *runConfigSpec) *types.RunConfig {
	if s == nil {
		return nil
	}
	out := &types.RunConfig{
		RunID:                s.RunID,
		Mode:                 s.Mode,
		SessionName:          s.SessionName,
		Prompt:               s.Prompt,
		LogLevel:             s.LogLevel,
		SystemPromptOverride: s.SystemPromptOverride,
		MaxTokenBudget:       s.MaxTokenBudget,
		MaxCostBudget:        s.MaxCostBudget,
		Timeout:              s.Timeout,
		FollowUpGrace:        s.FollowUpGrace,
		SensitiveData:        s.SensitiveData,
	}
	if s.MaxTurns != nil {
		out.MaxTurns = *s.MaxTurns
	}
	if s.Provider != nil {
		out.Provider = providerSpecToType(s.Provider)
	}
	if s.ModelRouter != nil {
		out.ModelRouter = modelRouterSpecToType(s.ModelRouter)
	}
	if s.PromptBuilder != nil {
		out.PromptBuilder = promptBuilderSpecToType(s.PromptBuilder)
	}
	if s.ContextStrategy != nil {
		out.ContextStrategy = contextStrategySpecToType(s.ContextStrategy)
	}
	if s.Executor != nil {
		out.Executor = executorSpecToType(s.Executor)
	}
	if s.EditStrategy != nil {
		out.EditStrategy = editStrategySpecToType(s.EditStrategy)
	}
	if s.Verifier != nil {
		out.Verifier = verifierSpecToType(s.Verifier)
	}
	if s.PermissionPolicy != nil {
		out.PermissionPolicy = permissionPolicySpecToType(s.PermissionPolicy)
	}
	if s.GitStrategy != nil {
		out.GitStrategy = types.GitStrategyConfig{Type: s.GitStrategy.Type}
	}
	if s.Transport != nil {
		out.Transport = types.TransportConfig{Type: s.Transport.Type, Address: s.Transport.Address}
	}
	if s.TraceEmitter != nil {
		out.TraceEmitter = traceEmitterSpecToType(s.TraceEmitter)
	}
	if s.Tools != nil {
		out.Tools = types.ToolsConfig{BuiltIn: s.Tools.BuiltIn}
	}
	if s.RuleOfTwo != nil {
		out.RuleOfTwo = &types.RuleOfTwoConfig{Enforce: s.RuleOfTwo.Enforce}
		if s.RuleOfTwo.Runtime != nil {
			out.RuleOfTwo.Runtime = &types.RuleOfTwoRuntimeConfig{
				Classifier:    s.RuleOfTwo.Runtime.Classifier,
				OnDetect:      s.RuleOfTwo.Runtime.OnDetect,
				GuardCriteria: s.RuleOfTwo.Runtime.GuardCriteria,
			}
		}
	}
	if s.CodeScanner != nil {
		out.CodeScanner = &types.CodeScannerConfig{
			Type:              s.CodeScanner.Type,
			Scanners:          s.CodeScanner.Scanners,
			BlockOnWarn:       s.CodeScanner.BlockOnWarn,
			SemgrepConfigPath: s.CodeScanner.SemgrepConfigPath,
		}
	}
	if s.GuardRail != nil {
		gr := guardRailSpecToType(*s.GuardRail)
		out.GuardRail = &gr
	}
	if s.Observability != nil {
		out.Observability = types.ObservabilityConfig{
			Environment:      s.Observability.Environment,
			ServiceNamespace: s.Observability.ServiceNamespace,
		}
	}
	return out
}

// runConfigOverridesSpecToType materialises a parsed runConfigOverridesSpec
// into a *types.RunConfigOverrides. Nil input returns nil.
func runConfigOverridesSpecToType(s *runConfigOverridesSpec) *types.RunConfigOverrides {
	if s == nil {
		return nil
	}
	out := &types.RunConfigOverrides{
		MaxTurns: s.MaxTurns,
	}
	if s.Provider != nil {
		p := providerSpecToType(s.Provider)
		out.Provider = &p
	}
	if s.ModelRouter != nil {
		mr := modelRouterSpecToType(s.ModelRouter)
		out.ModelRouter = &mr
	}
	if s.ContextStrategy != nil {
		cs := contextStrategySpecToType(s.ContextStrategy)
		out.ContextStrategy = &cs
	}
	if s.EditStrategy != nil {
		es := editStrategySpecToType(s.EditStrategy)
		out.EditStrategy = &es
	}
	if s.Verifier != nil {
		v := verifierSpecToType(s.Verifier)
		out.Verifier = &v
	}
	return out
}

func providerSpecToType(s *providerSpec) types.ProviderConfig {
	out := types.ProviderConfig{
		Type:               s.Type,
		APIKeyRef:          s.APIKeyRef,
		Region:             s.Region,
		Profile:            s.Profile,
		BaseURL:            s.BaseURL,
		APIKeyHeader:       s.APIKeyHeader,
		QueryParams:        s.QueryParams,
		GCPProject:         s.GCPProject,
		GCPLocation:        s.GCPLocation,
		GCPCredentialsFile: s.GCPCredentialsFile,
	}
	if s.Credential != nil {
		c := credentialSpecToType(*s.Credential)
		out.Credential = &c
	}
	if len(s.GeminiSafetySettings) > 0 {
		settings := make([]types.GeminiSafetySetting, 0, len(s.GeminiSafetySettings))
		for _, gs := range s.GeminiSafetySettings {
			settings = append(settings, types.GeminiSafetySetting{
				Category:  gs.Category,
				Threshold: gs.Threshold,
			})
		}
		out.GeminiSafetySettings = settings
	}
	return out
}

func credentialSpecToType(s credentialSpec) types.CredentialConfig {
	out := types.CredentialConfig{
		Type:             s.Type,
		RoleARN:          s.RoleARN,
		SessionName:      s.SessionName,
		Audience:         s.Audience,
		ServiceAccount:   s.ServiceAccount,
		FederationRuleID: s.FederationRuleID,
		OrganizationID:   s.OrganizationID,
		ServiceAccountID: s.ServiceAccountID,
		WorkspaceID:      s.WorkspaceID,
		AzureTenantID:    s.AzureTenantID,
		AzureClientID:    s.AzureClientID,
		AzureScope:       s.AzureScope,
		AzureTokenURL:    s.AzureTokenURL,
	}
	if s.TokenSource != nil {
		out.TokenSource = &types.TokenSourceConfig{
			Type:     s.TokenSource.Type,
			Audience: s.TokenSource.Audience,
			Path:     s.TokenSource.Path,
			EnvVar:   s.TokenSource.EnvVar,
			Resource: s.TokenSource.Resource,
			ClientID: s.TokenSource.ClientID,
		}
	}
	return out
}

func modelRouterSpecToType(s *modelRouterSpec) types.ModelRouterConfig {
	return types.ModelRouterConfig{
		Type:                    s.Type,
		Provider:                s.Provider,
		Model:                   s.Model,
		ModeModels:              s.ModeModels,
		CheapProvider:           s.CheapProvider,
		CheapModel:              s.CheapModel,
		ExpensiveProvider:       s.ExpensiveProvider,
		ExpensiveModel:          s.ExpensiveModel,
		ExpensiveTurnThreshold:  s.ExpensiveTurnThreshold,
		ExpensiveTokenThreshold: s.ExpensiveTokenThreshold,
		CheapStopReasons:        s.CheapStopReasons,
	}
}

func promptBuilderSpecToType(s *promptBuilderSpec) types.PromptBuilderConfig {
	return types.PromptBuilderConfig{Type: s.Type, Template: s.Template}
}

func contextStrategySpecToType(s *contextStrategySpec) types.ContextStrategyConfig {
	return types.ContextStrategyConfig{Type: s.Type, MaxTokens: s.MaxTokens}
}

func executorSpecToType(s *executorSpec) types.ExecutorConfig {
	out := types.ExecutorConfig{
		Type:      s.Type,
		Workspace: s.Workspace,
		Image:     s.Image,
		Proxy:     s.Proxy,
		Runtime:   s.Runtime,
	}
	if s.VcsBackend != nil {
		out.VcsBackend = &types.VcsBackendConfig{
			Type:      s.VcsBackend.Type,
			APIKeyRef: s.VcsBackend.APIKeyRef,
			Repo:      s.VcsBackend.Repo,
			Ref:       s.VcsBackend.Ref,
		}
	}
	if s.Network != nil {
		out.Network = &types.NetworkConfig{
			Mode:      s.Network.Mode,
			Allowlist: s.Network.Allowlist,
		}
	}
	if s.Resources != nil {
		out.Resources = &types.ResourceLimits{
			CPUs:     s.Resources.CPUs,
			MemoryMB: s.Resources.MemoryMB,
			DiskMB:   s.Resources.DiskMB,
			PIDs:     s.Resources.PIDs,
		}
	}
	return out
}

func editStrategySpecToType(s *editStrategySpec) types.EditStrategyConfig {
	return types.EditStrategyConfig{Type: s.Type, FuzzyThreshold: s.FuzzyThreshold}
}

func verifierSpecToType(s *verifierSpec) types.VerifierConfig {
	out := types.VerifierConfig{
		Type:     s.Type,
		Command:  s.Command,
		Timeout:  s.Timeout,
		Criteria: s.Criteria,
		Model:    s.Model,
	}
	if len(s.Verifiers) > 0 {
		out.Verifiers = make([]types.VerifierConfig, 0, len(s.Verifiers))
		for _, v := range s.Verifiers {
			vv := v
			out.Verifiers = append(out.Verifiers, verifierSpecToType(&vv))
		}
	}
	return out
}

func permissionPolicySpecToType(s *permissionPolicySpec) types.PermissionPolicyConfig {
	return types.PermissionPolicyConfig{
		Type:       s.Type,
		Timeout:    s.Timeout,
		PolicyFile: s.PolicyFile,
		Fallback:   s.Fallback,
	}
}

func traceEmitterSpecToType(s *traceEmitterSpec) types.TraceEmitterConfig {
	return types.TraceEmitterConfig{
		Type:            s.Type,
		FilePath:        s.FilePath,
		Endpoint:        s.Endpoint,
		MetricsEndpoint: s.MetricsEndpoint,
		Protocol:        s.Protocol,
		Headers:         s.Headers,
	}
}

func guardRailSpecToType(s guardRailSpec) types.GuardRailConfig {
	out := types.GuardRailConfig{
		Type:          s.Type,
		Phases:        s.Phases,
		Endpoint:      s.Endpoint,
		Model:         s.Model,
		Threshold:     s.Threshold,
		Criteria:      s.Criteria,
		Think:         s.Think,
		TimeoutMs:     s.TimeoutMs,
		FailOpen:      s.FailOpen,
		MinChunkChars: s.MinChunkChars,
	}
	if len(s.Stages) > 0 {
		out.Stages = make([]types.GuardRailConfig, 0, len(s.Stages))
		for _, st := range s.Stages {
			out.Stages = append(out.Stages, guardRailSpecToType(st))
		}
	}
	return out
}
