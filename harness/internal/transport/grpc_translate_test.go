package transport

import (
	"encoding/json"
	"maps"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

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

func TestRunConfigProtoRoundTrip_ClearToolResultsFields(t *testing.T) {
	keep, clearAtLeast := int32(0), int32(20000)
	original := &pb.RunConfig{ContextStrategy: &pb.ContextStrategyConfig{
		Type:         "clear-tool-results",
		MaxTokens:    100000,
		Keep:         &keep,
		ClearAtLeast: &clearAtLeast,
		ExcludeTools: []string{"run_command", "web_fetch"},
	}}
	wire, err := proto.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded pb.RunConfig
	if err := proto.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	got := runConfigFromProto(&decoded).ContextStrategy
	if got.Type != "clear-tool-results" || got.MaxTokens != 100000 {
		t.Errorf("Type/MaxTokens = %q/%d", got.Type, got.MaxTokens)
	}
	if got.Keep == nil || *got.Keep != 0 {
		t.Errorf("Keep = %v, want an explicit 0", got.Keep)
	}
	if got.ClearAtLeast == nil || *got.ClearAtLeast != 20000 {
		t.Errorf("ClearAtLeast = %v, want 20000", got.ClearAtLeast)
	}
	if want := []string{"run_command", "web_fetch"}; !reflect.DeepEqual(got.ExcludeTools, want) {
		t.Errorf("ExcludeTools = %v, want %v", got.ExcludeTools, want)
	}
	decoded.ContextStrategy.ExcludeTools[0] = "mutated"
	if got.ExcludeTools[0] != "run_command" {
		t.Error("ExcludeTools aliases the proto slice")
	}
}

func TestRunConfigFromProto_ClearToolResultsFieldsAbsentStayUnset(t *testing.T) {
	got := runConfigFromProto(&pb.RunConfig{ContextStrategy: &pb.ContextStrategyConfig{Type: "sliding-window"}}).ContextStrategy
	if got.Keep != nil || got.ClearAtLeast != nil || got.ExcludeTools != nil {
		t.Errorf("unset proto fields must stay unset, got %+v", got)
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
			ServiceName:      "agent-a",
			Environment:      "staging",
			ServiceNamespace: "eval",
		},
	}

	rc := runConfigFromProto(pc)

	if rc.Observability.ServiceName != "agent-a" {
		t.Errorf("Observability.ServiceName: got %q, want agent-a", rc.Observability.ServiceName)
	}

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

// TestRunConfigFromProto_TraceEmitterMetricsTemporalityPreserved guards
// TraceEmitterConfig.MetricsTemporality against silent drop in
// runConfigFromProto.
func TestRunConfigFromProto_TraceEmitterMetricsTemporalityPreserved(t *testing.T) {
	rc := runConfigFromProto(&pb.RunConfig{
		TraceEmitter: &pb.TraceEmitterConfig{Type: "otel", MetricsTemporality: "delta"},
	})
	if got, want := rc.TraceEmitter.MetricsTemporality, "delta"; got != want {
		t.Errorf("TraceEmitter.MetricsTemporality: got %q, want %q", got, want)
	}
}

// TestRunConfigFromProto_TraceEmitterCaptureContentMaxBytesPreserved
// guards TraceEmitterConfig.CaptureContentMaxBytes against silent drop in
// runConfigFromProto.
func TestRunConfigFromProto_TraceEmitterCaptureContentMaxBytesPreserved(t *testing.T) {
	rc := runConfigFromProto(&pb.RunConfig{
		TraceEmitter: &pb.TraceEmitterConfig{Type: "otel", CaptureContent: true, CaptureContentMaxBytes: 2048},
	})
	if !rc.TraceEmitter.CaptureContent {
		t.Error("TraceEmitter.CaptureContent dropped")
	}
	if got, want := rc.TraceEmitter.CaptureContentMaxBytes, 2048; got != want {
		t.Errorf("TraceEmitter.CaptureContentMaxBytes: got %d, want %d", got, want)
	}
}

// TestRunConfigFromProto_TraceEmitterPlatformPreserved guards
// TraceEmitterConfig.Platform and PlatformOptions against silent drop in
// runConfigFromProto.
func TestRunConfigFromProto_TraceEmitterPlatformPreserved(t *testing.T) {
	rc := runConfigFromProto(&pb.RunConfig{
		TraceEmitter: &pb.TraceEmitterConfig{
			Type:            "otel",
			Platform:        "example-platform",
			PlatformOptions: map[string]string{"region": "eu-west-2", "project": "agents"},
		},
	})
	if got, want := rc.TraceEmitter.Platform, "example-platform"; got != want {
		t.Errorf("TraceEmitter.Platform: got %q, want %q", got, want)
	}
	want := map[string]string{"region": "eu-west-2", "project": "agents"}
	if !maps.Equal(rc.TraceEmitter.PlatformOptions, want) {
		t.Errorf("TraceEmitter.PlatformOptions: got %v, want %v", rc.TraceEmitter.PlatformOptions, want)
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
		// "must be in range (0, timeout]".
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

// TestRunConfigFromProto_ReasoningEffortCarried pins the new
// provider-neutral reasoning knob across the wire boundary: a set value
// must survive verbatim and an absent one must stay empty (say nothing
// on the wire downstream).
func TestRunConfigFromProto_ReasoningEffortCarried(t *testing.T) {
	if got := runConfigFromProto(&pb.RunConfig{ReasoningEffort: "high"}).ReasoningEffort; got != "high" {
		t.Errorf("ReasoningEffort: got %q, want %q", got, "high")
	}
	if got := runConfigFromProto(&pb.RunConfig{}).ReasoningEffort; got != "" {
		t.Errorf("absent reasoning_effort translated to %q, want empty", got)
	}
}

// TestProviderConfigFromProto_GeminiFieldsCarried pins that the four
// Gemini provider fields survive the proto boundary. Before this test
// existed they were silently dropped by providerConfigFromProto, which
// locked the gemini provider out of the gRPC transport entirely: a
// TaskAssignment carrying gcp_project/gcp_location failed
// ValidateRunConfig ("gemini requires gcpProject") even though the
// wire payload was complete, and configured safety settings vanished
// without an error.
func TestProviderConfigFromProto_GeminiFieldsCarried(t *testing.T) {
	cfg := providerConfigFromProto(&pb.ProviderConfig{
		Type:               "gemini",
		GcpProject:         "proj-1",
		GcpLocation:        "us-central1",
		GcpCredentialsFile: "/etc/sa.json",
		GeminiSafetySettings: []*pb.GeminiSafetySetting{
			{Category: "HARM_CATEGORY_HATE_SPEECH", Threshold: "BLOCK_ONLY_HIGH"},
			nil, // wire payloads can carry nil entries; must not panic or emit a zero entry
			{Category: "HARM_CATEGORY_HARASSMENT", Threshold: "BLOCK_NONE"},
		},
	})
	if cfg.GCPProject != "proj-1" {
		t.Errorf("GCPProject: got %q, want %q", cfg.GCPProject, "proj-1")
	}
	if cfg.GCPLocation != "us-central1" {
		t.Errorf("GCPLocation: got %q, want %q", cfg.GCPLocation, "us-central1")
	}
	if cfg.GCPCredentialsFile != "/etc/sa.json" {
		t.Errorf("GCPCredentialsFile: got %q, want %q", cfg.GCPCredentialsFile, "/etc/sa.json")
	}
	want := []types.GeminiSafetySetting{
		{Category: "HARM_CATEGORY_HATE_SPEECH", Threshold: "BLOCK_ONLY_HIGH"},
		{Category: "HARM_CATEGORY_HARASSMENT", Threshold: "BLOCK_NONE"},
	}
	if !reflect.DeepEqual(cfg.GeminiSafetySettings, want) {
		t.Errorf("GeminiSafetySettings: got %+v, want %+v", cfg.GeminiSafetySettings, want)
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
				TokenUsage:  types.TokenUsage{Input: 120, Output: 45, CacheRead: 80, CacheWrite: 30, Reasoning: 12},
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
			if pt.CacheReadTokens != 80 || pt.CacheWriteTokens != 30 || pt.ReasoningTokens != 12 {
				t.Errorf("token breakdown: got cache_read=%d cache_write=%d reasoning=%d, want 80/30/12",
					pt.CacheReadTokens, pt.CacheWriteTokens, pt.ReasoningTokens)
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
			if decoded.CacheReadTokens != 80 || decoded.CacheWriteTokens != 30 || decoded.ReasoningTokens != 12 {
				t.Errorf("token breakdown did not survive wire round-trip: got %d/%d/%d, want 80/30/12",
					decoded.CacheReadTokens, decoded.CacheWriteTokens, decoded.ReasoningTokens)
			}
		})
	}
}

func TestRunTraceToProto_ZeroBreakdownRoundTrip(t *testing.T) {
	pt := runTraceToProto(&types.RunTrace{ID: "run-zero", TokenUsage: types.TokenUsage{Input: 120, Output: 45}})
	raw, err := proto.Marshal(pt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded pb.RunTrace
	if err := proto.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.InputTokens != 120 || decoded.OutputTokens != 45 {
		t.Errorf("tokens: got in=%d out=%d, want in=120 out=45", decoded.InputTokens, decoded.OutputTokens)
	}
	if decoded.CacheReadTokens != 0 || decoded.CacheWriteTokens != 0 || decoded.ReasoningTokens != 0 {
		t.Errorf("token breakdown: got %d/%d/%d, want all zero",
			decoded.CacheReadTokens, decoded.CacheWriteTokens, decoded.ReasoningTokens)
	}
}

func TestRunTraceToProto_SaturatesTokenCounts(t *testing.T) {
	pt := runTraceToProto(&types.RunTrace{TokenUsage: types.TokenUsage{
		Input:     math.MaxInt,
		Output:    math.MaxInt32 + 1,
		CacheRead: math.MaxInt32,
		Reasoning: math.MinInt,
	}})
	if pt.InputTokens != math.MaxInt32 || pt.OutputTokens != math.MaxInt32 {
		t.Errorf("tokens: got in=%d out=%d, want both MaxInt32", pt.InputTokens, pt.OutputTokens)
	}
	if pt.CacheReadTokens != math.MaxInt32 || pt.ReasoningTokens != math.MinInt32 {
		t.Errorf("breakdown: got cache_read=%d reasoning=%d, want MaxInt32/MinInt32", pt.CacheReadTokens, pt.ReasoningTokens)
	}
}

// TestRunTraceToProto_StopDetails pins the stop_details mirror through a
// wire round-trip, and that a nil StopDetails stays absent on the wire.
func TestRunTraceToProto_StopDetails(t *testing.T) {
	tr := &types.RunTrace{
		ID:      "run-refused",
		Outcome: "refusal",
		StopDetails: &types.StopDetails{
			Type:        "refusal",
			Category:    "reasoning_extraction",
			Explanation: "declined",
		},
	}
	raw, err := proto.Marshal(runTraceToProto(tr))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded pb.RunTrace
	if err := proto.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := decoded.GetStopDetails()
	if got.GetType() != "refusal" || got.GetCategory() != "reasoning_extraction" || got.GetExplanation() != "declined" {
		t.Errorf("stop_details = %v, want refusal/reasoning_extraction/declined", got)
	}

	if pt := runTraceToProto(&types.RunTrace{Outcome: "success"}); pt.StopDetails != nil {
		t.Errorf("stop_details = %v, want absent for a nil StopDetails", pt.StopDetails)
	}
}

// TestRunTraceToProto_PartialStopDetails pins that a refusal with no
// category or explanation still reaches the wire as a present message.
func TestRunTraceToProto_PartialStopDetails(t *testing.T) {
	raw, err := proto.Marshal(runTraceToProto(&types.RunTrace{
		Outcome:     "refusal",
		StopDetails: &types.StopDetails{Type: "refusal"},
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded pb.RunTrace
	if err := proto.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := decoded.GetStopDetails()
	if got == nil {
		t.Fatal("stop_details absent, want a message with type refusal")
	}
	if got.GetType() != "refusal" || got.GetCategory() != "" || got.GetExplanation() != "" {
		t.Errorf("stop_details = %v, want type refusal with empty category and explanation", got)
	}
}

// TestRunTraceToProto_Verification pins that the last verifier attempt
// reaches the wire through a round trip, and that a run with no recorded
// attempt leaves the field absent.
func TestRunTraceToProto_Verification(t *testing.T) {
	tr := &types.RunTrace{
		Outcome: "verification_error",
		VerificationResults: []types.VerificationResult{
			{Attempt: 1, Passed: false, Feedback: "tests failed"},
			{Attempt: 2, Passed: false, Error: "exec failed: [REDACTED]"},
		},
	}
	raw, err := proto.Marshal(runTraceToProto(tr))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded pb.RunTrace
	if err := proto.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := decoded.GetVerification()
	if got.GetAttempt() != 2 || got.GetPassed() || got.GetFeedback() != "" || got.GetError() != "exec failed: [REDACTED]" {
		t.Errorf("verification = %v, want attempt 2, failed, verifier error and no feedback", got)
	}

	passed := runTraceToProto(&types.RunTrace{
		Outcome:             "success",
		VerificationResults: []types.VerificationResult{{Attempt: 1, Passed: true}},
	})
	if v := passed.GetVerification(); v == nil || !v.GetPassed() || v.GetAttempt() != 1 {
		t.Errorf("verification = %v, want a passing attempt 1", v)
	}

	if pt := runTraceToProto(&types.RunTrace{Outcome: "success"}); pt.Verification != nil {
		t.Errorf("verification = %v, want absent with no recorded attempt", pt.Verification)
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

// TestHarnessEventToProto_AudiencePreserved guards the Audience field
// added for sandbox_token_request (issue #516 Part A) against the same
// silent-drop class of regression flagged by gh-95/gh-117/gh-118/gh-100: a
// HarnessEvent's Audience must survive the internal-to-proto hop
// unmodified, since it is the only signal the control plane gets for the
// intended JWT `aud` claim.
func TestHarnessEventToProto_AudiencePreserved(t *testing.T) {
	e := types.HarnessEvent{
		Type:      "sandbox_token_request",
		RequestID: "req-1",
		Audience:  "https://haybale.internal",
	}

	pe := harnessEventToProto(e)

	if pe.Audience != "https://haybale.internal" {
		t.Errorf("Audience: got %q, want %q", pe.Audience, "https://haybale.internal")
	}
	if pe.RequestId != "req-1" {
		t.Errorf("RequestId: got %q, want %q", pe.RequestId, "req-1")
	}
}

// TestControlEventFromProto_SandboxTokenFieldsPreserved guards Token and
// ExpiresAt (issue #516 Part A) against the same silent-drop regression
// class as gh-95/gh-117/gh-118/gh-100. The token/expires_at case exercises
// the actual wire path (Marshal -> Unmarshal) so the optional int64
// unset-vs-explicit-zero distinction on ExpiresAt — the same concern
// Temperature and Batch.MaxWaitSeconds guard above — is pinned rather than
// assumed from an in-memory pointer copy.
func TestControlEventFromProto_SandboxTokenFieldsPreserved(t *testing.T) {
	t.Run("token and expires_at round-trip", func(t *testing.T) {
		var exp int64 = 1700003600
		original := &pb.ControlEvent{
			Type:      "sandbox_token_response",
			RequestId: "req-1",
			Token:     "eyJhbGciOiJFUzI1NiJ9.eyJzdWIiOiJydW4tMSJ9.sig",
			ExpiresAt: &exp,
		}

		raw, err := proto.Marshal(original)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded pb.ControlEvent
		if err := proto.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		e := controlEventFromProto(&decoded)

		if e.Token != original.Token {
			t.Errorf("Token: got %q, want %q", e.Token, original.Token)
		}
		if e.ExpiresAt == nil {
			t.Fatal("ExpiresAt: got nil, want non-nil pointer")
		}
		if *e.ExpiresAt != exp {
			t.Errorf("ExpiresAt: got %d, want %d", *e.ExpiresAt, exp)
		}
	})

	t.Run("expires_at unset stays nil", func(t *testing.T) {
		original := &pb.ControlEvent{
			Type:      "sandbox_token_response",
			RequestId: "req-2",
			Token:     "some-token",
		}

		raw, err := proto.Marshal(original)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded pb.ControlEvent
		if err := proto.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		e := controlEventFromProto(&decoded)

		if e.ExpiresAt != nil {
			t.Errorf("ExpiresAt should be nil when proto field is unset, got %v", *e.ExpiresAt)
		}
	})

	t.Run("is_error and reason round-trip for a decline", func(t *testing.T) {
		original := &pb.ControlEvent{
			Type:      "sandbox_token_response",
			RequestId: "req-3",
			IsError:   &pb.OptionalBool{Value: true},
			Reason:    "no issuer configured",
		}

		e := controlEventFromProto(original)

		if e.IsError == nil || !*e.IsError {
			t.Fatalf("IsError: got %v, want pointer to true", e.IsError)
		}
		if e.Reason != "no issuer configured" {
			t.Errorf("Reason: got %q, want %q", e.Reason, "no issuer configured")
		}
		if e.Token != "" {
			t.Errorf("Token should be empty on an error response, got %q", e.Token)
		}
	})
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

// TestRunConfigFromProto_ExecutorSandboxFieldsPreserved pins the executor
// fields that had no proto mirror at all until they were added: registry
// pinning, workspace export, k8s egress wiring, and the sandbox identity /
// git proxy blocks. Each is marshalled and unmarshalled first so a field
// that compiles but never reaches the wire is caught here rather than at
// deployment time.
func TestRunConfigFromProto_ExecutorSandboxFieldsPreserved(t *testing.T) {
	original := &pb.RunConfig{
		Executor: &pb.ExecutorConfig{
			Type:              "k8s",
			Image:             "ghcr.io/rxbynerd/stirrup:latest",
			K8SNamespace:      "agents",
			K8SEgressProxyUrl: "http://egress.agents.svc:3128",
			RegistryAllowlist: []string{"ghcr.io/rxbynerd/*", "docker.io/library/*"},
			WorkspaceExportTo: "gs://stirrup-workspaces/runs",
			SandboxIdentity: &pb.SandboxIdentityConfig{
				Source:   "control-plane",
				Audience: "https://haybale.internal",
				EnvVar:   "HAYBALE_TOKEN",
			},
			GitProxy: &pb.GitProxyConfig{
				Url:         "https://haybale.internal",
				Hosts:       []string{"github.com", "gitlab.com"},
				RewriteSsh:  true,
				TokenEnvVar: "HAYBALE_TOKEN",
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

	ec := runConfigFromProto(&decoded).Executor

	if ec.K8sEgressProxyURL != "http://egress.agents.svc:3128" {
		t.Errorf("K8sEgressProxyURL: got %q", ec.K8sEgressProxyURL)
	}
	if ec.WorkspaceExportTo != "gs://stirrup-workspaces/runs" {
		t.Errorf("WorkspaceExportTo: got %q", ec.WorkspaceExportTo)
	}
	wantRegistry := []string{"ghcr.io/rxbynerd/*", "docker.io/library/*"}
	if !reflect.DeepEqual(ec.RegistryAllowlist, wantRegistry) {
		t.Errorf("RegistryAllowlist: got %v, want %v", ec.RegistryAllowlist, wantRegistry)
	}

	if ec.SandboxIdentity == nil {
		t.Fatal("SandboxIdentity nil after translation")
	}
	if ec.SandboxIdentity.Source != "control-plane" {
		t.Errorf("SandboxIdentity.Source: got %q", ec.SandboxIdentity.Source)
	}
	if ec.SandboxIdentity.Audience != "https://haybale.internal" {
		t.Errorf("SandboxIdentity.Audience: got %q", ec.SandboxIdentity.Audience)
	}
	if ec.SandboxIdentity.EnvVar != "HAYBALE_TOKEN" {
		t.Errorf("SandboxIdentity.EnvVar: got %q", ec.SandboxIdentity.EnvVar)
	}

	if ec.GitProxy == nil {
		t.Fatal("GitProxy nil after translation")
	}
	if ec.GitProxy.URL != "https://haybale.internal" {
		t.Errorf("GitProxy.URL: got %q", ec.GitProxy.URL)
	}
	if !reflect.DeepEqual(ec.GitProxy.Hosts, []string{"github.com", "gitlab.com"}) {
		t.Errorf("GitProxy.Hosts: got %v", ec.GitProxy.Hosts)
	}
	if !ec.GitProxy.RewriteSsh {
		t.Error("GitProxy.RewriteSsh not propagated")
	}
	if ec.GitProxy.TokenEnvVar != "HAYBALE_TOKEN" {
		t.Errorf("GitProxy.TokenEnvVar: got %q", ec.GitProxy.TokenEnvVar)
	}
}

// TestRunConfigFromProto_ExecutorSandboxFieldsAbsentStayNil documents the
// safe default: an ExecutorConfig that omits the new sub-messages must
// leave the internal pointers nil, so "operator did not configure" stays
// distinguishable from "configured empty" and the defaults apply.
func TestRunConfigFromProto_ExecutorSandboxFieldsAbsentStayNil(t *testing.T) {
	ec := runConfigFromProto(&pb.RunConfig{
		Executor: &pb.ExecutorConfig{Type: "container"},
	}).Executor

	if ec.SandboxIdentity != nil {
		t.Errorf("SandboxIdentity should be nil when proto field absent, got %+v", ec.SandboxIdentity)
	}
	if ec.GitProxy != nil {
		t.Errorf("GitProxy should be nil when proto field absent, got %+v", ec.GitProxy)
	}
	if ec.RegistryAllowlist != nil {
		t.Errorf("RegistryAllowlist should be nil when proto field absent, got %v", ec.RegistryAllowlist)
	}
	if ec.WorkspaceExportTo != "" {
		t.Errorf("WorkspaceExportTo should be empty when proto field absent, got %q", ec.WorkspaceExportTo)
	}
	if ec.K8sEgressProxyURL != "" {
		t.Errorf("K8sEgressProxyURL should be empty when proto field absent, got %q", ec.K8sEgressProxyURL)
	}
}

// TestRunConfigFromProto_ExecutorSlicesNotAliased confirms the repeated
// executor fields are copied rather than aliased, so mutating the wire
// payload after translation cannot reach internal config state.
func TestRunConfigFromProto_ExecutorSlicesNotAliased(t *testing.T) {
	src := &pb.RunConfig{
		Executor: &pb.ExecutorConfig{
			Type:              "container",
			RegistryAllowlist: []string{"ghcr.io/rxbynerd/*"},
			SandboxIdentity:   &pb.SandboxIdentityConfig{Source: "control-plane"},
			GitProxy: &pb.GitProxyConfig{
				Url:   "https://haybale.internal",
				Hosts: []string{"github.com"},
			},
		},
	}

	ec := runConfigFromProto(src).Executor

	src.Executor.RegistryAllowlist[0] = "mutated"
	src.Executor.GitProxy.Hosts[0] = "mutated"

	if ec.RegistryAllowlist[0] != "ghcr.io/rxbynerd/*" {
		t.Error("RegistryAllowlist shares backing array with proto source")
	}
	if ec.GitProxy.Hosts[0] != "github.com" {
		t.Error("GitProxy.Hosts shares backing array with proto source")
	}
}

// TestRunConfigFromProto_ResultSinkPreserved pins every ResultSinkConfig
// field across a wire round-trip. The sink's type discriminator decides
// whether the run's answer is delivered at all, so a silent drop here
// looks like a working run that produced nothing.
func TestRunConfigFromProto_ResultSinkPreserved(t *testing.T) {
	original := &pb.RunConfig{
		ResultSink: &pb.ResultSinkConfig{
			Type:                       "gcp-pubsub",
			Topic:                      "stirrup-results",
			Attributes:                 map[string]string{"tenant": "team-a", "auth": "secret://PUBSUB_TOKEN"},
			MaxFinalAssistantTextBytes: 4096,
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

	sink := runConfigFromProto(&decoded).ResultSink
	if sink == nil {
		t.Fatal("ResultSink nil after translation")
	}
	if sink.Type != "gcp-pubsub" {
		t.Errorf("ResultSink.Type: got %q", sink.Type)
	}
	if sink.Topic != "stirrup-results" {
		t.Errorf("ResultSink.Topic: got %q", sink.Topic)
	}
	if sink.MaxFinalAssistantTextBytes != 4096 {
		t.Errorf("ResultSink.MaxFinalAssistantTextBytes: got %d, want 4096", sink.MaxFinalAssistantTextBytes)
	}
	want := map[string]string{"tenant": "team-a", "auth": "secret://PUBSUB_TOKEN"}
	if !reflect.DeepEqual(sink.Attributes, want) {
		t.Errorf("ResultSink.Attributes: got %v, want %v", sink.Attributes, want)
	}

	// The attributes map must be copied, not aliased.
	decoded.ResultSink.Attributes["tenant"] = "mutated"
	if sink.Attributes["tenant"] != "team-a" {
		t.Error("ResultSink.Attributes shares the proto-owned map")
	}
}

// TestRunConfigFromProto_ResultSinkAbsentStaysNil documents that an absent
// sub-message leaves ResultSink nil — the "sink disabled" state — rather
// than synthesising a block whose empty Type fails validation.
func TestRunConfigFromProto_ResultSinkAbsentStaysNil(t *testing.T) {
	if sink := runConfigFromProto(&pb.RunConfig{}).ResultSink; sink != nil {
		t.Errorf("ResultSink should be nil when proto field absent, got %+v", sink)
	}
}

// TestRunConfigFromProto_ToolChoiceEscalationPreserved pins the escalation
// block across a wire round-trip, including the empty-sub-message case:
// present-but-unset must stay non-nil so it is distinguishable from absent.
func TestRunConfigFromProto_ToolChoiceEscalationPreserved(t *testing.T) {
	t.Run("populated", func(t *testing.T) {
		original := &pb.RunConfig{
			ToolChoiceEscalation: &pb.ToolChoiceEscalationConfig{
				Enabled:    true,
				MaxRetries: 3,
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

		esc := runConfigFromProto(&decoded).ToolChoiceEscalation
		if esc == nil {
			t.Fatal("ToolChoiceEscalation nil after translation")
		}
		if !esc.Enabled {
			t.Error("ToolChoiceEscalation.Enabled not propagated")
		}
		if esc.MaxRetries != 3 {
			t.Errorf("ToolChoiceEscalation.MaxRetries: got %d, want 3", esc.MaxRetries)
		}
	})

	t.Run("empty sub-message stays non-nil", func(t *testing.T) {
		esc := runConfigFromProto(&pb.RunConfig{
			ToolChoiceEscalation: &pb.ToolChoiceEscalationConfig{},
		}).ToolChoiceEscalation
		if esc == nil {
			t.Fatal("expected non-nil ToolChoiceEscalation for an empty sub-message")
		}
		if esc.Enabled {
			t.Error("empty sub-message must not enable escalation")
		}
		if esc.MaxRetries != 0 {
			t.Errorf("MaxRetries: got %d, want 0 (resolves to the default)", esc.MaxRetries)
		}
	})

	t.Run("absent stays nil", func(t *testing.T) {
		if esc := runConfigFromProto(&pb.RunConfig{}).ToolChoiceEscalation; esc != nil {
			t.Errorf("ToolChoiceEscalation should be nil when proto field absent, got %+v", esc)
		}
	})
}

// TestRunConfigFromProto_WindDownPreserved pins the wind-down block across
// a wire round-trip; an absent sub-message stays nil (off).
func TestRunConfigFromProto_WindDownPreserved(t *testing.T) {
	t.Run("enabled", func(t *testing.T) {
		raw, err := proto.Marshal(&pb.RunConfig{WindDown: &pb.WindDownConfig{Enabled: true}})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded pb.RunConfig
		if err := proto.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		rc := runConfigFromProto(&decoded)
		if rc.WindDown == nil || !rc.WindDown.Enabled || !rc.WindDownEnabled() {
			t.Fatalf("WindDown = %+v, want enabled", rc.WindDown)
		}
	})

	t.Run("empty sub-message stays non-nil and off", func(t *testing.T) {
		rc := runConfigFromProto(&pb.RunConfig{WindDown: &pb.WindDownConfig{}})
		if rc.WindDown == nil {
			t.Fatal("expected non-nil WindDown for an empty sub-message")
		}
		if rc.WindDownEnabled() {
			t.Error("empty sub-message must not enable wind-down")
		}
	})

	t.Run("absent stays nil", func(t *testing.T) {
		if wd := runConfigFromProto(&pb.RunConfig{}).WindDown; wd != nil {
			t.Errorf("WindDown should be nil when proto field absent, got %+v", wd)
		}
	})
}

// TestRunConfigFromProto_LogsExportPreserved pins observability.logs_export,
// which sits one level below a sub-message that was already translated —
// exactly the shape a field-by-field mirror is most likely to miss.
func TestRunConfigFromProto_LogsExportPreserved(t *testing.T) {
	t.Run("populated", func(t *testing.T) {
		original := &pb.RunConfig{
			Observability: &pb.ObservabilityConfig{
				Environment: "production",
				LogsExport: &pb.LogsExportConfig{
					Type:     "otlp",
					Endpoint: "otel-collector.observability.svc:4317",
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

		obs := runConfigFromProto(&decoded).Observability
		if obs.Environment != "production" {
			t.Errorf("Environment: got %q", obs.Environment)
		}
		if obs.LogsExport.Type != "otlp" {
			t.Errorf("LogsExport.Type: got %q, want otlp", obs.LogsExport.Type)
		}
		if obs.LogsExport.Endpoint != "otel-collector.observability.svc:4317" {
			t.Errorf("LogsExport.Endpoint: got %q", obs.LogsExport.Endpoint)
		}
	})

	t.Run("absent leaves stderr-only zero value", func(t *testing.T) {
		obs := runConfigFromProto(&pb.RunConfig{
			Observability: &pb.ObservabilityConfig{Environment: "staging"},
		}).Observability
		if obs.LogsExport.Type != "" {
			t.Errorf("LogsExport.Type should be empty when absent, got %q", obs.LogsExport.Type)
		}
		if obs.LogsExport.Endpoint != "" {
			t.Errorf("LogsExport.Endpoint should be empty when absent, got %q", obs.LogsExport.Endpoint)
		}
	})
}

// TestRunConfigFromProto_TransportIsGRPC pins the one field the translate
// layer sets rather than copies. RunConfig.transport has no proto mirror by
// design, but several cross-field rules key off it — sandbox identity and
// ruleOfTwo.runtime.onDetect="ask-upstream" both require "grpc", and the
// batch client dispatch switches on it. Leaving it empty would make those
// features unreachable over the wire even once their config fields exist.
func TestRunConfigFromProto_TransportIsGRPC(t *testing.T) {
	if got := runConfigFromProto(&pb.RunConfig{}).Transport.Type; got != "grpc" {
		t.Errorf("Transport.Type: got %q, want grpc", got)
	}
}

// TestRunConfigFromProto_SandboxIdentityPassesValidation drives the mirrored
// fields through ValidateRunConfig, the gate BuildLoopWithTransport applies
// to every wire-delivered assignment. Translation alone is not enough: the
// sandbox identity rules require transport "grpc", so a config that
// translates cleanly but fails validation would leave the feature just as
// unreachable as having no proto field at all.
func TestRunConfigFromProto_SandboxIdentityPassesValidation(t *testing.T) {
	timeout := int32(600)
	pc := &pb.RunConfig{
		RunId:    "run-1",
		Mode:     "execution",
		Prompt:   "fix the failing test",
		MaxTurns: 10,
		Timeout:  &timeout,
		Provider: &pb.ProviderConfig{
			Type:      "anthropic",
			ApiKeyRef: "secret://ANTHROPIC_API_KEY",
		},
		Executor: &pb.ExecutorConfig{
			Type:  "container",
			Image: "ghcr.io/rxbynerd/stirrup:latest",
			Network: &pb.NetworkConfig{
				Mode:      "allowlist",
				Allowlist: []string{"haybale.internal:443"},
			},
			SandboxIdentity: &pb.SandboxIdentityConfig{
				Source:   "control-plane",
				Audience: "https://haybale.internal",
			},
			GitProxy: &pb.GitProxyConfig{
				Url:   "https://haybale.internal",
				Hosts: []string{"github.com"},
			},
		},
		PermissionPolicy: &pb.PermissionPolicyConfig{Type: "allow-all"},
	}

	rc := runConfigFromProto(pc)
	if err := types.ValidateRunConfig(&rc); err != nil {
		t.Fatalf("wire-delivered sandbox identity config rejected by validation: %v", err)
	}
}

// TestRunConfigFromProto_ControlPlaneToolsPreserved pins the wire mapping
// of ToolsConfig.control_plane, including the Struct → JSON projection of
// input_schema, through a real marshal/unmarshal cycle.
func TestRunConfigFromProto_ControlPlaneToolsPreserved(t *testing.T) {
	schema, err := structpb.NewStruct(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string"},
			"limit": map[string]any{"type": "integer"},
		},
		"required": []any{"query"},
	})
	if err != nil {
		t.Fatalf("NewStruct: %v", err)
	}
	original := &pb.RunConfig{
		Tools: &pb.ToolsConfig{
			BuiltIn: []string{"read_file"},
			ControlPlane: []*pb.ControlPlaneToolConfig{
				{
					Name:             "search_memory",
					Description:      "Search long-term memory.",
					InputSchema:      schema,
					TimeoutSeconds:   30,
					RequiresApproval: true,
				},
				{
					Name:        "save_memory",
					Description: "Persist a memory.",
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
	if len(rc.Tools.ControlPlane) != 2 {
		t.Fatalf("ControlPlane: got %d entries, want 2", len(rc.Tools.ControlPlane))
	}

	first := rc.Tools.ControlPlane[0]
	if first.Name != "search_memory" || first.Description != "Search long-term memory." {
		t.Errorf("first entry = %+v", first)
	}
	if first.TimeoutSeconds != 30 || !first.RequiresApproval {
		t.Errorf("first timeout/approval = %d/%v, want 30/true", first.TimeoutSeconds, first.RequiresApproval)
	}
	var got map[string]any
	if err := json.Unmarshal(first.InputSchema, &got); err != nil {
		t.Fatalf("inputSchema is not a JSON object: %v (%s)", err, first.InputSchema)
	}
	if got["type"] != "object" {
		t.Errorf("inputSchema.type = %v, want object", got["type"])
	}
	props, _ := got["properties"].(map[string]any)
	if _, ok := props["query"]; !ok {
		t.Errorf("inputSchema.properties.query missing: %s", first.InputSchema)
	}
	if req, _ := got["required"].([]any); len(req) != 1 || req[0] != "query" {
		t.Errorf("inputSchema.required = %v, want [query]", got["required"])
	}

	second := rc.Tools.ControlPlane[1]
	if second.InputSchema != nil {
		t.Errorf("unset input_schema should map to nil, got %s", second.InputSchema)
	}
	if second.TimeoutSeconds != 0 || second.RequiresApproval {
		t.Errorf("second entry defaults = %+v", second)
	}
}

// TestRunConfigFromProto_MaxCostBudgetStillTranslated pins the deprecated
// field's translation: the budget is not enforced, but dropping it here
// would swallow the value before ValidateRunConfig can warn that the cap
// does nothing.
func TestRunConfigFromProto_MaxCostBudgetStillTranslated(t *testing.T) {
	budget := 42.5
	//nolint:staticcheck // SA1019: exercising the deprecated field on purpose.
	pc := &pb.RunConfig{MaxCostBudget: &budget}

	rc := runConfigFromProto(pc)

	if rc.MaxCostBudget == nil {
		t.Fatal("MaxCostBudget dropped in translation")
	}
	if *rc.MaxCostBudget != budget {
		t.Errorf("MaxCostBudget = %v, want %v", *rc.MaxCostBudget, budget)
	}

	if got := runConfigFromProto(&pb.RunConfig{}).MaxCostBudget; got != nil {
		t.Errorf("unset max_cost_budget should map to nil, got %v", *got)
	}
}

// TestRunTraceToProto_HandoffSummaryRoundTrip pins that the git handoff,
// the export receipt and the run error reach the "done" trace summary.
func TestRunTraceToProto_HandoffSummaryRoundTrip(t *testing.T) {
	tr := &types.RunTrace{
		ID:      "run-handoff",
		Outcome: "handoff_failed",
		Git: &types.GitHandoff{
			Branch: "stirrup/run-handoff", SHA: "abc123", BaseSHA: "def456", Dirty: true, Error: "dirty tree",
		},
		WorkspaceExport: &types.ExportReceipt{
			URI: "gs://bucket/ws.tar.gz", Status: types.ExportStatusFailed, Required: true, Error: "upload refused",
			Bytes: 2048, SHA256: "0123abcd", Files: 7,
		},
		Error: "workspace export: upload refused",
	}
	raw, err := proto.Marshal(runTraceToProto(tr))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded pb.RunTrace
	if err := proto.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	g := decoded.GetGit()
	if g.GetBranch() != "stirrup/run-handoff" || g.GetSha() != "abc123" || g.GetBaseSha() != "def456" || !g.GetDirty() || g.GetError() != "dirty tree" {
		t.Errorf("git = %v, want every GitHandoff field carried", g)
	}
	r := decoded.GetWorkspaceExport()
	if r.GetUri() != "gs://bucket/ws.tar.gz" || r.GetStatus() != "failed" || !r.GetRequired() || r.GetError() != "upload refused" ||
		r.GetBytes() != 2048 || r.GetSha256() != "0123abcd" || r.GetFiles() != 7 {
		t.Errorf("workspace_export = %v, want every ExportReceipt field carried", r)
	}
	if decoded.GetError() != "workspace export: upload refused" {
		t.Errorf("error = %q, want the run error", decoded.GetError())
	}

	bare := runTraceToProto(&types.RunTrace{Outcome: "success"})
	if bare.Git != nil || bare.WorkspaceExport != nil || bare.Error != "" {
		t.Errorf("bare summary = %v, want git, workspace_export and error absent", bare)
	}
}

// TestRunTraceToProto_SummaryStaysSmall bounds the "done" trace summary
// with every string field at its cap: it carries no config or tool
// bodies, so it stays far below the gRPC message limit.
func TestRunTraceToProto_SummaryStaysSmall(t *testing.T) {
	capped := strings.Repeat("x", 1024+64)
	tr := &types.RunTrace{
		ID:          strings.Repeat("r", 128),
		Outcome:     "handoff_failed",
		StopDetails: &types.StopDetails{Type: "refusal", Category: "other", Explanation: capped},
		Git: &types.GitHandoff{
			Branch: strings.Repeat("b", 256), SHA: strings.Repeat("a", 64), BaseSHA: strings.Repeat("c", 64), Error: capped,
		},
		WorkspaceExport: &types.ExportReceipt{URI: strings.Repeat("u", 1024), Status: "failed", Required: true, Error: capped},
		Error:           capped,
	}
	if n := proto.Size(runTraceToProto(tr)); n > 16<<10 {
		t.Errorf("summary size = %d bytes, want at most 16 KiB", n)
	}
}

// TestRunConfigFromProto_WorkspaceExportRequiredTriState pins that an
// unset workspace_export_required stays nil (so the destination default
// applies) and that both explicit values survive the wire and translate.
func TestRunConfigFromProto_WorkspaceExportRequiredTriState(t *testing.T) {
	for name, in := range map[string]*bool{"unset": nil, "true": proto.Bool(true), "false": proto.Bool(false)} {
		t.Run(name, func(t *testing.T) {
			raw, err := proto.Marshal(&pb.RunConfig{Executor: &pb.ExecutorConfig{
				Type:                    "local",
				WorkspaceExportTo:       "gs://bucket/ws.tar.gz",
				WorkspaceExportRequired: in,
			}})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var decoded pb.RunConfig
			if err := proto.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := runConfigFromProto(&decoded).Executor.WorkspaceExportRequired
			switch {
			case in == nil && got != nil:
				t.Errorf("WorkspaceExportRequired = %v, want nil", *got)
			case in != nil && (got == nil || *got != *in):
				t.Errorf("WorkspaceExportRequired = %v, want %v", got, *in)
			}
			wantRequired := in == nil || *in
			if req := runConfigFromProto(&decoded).Executor.ExportRequired(); req != wantRequired {
				t.Errorf("ExportRequired() = %v, want %v", req, wantRequired)
			}
		})
	}
}

// TestRunConfigFromProto_OutputSchemaRoundTrip pins the wire mapping of
// output_schema through a real marshal/unmarshal cycle, and that a
// translated schema passes validation.
func TestRunConfigFromProto_OutputSchemaRoundTrip(t *testing.T) {
	schema, err := structpb.NewStruct(map[string]any{
		"type":     "object",
		"required": []any{"title"},
		"properties": map[string]any{
			"title": map[string]any{"type": "string", "maxLength": 72},
			"label": map[string]any{"$ref": "#/$defs/label"},
		},
		"$defs": map[string]any{"label": map[string]any{"enum": []any{"bug", "feature"}}},
	})
	if err != nil {
		t.Fatalf("NewStruct: %v", err)
	}
	timeout := int32(60)
	raw, err := proto.Marshal(&pb.RunConfig{
		RunId:            "run-structured",
		Mode:             "planning",
		Prompt:           "triage",
		MaxTurns:         5,
		Timeout:          &timeout,
		Provider:         &pb.ProviderConfig{Type: "anthropic", ApiKeyRef: "secret://ANTHROPIC_API_KEY"},
		PermissionPolicy: &pb.PermissionPolicyConfig{Type: "deny-side-effects"},
		Tools:            &pb.ToolsConfig{BuiltIn: []string{"read_file"}},
		OutputSchema:     schema,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded pb.RunConfig
	if err := proto.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rc := runConfigFromProto(&decoded)

	var got map[string]any
	if err := json.Unmarshal(rc.OutputSchema, &got); err != nil {
		t.Fatalf("OutputSchema is not JSON: %v (%s)", err, rc.OutputSchema)
	}
	want := map[string]any{
		"type":     "object",
		"required": []any{"title"},
		"properties": map[string]any{
			"title": map[string]any{"type": "string", "maxLength": float64(72)},
			"label": map[string]any{"$ref": "#/$defs/label"},
		},
		"$defs": map[string]any{"label": map[string]any{"enum": []any{"bug", "feature"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("OutputSchema = %v, want %v", got, want)
	}
	if !strings.Contains(string(rc.OutputSchema), `"maxLength":72`) {
		t.Errorf("integer keyword rendered as %s, want an integral JSON number", rc.OutputSchema)
	}
	if err := types.ValidateRunConfig(&rc); err != nil {
		t.Errorf("translated schema rejected by validation: %v", err)
	}
}

func TestRunConfigFromProto_OutputSchemaAbsentStaysNil(t *testing.T) {
	rc := runConfigFromProto(&pb.RunConfig{RunId: "run-free-text"})
	if rc.OutputSchema != nil {
		t.Errorf("OutputSchema = %s, want nil when the field is unset", rc.OutputSchema)
	}
}

func TestRunConfigFromProto_UnrenderableOutputSchemaIsRejected(t *testing.T) {
	schema := &structpb.Struct{Fields: map[string]*structpb.Value{
		"maximum": structpb.NewNumberValue(math.NaN()),
	}}
	rc := runConfigFromProto(&pb.RunConfig{OutputSchema: schema})
	if string(rc.OutputSchema) != "null" {
		t.Fatalf("OutputSchema = %s, want JSON null for a schema protojson cannot render", rc.OutputSchema)
	}
	c := types.RunConfig{OutputSchema: rc.OutputSchema}
	err := types.ValidateRunConfig(&c)
	if err == nil || !strings.Contains(err.Error(), "outputSchema must be a JSON Schema object") {
		t.Errorf("ValidateRunConfig error = %v, want the outputSchema rejected", err)
	}
}

func TestRunTraceToProto_StructuredOutputRoundTrip(t *testing.T) {
	doc := `{"title":"Fix the flaky test","labels":["bug"],"risk":0.25}`
	raw, err := proto.Marshal(runTraceToProto(&types.RunTrace{
		Outcome:          "success",
		StructuredOutput: json.RawMessage(doc),
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded pb.RunTrace
	if err := proto.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.GetStructuredOutput() != doc {
		t.Errorf("structured_output = %q, want %q", decoded.GetStructuredOutput(), doc)
	}

	if bare := runTraceToProto(&types.RunTrace{Outcome: "success"}); bare.StructuredOutput != "" {
		t.Errorf("structured_output = %q, want empty without a structured answer", bare.StructuredOutput)
	}
}

// TestRunConfigFromProto_CommandOutputPreserved pins every
// tools.commandOutput field through the wire format so a config arriving
// over task_assignment keeps its compliance-capture bounds.
func TestRunConfigFromProto_CommandOutputPreserved(t *testing.T) {
	disabled := false
	original := &pb.RunConfig{
		Tools: &pb.ToolsConfig{
			CommandOutput: &pb.CommandOutputConfig{
				Enabled:               &disabled,
				FailurePosture:        types.CommandOutputPostureBestEffort,
				InlineMaxBytes:        1 << 14,
				PreviewBytesPerStream: 1 << 11,
				MaxBytesPerStream:     1 << 24,
				MaxBytesPerRun:        1 << 26,
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

	got := runConfigFromProto(&decoded).Tools.CommandOutput
	if got.Enabled == nil || *got.Enabled {
		t.Fatalf("Enabled: got %v, want explicit false", got.Enabled)
	}
	want := types.CommandOutputConfig{
		Enabled:               got.Enabled,
		FailurePosture:        types.CommandOutputPostureBestEffort,
		InlineMaxBytes:        1 << 14,
		PreviewBytesPerStream: 1 << 11,
		MaxBytesPerStream:     1 << 24,
		MaxBytesPerRun:        1 << 26,
	}
	if got != want {
		t.Errorf("CommandOutput: got %+v, want %+v", got, want)
	}

	*decoded.Tools.CommandOutput.Enabled = true
	if *got.Enabled {
		t.Error("translated Enabled aliases the proto message's bool")
	}
}

// TestRunConfigFromProto_CommandOutputUnsetKeepsDefaults pins that an
// absent command_output leaves the zero value, so capture stays enabled
// with the documented defaults.
func TestRunConfigFromProto_CommandOutputUnsetKeepsDefaults(t *testing.T) {
	rc := runConfigFromProto(&pb.RunConfig{Tools: &pb.ToolsConfig{BuiltIn: []string{"run_command"}}})
	if rc.Tools.CommandOutput != (types.CommandOutputConfig{}) {
		t.Errorf("CommandOutput: got %+v, want zero value", rc.Tools.CommandOutput)
	}
	if !rc.Tools.CommandOutputCaptureEnabled() {
		t.Error("capture must default to enabled when command_output is unset")
	}
}

// TestRunConfigFromProto_MaxResultBytesPreserved pins the tools.maxResultBytes
// mirror through the wire format.
func TestRunConfigFromProto_MaxResultBytesPreserved(t *testing.T) {
	raw, err := proto.Marshal(&pb.RunConfig{Tools: &pb.ToolsConfig{MaxResultBytes: 65536}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded pb.RunConfig
	if err := proto.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := runConfigFromProto(&decoded).Tools.MaxResultBytes; got != 65536 {
		t.Errorf("MaxResultBytes: got %d, want 65536", got)
	}
}

// TestRunConfigFromProto_MCPOptionalPreserved pins the optional flag across
// the wire format and the translate step; the zero value must stay
// "required".
func TestRunConfigFromProto_MCPOptionalPreserved(t *testing.T) {
	original := &pb.RunConfig{
		Tools: &pb.ToolsConfig{
			McpServers: []*pb.MCPServerConfig{
				{Name: "required", Uri: "https://mcp.example.com/v1"},
				{Name: "optional", Uri: "https://mcp.example.com/v2", Optional: true},
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
	if len(rc.Tools.MCPServers) != 2 {
		t.Fatalf("MCPServers: got %d entries, want 2", len(rc.Tools.MCPServers))
	}
	if rc.Tools.MCPServers[0].Optional {
		t.Error("a server with no optional field must translate as required")
	}
	if !rc.Tools.MCPServers[1].Optional {
		t.Error("Optional was dropped between proto and RunConfig")
	}
}

func TestRunTraceToProto_DegradedCapabilities(t *testing.T) {
	pt := runTraceToProto(&types.RunTrace{
		ID: "run-degraded",
		DegradedCapabilities: []types.DegradedCapability{
			{Kind: types.DegradedCapabilityMCP, Name: "docs", Error: "connection refused"},
		},
	})

	raw, err := proto.Marshal(pt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded pb.RunTrace
	if err := proto.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.DegradedCapabilities) != 1 {
		t.Fatalf("DegradedCapabilities: got %d entries, want 1", len(decoded.DegradedCapabilities))
	}
	got := decoded.DegradedCapabilities[0]
	if got.Kind != "mcp" || got.Name != "docs" || got.Error != "connection refused" {
		t.Errorf("DegradedCapabilities[0] = %+v, want mcp/docs/connection refused", got)
	}

	if empty := runTraceToProto(&types.RunTrace{ID: "run-clean"}); len(empty.DegradedCapabilities) != 0 {
		t.Errorf("a run with nothing degraded must carry no entries, got %v", empty.DegradedCapabilities)
	}
}
