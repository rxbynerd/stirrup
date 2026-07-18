package transport

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

// TestRunConfigFromProto_SessionNamePropagates ensures SessionName set on
// the proto RunConfig is copied into the internal types.RunConfig.
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

// TestRunConfigFromProto_PromptBuilderFieldsPreserved confirms
// prompt_builder.template and prompt_builder.prompt_model survive
// translation.
func TestRunConfigFromProto_PromptBuilderFieldsPreserved(t *testing.T) {
	pc := &pb.RunConfig{
		PromptBuilder: &pb.PromptBuilderConfig{
			Type:        "composed",
			Template:    `Custom agent.{{if eq .Tier "frontier"}} Act when ready.{{end}}`,
			PromptModel: "claude-fable-5",
		},
	}

	rc := runConfigFromProto(pc)

	if rc.PromptBuilder.Type != "composed" {
		t.Errorf("PromptBuilder.Type not propagated: got %q", rc.PromptBuilder.Type)
	}
	if rc.PromptBuilder.Template != pc.PromptBuilder.Template {
		t.Errorf("PromptBuilder.Template not propagated: got %q", rc.PromptBuilder.Template)
	}
	if rc.PromptBuilder.PromptModel != "claude-fable-5" {
		t.Errorf("PromptBuilder.PromptModel not propagated: got %q", rc.PromptBuilder.PromptModel)
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

// TestRuleOfTwoProto_RuntimeTranslation pins the runtime sub-message
// translation. The nil-vs-present distinction matters the same way it
// does for enforce: an absent proto Runtime must stay nil on the
// internal config so the factory's default arming applies, while a
// present-but-empty one must surface as a present-but-empty block.
func TestRuleOfTwoProto_RuntimeTranslation(t *testing.T) {
	t.Run("absent runtime stays nil", func(t *testing.T) {
		rc := runConfigFromProto(&pb.RunConfig{RuleOfTwo: &pb.RuleOfTwoConfig{}})
		if rc.RuleOfTwo == nil {
			t.Fatal("expected RuleOfTwo non-nil when proto sub-message present")
		}
		if rc.RuleOfTwo.Runtime != nil {
			t.Fatalf("expected Runtime nil when proto runtime absent, got %+v", rc.RuleOfTwo.Runtime)
		}
	})

	t.Run("empty runtime surfaces as present-but-empty", func(t *testing.T) {
		rc := runConfigFromProto(&pb.RunConfig{
			RuleOfTwo: &pb.RuleOfTwoConfig{Runtime: &pb.RuleOfTwoRuntimeConfig{}},
		})
		if rc.RuleOfTwo == nil || rc.RuleOfTwo.Runtime == nil {
			t.Fatalf("expected present-but-empty Runtime, got %+v", rc.RuleOfTwo)
		}
		if rc.RuleOfTwo.Runtime.Classifier != "" || rc.RuleOfTwo.Runtime.OnDetect != "" || rc.RuleOfTwo.Runtime.GuardCriteria != nil {
			t.Errorf("expected zero-valued Runtime, got %+v", rc.RuleOfTwo.Runtime)
		}
	})

	t.Run("populated runtime translates fields", func(t *testing.T) {
		src := &pb.RunConfig{
			RuleOfTwo: &pb.RuleOfTwoConfig{
				Runtime: &pb.RuleOfTwoRuntimeConfig{
					Classifier:    "patterns",
					OnDetect:      "block-external",
					GuardCriteria: []string{"sensitive_data", "pii"},
				},
			},
		}
		rc := runConfigFromProto(src)
		rt := rc.RuleOfTwo.Runtime
		if rt == nil {
			t.Fatal("expected Runtime non-nil")
		}
		if rt.Classifier != "patterns" {
			t.Errorf("Classifier: got %q, want %q", rt.Classifier, "patterns")
		}
		if rt.OnDetect != "block-external" {
			t.Errorf("OnDetect: got %q, want %q", rt.OnDetect, "block-external")
		}
		if len(rt.GuardCriteria) != 2 || rt.GuardCriteria[0] != "sensitive_data" || rt.GuardCriteria[1] != "pii" {
			t.Errorf("GuardCriteria: got %v", rt.GuardCriteria)
		}
		// GuardCriteria must be copied, not aliased, so mutating the
		// source after translation cannot reach the translated copy.
		src.RuleOfTwo.Runtime.GuardCriteria[0] = "mutated"
		if rt.GuardCriteria[0] != "sensitive_data" {
			t.Error("GuardCriteria shares backing array with proto source")
		}
	})
}

// TestRunConfigFromProto_TranslatesNewSafetyFields confirms
// executor.runtime, ruleOfTwo, codeScanner, and
// permissionPolicy.policy_file/fallback populate the internal RunConfig.
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
// fields survive the gRPC translate layer.
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

// TestRunConfigFromProto_TraceEmitterGCSFieldsPreserved pins that the
// gcs trace emitter's Bucket and ObjectPrefix survive translation.
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

	rcEmpty := runConfigFromProto(&pb.RunConfig{})
	if rcEmpty.RuleOfTwo != nil {
		t.Error("RuleOfTwo should be nil when proto field absent")
	}
	if rcEmpty.CodeScanner != nil {
		t.Error("CodeScanner should be nil when proto field absent")
	}

	pcUnset := &pb.RunConfig{RuleOfTwo: &pb.RuleOfTwoConfig{}}
	rcUnset := runConfigFromProto(pcUnset)
	if rcUnset.RuleOfTwo == nil {
		t.Fatal("RuleOfTwo non-nil expected when proto field present")
	}
	if rcUnset.RuleOfTwo.Enforce != nil {
		t.Errorf("RuleOfTwo.Enforce should be nil for empty proto sub-message; got %v", *rcUnset.RuleOfTwo.Enforce)
	}
}

// TestRunConfigProtoRoundTrip_GuardRail asserts that every field on the
// proto GuardRailConfig is preserved by runConfigFromProto, including
// the recursive Stages case.
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

// TestRunConfigFromProto_ObservabilityPropagates ensures an
// ObservabilityConfig set on the proto RunConfig is copied into the
// internal types.RunConfig.
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
// on a nil dereference.
func TestRunConfigFromProto_ObservabilityAbsentWhenNil(t *testing.T) {
	rc := runConfigFromProto(&pb.RunConfig{})

	if rc.Observability.Environment != "" {
		t.Errorf("Observability.Environment should be empty when proto field is nil, got %q", rc.Observability.Environment)
	}
	if rc.Observability.ServiceNamespace != "" {
		t.Errorf("Observability.ServiceNamespace should be empty when proto field is nil, got %q", rc.Observability.ServiceNamespace)
	}
}

// TestRunConfigFromProto_AzureWIFFieldsPreserved guards the Azure
// Workload Identity Federation fields (azure_tenant_id, azure_client_id,
// azure_scope) against silent drop in credentialConfigFromProto.
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

// TestRunConfigFromProto_CredentialWIFFieldsPreserved guards the GCP
// credential federation proto fields (audience, serviceAccount, and the
// nested TokenSourceConfig's resource/clientId) against silent drop.
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
// drop in credentialConfigFromProto.
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

// TestRunConfigFromProto_OpenAIWIFFieldsPreserved guards the three
// OpenAI Workload Identity Federation proto fields against silent drop
// in credentialConfigFromProto.
func TestRunConfigFromProto_OpenAIWIFFieldsPreserved(t *testing.T) {
	pc := &pb.RunConfig{
		Provider: &pb.ProviderConfig{
			Type: "openai-compatible",
			Credential: &pb.CredentialConfig{
				Type:                     "openai-wif",
				OpenaiIdentityProviderId: "idp_abc123",
				OpenaiServiceAccountId:   "sa_xyz789",
				OpenaiSubjectTokenType:   "urn:ietf:params:oauth:token-type:jwt",
				TokenSource: &pb.TokenSourceConfig{
					Type:     "github-actions-oidc",
					Audience: "https://api.openai.com/v1",
				},
			},
		},
	}

	rc := runConfigFromProto(pc)

	if rc.Provider.Credential == nil {
		t.Fatal("Credential dropped during proto translation")
	}
	if got, want := rc.Provider.Credential.Type, "openai-wif"; got != want {
		t.Errorf("Credential.Type: got %q, want %q", got, want)
	}
	if got, want := rc.Provider.Credential.OpenAIIdentityProviderID, "idp_abc123"; got != want {
		t.Errorf("Credential.OpenAIIdentityProviderID: got %q, want %q", got, want)
	}
	if got, want := rc.Provider.Credential.OpenAIServiceAccountID, "sa_xyz789"; got != want {
		t.Errorf("Credential.OpenAIServiceAccountID: got %q, want %q", got, want)
	}
	if got, want := rc.Provider.Credential.OpenAISubjectTokenType, "urn:ietf:params:oauth:token-type:jwt"; got != want {
		t.Errorf("Credential.OpenAISubjectTokenType: got %q, want %q", got, want)
	}
	if rc.Provider.Credential.TokenSource == nil || rc.Provider.Credential.TokenSource.Audience != "https://api.openai.com/v1" {
		t.Errorf("TokenSource dropped or mangled: %+v", rc.Provider.Credential.TokenSource)
	}
}

// TestRunConfigFromProto_TraceEmitterProtocolAndHeadersPreserved guards
// TraceEmitterConfig's Protocol and Headers fields against silent drop
// in runConfigFromProto.
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
// ProviderRetryConfig wire fields against silent drop. The non-nil case
// checks all four fields round-trip; the nil case checks that a
// wire-absent retry block produces a nil pointer so the validator's
// defaulter runs.
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
// format into types.RunConfig.ToolDispatch.MaxParallel.
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

// TestRunConfigFromProto_BatchProviderConfigPreserved pins that the
// proto's ProviderConfig.batch round-trips into the internal
// types.ProviderConfig.Batch with every field preserved and the
// nil/non-nil distinction on MaxWaitSeconds intact.
func TestRunConfigFromProto_BatchProviderConfigPreserved(t *testing.T) {
	t.Run("non-nil batch round-trips all six fields", func(t *testing.T) {
		var maxWait int32 = 3600
		// Marshal/unmarshal to exercise the actual wire format, not just
		// an in-memory pointer copy.
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
// internal copy of ProviderConfig.CompatProfile. The translate hop is
// forward-only — operators set CompatProfile on the wire and the
// harness consumes it, with no symmetric Go-to-proto path. An
// unrecognised value must round-trip verbatim; ValidateRunConfig is
// responsible for rejecting it, not the translate layer.
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
// distinction across the wire boundary for RunConfig.Temperature: unset
// must inherit the harness default, while an explicit 0.0 (greedy
// decoding) must stay distinguishable from unset.
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

// TestRunTraceToProto_OutcomePopulated pins that the canonical Outcome
// lands on the proto outcome field verbatim, and stop_reason mirrors it
// for consumers predating the outcome field. The proto.Marshal/Unmarshal
// round-trip pins the field's wire tag, which a struct-copy assertion
// alone would not catch.
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

// TestRunConfigFromProto_HooksAbsentStaysNil pins the nil/absent case:
// a TaskAssignment with no hooks sub-message must not synthesise a
// non-nil types.HooksConfig.
func TestRunConfigFromProto_HooksAbsentStaysNil(t *testing.T) {
	rc := runConfigFromProto(&pb.RunConfig{})
	if rc.Hooks != nil {
		t.Errorf("Hooks should be nil when proto field absent; got %+v", rc.Hooks)
	}
}

// TestRunConfigFromProto_HooksEmptyStaysNonNil pins the companion case:
// an explicit-but-empty proto HooksConfig{} (e.g. a control plane that
// always sets the sub-message) must translate to a non-nil, empty
// types.HooksConfig — distinguishable from "field absent" the same way
// ToolDispatch preserves the unset/explicit-zero distinction.
func TestRunConfigFromProto_HooksEmptyStaysNonNil(t *testing.T) {
	rc := runConfigFromProto(&pb.RunConfig{Hooks: &pb.HooksConfig{}})
	if rc.Hooks == nil {
		t.Fatal("Hooks should be non-nil for an explicit-but-empty proto sub-message")
	}
	if len(rc.Hooks.PreRun) != 0 || len(rc.Hooks.PostRun) != 0 {
		t.Errorf("expected empty PreRun/PostRun, got %+v", rc.Hooks)
	}
}

// TestRunConfigFromProto_HooksFieldsPreserved guards the translate hop
// that copies every HookConfig field from the wire format into
// types.HookConfig, exercising the actual Marshal -> Unmarshal wire path.
func TestRunConfigFromProto_HooksFieldsPreserved(t *testing.T) {
	original := &pb.RunConfig{
		Hooks: &pb.HooksConfig{
			PreRun: []*pb.HookConfig{
				{Type: "command", Name: "clone", Command: "git clone . .", TimeoutSeconds: 120},
			},
			PostRun: []*pb.HookConfig{
				{Command: "test -f marker", RunOn: "success", ContinueOnError: true},
				{Command: "curl -X POST https://example.com", RunOn: "failure"},
			},
		},
	}

	raw, err := proto.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded pb.RunConfig
	if err := proto.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	rc := runConfigFromProto(&decoded)
	if rc.Hooks == nil {
		t.Fatal("Hooks dropped across the wire round-trip")
	}
	if len(rc.Hooks.PreRun) != 1 {
		t.Fatalf("PreRun: got %d entries, want 1", len(rc.Hooks.PreRun))
	}
	pre := rc.Hooks.PreRun[0]
	if pre.Type != "command" || pre.Name != "clone" || pre.Command != "git clone . ." || pre.TimeoutSeconds != 120 {
		t.Errorf("PreRun[0] = %+v, want {command, clone, git clone . ., 120, false, \"\"}", pre)
	}

	if len(rc.Hooks.PostRun) != 2 {
		t.Fatalf("PostRun: got %d entries, want 2", len(rc.Hooks.PostRun))
	}
	post0 := rc.Hooks.PostRun[0]
	if post0.Command != "test -f marker" || post0.RunOn != "success" || !post0.ContinueOnError {
		t.Errorf("PostRun[0] = %+v, want {test -f marker, success, true}", post0)
	}
	post1 := rc.Hooks.PostRun[1]
	if post1.Command != "curl -X POST https://example.com" || post1.RunOn != "failure" || post1.ContinueOnError {
		t.Errorf("PostRun[1] = %+v, want {curl -X POST https://example.com, failure, false}", post1)
	}
}

// TestRunConfigFromProto_HooksNotAliased pins that mutating the source
// *pb.RunConfig after translation does not reach the translated
// types.RunConfig.
func TestRunConfigFromProto_HooksNotAliased(t *testing.T) {
	pc := &pb.RunConfig{
		Hooks: &pb.HooksConfig{
			PreRun: []*pb.HookConfig{{Command: "original"}},
		},
	}

	rc := runConfigFromProto(pc)

	pc.Hooks.PreRun[0].Command = "mutated-after-translate"

	if rc.Hooks.PreRun[0].Command != "original" {
		t.Errorf("translated HookConfig aliases the proto's backing data: got %q, want %q",
			rc.Hooks.PreRun[0].Command, "original")
	}
}

// TestRunConfigFromProto_MCPTrustFieldsPreserved guards the MCP trust
// fields (AllowedTools, AllowedMCPHosts) against silent drop in
// toolsConfigFromProto. Marshal/Unmarshal through the actual wire format
// so a future field-number collision on MCPServerConfig shows up here.
func TestRunConfigFromProto_MCPTrustFieldsPreserved(t *testing.T) {
	original := &pb.RunConfig{
		Tools: &pb.ToolsConfig{
			McpServers: []*pb.MCPServerConfig{
				{
					Name:            "search",
					Uri:             "https://mcp.example.com/v1",
					AllowedTools:    []string{"lookup", "query"},
					AllowedMcpHosts: []string{"mcp.example.com"},
				},
			},
		},
	}

	raw, err := proto.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded pb.RunConfig
	if err := proto.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	rc := runConfigFromProto(&decoded)

	if len(rc.Tools.MCPServers) != 1 {
		t.Fatalf("MCPServers: got %d entries, want 1", len(rc.Tools.MCPServers))
	}
	srv := rc.Tools.MCPServers[0]

	wantTools := []string{"lookup", "query"}
	if len(srv.AllowedTools) != len(wantTools) {
		t.Fatalf("AllowedTools: got %v, want %v", srv.AllowedTools, wantTools)
	}
	for i, want := range wantTools {
		if srv.AllowedTools[i] != want {
			t.Errorf("AllowedTools[%d]: got %q, want %q", i, srv.AllowedTools[i], want)
		}
	}

	wantHosts := []string{"mcp.example.com"}
	if len(srv.AllowedMCPHosts) != len(wantHosts) {
		t.Fatalf("AllowedMCPHosts: got %v, want %v", srv.AllowedMCPHosts, wantHosts)
	}
	for i, want := range wantHosts {
		if srv.AllowedMCPHosts[i] != want {
			t.Errorf("AllowedMCPHosts[%d]: got %q, want %q", i, srv.AllowedMCPHosts[i], want)
		}
	}
}

// TestRunConfigFromProto_MCPTrustFieldsAbsentWhenNil documents the
// backward-compatible default: an MCP server config that omits the
// trust fields entirely must translate to nil/empty slices, not a
// spurious default allowlist or host pin.
func TestRunConfigFromProto_MCPTrustFieldsAbsentWhenNil(t *testing.T) {
	original := &pb.RunConfig{
		Tools: &pb.ToolsConfig{
			McpServers: []*pb.MCPServerConfig{
				{
					Name: "search",
					Uri:  "https://mcp.example.com/v1",
				},
			},
		},
	}

	raw, err := proto.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded pb.RunConfig
	if err := proto.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	rc := runConfigFromProto(&decoded)

	if len(rc.Tools.MCPServers) != 1 {
		t.Fatalf("MCPServers: got %d entries, want 1", len(rc.Tools.MCPServers))
	}
	srv := rc.Tools.MCPServers[0]

	if len(srv.AllowedTools) != 0 {
		t.Errorf("AllowedTools should be empty when proto field absent, got %v", srv.AllowedTools)
	}
	if len(srv.AllowedMCPHosts) != 0 {
		t.Errorf("AllowedMCPHosts should be empty when proto field absent, got %v", srv.AllowedMCPHosts)
	}
}
