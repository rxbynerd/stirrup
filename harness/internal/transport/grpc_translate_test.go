package transport

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

// TestRunConfigFromProto_SessionNamePropagates ensures that a SessionName set
// on the proto RunConfig (the wire format used by the gRPC / K8s job path) is
// copied into the internal types.RunConfig. Without this, every job dispatched
// via the control plane would silently lose its session label.
func TestRunConfigFromProto_SessionNamePropagates(t *testing.T) {
	name := "nightly-eval"
	pc := &pb.RunConfig{
		SessionName: &name,
	}

	rc := runConfigFromProto(pc)

	if rc.SessionName != "nightly-eval" {
		t.Fatalf("SessionName not propagated: got %q, want %q", rc.SessionName, "nightly-eval")
	}
}

// TestRuleOfTwoProto_EmptyMessageDoesNotDisableEnforcement guards the
// proto3 wire default for RuleOfTwoConfig.enforce. The field is declared
// `optional bool` so an unset value is wire-distinguishable from an
// explicit `false`. If a future change drops the `optional` modifier,
// any control plane that includes a RuleOfTwoConfig sub-message at all
// (even an empty one) would silently bypass Rule-of-Two enforcement —
// the opposite of the intended secure default.
func TestRuleOfTwoProto_EmptyMessageDoesNotDisableEnforcement(t *testing.T) {
	t.Run("empty message round-trips as nil enforce", func(t *testing.T) {
		original := &pb.RuleOfTwoConfig{}
		bytes, err := proto.Marshal(original)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		var decoded pb.RuleOfTwoConfig
		if err := proto.Unmarshal(bytes, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		if decoded.Enforce != nil {
			t.Fatalf("expected Enforce nil for empty message, got %v", *decoded.Enforce)
		}

		// Confirm runConfigFromProto preserves the nil pointer so the
		// validator's secure default (enforce) applies.
		rc := runConfigFromProto(&pb.RunConfig{RuleOfTwo: &decoded})
		if rc.RuleOfTwo == nil {
			t.Fatal("expected types.RunConfig.RuleOfTwo to be non-nil when proto sub-message is present")
		}
		if rc.RuleOfTwo.Enforce != nil {
			t.Fatalf("expected types Enforce nil, got %v", *rc.RuleOfTwo.Enforce)
		}
	})

	t.Run("explicit false round-trips as &false", func(t *testing.T) {
		f := false
		original := &pb.RuleOfTwoConfig{Enforce: &f}
		bytes, err := proto.Marshal(original)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		var decoded pb.RuleOfTwoConfig
		if err := proto.Unmarshal(bytes, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		if decoded.Enforce == nil {
			t.Fatal("expected Enforce non-nil after explicit false")
		}
		if *decoded.Enforce != false {
			t.Fatalf("expected Enforce false, got %v", *decoded.Enforce)
		}
	})

	t.Run("explicit true round-trips as &true", func(t *testing.T) {
		tr := true
		original := &pb.RuleOfTwoConfig{Enforce: &tr}
		bytes, err := proto.Marshal(original)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		var decoded pb.RuleOfTwoConfig
		if err := proto.Unmarshal(bytes, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		if decoded.Enforce == nil {
			t.Fatal("expected Enforce non-nil after explicit true")
		}
		if *decoded.Enforce != true {
			t.Fatalf("expected Enforce true, got %v", *decoded.Enforce)
		}
	})
}

// TestRunConfigFromProto_TranslatesNewSafetyFields confirms that proto
// fields added in #42 (executor.runtime, ruleOfTwo, codeScanner,
// permissionPolicy.policy_file/fallback) are populated on the internal
// RunConfig. Without this, a control plane that sets these fields gets
// no visible behaviour change because the translate layer drops them.
func TestRunConfigFromProto_TranslatesNewSafetyFields(t *testing.T) {
	enforceFalse := false
	pc := &pb.RunConfig{
		Executor: &pb.ExecutorConfig{
			Type:    "container",
			Runtime: "runsc",
		},
		PermissionPolicy: &pb.PermissionPolicyConfig{
			Type:       "policy-engine",
			PolicyFile: "/etc/stirrup/policy.cedar",
			Fallback:   "deny-side-effects",
		},
		RuleOfTwo: &pb.RuleOfTwoConfig{Enforce: &enforceFalse},
		CodeScanner: &pb.CodeScannerConfig{
			Type:        "composite",
			Scanners:    []string{"patterns", "semgrep"},
			BlockOnWarn: true,
		},
	}

	rc := runConfigFromProto(pc)

	if rc.Executor.Runtime != "runsc" {
		t.Errorf("Executor.Runtime: got %q, want runsc", rc.Executor.Runtime)
	}
	if rc.PermissionPolicy.PolicyFile != "/etc/stirrup/policy.cedar" {
		t.Errorf("PolicyFile: got %q", rc.PermissionPolicy.PolicyFile)
	}
	if rc.PermissionPolicy.Fallback != "deny-side-effects" {
		t.Errorf("Fallback: got %q", rc.PermissionPolicy.Fallback)
	}
	if rc.RuleOfTwo == nil || rc.RuleOfTwo.Enforce == nil || *rc.RuleOfTwo.Enforce != false {
		t.Errorf("RuleOfTwo.Enforce not preserved: %+v", rc.RuleOfTwo)
	}
	if rc.CodeScanner == nil || rc.CodeScanner.Type != "composite" {
		t.Errorf("CodeScanner not translated: %+v", rc.CodeScanner)
	}
	if rc.CodeScanner != nil && len(rc.CodeScanner.Scanners) != 2 {
		t.Errorf("CodeScanner.Scanners: got %v", rc.CodeScanner.Scanners)
	}
	if rc.CodeScanner != nil && !rc.CodeScanner.BlockOnWarn {
		t.Errorf("CodeScanner.BlockOnWarn not translated")
	}
}

// TestRunConfigFromProto_TranslatesK8sExecutorFields pins that the k8s
// executor's namespace, kubeconfig, nodeSelector, and serviceAccount
// fields survive the gRPC translate layer. Without this, a control plane
// that dispatches a k8s run gets a Pod with no namespace and validation
// fails downstream with no visible cause on the wire.
func TestRunConfigFromProto_TranslatesK8sExecutorFields(t *testing.T) {
	pc := &pb.RunConfig{
		Executor: &pb.ExecutorConfig{
			Type:              "k8s",
			Image:             "ghcr.io/rxbynerd/stirrup:latest",
			Runtime:           "gvisor",
			K8SNamespace:      "agents",
			K8SKubeconfig:     "/home/u/.kube/config",
			K8SNodeSelector:   map[string]string{"disktype": "ssd"},
			K8SServiceAccount: "agent-sa",
		},
	}

	rc := runConfigFromProto(pc)

	if rc.Executor.Type != "k8s" {
		t.Errorf("Executor.Type: got %q, want k8s", rc.Executor.Type)
	}
	if rc.Executor.Runtime != "gvisor" {
		t.Errorf("Executor.Runtime: got %q, want gvisor", rc.Executor.Runtime)
	}
	if rc.Executor.K8sNamespace != "agents" {
		t.Errorf("K8sNamespace: got %q, want agents", rc.Executor.K8sNamespace)
	}
	if rc.Executor.K8sKubeconfig != "/home/u/.kube/config" {
		t.Errorf("K8sKubeconfig: got %q", rc.Executor.K8sKubeconfig)
	}
	if rc.Executor.K8sServiceAccount != "agent-sa" {
		t.Errorf("K8sServiceAccount: got %q, want agent-sa", rc.Executor.K8sServiceAccount)
	}
	if got := rc.Executor.K8sNodeSelector["disktype"]; got != "ssd" {
		t.Errorf("K8sNodeSelector[disktype]: got %q, want ssd", got)
	}
}

// TestRunConfigFromProto_TraceEmitterGCSFieldsPreserved pins the M1 fix:
// the gRPC RunConfig path used by `stirrup job` must carry the gcs
// trace emitter's Bucket and ObjectPrefix into the internal RunConfig.
// Without these fields the runtime either falls through to the jsonl
// fallback (Type=="" after copy) or hits a "bucket is required"
// construction error at the factory — both silently wrong on the
// Cloud Run dispatch path.
func TestRunConfigFromProto_TraceEmitterGCSFieldsPreserved(t *testing.T) {
	pc := &pb.RunConfig{
		TraceEmitter: &pb.TraceEmitterConfig{
			Type:         "gcs",
			Bucket:       "stirrup-results",
			ObjectPrefix: "traces/",
		},
	}
	rc := runConfigFromProto(pc)
	if rc.TraceEmitter.Type != "gcs" {
		t.Errorf("TraceEmitter.Type: got %q, want gcs", rc.TraceEmitter.Type)
	}
	if rc.TraceEmitter.Bucket != "stirrup-results" {
		t.Errorf("TraceEmitter.Bucket: got %q, want stirrup-results", rc.TraceEmitter.Bucket)
	}
	if rc.TraceEmitter.ObjectPrefix != "traces/" {
		t.Errorf("TraceEmitter.ObjectPrefix: got %q, want traces/", rc.TraceEmitter.ObjectPrefix)
	}
}

// TestRunConfigFromProto_SessionNameAbsentWhenNil documents the safe default:
// when the proto omits SessionName (the field is a proto3 optional, so the
// zero value is a nil pointer), the internal RunConfig must surface the empty
// string rather than panicking on a nil dereference.
func TestRunConfigFromProto_SessionNameAbsentWhenNil(t *testing.T) {
	pc := &pb.RunConfig{}

	rc := runConfigFromProto(pc)

	if rc.SessionName != "" {
		t.Fatalf("SessionName should be empty when proto field is nil, got %q", rc.SessionName)
	}

	// Cross-check: when the proto sub-message is omitted entirely,
	// types fields stay nil so ValidateRunConfig applies its defaults.
	rcEmpty := runConfigFromProto(&pb.RunConfig{})
	if rcEmpty.RuleOfTwo != nil {
		t.Error("RuleOfTwo should be nil when proto field absent")
	}
	if rcEmpty.CodeScanner != nil {
		t.Error("CodeScanner should be nil when proto field absent")
	}

	// Validator-level coupling: an unset proto Enforce must be treated
	// as "enforce" (the secure default). Add a separate types-level
	// assertion to defend against accidental sentinel-pointer rewrites.
	pcUnset := &pb.RunConfig{RuleOfTwo: &pb.RuleOfTwoConfig{}}
	rcUnset := runConfigFromProto(pcUnset)
	if rcUnset.RuleOfTwo == nil {
		t.Fatal("RuleOfTwo non-nil expected when proto field present")
	}
	if rcUnset.RuleOfTwo.Enforce != nil {
		t.Errorf("RuleOfTwo.Enforce should be nil for empty proto sub-message; got %v", *rcUnset.RuleOfTwo.Enforce)
	}
}

// TestRunConfigProtoRoundTrip_GuardRail asserts that every field on
// the proto GuardRailConfig is preserved by runConfigFromProto. A
// missing translation block for guard_rail (proto field 27) silently
// drops guard configuration on the K8s job path, leaving operators
// running a guardless harness even when their wire payload says
// otherwise. The composite stage exercises the recursion path so
// nested configurations also survive.
func TestRunConfigProtoRoundTrip_GuardRail(t *testing.T) {
	think := true
	pc := &pb.RunConfig{
		GuardRail: &pb.GuardRailConfig{
			Type:          "composite",
			Phases:        []string{"pre_turn", "post_turn"},
			Endpoint:      "",
			Model:         "",
			TimeoutMs:     1500,
			FailOpen:      true,
			MinChunkChars: 256,
			Stages: []*pb.GuardRailConfig{
				{
					Type:      "granite-guardian",
					Endpoint:  "http://classifier.local:9999",
					Model:     "ibm-granite/granite-guardian-4.1-8b",
					Threshold: 0,
					Criteria:  []string{"harm", "jailbreak"},
					CustomCriteria: map[string]string{
						"my_rule": "no profanity",
					},
					Think:         &think,
					TimeoutMs:     1500,
					FailOpen:      false,
					MinChunkChars: 256,
				},
				{
					Type:      "cloud-judge",
					Model:     "claude-haiku-4-5-20251001",
					TimeoutMs: 5000,
				},
			},
		},
	}

	rc := runConfigFromProto(pc)

	if rc.GuardRail == nil {
		t.Fatal("GuardRail dropped during proto translation")
	}
	if rc.GuardRail.Type != "composite" {
		t.Errorf("Type: got %q, want composite", rc.GuardRail.Type)
	}
	if got, want := rc.GuardRail.Phases, []string{"pre_turn", "post_turn"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Phases: got %v, want %v", got, want)
	}
	if rc.GuardRail.TimeoutMs != 1500 {
		t.Errorf("TimeoutMs: got %d, want 1500", rc.GuardRail.TimeoutMs)
	}
	if !rc.GuardRail.FailOpen {
		t.Errorf("FailOpen: got false, want true")
	}
	if rc.GuardRail.MinChunkChars != 256 {
		t.Errorf("MinChunkChars: got %d, want 256", rc.GuardRail.MinChunkChars)
	}
	if len(rc.GuardRail.Stages) != 2 {
		t.Fatalf("Stages: got %d, want 2", len(rc.GuardRail.Stages))
	}

	gg := rc.GuardRail.Stages[0]
	if gg.Type != "granite-guardian" {
		t.Errorf("Stages[0].Type: got %q", gg.Type)
	}
	if gg.Endpoint != "http://classifier.local:9999" {
		t.Errorf("Stages[0].Endpoint: got %q", gg.Endpoint)
	}
	if gg.Model != "ibm-granite/granite-guardian-4.1-8b" {
		t.Errorf("Stages[0].Model: got %q", gg.Model)
	}
	if len(gg.Criteria) != 2 || gg.Criteria[0] != "harm" || gg.Criteria[1] != "jailbreak" {
		t.Errorf("Stages[0].Criteria: got %v", gg.Criteria)
	}
	if gg.CustomCriteria["my_rule"] != "no profanity" {
		t.Errorf("Stages[0].CustomCriteria: got %v", gg.CustomCriteria)
	}
	if gg.Think == nil || *gg.Think != true {
		t.Errorf("Stages[0].Think: got %v, want pointer to true", gg.Think)
	}

	cj := rc.GuardRail.Stages[1]
	if cj.Type != "cloud-judge" {
		t.Errorf("Stages[1].Type: got %q", cj.Type)
	}
	if cj.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("Stages[1].Model: got %q", cj.Model)
	}
	if cj.TimeoutMs != 5000 {
		t.Errorf("Stages[1].TimeoutMs: got %d", cj.TimeoutMs)
	}
}

// TestRunConfigProtoRoundTrip_GuardRailNilStays_Nil documents that an
// absent guard_rail proto field translates to a nil GuardRail (the
// validator then applies the default — guards are opt-in).
func TestRunConfigProtoRoundTrip_GuardRailNilStaysNil(t *testing.T) {
	rc := runConfigFromProto(&pb.RunConfig{})
	if rc.GuardRail != nil {
		t.Errorf("GuardRail should be nil when proto field absent; got %+v", rc.GuardRail)
	}
}

// TestRunConfigFromProto_ObservabilityPropagates ensures that an
// ObservabilityConfig set on the proto RunConfig (the wire format used by
// the gRPC / K8s job path) is copied into the internal types.RunConfig.
// Without this translation block, every K8s job dispatched via the control
// plane would silently land in deployment.environment=local because the
// resource builder would only see the empty fallback after the proto value
// was dropped on the floor — the same class of bug as the prior SessionName
// (issue #50) and GuardRail (issue #43) regressions.
func TestRunConfigFromProto_ObservabilityPropagates(t *testing.T) {
	pc := &pb.RunConfig{
		Observability: &pb.ObservabilityConfig{
			Environment:      "staging",
			ServiceNamespace: "eval",
		},
	}

	rc := runConfigFromProto(pc)

	if rc.Observability.Environment != "staging" {
		t.Errorf("Observability.Environment: got %q, want staging", rc.Observability.Environment)
	}
	if rc.Observability.ServiceNamespace != "eval" {
		t.Errorf("Observability.ServiceNamespace: got %q, want eval", rc.Observability.ServiceNamespace)
	}
}

// TestRunConfigFromProto_ObservabilityAbsentWhenNil documents the safe
// default: when the proto omits the Observability sub-message, the internal
// RunConfig surfaces a zero-value ObservabilityConfig rather than panicking
// on a nil dereference. The resource builder then falls through to env-var
// fallbacks and finally to the documented defaults, which is exactly the
// path a no-config K8s job (operator pinning environment via OTEL_*
// variables on the pod spec) takes.
func TestRunConfigFromProto_ObservabilityAbsentWhenNil(t *testing.T) {
	rc := runConfigFromProto(&pb.RunConfig{})

	if rc.Observability.Environment != "" {
		t.Errorf("Observability.Environment should be empty when proto field is nil, got %q", rc.Observability.Environment)
	}
	if rc.Observability.ServiceNamespace != "" {
		t.Errorf("Observability.ServiceNamespace should be empty when proto field is nil, got %q", rc.Observability.ServiceNamespace)
	}
}

// TestRunConfigFromProto_AzureWIFFieldsPreserved guards the three
// Azure Workload Identity Federation fields (azure_tenant_id,
// azure_client_id, azure_scope) against silent drop in
// credentialConfigFromProto. Without this coverage, a control plane
// dispatching an Azure WIF run via the K8s job path would have its
// credential block stripped to type+tokenSource on the harness side,
// and the run would fail with a misleading "azureTenantId is required"
// validation error — the wire payload was correct but the translation
// layer dropped it on the floor. Mirrors the GCP federation coverage in
// TestRunConfigFromProto_CredentialWIFFieldsPreserved.
func TestRunConfigFromProto_AzureWIFFieldsPreserved(t *testing.T) {
	pc := &pb.RunConfig{
		Provider: &pb.ProviderConfig{
			Type: "openai-compatible",
			Credential: &pb.CredentialConfig{
				Type:          "azure-workload-identity",
				AzureTenantId: "11111111-2222-3333-4444-555555555555",
				AzureClientId: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
				AzureScope:    "https://cognitiveservices.azure.com/.default",
				AzureTokenUrl: "https://login.microsoftonline.us/11111111-2222-3333-4444-555555555555/oauth2/v2.0/token",
				TokenSource: &pb.TokenSourceConfig{
					Type: "file",
					Path: "/var/run/secrets/azure/token",
				},
			},
		},
	}

	rc := runConfigFromProto(pc)

	if rc.Provider.Credential == nil {
		t.Fatal("Credential dropped during proto translation")
	}
	if got, want := rc.Provider.Credential.Type, "azure-workload-identity"; got != want {
		t.Errorf("Credential.Type: got %q, want %q", got, want)
	}
	if got, want := rc.Provider.Credential.AzureTenantID, "11111111-2222-3333-4444-555555555555"; got != want {
		t.Errorf("Credential.AzureTenantID: got %q, want %q", got, want)
	}
	if got, want := rc.Provider.Credential.AzureClientID, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"; got != want {
		t.Errorf("Credential.AzureClientID: got %q, want %q", got, want)
	}
	if got, want := rc.Provider.Credential.AzureScope, "https://cognitiveservices.azure.com/.default"; got != want {
		t.Errorf("Credential.AzureScope: got %q, want %q", got, want)
	}
	if got, want := rc.Provider.Credential.AzureTokenURL, "https://login.microsoftonline.us/11111111-2222-3333-4444-555555555555/oauth2/v2.0/token"; got != want {
		t.Errorf("Credential.AzureTokenURL: got %q, want %q", got, want)
	}
	if rc.Provider.Credential.TokenSource == nil || rc.Provider.Credential.TokenSource.Path != "/var/run/secrets/azure/token" {
		t.Errorf("TokenSource dropped or mangled: %+v", rc.Provider.Credential.TokenSource)
	}
}

// TestRunConfigFromProto_CredentialWIFFieldsPreserved guards the
// credential federation proto fields against silent drop on the
// control-plane / K8s-job path. Without coverage here, a future edit
// to credentialConfigFromProto could omit one of the four fields
// (audience, serviceAccount on CredentialConfig; resource, clientId
// on the nested TokenSourceConfig) and operators would only discover
// it as a 401 from the federation endpoint at first run.
func TestRunConfigFromProto_CredentialWIFFieldsPreserved(t *testing.T) {
	pc := &pb.RunConfig{
		Provider: &pb.ProviderConfig{
			Type: "gemini",
			Credential: &pb.CredentialConfig{
				Type:           "gcp-workload-identity-federation",
				Audience:       "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/azure-pool/providers/azure-provider",
				ServiceAccount: "vertex@my-project.iam.gserviceaccount.com",
				TokenSource: &pb.TokenSourceConfig{
					Type:     "azure-imds",
					Resource: "api://AzureADTokenExchange",
					ClientId: "11111111-1111-1111-1111-111111111111",
				},
			},
		},
	}

	rc := runConfigFromProto(pc)

	if rc.Provider.Credential == nil {
		t.Fatal("Credential dropped during proto translation")
	}
	if got, want := rc.Provider.Credential.Type, "gcp-workload-identity-federation"; got != want {
		t.Errorf("Credential.Type: got %q, want %q", got, want)
	}
	if got, want := rc.Provider.Credential.Audience, "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/azure-pool/providers/azure-provider"; got != want {
		t.Errorf("Credential.Audience: got %q, want %q", got, want)
	}
	if got, want := rc.Provider.Credential.ServiceAccount, "vertex@my-project.iam.gserviceaccount.com"; got != want {
		t.Errorf("Credential.ServiceAccount: got %q, want %q", got, want)
	}

	ts := rc.Provider.Credential.TokenSource
	if ts == nil {
		t.Fatal("TokenSource dropped during proto translation")
	}
	if got, want := ts.Type, "azure-imds"; got != want {
		t.Errorf("TokenSource.Type: got %q, want %q", got, want)
	}
	if got, want := ts.Resource, "api://AzureADTokenExchange"; got != want {
		t.Errorf("TokenSource.Resource: got %q, want %q", got, want)
	}
	if got, want := ts.ClientID, "11111111-1111-1111-1111-111111111111"; got != want {
		t.Errorf("TokenSource.ClientID: got %q, want %q", got, want)
	}
}

// TestRunConfigFromProto_AnthropicWIFFieldsPreserved guards the four
// Anthropic Workload Identity Federation proto fields against silent
// drop on the control-plane / K8s-job path (issue #117 BLOCKING B1).
// Without coverage here, an operator delivering a RunConfig with
// credential.type=anthropic-wif via gRPC receives a CredentialConfig
// with all four required fields as empty strings — validation then
// rejects the config and the K8s job fails to start. Mirrors the GCP
// WIF round-trip test above; uses the generated getters in the
// translate layer to stay nil-safe.
func TestRunConfigFromProto_AnthropicWIFFieldsPreserved(t *testing.T) {
	pc := &pb.RunConfig{
		Provider: &pb.ProviderConfig{
			Type: "anthropic",
			Credential: &pb.CredentialConfig{
				Type:             "anthropic-wif",
				FederationRuleId: "fdrl_abc123",
				OrganizationId:   "550e8400-e29b-41d4-a716-446655440000",
				ServiceAccountId: "svac_xyz789",
				WorkspaceId:      "default",
				TokenSource: &pb.TokenSourceConfig{
					Type: "file",
					Path: "/var/run/secrets/idp/jwt",
				},
			},
		},
	}

	rc := runConfigFromProto(pc)

	if rc.Provider.Credential == nil {
		t.Fatal("Credential dropped during proto translation")
	}
	if got, want := rc.Provider.Credential.Type, "anthropic-wif"; got != want {
		t.Errorf("Credential.Type: got %q, want %q", got, want)
	}
	if got, want := rc.Provider.Credential.FederationRuleID, "fdrl_abc123"; got != want {
		t.Errorf("Credential.FederationRuleID: got %q, want %q", got, want)
	}
	if got, want := rc.Provider.Credential.OrganizationID, "550e8400-e29b-41d4-a716-446655440000"; got != want {
		t.Errorf("Credential.OrganizationID: got %q, want %q", got, want)
	}
	if got, want := rc.Provider.Credential.ServiceAccountID, "svac_xyz789"; got != want {
		t.Errorf("Credential.ServiceAccountID: got %q, want %q", got, want)
	}
	if got, want := rc.Provider.Credential.WorkspaceID, "default"; got != want {
		t.Errorf("Credential.WorkspaceID: got %q, want %q", got, want)
	}
}

// TestRunConfigFromProto_TraceEmitterProtocolAndHeadersPreserved guards
// the two new TraceEmitterConfig fields added by gh-100 (Protocol,
// Headers) against silent drop in runConfigFromProto. Without this
// coverage, a control plane dispatching a Grafana-Cloud-bound run via
// the K8s job path would have its protocol and Authorization header
// stripped on the harness side, falling back to the gRPC default with
// no exporter able to reach the gateway. The translation layer at
// grpc_translate.go:171-180 has caused this exact class of regression
// in prior PRs (#50 SessionName, #95/#117/#118 federation fields), and
// three reviewers independently flagged the gap on this PR — making
// it a recurring pattern this test pins shut.
//
// Mirrors TestRunConfigFromProto_ObservabilityPropagates above. Per
// synthesis MF-5 (3-reviewer consensus).
func TestRunConfigFromProto_TraceEmitterProtocolAndHeadersPreserved(t *testing.T) {
	pc := &pb.RunConfig{
		TraceEmitter: &pb.TraceEmitterConfig{
			Type:     "otel",
			Protocol: "http/protobuf",
			Endpoint: "https://otlp-gateway-prod-us-east-0.grafana.net/otlp",
			Headers: map[string]string{
				"Authorization": "Basic xxx",
				"X-Tenant":      "team-a",
			},
		},
	}

	rc := runConfigFromProto(pc)

	if got, want := rc.TraceEmitter.Protocol, "http/protobuf"; got != want {
		t.Errorf("TraceEmitter.Protocol: got %q, want %q", got, want)
	}
	if got, want := rc.TraceEmitter.Endpoint, "https://otlp-gateway-prod-us-east-0.grafana.net/otlp"; got != want {
		t.Errorf("TraceEmitter.Endpoint: got %q, want %q", got, want)
	}
	if got, want := rc.TraceEmitter.Headers["Authorization"], "Basic xxx"; got != want {
		t.Errorf("TraceEmitter.Headers[Authorization]: got %q, want %q", got, want)
	}
	if got, want := rc.TraceEmitter.Headers["X-Tenant"], "team-a"; got != want {
		t.Errorf("TraceEmitter.Headers[X-Tenant]: got %q, want %q", got, want)
	}
}

// TestRunConfigFromProto_ProviderRetryConfigPreserved guards the
// ProviderRetryConfig wire fields against the same silent-drop class of
// regression that occurred in gh-95 (SessionName), gh-117 (Anthropic
// WIF), gh-118 (Azure WIF), and gh-100 (TraceEmitter Protocol/Headers).
// Without this coverage, an operator who dispatches a tuned retry
// policy over gRPC would have it stripped on the harness side and
// silently replaced with the documented defaults — no log, no error.
// The non-nil case checks all four fields round-trip; the nil case
// checks that the wire-absent retry block produces a nil pointer on
// the Go side so the validator's defaulter runs (rather than the
// translate layer cross-binding "field unset" to "explicit zero").
func TestRunConfigFromProto_ProviderRetryConfigPreserved(t *testing.T) {
	t.Run("non-nil retry round-trips all four fields", func(t *testing.T) {
		pc := &pb.RunConfig{
			Provider: &pb.ProviderConfig{
				Type: "openai-compatible",
				Retry: &pb.ProviderRetryConfig{
					MaxAttempts:       4,
					InitialDelayMs:    250,
					MaxDelayMs:        12000,
					WallClockBudgetMs: 60000,
				},
			},
		}

		rc := runConfigFromProto(pc)

		if rc.Provider.Retry == nil {
			t.Fatal("Retry dropped during proto translation")
		}
		if got, want := rc.Provider.Retry.MaxAttempts, 4; got != want {
			t.Errorf("Retry.MaxAttempts: got %d, want %d", got, want)
		}
		if got, want := rc.Provider.Retry.InitialDelayMs, 250; got != want {
			t.Errorf("Retry.InitialDelayMs: got %d, want %d", got, want)
		}
		if got, want := rc.Provider.Retry.MaxDelayMs, 12000; got != want {
			t.Errorf("Retry.MaxDelayMs: got %d, want %d", got, want)
		}
		if got, want := rc.Provider.Retry.WallClockBudgetMs, 60000; got != want {
			t.Errorf("Retry.WallClockBudgetMs: got %d, want %d", got, want)
		}
	})

	t.Run("nil retry stays nil on the Go side", func(t *testing.T) {
		pc := &pb.RunConfig{
			Provider: &pb.ProviderConfig{
				Type: "openai-compatible",
			},
		}

		rc := runConfigFromProto(pc)

		if rc.Provider.Retry != nil {
			t.Fatalf("expected nil Retry when proto omits the field, got %+v", rc.Provider.Retry)
		}
	})
}

// TestRunConfigFromProto_ToolDispatchMaxParallelPreserved guards the
// translate hop that copies tool_dispatch.max_parallel from the wire
// format into types.RunConfig.ToolDispatch.MaxParallel (issue #184).
// The miss-pattern flagged by issues #117 and #118 is "new
// grpc_translate.go field added without a round-trip test then later
// drops on the floor"; this test pins the field so a future field
// reshuffle cannot silently regress the dispatch knob.
func TestRunConfigFromProto_ToolDispatchMaxParallelPreserved(t *testing.T) {
	pc := &pb.RunConfig{
		ToolDispatch: &pb.ToolDispatchConfig{MaxParallel: 8},
	}

	rc := runConfigFromProto(pc)

	if rc.ToolDispatch == nil {
		t.Fatal("ToolDispatch sub-config dropped: got nil, want populated")
	}
	if rc.ToolDispatch.MaxParallel != 8 {
		t.Errorf("ToolDispatch.MaxParallel: got %d, want 8", rc.ToolDispatch.MaxParallel)
	}
}

// TestRunConfigFromProto_BatchProviderConfigPreserved pins the phase-1
// review fix for the silent K8s-job batch drop: the proto's
// ProviderConfig.batch (field 14) must round-trip into the internal
// types.ProviderConfig.Batch with every field preserved and the
// nil/non-nil distinction on MaxWaitSeconds intact. Without the
// translation block any TaskAssignment with batch.enabled=true was
// silently degraded to a streaming run with no log entry — the same
// failure mode that caused gh-95, gh-117, gh-118, and gh-100.
func TestRunConfigFromProto_BatchProviderConfigPreserved(t *testing.T) {
	t.Run("non-nil batch round-trips all six fields", func(t *testing.T) {
		var maxWait int32 = 3600
		// Use protobuf marshal/unmarshal so we exercise the actual wire
		// format and not just an in-memory pointer copy. The phase-1
		// reviewer specifically called out that the gap is on the gRPC
		// path, so the test should cover Marshal -> Unmarshal as well as
		// the Go-side translate call.
		original := &pb.RunConfig{
			Provider: &pb.ProviderConfig{
				Type: "anthropic",
				Batch: &pb.BatchProviderConfig{
					Enabled:                 true,
					MaxWaitSeconds:          &maxWait,
					HarnessSidePolling:      true,
					FallbackOnTimeout:       true,
					CancelBundleOnRunCancel: true,
					AllowInteractiveModes:   true,
				},
			},
		}

		bytes, err := proto.Marshal(original)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded pb.RunConfig
		if err := proto.Unmarshal(bytes, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		rc := runConfigFromProto(&decoded)

		if rc.Provider.Batch == nil {
			t.Fatal("Batch dropped during proto translation")
		}
		if !rc.Provider.Batch.Enabled {
			t.Errorf("Batch.Enabled: got false, want true")
		}
		if rc.Provider.Batch.MaxWaitSeconds == nil {
			t.Fatal("Batch.MaxWaitSeconds: got nil, want non-nil pointer")
		}
		if got := *rc.Provider.Batch.MaxWaitSeconds; got != 3600 {
			t.Errorf("Batch.MaxWaitSeconds: got %d, want 3600", got)
		}
		if !rc.Provider.Batch.HarnessSidePolling {
			t.Errorf("Batch.HarnessSidePolling: got false, want true")
		}
		if !rc.Provider.Batch.FallbackOnTimeout {
			t.Errorf("Batch.FallbackOnTimeout: got false, want true")
		}
		if !rc.Provider.Batch.CancelBundleOnRunCancel {
			t.Errorf("Batch.CancelBundleOnRunCancel: got false, want true")
		}
		if !rc.Provider.Batch.AllowInteractiveModes {
			t.Errorf("Batch.AllowInteractiveModes: got false, want true")
		}
	})

	t.Run("nil batch stays nil on the Go side", func(t *testing.T) {
		pc := &pb.RunConfig{
			Provider: &pb.ProviderConfig{Type: "anthropic"},
		}

		rc := runConfigFromProto(pc)

		if rc.Provider.Batch != nil {
			t.Fatalf("expected nil Batch when proto omits the field, got %+v", rc.Provider.Batch)
		}
	})

	t.Run("batch present with MaxWaitSeconds unset preserves nil", func(t *testing.T) {
		// The wire's `optional int32` distinguishes unset from explicit
		// zero. ValidateRunConfig depends on the nil to apply the
		// default; an always-allocated *int would erase that and pin
		// MaxWaitSeconds at 0, which the validator then rejects as
		// "must be in range (0, 86400]".
		original := &pb.RunConfig{
			Provider: &pb.ProviderConfig{
				Type:  "anthropic",
				Batch: &pb.BatchProviderConfig{Enabled: true},
			},
		}
		bytes, err := proto.Marshal(original)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded pb.RunConfig
		if err := proto.Unmarshal(bytes, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		rc := runConfigFromProto(&decoded)
		if rc.Provider.Batch == nil {
			t.Fatal("expected non-nil Batch when proto sub-message is present")
		}
		if rc.Provider.Batch.MaxWaitSeconds != nil {
			t.Errorf("MaxWaitSeconds should be nil when proto field is unset; got %v", *rc.Provider.Batch.MaxWaitSeconds)
		}
	})
}

// TestRunConfigFromProto_CompatProfileRoundTrip pins the wire-to-
// internal copy of ProviderConfig.CompatProfile (issue #221 Wave 2).
// The translate hop is forward-only — operators set CompatProfile on
// the wire and the harness consumes it; there is no symmetric Go-to-
// proto path for this field. The same silent-drop class of regression
// that bit SessionName (gh-95), Anthropic WIF (gh-117), Azure WIF
// (gh-118), and TraceEmitter (gh-100) is the failure mode this test
// guards against: a future ProviderConfig field reshuffle could drop
// CompatProfile from the translate block and a Z.ai-targeted run
// would silently fall back to the openai-compatible defaults — a
// HTTP 400 on the first turn rather than a clean validation error.
//
// Three sub-cases cover the wire→internal contract:
//   - explicit "zai-glm" round-trips verbatim
//   - empty string round-trips as empty (default profile)
//   - unrecognised string round-trips verbatim; ValidateRunConfig is
//     responsible for rejecting it — the translate layer must not
//     silently coerce unknown values
func TestRunConfigFromProto_CompatProfileRoundTrip(t *testing.T) {
	cases := []struct {
		name        string
		profile     string
		wantProfile string
	}{
		{name: "zai-glm", profile: "zai-glm", wantProfile: "zai-glm"},
		{name: "empty default", profile: "", wantProfile: ""},
		{name: "unrecognised value passes through", profile: "future-profile", wantProfile: "future-profile"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pc := &pb.RunConfig{
				Provider: &pb.ProviderConfig{
					Type:          "openai-compatible",
					CompatProfile: tc.profile,
				},
			}
			rc := runConfigFromProto(pc)
			if rc.Provider.CompatProfile != tc.wantProfile {
				t.Errorf("CompatProfile: got %q, want %q", rc.Provider.CompatProfile, tc.wantProfile)
			}
		})
	}
}

// TestRunConfigFromProto_TemperatureRoundTrip pins the pointer-vs-nil
// distinction across the wire boundary for RunConfig.Temperature
// (issue #217). The control plane must be able to (a) leave the field
// unset and inherit the harness default, (b) request a non-zero
// temperature, and (c) request greedy decoding by sending an explicit
// 0.0. Without the optional-double field on the proto side, case (c)
// is wire-indistinguishable from case (a) — exactly the failure mode
// the proto's `optional` keyword exists to prevent.
func TestRunConfigFromProto_TemperatureRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		set  *float64
	}{
		{name: "nil_unset", set: nil},
		{name: "explicit_zero_greedy", set: proto.Float64(0.0)},
		{name: "mid_range", set: proto.Float64(0.7)},
		{name: "above_anthropic_ceiling", set: proto.Float64(1.5)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pc := &pb.RunConfig{Temperature: tc.set}
			rc := runConfigFromProto(pc)
			switch {
			case tc.set == nil && rc.Temperature != nil:
				t.Fatalf("nil-on-wire translated to non-nil internal: got %v", *rc.Temperature)
			case tc.set != nil && rc.Temperature == nil:
				t.Fatalf("set-on-wire (%v) dropped to nil internal", *tc.set)
			case tc.set != nil && *rc.Temperature != *tc.set:
				t.Errorf("Temperature: got %v, want %v", *rc.Temperature, *tc.set)
			}
		})
	}
}

// TestRunTraceToProto_OutcomePopulated pins #141: the canonical Outcome must
// land on the proto outcome field verbatim, and stop_reason must mirror it so
// pre-outcome consumers keep the value they have always read. The case set is
// the full 11-value types.RunTrace.Outcome superset of the loop's stop reasons
// — including the proto-stop_reason-foreign "success", "verification_failed",
// "verification_error", and "max_tokens" — plus the empty zero value. The
// proto.Marshal/Unmarshal round-trip pins the field's wire tag (outcome = 8),
// which a struct-copy assertion alone would not catch.
func TestRunTraceToProto_OutcomePopulated(t *testing.T) {
	cases := []struct {
		name    string
		outcome string
	}{
		{name: "success", outcome: "success"},
		{name: "error", outcome: "error"},
		{name: "max_turns", outcome: "max_turns"},
		{name: "verification_failed", outcome: "verification_failed"},
		{name: "verification_error", outcome: "verification_error"},
		{name: "budget_exceeded", outcome: "budget_exceeded"},
		{name: "stalled", outcome: "stalled"},
		{name: "tool_failures", outcome: "tool_failures"},
		{name: "cancelled", outcome: "cancelled"},
		{name: "timeout", outcome: "timeout"},
		{name: "max_tokens", outcome: "max_tokens"},
		{name: "empty", outcome: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Unix(1700000000, 0)
			tr := &types.RunTrace{
				ID:          "run-141",
				Turns:       3,
				TokenUsage:  types.TokenUsage{Input: 120, Output: 45},
				StartedAt:   start,
				CompletedAt: start.Add(2500 * time.Millisecond),
				Outcome:     tc.outcome,
			}
			pt := runTraceToProto(tr)
			if pt.Outcome != tc.outcome {
				t.Errorf("Outcome: got %q, want %q", pt.Outcome, tc.outcome)
			}
			if pt.StopReason != tc.outcome {
				t.Errorf("StopReason should mirror Outcome: got %q, want %q", pt.StopReason, tc.outcome)
			}
			if pt.RunId != "run-141" {
				t.Errorf("RunId: got %q, want run-141", pt.RunId)
			}
			if pt.Turns != 3 {
				t.Errorf("Turns: got %d, want 3", pt.Turns)
			}
			if pt.InputTokens != 120 || pt.OutputTokens != 45 {
				t.Errorf("tokens: got in=%d out=%d, want in=120 out=45", pt.InputTokens, pt.OutputTokens)
			}
			if pt.DurationMs != 2500 {
				t.Errorf("DurationMs: got %d, want 2500", pt.DurationMs)
			}

			raw, err := proto.Marshal(pt)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var decoded pb.RunTrace
			if err := proto.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if decoded.Outcome != tc.outcome {
				t.Errorf("outcome did not survive wire round-trip: got %q, want %q", decoded.Outcome, tc.outcome)
			}
			if decoded.StopReason != tc.outcome {
				t.Errorf("stop_reason did not survive wire round-trip: got %q, want %q", decoded.StopReason, tc.outcome)
			}
		})
	}
}
