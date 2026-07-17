package transport

import (
	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

// harnessEventToProto translates an internal HarnessEvent to its proto
// wire-format representation.
func harnessEventToProto(e types.HarnessEvent) *pb.HarnessEvent {
	pe := &pb.HarnessEvent{
		Type:           e.Type,
		Text:           e.Text,
		Id:             e.ID,
		Name:           e.Name,
		Input:          []byte(e.Input),
		ToolUseId:      e.ToolUseID,
		Content:        e.Content,
		StopReason:     e.StopReason,
		Message:        e.Message,
		RequestId:      e.RequestID,
		ToolName:       e.ToolName,
		HarnessVersion: e.HarnessVersion,
		Audience:       e.Audience,
	}

	if e.Trace != nil {
		pe.Trace = runTraceToProto(e.Trace)
	}

	return pe
}

// controlEventFromProto translates a proto ControlEvent to the internal
// types.ControlEvent representation.
func controlEventFromProto(pe *pb.ControlEvent) types.ControlEvent {
	e := types.ControlEvent{
		Type:         pe.Type,
		UserResponse: pe.UserResponse,
		RequestID:    pe.RequestId,
		Reason:       pe.Reason,
		Content:      pe.Content,
		Token:        pe.Token,
	}

	if pe.Allowed != nil {
		v := pe.Allowed.Value
		e.Allowed = &v
	}

	if pe.IsError != nil {
		v := pe.IsError.Value
		e.IsError = &v
	}

	if pe.ExpiresAt != nil {
		v := *pe.ExpiresAt
		e.ExpiresAt = &v
	}

	if pe.Task != nil {
		rc := runConfigFromProto(pe.Task)
		e.Task = &rc
	}

	return e
}

// runTraceToProto translates the internal RunTrace to a simplified proto
// wire format suitable for streaming back to the control plane.
func runTraceToProto(t *types.RunTrace) *pb.RunTrace {
	// Outcome is the canonical analytics field (#141). StopReason has always
	// carried t.Outcome too; it keeps doing so for consumers predating the
	// outcome field.
	return &pb.RunTrace{
		RunId:        t.ID,
		Turns:        int32(t.Turns),
		InputTokens:  int32(t.TokenUsage.Input),
		OutputTokens: int32(t.TokenUsage.Output),
		DurationMs:   t.CompletedAt.Sub(t.StartedAt).Milliseconds(),
		Outcome:      t.Outcome,
		StopReason:   t.Outcome,
	}
}

// runConfigFromProto translates a proto RunConfig to the internal
// types.RunConfig. This is the primary path for TaskAssignment payloads.
func runConfigFromProto(pc *pb.RunConfig) types.RunConfig {
	rc := types.RunConfig{
		RunID:          pc.RunId,
		Mode:           pc.Mode,
		Prompt:         pc.Prompt,
		DynamicContext: dynamicContextFromProto(pc.DynamicContext),
		MaxTurns:       int(pc.MaxTurns),
	}

	if pc.MaxTokenBudget != nil {
		v := int(*pc.MaxTokenBudget)
		rc.MaxTokenBudget = &v
	}
	if pc.MaxCostBudget != nil {
		rc.MaxCostBudget = pc.MaxCostBudget
	}
	if pc.Temperature != nil {
		rc.Temperature = pc.Temperature
	}
	if pc.Timeout != nil {
		v := int(*pc.Timeout)
		rc.Timeout = &v
	}
	if pc.FollowUpGrace != nil {
		v := int(*pc.FollowUpGrace)
		rc.FollowUpGrace = &v
	}

	if pc.Provider != nil {
		rc.Provider = providerConfigFromProto(pc.Provider)
	}
	if len(pc.Providers) > 0 {
		rc.Providers = make(map[string]types.ProviderConfig, len(pc.Providers))
		for name, provider := range pc.Providers {
			rc.Providers[name] = providerConfigFromProto(provider)
		}
	}
	if pc.ModelRouter != nil {
		rc.ModelRouter = modelRouterConfigFromProto(pc.ModelRouter)
	}
	if pc.PromptBuilder != nil {
		rc.PromptBuilder = types.PromptBuilderConfig{
			Type:        pc.PromptBuilder.Type,
			Template:    pc.PromptBuilder.Template,
			PromptModel: pc.PromptBuilder.PromptModel,
		}
	}
	if pc.ContextStrategy != nil {
		rc.ContextStrategy = types.ContextStrategyConfig{
			Type:      pc.ContextStrategy.Type,
			MaxTokens: int(pc.ContextStrategy.MaxTokens),
		}
	}
	if pc.Executor != nil {
		rc.Executor = executorConfigFromProto(pc.Executor)
	}
	if pc.EditStrategy != nil {
		rc.EditStrategy = types.EditStrategyConfig{Type: pc.EditStrategy.Type}
		if pc.EditStrategy.FuzzyThreshold != nil {
			rc.EditStrategy.FuzzyThreshold = pc.EditStrategy.FuzzyThreshold
		}
	}
	if pc.Verifier != nil {
		rc.Verifier = verifierConfigFromProto(pc.Verifier)
	}
	if pc.PermissionPolicy != nil {
		rc.PermissionPolicy = types.PermissionPolicyConfig{
			Type:       pc.PermissionPolicy.Type,
			Timeout:    int(pc.PermissionPolicy.Timeout),
			PolicyFile: pc.PermissionPolicy.PolicyFile,
			Fallback:   pc.PermissionPolicy.Fallback,
		}
	}
	if pc.RuleOfTwo != nil {
		// Enforce is proto3 `optional bool`, generated as *bool. Preserve
		// the unset/false distinction here — the validator depends on it
		// to apply the secure default (enforce) when the field is omitted.
		rc.RuleOfTwo = &types.RuleOfTwoConfig{Enforce: pc.RuleOfTwo.Enforce}
		// Runtime mirrors the same nil-vs-present distinction: an absent
		// sub-message stays nil so the factory's default arming applies,
		// rather than a synthesised empty block masquerading as an
		// operator declaration.
		if pc.RuleOfTwo.Runtime != nil {
			rc.RuleOfTwo.Runtime = &types.RuleOfTwoRuntimeConfig{
				Classifier:    pc.RuleOfTwo.Runtime.Classifier,
				OnDetect:      pc.RuleOfTwo.Runtime.OnDetect,
				GuardCriteria: append([]string(nil), pc.RuleOfTwo.Runtime.GuardCriteria...),
			}
		}
	}
	if pc.SensitiveData != nil {
		// proto3 `optional bool`, generated as *bool. Preserve the
		// unset/false distinction so the validator's secure default
		// ("not sensitive unless declared") applies when the field is
		// omitted on the wire.
		v := *pc.SensitiveData
		rc.SensitiveData = &v
	}
	if pc.CodeScanner != nil {
		rc.CodeScanner = &types.CodeScannerConfig{
			Type:              pc.CodeScanner.Type,
			Scanners:          pc.CodeScanner.Scanners,
			BlockOnWarn:       pc.CodeScanner.BlockOnWarn,
			SemgrepConfigPath: pc.CodeScanner.SemgrepConfigPath,
		}
	}
	if pc.GitStrategy != nil {
		rc.GitStrategy = types.GitStrategyConfig{Type: pc.GitStrategy.Type}
	}
	if pc.TraceEmitter != nil {
		// Bucket and ObjectPrefix carry the GCS trace-emitter routing
		// data. Dropping them here would silently fall back to the
		// jsonl emitter (when Type=="" after the zero-value copy) or
		// produce a "bucket is required" construction error at the
		// factory — both invisible to a control plane that just sent
		// a Type=="gcs" config. Credential is intentionally not on the
		// proto yet (see harness.proto TraceEmitterConfig comment); a
		// proto field and matching translation will land alongside
		// the broader ResultSinkConfig wiring follow-up.
		rc.TraceEmitter = types.TraceEmitterConfig{
			Type:            pc.TraceEmitter.Type,
			FilePath:        pc.TraceEmitter.FilePath,
			Endpoint:        pc.TraceEmitter.Endpoint,
			MetricsEndpoint: pc.TraceEmitter.MetricsEndpoint,
			Protocol:        pc.TraceEmitter.Protocol,
			Headers:         pc.TraceEmitter.Headers,
			Bucket:          pc.TraceEmitter.Bucket,
			ObjectPrefix:    pc.TraceEmitter.ObjectPrefix,
			CaptureContent:  pc.TraceEmitter.CaptureContent,
		}
	}
	if pc.Tools != nil {
		rc.Tools = toolsConfigFromProto(pc.Tools)
	}
	if pc.GuardRail != nil {
		gr := guardRailConfigFromProto(pc.GuardRail)
		rc.GuardRail = &gr
	}
	// Observability is a value-typed sub-config in types.RunConfig but a
	// pointer-typed message on the wire; the nil-guard here is mandatory
	// to keep an absent proto sub-message from synthesising a zero-value
	// types.ObservabilityConfig. Same pattern as SessionName / GuardRail
	// translation: silently dropping this would make a K8s job land in
	// deployment.environment=local even when the control plane sent a
	// staging label, which is the exact regression issue #95 fixed.
	if pc.Observability != nil {
		rc.Observability = types.ObservabilityConfig{
			Environment:      pc.Observability.GetEnvironment(),
			ServiceNamespace: pc.Observability.GetServiceNamespace(),
		}
	}
	if pc.ToolDispatch != nil {
		// Preserve the unset/zero distinction the validator depends on:
		// an empty proto sub-message carries MaxParallel == 0, which is
		// legal and resolves to DefaultToolDispatchMaxParallel via
		// EffectiveToolDispatchMaxParallel. Constructing the internal
		// struct only when the proto sub-message is present keeps a nil
		// types.RunConfig.ToolDispatch wire-distinguishable from an
		// explicit ToolDispatchConfig{}.
		rc.ToolDispatch = &types.ToolDispatchConfig{MaxParallel: int(pc.ToolDispatch.GetMaxParallel())}
	}
	if pc.Hooks != nil {
		hooks := hooksConfigFromProto(pc.Hooks)
		rc.Hooks = &hooks
	}

	rc.LogLevel = pc.LogLevel
	rc.SystemPromptOverride = pc.SystemPromptOverride
	rc.SessionName = pc.GetSessionName()

	return rc
}

// guardRailConfigFromProto recursively translates a proto GuardRailConfig
// to the internal types form. Stages are walked recursively so a
// composite payload survives the round-trip; the validator (run in the
// factory) still rejects composite-of-composite, so only one level of
// recursion is operationally meaningful.
func guardRailConfigFromProto(pc *pb.GuardRailConfig) types.GuardRailConfig {
	cfg := types.GuardRailConfig{
		Type:          pc.Type,
		Phases:        append([]string(nil), pc.Phases...),
		Endpoint:      pc.Endpoint,
		Model:         pc.Model,
		Threshold:     pc.Threshold,
		Criteria:      append([]string(nil), pc.Criteria...),
		TimeoutMs:     int(pc.TimeoutMs),
		FailOpen:      pc.FailOpen,
		MinChunkChars: int(pc.MinChunkChars),
	}
	if len(pc.CustomCriteria) > 0 {
		// Copy the proto-owned map so later mutations to the wire payload
		// can't reach internal config state.
		cfg.CustomCriteria = make(map[string]string, len(pc.CustomCriteria))
		for k, v := range pc.CustomCriteria {
			cfg.CustomCriteria[k] = v
		}
	}
	// Think is `optional bool`, generated as *bool. Preserve the
	// unset/false distinction so the validator and adapter constructor
	// can apply the documented default ("false") when the field is
	// omitted on the wire.
	if pc.Think != nil {
		v := *pc.Think
		cfg.Think = &v
	}
	if len(pc.Stages) > 0 {
		cfg.Stages = make([]types.GuardRailConfig, 0, len(pc.Stages))
		for _, stage := range pc.Stages {
			if stage == nil {
				continue
			}
			cfg.Stages = append(cfg.Stages, guardRailConfigFromProto(stage))
		}
	}
	return cfg
}

func providerConfigFromProto(pc *pb.ProviderConfig) types.ProviderConfig {
	cfg := types.ProviderConfig{
		Type:          pc.Type,
		APIKeyRef:     pc.ApiKeyRef,
		Region:        pc.Region,
		Profile:       pc.Profile,
		BaseURL:       pc.BaseUrl,
		APIKeyHeader:  pc.ApiKeyHeader,
		CompatProfile: pc.CompatProfile,
	}
	if len(pc.QueryParams) > 0 {
		// Copy the proto-owned map so later mutations to the wire payload
		// can't reach internal config state.
		cfg.QueryParams = make(map[string]string, len(pc.QueryParams))
		for k, v := range pc.QueryParams {
			cfg.QueryParams[k] = v
		}
	}
	if pc.Credential != nil {
		cfg.Credential = credentialConfigFromProto(pc.Credential)
	}
	if pc.Retry != nil {
		// Nil-guarded: a wire-absent retry block must produce a nil
		// types.ProviderRetryConfig so ValidateRunConfig's defaulter
		// allocates and populates a fresh struct. Mapping into an
		// always-allocated zero value would cross-bind the wire's
		// "field unset" semantics into the harness's "explicit zero"
		// semantics and silently override the documented defaults.
		// This is the recurring control-plane translation gap (gh-95,
		// gh-117, gh-118, gh-100): every operator-supplied policy
		// would otherwise be dropped silently, with no log and no
		// error.
		cfg.Retry = &types.ProviderRetryConfig{
			MaxAttempts:       int(pc.Retry.GetMaxAttempts()),
			InitialDelayMs:    int(pc.Retry.GetInitialDelayMs()),
			MaxDelayMs:        int(pc.Retry.GetMaxDelayMs()),
			WallClockBudgetMs: int(pc.Retry.GetWallClockBudgetMs()),
		}
	}
	if pc.Batch != nil {
		// Nil-guarded for the same reason as Retry above: a wire-absent
		// batch block must produce a nil types.BatchProviderConfig so
		// ValidateRunConfig's per-mode invariants stay quiet and the
		// run executes as a streaming turn. Allocating a zero value
		// here would also collapse the "operator did not configure"
		// vs. "explicit Enabled=false" distinction the phase-2 adapter
		// wiring (#135) depends on. MaxWaitSeconds is wire-`optional`
		// so the int32 pointer is unset when absent; preserve the
		// nil/non-nil distinction in the *int translation so the
		// validator's default-apply path still owns "filled by harness"
		// vs. "explicit value".
		batch := &types.BatchProviderConfig{
			Enabled:                 pc.Batch.GetEnabled(),
			HarnessSidePolling:      pc.Batch.GetHarnessSidePolling(),
			FallbackOnTimeout:       pc.Batch.GetFallbackOnTimeout(),
			CancelBundleOnRunCancel: pc.Batch.GetCancelBundleOnRunCancel(),
			AllowInteractiveModes:   pc.Batch.GetAllowInteractiveModes(),
		}
		if pc.Batch.MaxWaitSeconds != nil {
			v := int(*pc.Batch.MaxWaitSeconds)
			batch.MaxWaitSeconds = &v
		}
		cfg.Batch = batch
	}
	return cfg
}

func credentialConfigFromProto(pc *pb.CredentialConfig) *types.CredentialConfig {
	cfg := &types.CredentialConfig{
		Type:           pc.Type,
		RoleARN:        pc.RoleArn,
		SessionName:    pc.SessionName,
		Audience:       pc.Audience,
		ServiceAccount: pc.ServiceAccount,
		// Anthropic Workload Identity Federation fields (issue #117).
		// Use the generated getters so a future change to the proto
		// (e.g. promoting these to a oneof) keeps the translate layer
		// nil-safe; without copying these here, every K8s job that
		// ships an `anthropic-wif` credential over the wire fails
		// validation because all four required fields arrive empty.
		FederationRuleID: pc.GetFederationRuleId(),
		OrganizationID:   pc.GetOrganizationId(),
		ServiceAccountID: pc.GetServiceAccountId(),
		WorkspaceID:      pc.GetWorkspaceId(),
		// Azure Entra ID Workload Identity Federation fields (issue #118).
		AzureTenantID: pc.GetAzureTenantId(),
		AzureClientID: pc.GetAzureClientId(),
		AzureScope:    pc.GetAzureScope(),
		AzureTokenURL: pc.GetAzureTokenUrl(),
		// OpenAI Workload Identity Federation fields. Use the generated
		// getters so a K8s job that ships an `openai-wif` credential over the
		// wire keeps its identity_provider_id / service_account_id rather than
		// arriving empty and failing validation.
		OpenAIIdentityProviderID: pc.GetOpenaiIdentityProviderId(),
		OpenAIServiceAccountID:   pc.GetOpenaiServiceAccountId(),
		OpenAISubjectTokenType:   pc.GetOpenaiSubjectTokenType(),
	}
	if pc.TokenSource != nil {
		cfg.TokenSource = &types.TokenSourceConfig{
			Type:     pc.TokenSource.Type,
			Audience: pc.TokenSource.Audience,
			Path:     pc.TokenSource.Path,
			EnvVar:   pc.TokenSource.EnvVar,
			Resource: pc.TokenSource.Resource,
			ClientID: pc.TokenSource.ClientId,
		}
	}
	return cfg
}

func modelRouterConfigFromProto(pc *pb.ModelRouterConfig) types.ModelRouterConfig {
	return types.ModelRouterConfig{
		Type:                    pc.Type,
		Provider:                pc.Provider,
		Model:                   pc.Model,
		ModeModels:              pc.ModeModels,
		CheapProvider:           pc.CheapProvider,
		CheapModel:              pc.CheapModel,
		ExpensiveProvider:       pc.ExpensiveProvider,
		ExpensiveModel:          pc.ExpensiveModel,
		ExpensiveTurnThreshold:  int(pc.ExpensiveTurnThreshold),
		ExpensiveTokenThreshold: int(pc.ExpensiveTokenThreshold),
		CheapStopReasons:        pc.CheapStopReasons,
	}
}

func executorConfigFromProto(pc *pb.ExecutorConfig) types.ExecutorConfig {
	ec := types.ExecutorConfig{
		Type:              pc.Type,
		Workspace:         pc.Workspace,
		Image:             pc.Image,
		Proxy:             pc.Proxy,
		Runtime:           pc.Runtime,
		K8sNamespace:      pc.K8SNamespace,
		K8sKubeconfig:     pc.K8SKubeconfig,
		K8sNodeSelector:   pc.K8SNodeSelector,
		K8sServiceAccount: pc.K8SServiceAccount,
	}
	if pc.VcsBackend != nil {
		ec.VcsBackend = &types.VcsBackendConfig{
			Type:      pc.VcsBackend.Type,
			APIKeyRef: pc.VcsBackend.ApiKeyRef,
			Repo:      pc.VcsBackend.Repo,
			Ref:       pc.VcsBackend.Ref,
		}
	}
	if pc.Network != nil {
		ec.Network = &types.NetworkConfig{
			Mode:      pc.Network.Mode,
			Allowlist: pc.Network.Allowlist,
		}
	}
	if pc.Resources != nil {
		ec.Resources = &types.ResourceLimits{
			CPUs:     pc.Resources.Cpus,
			MemoryMB: int(pc.Resources.MemoryMb),
			DiskMB:   int(pc.Resources.DiskMb),
			PIDs:     int(pc.Resources.Pids),
		}
	}
	return ec
}

// hooksConfigFromProto translates a proto HooksConfig to the internal
// types form (issue #461). Called only when pc.Hooks is non-nil; the
// caller wraps the returned value in a fresh pointer so an absent proto
// sub-message stays wire-distinguishable from an explicit-but-empty one.
func hooksConfigFromProto(pc *pb.HooksConfig) types.HooksConfig {
	hc := types.HooksConfig{}
	for _, h := range pc.PreRun {
		hc.PreRun = append(hc.PreRun, hookConfigFromProto(h))
	}
	for _, h := range pc.PostRun {
		hc.PostRun = append(hc.PostRun, hookConfigFromProto(h))
	}
	return hc
}

// hookConfigFromProto translates a single proto HookConfig. Every field
// is a scalar (string/int32/bool), so a plain value copy is aliasing-safe
// — there is no proto-owned slice or map to defensively re-copy, unlike
// guardRailConfigFromProto's Stages/CustomCriteria.
func hookConfigFromProto(pc *pb.HookConfig) types.HookConfig {
	if pc == nil {
		return types.HookConfig{}
	}
	return types.HookConfig{
		Type:            pc.Type,
		Name:            pc.Name,
		Command:         pc.Command,
		TimeoutSeconds:  int(pc.TimeoutSeconds),
		ContinueOnError: pc.ContinueOnError,
		RunOn:           pc.RunOn,
	}
}

func verifierConfigFromProto(pc *pb.VerifierConfig) types.VerifierConfig {
	vc := types.VerifierConfig{
		Type:     pc.Type,
		Command:  pc.Command,
		Timeout:  int(pc.Timeout),
		Criteria: pc.Criteria,
		Model:    pc.Model,
	}
	for _, sub := range pc.Verifiers {
		vc.Verifiers = append(vc.Verifiers, verifierConfigFromProto(sub))
	}
	return vc
}

// dynamicContextFromProto translates the wire-format dynamic context
// (map[string]*DynamicContextValue) to the internal entry-typed map.
// nil-safe: returns a nil map when the wire payload is empty so an
// empty inbound message does not allocate.
func dynamicContextFromProto(pc map[string]*pb.DynamicContextValue) map[string]types.DynamicContextValue {
	if len(pc) == 0 {
		return nil
	}
	out := make(map[string]types.DynamicContextValue, len(pc))
	for k, v := range pc {
		if v == nil {
			out[k] = types.DynamicContextValue{}
			continue
		}
		out[k] = types.DynamicContextValue{
			Value:     v.Value,
			Sensitive: v.Sensitive,
		}
	}
	return out
}

func toolsConfigFromProto(pc *pb.ToolsConfig) types.ToolsConfig {
	tc := types.ToolsConfig{
		BuiltIn: pc.BuiltIn,
	}
	for _, srv := range pc.McpServers {
		tc.MCPServers = append(tc.MCPServers, types.MCPServerConfig{
			Name:            srv.Name,
			URI:             srv.Uri,
			APIKeyRef:       srv.ApiKeyRef,
			AllowedTools:    srv.AllowedTools,
			AllowedMCPHosts: srv.AllowedMcpHosts,
		})
	}
	return tc
}
