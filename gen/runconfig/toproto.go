package runconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

// ErrNotOnWire is wrapped by the error ToProto returns for a field the
// RunConfig message has no field for.
var ErrNotOnWire = errors.New("field has no RunConfig wire field")

// ToProto translates rc into the RunConfig message a control plane sends
// in a task_assignment. It does not validate rc; call
// types.ValidateRunConfig first. Transport must be exactly
// {type: "grpc"}, the transport the harness validates a task_assignment
// with, so a config validated on the control plane is the config the
// harness validates. It returns an error, one line per field, for every
// field it cannot carry: an unset or other transport, a field with no
// wire counterpart (wrapping ErrNotOnWire), an integer outside the wire
// field's range, or a schema that is not a JSON object. A nil rc is an
// error.
func ToProto(rc *types.RunConfig) (*pb.RunConfig, error) {
	if rc == nil {
		return nil, errors.New("runconfig: nil RunConfig")
	}
	e := &encoder{}
	pc := e.runConfig(rc)
	if len(e.errs) > 0 {
		return nil, errors.Join(e.errs...)
	}
	return pc, nil
}

// encoder collects every translation error so ToProto reports them all
// at once.
type encoder struct {
	errs []error
}

func (e *encoder) fail(path, format string, args ...any) {
	e.errs = append(e.errs, fmt.Errorf("runconfig: %s: "+format, append([]any{path}, args...)...))
}

func (e *encoder) notOnWire(path string) {
	e.fail(path, "%w", ErrNotOnWire)
}

func (e *encoder) transport(t types.TransportConfig) {
	switch t.Type {
	case "grpc":
	case "":
		e.fail("transport.type", "unset; set it to %q, the transport the harness validates a task_assignment with", "grpc")
	default:
		e.fail("transport.type", "%q: %w; a task_assignment always runs over %q", t.Type, ErrNotOnWire, "grpc")
	}
	if t.Address != "" {
		e.notOnWire("transport.address")
	}
	t.Type, t.Address = "", ""
	if t != (types.TransportConfig{}) {
		e.notOnWire("transport")
	}
}

func (e *encoder) int32(path string, v int) int32 {
	if v > math.MaxInt32 || v < math.MinInt32 {
		e.fail(path, "%d is outside the wire field's int32 range", v)
		return 0
	}
	return int32(v)
}

func (e *encoder) int32Ptr(path string, v *int) *int32 {
	if v == nil {
		return nil
	}
	n := e.int32(path, *v)
	return &n
}

// structFromJSON parses a JSON object into a Struct. Empty input is an
// unset field.
func (e *encoder) structFromJSON(path string, raw json.RawMessage) *structpb.Struct {
	if len(raw) == 0 {
		return nil
	}
	s := &structpb.Struct{}
	if err := protojson.Unmarshal(raw, s); err != nil {
		e.fail(path, "not a JSON object: %v", err)
		return nil
	}
	return s
}

func isZero[T any](v T) bool {
	return reflect.ValueOf(&v).Elem().IsZero()
}

func clonePtr[T any](v *T) *T {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

func (e *encoder) runConfig(rc *types.RunConfig) *pb.RunConfig {
	e.transport(rc.Transport)
	pc := &pb.RunConfig{
		RunId:                rc.RunID,
		Mode:                 rc.Mode,
		Prompt:               rc.Prompt,
		DynamicContext:       dynamicContextToProto(rc.DynamicContext),
		MaxTurns:             e.int32("maxTurns", rc.MaxTurns),
		MaxTokenBudget:       e.int32Ptr("maxTokenBudget", rc.MaxTokenBudget),
		Temperature:          clonePtr(rc.Temperature),
		ReasoningEffort:      rc.ReasoningEffort,
		ReasoningSummary:     rc.ReasoningSummary,
		Timeout:              e.int32Ptr("timeout", rc.Timeout),
		FollowUpGrace:        e.int32Ptr("followUpGrace", rc.FollowUpGrace),
		FollowUpHistory:      rc.FollowUpHistory,
		HeartbeatSeconds:     e.int32("heartbeatSeconds", rc.HeartbeatSeconds),
		SensitiveData:        clonePtr(rc.SensitiveData),
		LogLevel:             rc.LogLevel,
		SystemPromptOverride: rc.SystemPromptOverride,
		OutputSchema:         e.structFromJSON("outputSchema", rc.OutputSchema),
	}
	if name := rc.SessionName; name != "" {
		pc.SessionName = &name
	}
	if !isZero(rc.Provider) {
		pc.Provider = e.providerConfig("provider", &rc.Provider)
	}
	if len(rc.Providers) > 0 {
		pc.Providers = make(map[string]*pb.ProviderConfig, len(rc.Providers))
		for name, provider := range rc.Providers {
			pc.Providers[name] = e.providerConfig("providers."+name, &provider)
		}
	}
	if !isZero(rc.ModelRouter) {
		pc.ModelRouter = e.modelRouterConfig(&rc.ModelRouter)
	}
	if !isZero(rc.PromptBuilder) {
		pc.PromptBuilder = &pb.PromptBuilderConfig{
			Type:        rc.PromptBuilder.Type,
			Template:    rc.PromptBuilder.Template,
			PromptModel: rc.PromptBuilder.PromptModel,
		}
	}
	if !isZero(rc.ContextStrategy) {
		pc.ContextStrategy = e.contextStrategyConfig(&rc.ContextStrategy)
	}
	if !isZero(rc.Executor) {
		pc.Executor = e.executorConfig(&rc.Executor)
	}
	if !isZero(rc.EditStrategy) {
		pc.EditStrategy = &pb.EditStrategyConfig{
			Type:           rc.EditStrategy.Type,
			FuzzyThreshold: clonePtr(rc.EditStrategy.FuzzyThreshold),
		}
	}
	if !isZero(rc.Verifier) {
		pc.Verifier = e.verifierConfig("verifier", &rc.Verifier)
	}
	if !isZero(rc.PermissionPolicy) {
		pc.PermissionPolicy = &pb.PermissionPolicyConfig{
			Type:       rc.PermissionPolicy.Type,
			Timeout:    e.int32("permissionPolicy.timeout", rc.PermissionPolicy.Timeout),
			PolicyFile: rc.PermissionPolicy.PolicyFile,
			Fallback:   rc.PermissionPolicy.Fallback,
		}
	}
	if r2 := rc.RuleOfTwo; r2 != nil {
		pc.RuleOfTwo = &pb.RuleOfTwoConfig{Enforce: clonePtr(r2.Enforce)}
		if rt := r2.Runtime; rt != nil {
			pc.RuleOfTwo.Runtime = &pb.RuleOfTwoRuntimeConfig{
				Classifier:    rt.Classifier,
				OnDetect:      rt.OnDetect,
				GuardCriteria: slices.Clone(rt.GuardCriteria),
			}
		}
	}
	if cs := rc.CodeScanner; cs != nil {
		pc.CodeScanner = &pb.CodeScannerConfig{
			Type:              cs.Type,
			Scanners:          slices.Clone(cs.Scanners),
			BlockOnWarn:       cs.BlockOnWarn,
			SemgrepConfigPath: cs.SemgrepConfigPath,
		}
	}
	if !isZero(rc.GitStrategy) {
		pc.GitStrategy = &pb.GitStrategyConfig{Type: rc.GitStrategy.Type}
	}
	if !isZero(rc.TraceEmitter) {
		pc.TraceEmitter = e.traceEmitterConfig(&rc.TraceEmitter)
	}
	if !isZero(rc.Tools) {
		pc.Tools = e.toolsConfig(&rc.Tools)
	}
	if rc.GuardRail != nil {
		pc.GuardRail = e.guardRailConfig("guardRail", rc.GuardRail)
	}
	if !isZero(rc.Observability) {
		pc.Observability = &pb.ObservabilityConfig{
			ServiceName:      rc.Observability.ServiceName,
			Environment:      rc.Observability.Environment,
			ServiceNamespace: rc.Observability.ServiceNamespace,
		}
		if le := rc.Observability.LogsExport; !isZero(le) {
			pc.Observability.LogsExport = &pb.LogsExportConfig{
				Type:     le.Type,
				Endpoint: le.Endpoint,
			}
		}
	}
	if rs := rc.ResultSink; rs != nil {
		pc.ResultSink = &pb.ResultSinkConfig{
			Type:                       rs.Type,
			Topic:                      rs.Topic,
			MaxFinalAssistantTextBytes: e.int32("resultSink.maxFinalAssistantTextBytes", rs.MaxFinalAssistantTextBytes),
			Bucket:                     rs.Bucket,
			ObjectPrefix:               rs.ObjectPrefix,
			OrderingKey:                rs.OrderingKey,
			Attributes:                 maps.Clone(rs.Attributes),
			Credential:                 credentialConfigToProto(rs.Credential),
		}
	}
	if tce := rc.ToolChoiceEscalation; tce != nil {
		pc.ToolChoiceEscalation = &pb.ToolChoiceEscalationConfig{
			Enabled:    tce.Enabled,
			MaxRetries: e.int32("toolChoiceEscalation.maxRetries", tce.MaxRetries),
		}
	}
	if wd := rc.WindDown; wd != nil {
		pc.WindDown = &pb.WindDownConfig{Enabled: wd.Enabled}
	}
	if td := rc.ToolDispatch; td != nil {
		pc.ToolDispatch = &pb.ToolDispatchConfig{MaxParallel: e.int32("toolDispatch.maxParallel", td.MaxParallel)}
	}
	if rc.Hooks != nil {
		pc.Hooks = e.hooksConfig(rc.Hooks)
	}
	if rg := rc.RepositoryGuidance; rg != nil {
		pc.RepositoryGuidance = &pb.RepositoryGuidanceConfig{
			Files:        slices.Clone(rg.Files),
			MaxBytes:     e.int32("repositoryGuidance.maxBytes", rg.MaxBytes),
			PinnedSha256: maps.Clone(rg.PinnedSHA256),
		}
	}
	if cc := rc.Correlation; cc != nil {
		pc.Correlation = &pb.CorrelationConfig{
			SessionId: cc.SessionID,
			TaskId:    cc.TaskID,
			AttemptId: cc.AttemptID,
			UserId:    cc.UserID,
			Url:       cc.URL,
			Labels:    maps.Clone(cc.Labels),
		}
	}
	return pc
}

func (e *encoder) contextStrategyConfig(c *types.ContextStrategyConfig) *pb.ContextStrategyConfig {
	return &pb.ContextStrategyConfig{
		Type:         c.Type,
		MaxTokens:    e.int32("contextStrategy.maxTokens", c.MaxTokens),
		Keep:         e.int32Ptr("contextStrategy.keep", c.Keep),
		ClearAtLeast: e.int32Ptr("contextStrategy.clearAtLeast", c.ClearAtLeast),
		ExcludeTools: slices.Clone(c.ExcludeTools),
	}
}

func (e *encoder) guardRailConfig(path string, c *types.GuardRailConfig) *pb.GuardRailConfig {
	pc := &pb.GuardRailConfig{
		Type:           c.Type,
		Phases:         slices.Clone(c.Phases),
		Endpoint:       c.Endpoint,
		ApiKeyRef:      c.APIKeyRef,
		Model:          c.Model,
		Threshold:      c.Threshold,
		Criteria:       slices.Clone(c.Criteria),
		CustomCriteria: maps.Clone(c.CustomCriteria),
		Think:          clonePtr(c.Think),
		TimeoutMs:      e.int32(path+".timeoutMs", c.TimeoutMs),
		FailOpen:       c.FailOpen,
		MinChunkChars:  e.int32(path+".minChunkChars", c.MinChunkChars),
	}
	for i := range c.Stages {
		pc.Stages = append(pc.Stages, e.guardRailConfig(fmt.Sprintf("%s.stages[%d]", path, i), &c.Stages[i]))
	}
	return pc
}

func (e *encoder) providerConfig(path string, c *types.ProviderConfig) *pb.ProviderConfig {
	pc := &pb.ProviderConfig{
		Type:               c.Type,
		ApiKeyRef:          c.APIKeyRef,
		Region:             c.Region,
		Profile:            c.Profile,
		BaseUrl:            c.BaseURL,
		ApiKeyHeader:       c.APIKeyHeader,
		QueryParams:        maps.Clone(c.QueryParams),
		CompatProfile:      c.CompatProfile,
		EndpointProfile:    c.EndpointProfile,
		GcpProject:         c.GCPProject,
		GcpLocation:        c.GCPLocation,
		GcpCredentialsFile: c.GCPCredentialsFile,
		ReplayFile:         c.ReplayFile,
		Credential:         credentialConfigToProto(c.Credential),
	}
	for _, s := range c.GeminiSafetySettings {
		pc.GeminiSafetySettings = append(pc.GeminiSafetySettings, &pb.GeminiSafetySetting{
			Category:  s.Category,
			Threshold: s.Threshold,
		})
	}
	if r := c.Retry; r != nil {
		pc.Retry = &pb.ProviderRetryConfig{
			MaxAttempts:       e.int32(path+".retry.maxAttempts", r.MaxAttempts),
			InitialDelayMs:    e.int32(path+".retry.initialDelayMs", r.InitialDelayMs),
			MaxDelayMs:        e.int32(path+".retry.maxDelayMs", r.MaxDelayMs),
			WallClockBudgetMs: e.int32(path+".retry.wallClockBudgetMs", r.WallClockBudgetMs),
		}
	}
	if b := c.Batch; b != nil {
		pc.Batch = &pb.BatchProviderConfig{
			Enabled:                 b.Enabled,
			MaxWaitSeconds:          e.int32Ptr(path+".batch.maxWaitSeconds", b.MaxWaitSeconds),
			HarnessSidePolling:      b.HarnessSidePolling,
			FallbackOnTimeout:       b.FallbackOnTimeout,
			CancelBundleOnRunCancel: b.CancelBundleOnRunCancel,
			AllowInteractiveModes:   b.AllowInteractiveModes,
		}
	}
	return pc
}

func credentialConfigToProto(c *types.CredentialConfig) *pb.CredentialConfig {
	if c == nil {
		return nil
	}
	pc := &pb.CredentialConfig{
		Type:                     c.Type,
		RoleArn:                  c.RoleARN,
		SessionName:              c.SessionName,
		Audience:                 c.Audience,
		ServiceAccount:           c.ServiceAccount,
		FederationRuleId:         c.FederationRuleID,
		OrganizationId:           c.OrganizationID,
		ServiceAccountId:         c.ServiceAccountID,
		WorkspaceId:              c.WorkspaceID,
		AzureTenantId:            c.AzureTenantID,
		AzureClientId:            c.AzureClientID,
		AzureScope:               c.AzureScope,
		AzureTokenUrl:            c.AzureTokenURL,
		OpenaiIdentityProviderId: c.OpenAIIdentityProviderID,
		OpenaiServiceAccountId:   c.OpenAIServiceAccountID,
		OpenaiSubjectTokenType:   c.OpenAISubjectTokenType,
	}
	if ts := c.TokenSource; ts != nil {
		pc.TokenSource = &pb.TokenSourceConfig{
			Type:     ts.Type,
			Audience: ts.Audience,
			Path:     ts.Path,
			EnvVar:   ts.EnvVar,
			Resource: ts.Resource,
			ClientId: ts.ClientID,
		}
	}
	return pc
}

func (e *encoder) modelRouterConfig(c *types.ModelRouterConfig) *pb.ModelRouterConfig {
	return &pb.ModelRouterConfig{
		Type:                    c.Type,
		Provider:                c.Provider,
		Model:                   c.Model,
		ModeModels:              maps.Clone(c.ModeModels),
		CheapProvider:           c.CheapProvider,
		CheapModel:              c.CheapModel,
		ExpensiveProvider:       c.ExpensiveProvider,
		ExpensiveModel:          c.ExpensiveModel,
		ExpensiveTurnThreshold:  e.int32("modelRouter.expensiveTurnThreshold", c.ExpensiveTurnThreshold),
		ExpensiveTokenThreshold: e.int32("modelRouter.expensiveTokenThreshold", c.ExpensiveTokenThreshold),
		CheapStopReasons:        slices.Clone(c.CheapStopReasons),
	}
}

func (e *encoder) executorConfig(c *types.ExecutorConfig) *pb.ExecutorConfig {
	pc := &pb.ExecutorConfig{
		Type:                    c.Type,
		Workspace:               c.Workspace,
		Image:                   c.Image,
		Proxy:                   c.Proxy,
		Runtime:                 c.Runtime,
		K8SNamespace:            c.K8sNamespace,
		K8SKubeconfig:           c.K8sKubeconfig,
		K8SNodeSelector:         maps.Clone(c.K8sNodeSelector),
		K8SServiceAccount:       c.K8sServiceAccount,
		K8SEgressProxyUrl:       c.K8sEgressProxyURL,
		WorkspaceExportTo:       c.WorkspaceExportTo,
		WorkspaceExportRequired: clonePtr(c.WorkspaceExportRequired),
	}
	if si := c.SandboxIdentity; si != nil {
		pc.SandboxIdentity = &pb.SandboxIdentityConfig{
			Source:   si.Source,
			Audience: si.Audience,
			EnvVar:   si.EnvVar,
		}
	}
	if gp := c.GitProxy; gp != nil {
		pc.GitProxy = &pb.GitProxyConfig{
			Url:         gp.URL,
			Hosts:       slices.Clone(gp.Hosts),
			RewriteSsh:  gp.RewriteSsh,
			TokenEnvVar: gp.TokenEnvVar,
		}
	}
	if vb := c.VcsBackend; vb != nil {
		pc.VcsBackend = &pb.VcsBackendConfig{
			Type:      vb.Type,
			ApiKeyRef: vb.APIKeyRef,
			Repo:      vb.Repo,
			Ref:       vb.Ref,
		}
	}
	if n := c.Network; n != nil {
		pc.Network = &pb.NetworkConfig{
			Mode:      n.Mode,
			Allowlist: slices.Clone(n.Allowlist),
		}
	}
	if r := c.Resources; r != nil {
		pc.Resources = &pb.ResourceLimits{
			Cpus:      r.CPUs,
			MemoryMb:  e.int32("executor.resources.memoryMb", r.MemoryMB),
			DiskMb:    e.int32("executor.resources.diskMb", r.DiskMB),
			Pids:      e.int32("executor.resources.pids", r.PIDs),
			ScratchMb: e.int32("executor.resources.scratchMb", r.ScratchMB),
		}
	}
	return pc
}

func (e *encoder) hooksConfig(c *types.HooksConfig) *pb.HooksConfig {
	pc := &pb.HooksConfig{}
	for i := range c.PreRun {
		pc.PreRun = append(pc.PreRun, e.hookConfig(fmt.Sprintf("hooks.preRun[%d]", i), &c.PreRun[i]))
	}
	for i := range c.PostRun {
		pc.PostRun = append(pc.PostRun, e.hookConfig(fmt.Sprintf("hooks.postRun[%d]", i), &c.PostRun[i]))
	}
	return pc
}

func (e *encoder) hookConfig(path string, c *types.HookConfig) *pb.HookConfig {
	return &pb.HookConfig{
		Type:            c.Type,
		Name:            c.Name,
		Command:         c.Command,
		TimeoutSeconds:  e.int32(path+".timeoutSeconds", c.TimeoutSeconds),
		ContinueOnError: c.ContinueOnError,
		RunOn:           c.RunOn,
	}
}

func (e *encoder) verifierConfig(path string, c *types.VerifierConfig) *pb.VerifierConfig {
	pc := &pb.VerifierConfig{
		Type:     c.Type,
		Command:  c.Command,
		Timeout:  e.int32(path+".timeout", c.Timeout),
		Criteria: c.Criteria,
		Model:    c.Model,
	}
	for i := range c.Verifiers {
		pc.Verifiers = append(pc.Verifiers, e.verifierConfig(fmt.Sprintf("%s.verifiers[%d]", path, i), &c.Verifiers[i]))
	}
	return pc
}

func dynamicContextToProto(dc map[string]types.DynamicContextValue) map[string]*pb.DynamicContextValue {
	if len(dc) == 0 {
		return nil
	}
	out := make(map[string]*pb.DynamicContextValue, len(dc))
	for k, v := range dc {
		out[k] = &pb.DynamicContextValue{Value: v.Value, Sensitive: v.Sensitive}
	}
	return out
}

func (e *encoder) traceEmitterConfig(c *types.TraceEmitterConfig) *pb.TraceEmitterConfig {
	if c.Credential != nil {
		e.notOnWire("traceEmitter.credential")
	}
	pc := &pb.TraceEmitterConfig{
		Type:                   c.Type,
		FilePath:               c.FilePath,
		Endpoint:               c.Endpoint,
		MetricsEndpoint:        c.MetricsEndpoint,
		MetricsTemporality:     c.MetricsTemporality,
		Protocol:               c.Protocol,
		Headers:                maps.Clone(c.Headers),
		Bucket:                 c.Bucket,
		ObjectPrefix:           c.ObjectPrefix,
		CaptureContent:         c.CaptureContent,
		CaptureContentMaxBytes: e.int32("traceEmitter.captureContentMaxBytes", c.CaptureContentMaxBytes),
		Platform:               c.Platform,
		PlatformOptions:        maps.Clone(c.PlatformOptions),
	}
	if a := c.Archive; a != nil {
		if a.Credential != nil {
			e.notOnWire("traceEmitter.archive.credential")
		}
		pc.Archive = &pb.TraceArchiveConfig{
			Type:         a.Type,
			FilePath:     a.FilePath,
			Bucket:       a.Bucket,
			ObjectPrefix: a.ObjectPrefix,
		}
	}
	return pc
}

func (e *encoder) toolsConfig(c *types.ToolsConfig) *pb.ToolsConfig {
	pc := &pb.ToolsConfig{
		BuiltIn:               slices.Clone(c.BuiltIn),
		NoBuiltIn:             c.NoBuiltIn,
		MaxResultBytes:        e.int32("tools.maxResultBytes", c.MaxResultBytes),
		Profile:               c.Profile,
		ProtectedWriteTargets: slices.Clone(c.ProtectedWriteTargets),
	}
	if lazy := c.LazyLoading; !isZero(lazy) {
		pc.LazyLoading = &pb.LazyToolLoadingConfig{
			Enabled:    lazy.Enabled,
			Strategy:   lazy.Strategy,
			MaxResults: e.int32("tools.lazyLoading.maxResults", lazy.MaxResults),
			Eager:      slices.Clone(lazy.Eager),
		}
	}
	for _, srv := range c.MCPServers {
		pc.McpServers = append(pc.McpServers, &pb.MCPServerConfig{
			Name:            srv.Name,
			Uri:             srv.URI,
			ApiKeyRef:       srv.APIKeyRef,
			AllowedTools:    slices.Clone(srv.AllowedTools),
			AllowedMcpHosts: slices.Clone(srv.AllowedMCPHosts),
			Optional:        srv.Optional,
		})
	}
	for i, ct := range c.ControlPlane {
		path := fmt.Sprintf("tools.controlPlane[%d]", i)
		pc.ControlPlane = append(pc.ControlPlane, &pb.ControlPlaneToolConfig{
			Name:             ct.Name,
			Description:      ct.Description,
			InputSchema:      e.structFromJSON(path+".inputSchema", ct.InputSchema),
			TimeoutSeconds:   e.int32(path+".timeoutSeconds", ct.TimeoutSeconds),
			RequiresApproval: ct.RequiresApproval,
		})
	}
	if co := c.CommandOutput; !isZero(co) {
		pc.CommandOutput = &pb.CommandOutputConfig{
			Enabled:               clonePtr(co.Enabled),
			FailurePosture:        co.FailurePosture,
			InlineMaxBytes:        co.InlineMaxBytes,
			PreviewBytesPerStream: co.PreviewBytesPerStream,
			MaxBytesPerStream:     co.MaxBytesPerStream,
			MaxBytesPerRun:        co.MaxBytesPerRun,
		}
	}
	for i, pe := range c.PostEdit {
		pc.PostEdit = append(pc.PostEdit, &pb.PostEditCheck{
			Glob:           pe.Glob,
			Command:        pe.Command,
			TimeoutSeconds: e.int32(fmt.Sprintf("tools.postEdit[%d].timeoutSeconds", i), pe.TimeoutSeconds),
		})
	}
	return pc
}
