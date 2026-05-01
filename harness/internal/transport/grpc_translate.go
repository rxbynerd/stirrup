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
	}

	if pe.Allowed != nil {
		v := pe.Allowed.Value
		e.Allowed = &v
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
	return &pb.RunTrace{
		RunId:        t.ID,
		Turns:        int32(t.Turns),
		InputTokens:  int32(t.TokenUsage.Input),
		OutputTokens: int32(t.TokenUsage.Output),
		DurationMs:   t.CompletedAt.Sub(t.StartedAt).Milliseconds(),
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
		DynamicContext: pc.DynamicContext,
		MaxTurns:       int(pc.MaxTurns),
	}

	if pc.MaxTokenBudget != nil {
		v := int(*pc.MaxTokenBudget)
		rc.MaxTokenBudget = &v
	}
	if pc.MaxCostBudget != nil {
		rc.MaxCostBudget = pc.MaxCostBudget
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
			Type:     pc.PromptBuilder.Type,
			Template: pc.PromptBuilder.Template,
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
	}
	if pc.CodeScanner != nil {
		rc.CodeScanner = &types.CodeScannerConfig{
			Type:        pc.CodeScanner.Type,
			Scanners:    pc.CodeScanner.Scanners,
			BlockOnWarn: pc.CodeScanner.BlockOnWarn,
		}
	}
	if pc.GitStrategy != nil {
		rc.GitStrategy = types.GitStrategyConfig{Type: pc.GitStrategy.Type}
	}
	if pc.TraceEmitter != nil {
		rc.TraceEmitter = types.TraceEmitterConfig{
			Type:            pc.TraceEmitter.Type,
			FilePath:        pc.TraceEmitter.FilePath,
			Endpoint:        pc.TraceEmitter.Endpoint,
			MetricsEndpoint: pc.TraceEmitter.MetricsEndpoint,
		}
	}
	if pc.Tools != nil {
		rc.Tools = toolsConfigFromProto(pc.Tools)
	}

	rc.LogLevel = pc.LogLevel
	rc.SystemPromptOverride = pc.SystemPromptOverride
	rc.SessionName = pc.GetSessionName()

	return rc
}

func providerConfigFromProto(pc *pb.ProviderConfig) types.ProviderConfig {
	cfg := types.ProviderConfig{
		Type:         pc.Type,
		APIKeyRef:    pc.ApiKeyRef,
		Region:       pc.Region,
		Profile:      pc.Profile,
		BaseURL:      pc.BaseUrl,
		APIKeyHeader: pc.ApiKeyHeader,
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
	return cfg
}

func credentialConfigFromProto(pc *pb.CredentialConfig) *types.CredentialConfig {
	cfg := &types.CredentialConfig{
		Type:        pc.Type,
		RoleARN:     pc.RoleArn,
		SessionName: pc.SessionName,
	}
	if pc.TokenSource != nil {
		cfg.TokenSource = &types.TokenSourceConfig{
			Type:     pc.TokenSource.Type,
			Audience: pc.TokenSource.Audience,
			Path:     pc.TokenSource.Path,
			EnvVar:   pc.TokenSource.EnvVar,
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
		Type:      pc.Type,
		Workspace: pc.Workspace,
		Image:     pc.Image,
		Proxy:     pc.Proxy,
		Runtime:   pc.Runtime,
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

func toolsConfigFromProto(pc *pb.ToolsConfig) types.ToolsConfig {
	tc := types.ToolsConfig{
		BuiltIn: pc.BuiltIn,
	}
	for _, srv := range pc.McpServers {
		tc.MCPServers = append(tc.MCPServers, types.MCPServerConfig{
			Name:      srv.Name,
			URI:       srv.Uri,
			APIKeyRef: srv.ApiKeyRef,
		})
	}
	return tc
}
