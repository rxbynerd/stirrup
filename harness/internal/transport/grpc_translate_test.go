package transport

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

// TestRunConfigFromProto_SessionNamePropagates ensures SessionName set on
// the proto RunConfig is copied into the internal types.RunConfig.
func TestRunConfigFromProto_SessionNamePropagates(t *testing.T) {
	name := "nightly-eval"

	rc := runConfigFromProto(&pb.RunConfig{SessionName: &name})

	require.Equal(t, "nightly-eval", rc.SessionName)
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

	require.Equal(t, "composed", rc.PromptBuilder.Type)
	require.Equal(t, pc.PromptBuilder.Template, rc.PromptBuilder.Template)
	require.Equal(t, "claude-fable-5", rc.PromptBuilder.PromptModel)
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
		wire, err := proto.Marshal(&pb.RuleOfTwoConfig{})
		require.NoError(t, err)

		var decoded pb.RuleOfTwoConfig
		require.NoError(t, proto.Unmarshal(wire, &decoded))
		require.Nil(t, decoded.Enforce)

		rc := runConfigFromProto(&pb.RunConfig{RuleOfTwo: &decoded})
		require.NotNil(t, rc.RuleOfTwo, "RuleOfTwo must be set when the proto sub-message is present")
		require.Nil(t, rc.RuleOfTwo.Enforce)
	})

	t.Run("explicit false round-trips as &false", func(t *testing.T) {
		enforce := false
		wire, err := proto.Marshal(&pb.RuleOfTwoConfig{Enforce: &enforce})
		require.NoError(t, err)

		var decoded pb.RuleOfTwoConfig
		require.NoError(t, proto.Unmarshal(wire, &decoded))
		require.NotNil(t, decoded.Enforce)
		require.False(t, *decoded.Enforce)
	})

	t.Run("explicit true round-trips as &true", func(t *testing.T) {
		enforce := true
		wire, err := proto.Marshal(&pb.RuleOfTwoConfig{Enforce: &enforce})
		require.NoError(t, err)

		var decoded pb.RuleOfTwoConfig
		require.NoError(t, proto.Unmarshal(wire, &decoded))
		require.NotNil(t, decoded.Enforce)
		require.True(t, *decoded.Enforce)
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

		require.NotNil(t, rc.RuleOfTwo)
		require.Nil(t, rc.RuleOfTwo.Runtime)
	})

	t.Run("empty runtime surfaces as present-but-empty", func(t *testing.T) {
		rc := runConfigFromProto(&pb.RunConfig{
			RuleOfTwo: &pb.RuleOfTwoConfig{Runtime: &pb.RuleOfTwoRuntimeConfig{}},
		})

		require.NotNil(t, rc.RuleOfTwo)
		require.NotNil(t, rc.RuleOfTwo.Runtime)
		require.Empty(t, rc.RuleOfTwo.Runtime.Classifier)
		require.Empty(t, rc.RuleOfTwo.Runtime.OnDetect)
		require.Nil(t, rc.RuleOfTwo.Runtime.GuardCriteria)
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
		require.NotNil(t, rt)
		require.Equal(t, "patterns", rt.Classifier)
		require.Equal(t, "block-external", rt.OnDetect)
		require.Equal(t, []string{"sensitive_data", "pii"}, rt.GuardCriteria)

		// GuardCriteria must be copied, not aliased, so mutating the
		// source after translation cannot reach the translated copy.
		src.RuleOfTwo.Runtime.GuardCriteria[0] = "mutated"
		require.Equal(t, "sensitive_data", rt.GuardCriteria[0], "GuardCriteria shares backing array with proto source")
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

	require.Equal(t, "runsc", rc.Executor.Runtime)
	require.Equal(t, "/etc/stirrup/policy.cedar", rc.PermissionPolicy.PolicyFile)
	require.Equal(t, "deny-side-effects", rc.PermissionPolicy.Fallback)
	require.NotNil(t, rc.RuleOfTwo)
	require.NotNil(t, rc.RuleOfTwo.Enforce)
	require.False(t, *rc.RuleOfTwo.Enforce)
	require.NotNil(t, rc.CodeScanner)
	require.Equal(t, "composite", rc.CodeScanner.Type)
	require.Len(t, rc.CodeScanner.Scanners, 2)
	require.True(t, rc.CodeScanner.BlockOnWarn)
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

	require.Equal(t, "k8s", rc.Executor.Type)
	require.Equal(t, "gvisor", rc.Executor.Runtime)
	require.Equal(t, "agents", rc.Executor.K8sNamespace)
	require.Equal(t, "/home/u/.kube/config", rc.Executor.K8sKubeconfig)
	require.Equal(t, "agent-sa", rc.Executor.K8sServiceAccount)
	require.Equal(t, "ssd", rc.Executor.K8sNodeSelector["disktype"])
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

	require.Equal(t, "gcs", rc.TraceEmitter.Type)
	require.Equal(t, "stirrup-results", rc.TraceEmitter.Bucket)
	require.Equal(t, "traces/", rc.TraceEmitter.ObjectPrefix)
}

// TestRunConfigFromProto_SessionNameAbsentWhenNil documents the safe default:
// when the proto omits SessionName (the field is a proto3 optional, so the
// zero value is a nil pointer), the internal RunConfig must surface the empty
// string rather than panicking on a nil dereference.
func TestRunConfigFromProto_SessionNameAbsentWhenNil(t *testing.T) {
	rc := runConfigFromProto(&pb.RunConfig{})

	require.Empty(t, rc.SessionName)
	require.Nil(t, rc.RuleOfTwo)
	require.Nil(t, rc.CodeScanner)

	rcUnset := runConfigFromProto(&pb.RunConfig{RuleOfTwo: &pb.RuleOfTwoConfig{}})
	require.NotNil(t, rcUnset.RuleOfTwo)
	require.Nil(t, rcUnset.RuleOfTwo.Enforce)
}

// TestRunConfigFromProto_GuardRailFieldsPreserved asserts that every field on
// the proto GuardRailConfig is preserved by runConfigFromProto, including
// the recursive Stages case.
func TestRunConfigFromProto_GuardRailFieldsPreserved(t *testing.T) {
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
					Model:     "claude-haiku-5-5",
					TimeoutMs: 5000,
				},
				{
					Type:      "shieldstral",
					Endpoint:  "https://api.mistral.ai",
					Model:     "mistralai/Shieldstral-1.0-3B",
					ApiKeyRef: "secret://MISTRAL_API_KEY",
					TimeoutMs: 3000,
				},
			},
		},
	}

	rc := runConfigFromProto(pc)

	require.NotNil(t, rc.GuardRail, "GuardRail dropped during proto translation")
	require.Equal(t, "composite", rc.GuardRail.Type)
	require.Equal(t, []string{"pre_turn", "post_turn"}, rc.GuardRail.Phases)
	require.Equal(t, 1500, rc.GuardRail.TimeoutMs)
	require.True(t, rc.GuardRail.FailOpen)
	require.Equal(t, 256, rc.GuardRail.MinChunkChars)
	require.Len(t, rc.GuardRail.Stages, 3)

	gg := rc.GuardRail.Stages[0]
	require.Equal(t, "granite-guardian", gg.Type)
	require.Equal(t, "http://classifier.local:9999", gg.Endpoint)
	require.Equal(t, "ibm-granite/granite-guardian-4.1-8b", gg.Model)
	require.Equal(t, []string{"harm", "jailbreak"}, gg.Criteria)
	require.Equal(t, "no profanity", gg.CustomCriteria["my_rule"])
	require.NotNil(t, gg.Think)
	require.True(t, *gg.Think)

	cj := rc.GuardRail.Stages[1]
	require.Equal(t, "cloud-judge", cj.Type)
	require.Equal(t, "claude-haiku-5-5", cj.Model)
	require.Equal(t, 5000, cj.TimeoutMs)

	ss := rc.GuardRail.Stages[2]
	require.Equal(t, "shieldstral", ss.Type)
	require.Equal(t, "https://api.mistral.ai", ss.Endpoint)
	require.Equal(t, "mistralai/Shieldstral-1.0-3B", ss.Model)
	require.Equal(t, "secret://MISTRAL_API_KEY", ss.APIKeyRef)
	require.Equal(t, 3000, ss.TimeoutMs)
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
	require.NoError(t, err)

	var decoded pb.RunConfig
	require.NoError(t, proto.Unmarshal(wire, &decoded))

	got := runConfigFromProto(&decoded).ContextStrategy
	require.Equal(t, "clear-tool-results", got.Type)
	require.Equal(t, 100000, got.MaxTokens)
	require.NotNil(t, got.Keep, "keep must survive as an explicit 0, not unset")
	require.Equal(t, 0, *got.Keep)
	require.NotNil(t, got.ClearAtLeast)
	require.Equal(t, 20000, *got.ClearAtLeast)
	require.Equal(t, []string{"run_command", "web_fetch"}, got.ExcludeTools)

	decoded.ContextStrategy.ExcludeTools[0] = "mutated"
	require.Equal(t, "run_command", got.ExcludeTools[0], "ExcludeTools aliases the proto slice")
}

func TestRunConfigFromProto_ClearToolResultsFieldsAbsentStayUnset(t *testing.T) {
	got := runConfigFromProto(&pb.RunConfig{ContextStrategy: &pb.ContextStrategyConfig{Type: "sliding-window"}}).ContextStrategy

	require.Nil(t, got.Keep)
	require.Nil(t, got.ClearAtLeast)
	require.Nil(t, got.ExcludeTools)
}

// TestRunConfigFromProto_GuardRailAbsentStaysNil documents that an
// absent guard_rail proto field translates to a nil GuardRail (the
// validator then applies the default — guards are opt-in).
func TestRunConfigFromProto_GuardRailAbsentStaysNil(t *testing.T) {
	rc := runConfigFromProto(&pb.RunConfig{})

	require.Nil(t, rc.GuardRail)
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

	require.Equal(t, "agent-a", rc.Observability.ServiceName)
	require.Equal(t, "staging", rc.Observability.Environment)
	require.Equal(t, "eval", rc.Observability.ServiceNamespace)
}

// TestRunConfigFromProto_ObservabilityAbsentWhenNil documents the safe
// default: when the proto omits the Observability sub-message, the internal
// RunConfig surfaces a zero-value ObservabilityConfig rather than panicking
// on a nil dereference.
func TestRunConfigFromProto_ObservabilityAbsentWhenNil(t *testing.T) {
	rc := runConfigFromProto(&pb.RunConfig{})

	require.Empty(t, rc.Observability.Environment)
	require.Empty(t, rc.Observability.ServiceNamespace)
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

	cred := rc.Provider.Credential
	require.NotNil(t, cred, "Credential dropped during proto translation")
	require.Equal(t, "azure-workload-identity", cred.Type)
	require.Equal(t, "11111111-2222-3333-4444-555555555555", cred.AzureTenantID)
	require.Equal(t, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", cred.AzureClientID)
	require.Equal(t, "https://cognitiveservices.azure.com/.default", cred.AzureScope)
	require.Equal(t, "https://login.microsoftonline.us/11111111-2222-3333-4444-555555555555/oauth2/v2.0/token", cred.AzureTokenURL)
	require.NotNil(t, cred.TokenSource, "TokenSource dropped during proto translation")
	require.Equal(t, "/var/run/secrets/azure/token", cred.TokenSource.Path)
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

	cred := rc.Provider.Credential
	require.NotNil(t, cred, "Credential dropped during proto translation")
	require.Equal(t, "gcp-workload-identity-federation", cred.Type)
	require.Equal(t, "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/azure-pool/providers/azure-provider", cred.Audience)
	require.Equal(t, "vertex@my-project.iam.gserviceaccount.com", cred.ServiceAccount)

	ts := cred.TokenSource
	require.NotNil(t, ts, "TokenSource dropped during proto translation")
	require.Equal(t, "azure-imds", ts.Type)
	require.Equal(t, "api://AzureADTokenExchange", ts.Resource)
	require.Equal(t, "11111111-1111-1111-1111-111111111111", ts.ClientID)
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

	cred := rc.Provider.Credential
	require.NotNil(t, cred, "Credential dropped during proto translation")
	require.Equal(t, "anthropic-wif", cred.Type)
	require.Equal(t, "fdrl_abc123", cred.FederationRuleID)
	require.Equal(t, "550e8400-e29b-41d4-a716-446655440000", cred.OrganizationID)
	require.Equal(t, "svac_xyz789", cred.ServiceAccountID)
	require.Equal(t, "default", cred.WorkspaceID)
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

	require.NotNil(t, rc.Provider.Credential, "Credential dropped during proto translation")
	assert.Equal(t, "openai-wif", rc.Provider.Credential.Type, "Credential.Type")
	assert.Equal(t, "idp_abc123", rc.Provider.Credential.OpenAIIdentityProviderID, "Credential.OpenAIIdentityProviderID")
	assert.Equal(t, "sa_xyz789", rc.Provider.Credential.OpenAIServiceAccountID, "Credential.OpenAIServiceAccountID")
	assert.Equal(t, "urn:ietf:params:oauth:token-type:jwt", rc.Provider.Credential.OpenAISubjectTokenType, "Credential.OpenAISubjectTokenType")
	require.NotNil(t, rc.Provider.Credential.TokenSource, "TokenSource dropped")
	assert.Equal(t, "https://api.openai.com/v1", rc.Provider.Credential.TokenSource.Audience, "TokenSource.Audience")
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

	assert.Equal(t, "http/protobuf", rc.TraceEmitter.Protocol, "TraceEmitter.Protocol")
	assert.Equal(t, "https://otlp-gateway-prod-us-east-0.grafana.net/otlp", rc.TraceEmitter.Endpoint, "TraceEmitter.Endpoint")
	assert.Equal(t, "Basic xxx", rc.TraceEmitter.Headers["Authorization"], "TraceEmitter.Headers[Authorization]")
	assert.Equal(t, "team-a", rc.TraceEmitter.Headers["X-Tenant"], "TraceEmitter.Headers[X-Tenant]")
}

// TestRunConfigFromProto_TraceEmitterMetricsTemporalityPreserved guards
// TraceEmitterConfig.MetricsTemporality against silent drop in
// runConfigFromProto.
func TestRunConfigFromProto_TraceEmitterMetricsTemporalityPreserved(t *testing.T) {
	rc := runConfigFromProto(&pb.RunConfig{
		TraceEmitter: &pb.TraceEmitterConfig{Type: "otel", MetricsTemporality: "delta"},
	})
	require.Equal(t, "delta", rc.TraceEmitter.MetricsTemporality)
}

// TestRunConfigFromProto_TraceEmitterCaptureContentMaxBytesPreserved
// guards TraceEmitterConfig.CaptureContentMaxBytes against silent drop in
// runConfigFromProto.
func TestRunConfigFromProto_TraceEmitterCaptureContentMaxBytesPreserved(t *testing.T) {
	rc := runConfigFromProto(&pb.RunConfig{
		TraceEmitter: &pb.TraceEmitterConfig{Type: "otel", CaptureContent: true, CaptureContentMaxBytes: 2048},
	})
	assert.True(t, rc.TraceEmitter.CaptureContent, "TraceEmitter.CaptureContent dropped")
	assert.Equal(t, 2048, rc.TraceEmitter.CaptureContentMaxBytes, "TraceEmitter.CaptureContentMaxBytes")
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
	assert.Equal(t, "example-platform", rc.TraceEmitter.Platform, "TraceEmitter.Platform")
	assert.Equal(t, map[string]string{"region": "eu-west-2", "project": "agents"}, rc.TraceEmitter.PlatformOptions, "TraceEmitter.PlatformOptions")
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

		require.NotNil(t, rc.Provider.Retry, "Retry dropped during proto translation")
		require.Equal(t, 4, rc.Provider.Retry.MaxAttempts)
		require.Equal(t, 250, rc.Provider.Retry.InitialDelayMs)
		require.Equal(t, 12000, rc.Provider.Retry.MaxDelayMs)
		require.Equal(t, 60000, rc.Provider.Retry.WallClockBudgetMs)
	})

	t.Run("nil retry stays nil on the Go side", func(t *testing.T) {
		pc := &pb.RunConfig{
			Provider: &pb.ProviderConfig{
				Type: "openai-compatible",
			},
		}

		rc := runConfigFromProto(pc)

		require.Nil(t, rc.Provider.Retry, "Retry should be nil when proto omits the field")
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

	require.NotNil(t, rc.ToolDispatch, "ToolDispatch sub-config dropped: got nil, want populated")
	require.Equal(t, 8, rc.ToolDispatch.MaxParallel)
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
		require.NoError(t, err, "marshal")
		var decoded pb.RunConfig
		require.NoError(t, proto.Unmarshal(bytes, &decoded), "unmarshal")

		rc := runConfigFromProto(&decoded)

		require.NotNil(t, rc.Provider.Batch, "Batch dropped during proto translation")
		require.True(t, rc.Provider.Batch.Enabled, "Batch.Enabled")
		require.NotNil(t, rc.Provider.Batch.MaxWaitSeconds, "Batch.MaxWaitSeconds")
		require.Equal(t, 3600, *rc.Provider.Batch.MaxWaitSeconds, "Batch.MaxWaitSeconds")
		require.True(t, rc.Provider.Batch.HarnessSidePolling, "Batch.HarnessSidePolling")
		require.True(t, rc.Provider.Batch.FallbackOnTimeout, "Batch.FallbackOnTimeout")
		require.True(t, rc.Provider.Batch.CancelBundleOnRunCancel, "Batch.CancelBundleOnRunCancel")
		require.True(t, rc.Provider.Batch.AllowInteractiveModes, "Batch.AllowInteractiveModes")
	})

	t.Run("nil batch stays nil on the Go side", func(t *testing.T) {
		pc := &pb.RunConfig{
			Provider: &pb.ProviderConfig{Type: "anthropic"},
		}

		rc := runConfigFromProto(pc)

		require.Nil(t, rc.Provider.Batch, "Batch should be nil when proto omits the field")
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
		require.NoError(t, err, "marshal")
		var decoded pb.RunConfig
		require.NoError(t, proto.Unmarshal(bytes, &decoded), "unmarshal")

		rc := runConfigFromProto(&decoded)
		require.NotNil(t, rc.Provider.Batch, "Batch should be present when proto sub-message is set")
		require.Nil(t, rc.Provider.Batch.MaxWaitSeconds, "MaxWaitSeconds should be nil when proto field is unset")
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
			require.Equal(t, tc.wantProfile, rc.Provider.CompatProfile)
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
		{name: "nil unset", set: nil},
		{name: "explicit zero greedy", set: proto.Float64(0.0)},
		{name: "mid range", set: proto.Float64(0.7)},
		{name: "above anthropic ceiling", set: proto.Float64(1.5)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pc := &pb.RunConfig{Temperature: tc.set}
			rc := runConfigFromProto(pc)
			if tc.set == nil {
				require.Nil(t, rc.Temperature)
				return
			}
			require.NotNil(t, rc.Temperature)
			require.InDelta(t, *tc.set, *rc.Temperature, 1e-9)
		})
	}
}

// TestRunConfigFromProto_ReasoningEffortCarried pins the new
// provider-neutral reasoning knob across the wire boundary: a set value
// must survive verbatim and an absent one must stay empty (say nothing
// on the wire downstream).
func TestRunConfigFromProto_ReasoningEffortCarried(t *testing.T) {
	assert.Equal(t, "high", runConfigFromProto(&pb.RunConfig{ReasoningEffort: "high"}).ReasoningEffort, "set value")
	assert.Empty(t, runConfigFromProto(&pb.RunConfig{}).ReasoningEffort, "absent value")
}

func TestRunConfigFromProto_ReasoningSummaryCarried(t *testing.T) {
	assert.Equal(t, "off", runConfigFromProto(&pb.RunConfig{ReasoningSummary: "off"}).ReasoningSummary, "set value")
	assert.Empty(t, runConfigFromProto(&pb.RunConfig{}).ReasoningSummary, "absent value")
}

// TestRunConfigFromProto_FollowUpHistoryCarried pins follow_up_history
// across the wire boundary: without the copy a control plane asking for
// "continue" would silently get the reset default.
func TestRunConfigFromProto_FollowUpHistoryCarried(t *testing.T) {
	assert.Equal(t, types.FollowUpHistoryContinue, runConfigFromProto(&pb.RunConfig{FollowUpHistory: types.FollowUpHistoryContinue}).FollowUpHistory, "set value")
	assert.Empty(t, runConfigFromProto(&pb.RunConfig{}).FollowUpHistory, "absent value")
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
	assert.Equal(t, "proj-1", cfg.GCPProject, "GCPProject")
	assert.Equal(t, "us-central1", cfg.GCPLocation, "GCPLocation")
	assert.Equal(t, "/etc/sa.json", cfg.GCPCredentialsFile, "GCPCredentialsFile")
	want := []types.GeminiSafetySetting{
		{Category: "HARM_CATEGORY_HATE_SPEECH", Threshold: "BLOCK_ONLY_HIGH"},
		{Category: "HARM_CATEGORY_HARASSMENT", Threshold: "BLOCK_NONE"},
	}
	assert.Equal(t, want, cfg.GeminiSafetySettings, "GeminiSafetySettings")
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
			require.Equal(t, tc.outcome, pt.Outcome, "Outcome")
			require.Equal(t, tc.outcome, pt.StopReason, "StopReason should mirror Outcome")
			require.Equal(t, "run-141", pt.RunId, "RunId")
			require.EqualValues(t, 3, pt.Turns, "Turns")
			require.EqualValues(t, 120, pt.InputTokens, "InputTokens")
			require.EqualValues(t, 45, pt.OutputTokens, "OutputTokens")
			require.EqualValues(t, 80, pt.CacheReadTokens, "CacheReadTokens")
			require.EqualValues(t, 30, pt.CacheWriteTokens, "CacheWriteTokens")
			require.EqualValues(t, 12, pt.ReasoningTokens, "ReasoningTokens")
			require.EqualValues(t, 2500, pt.DurationMs, "DurationMs")

			raw, err := proto.Marshal(pt)
			require.NoError(t, err, "marshal")
			var decoded pb.RunTrace
			require.NoError(t, proto.Unmarshal(raw, &decoded), "unmarshal")
			require.Equal(t, tc.outcome, decoded.Outcome, "Outcome after wire round-trip")
			require.Equal(t, tc.outcome, decoded.StopReason, "StopReason after wire round-trip")
			require.EqualValues(t, 80, decoded.CacheReadTokens, "CacheReadTokens after wire round-trip")
			require.EqualValues(t, 30, decoded.CacheWriteTokens, "CacheWriteTokens after wire round-trip")
			require.EqualValues(t, 12, decoded.ReasoningTokens, "ReasoningTokens after wire round-trip")
		})
	}
}

func TestRunTraceToProto_ZeroBreakdownRoundTrip(t *testing.T) {
	pt := runTraceToProto(&types.RunTrace{ID: "run-zero", TokenUsage: types.TokenUsage{Input: 120, Output: 45}})
	raw, err := proto.Marshal(pt)
	require.NoError(t, err, "marshal")
	var decoded pb.RunTrace
	require.NoError(t, proto.Unmarshal(raw, &decoded), "unmarshal")
	assert.EqualValues(t, 120, decoded.InputTokens, "InputTokens")
	assert.EqualValues(t, 45, decoded.OutputTokens, "OutputTokens")
	assert.Zero(t, decoded.CacheReadTokens, "CacheReadTokens")
	assert.Zero(t, decoded.CacheWriteTokens, "CacheWriteTokens")
	assert.Zero(t, decoded.ReasoningTokens, "ReasoningTokens")
}

func TestRunTraceToProto_SaturatesTokenCounts(t *testing.T) {
	pt := runTraceToProto(&types.RunTrace{TokenUsage: types.TokenUsage{
		Input:     math.MaxInt,
		Output:    math.MaxInt32 + 1,
		CacheRead: math.MaxInt32,
		Reasoning: math.MinInt,
	}})
	assert.EqualValues(t, math.MaxInt32, pt.InputTokens, "InputTokens")
	assert.EqualValues(t, math.MaxInt32, pt.OutputTokens, "OutputTokens")
	assert.EqualValues(t, math.MaxInt32, pt.CacheReadTokens, "CacheReadTokens")
	assert.EqualValues(t, math.MinInt32, pt.ReasoningTokens, "ReasoningTokens")
}

func TestRunTraceToProto_LedgerUsage(t *testing.T) {
	pt := runTraceToProto(&types.RunTrace{
		ID:                  "run-aux",
		TokenUsage:          types.TokenUsage{Input: 500, Output: 60},
		AuxiliaryTokens:     types.TokenUsage{Input: 200, Output: 10, CacheRead: 50, CacheWrite: 7, Reasoning: 3},
		ExternalGuardTokens: types.TokenUsage{Input: math.MaxInt, Output: 1},
	})
	raw, err := proto.Marshal(pt)
	require.NoError(t, err, "marshal")
	var decoded pb.RunTrace
	require.NoError(t, proto.Unmarshal(raw, &decoded), "unmarshal")
	aux := decoded.GetAuxiliaryTokens()
	assert.EqualValues(t, 200, aux.GetInputTokens(), "AuxiliaryTokens.InputTokens")
	assert.EqualValues(t, 10, aux.GetOutputTokens(), "AuxiliaryTokens.OutputTokens")
	assert.EqualValues(t, 50, aux.GetCacheReadTokens(), "AuxiliaryTokens.CacheReadTokens")
	assert.EqualValues(t, 7, aux.GetCacheWriteTokens(), "AuxiliaryTokens.CacheWriteTokens")
	assert.EqualValues(t, 3, aux.GetReasoningTokens(), "AuxiliaryTokens.ReasoningTokens")
	ext := decoded.GetExternalGuardTokens()
	assert.EqualValues(t, math.MaxInt32, ext.GetInputTokens(), "ExternalGuardTokens.InputTokens saturates")
	assert.EqualValues(t, 1, ext.GetOutputTokens(), "ExternalGuardTokens.OutputTokens")
}

func TestRunTraceToProto_ZeroLedgerUsageIsAbsent(t *testing.T) {
	pt := runTraceToProto(&types.RunTrace{ID: "run-plain", TokenUsage: types.TokenUsage{Input: 5}})
	assert.Nil(t, pt.AuxiliaryTokens, "AuxiliaryTokens")
	assert.Nil(t, pt.ExternalGuardTokens, "ExternalGuardTokens")
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
	require.NoError(t, err, "marshal")
	var decoded pb.RunTrace
	require.NoError(t, proto.Unmarshal(raw, &decoded), "unmarshal")
	got := decoded.GetStopDetails()
	assert.Equal(t, "refusal", got.GetType(), "StopDetails.Type")
	assert.Equal(t, "reasoning_extraction", got.GetCategory(), "StopDetails.Category")
	assert.Equal(t, "declined", got.GetExplanation(), "StopDetails.Explanation")

	assert.Nil(t, runTraceToProto(&types.RunTrace{Outcome: "success"}).StopDetails, "StopDetails absent for a nil StopDetails")
}

// TestRunTraceToProto_PartialStopDetails pins that a refusal with no
// category or explanation still reaches the wire as a present message.
func TestRunTraceToProto_PartialStopDetails(t *testing.T) {
	raw, err := proto.Marshal(runTraceToProto(&types.RunTrace{
		Outcome:     "refusal",
		StopDetails: &types.StopDetails{Type: "refusal"},
	}))
	require.NoError(t, err, "marshal")
	var decoded pb.RunTrace
	require.NoError(t, proto.Unmarshal(raw, &decoded), "unmarshal")
	got := decoded.GetStopDetails()
	require.NotNil(t, got, "StopDetails should be present with type refusal")
	assert.Equal(t, "refusal", got.GetType(), "StopDetails.Type")
	assert.Empty(t, got.GetCategory(), "StopDetails.Category")
	assert.Empty(t, got.GetExplanation(), "StopDetails.Explanation")
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
	require.NoError(t, err, "marshal")
	var decoded pb.RunTrace
	require.NoError(t, proto.Unmarshal(raw, &decoded), "unmarshal")
	got := decoded.GetVerification()
	assert.EqualValues(t, 2, got.GetAttempt(), "Verification.Attempt")
	assert.False(t, got.GetPassed(), "Verification.Passed")
	assert.Empty(t, got.GetFeedback(), "Verification.Feedback")
	assert.Equal(t, "exec failed: [REDACTED]", got.GetError(), "Verification.Error")

	passed := runTraceToProto(&types.RunTrace{
		Outcome:             "success",
		VerificationResults: []types.VerificationResult{{Attempt: 1, Passed: true}},
	})
	assert.True(t, passed.GetVerification().GetPassed(), "passing Verification.Passed")
	assert.EqualValues(t, 1, passed.GetVerification().GetAttempt(), "passing Verification.Attempt")

	assert.Nil(t, runTraceToProto(&types.RunTrace{Outcome: "success"}).Verification, "Verification absent with no recorded attempt")
}

// TestRunConfigFromProto_HooksAbsentStaysNil pins the nil/absent case:
// a TaskAssignment with no hooks sub-message must not synthesise a
// non-nil types.HooksConfig.
func TestRunConfigFromProto_HooksAbsentStaysNil(t *testing.T) {
	rc := runConfigFromProto(&pb.RunConfig{})
	require.Nil(t, rc.Hooks, "Hooks should be nil when proto field absent")
}

// TestRunConfigFromProto_HooksEmptyStaysNonNil pins the companion case:
// an explicit-but-empty proto HooksConfig{} (e.g. a control plane that
// always sets the sub-message) must translate to a non-nil, empty
// types.HooksConfig — distinguishable from "field absent" the same way
// ToolDispatch preserves the unset/explicit-zero distinction.
func TestRunConfigFromProto_HooksEmptyStaysNonNil(t *testing.T) {
	rc := runConfigFromProto(&pb.RunConfig{Hooks: &pb.HooksConfig{}})
	require.NotNil(t, rc.Hooks, "Hooks should be non-nil for an explicit-but-empty proto sub-message")
	assert.Empty(t, rc.Hooks.PreRun, "PreRun")
	assert.Empty(t, rc.Hooks.PostRun, "PostRun")
}

// wireRoundTrip encodes in and decodes the bytes into out, so a test reads
// values that have passed through the wire format.
func wireRoundTrip(t *testing.T, in, out proto.Message) {
	t.Helper()
	raw, err := proto.Marshal(in)
	require.NoError(t, err, "marshal")
	require.NoError(t, proto.Unmarshal(raw, out), "unmarshal")
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

	var decoded pb.RunConfig
	wireRoundTrip(t, original, &decoded)

	rc := runConfigFromProto(&decoded)
	require.NotNil(t, rc.Hooks, "Hooks dropped across the wire round-trip")

	require.Len(t, rc.Hooks.PreRun, 1)
	pre := rc.Hooks.PreRun[0]
	assert.Equal(t, "command", pre.Type, "PreRun[0].Type")
	assert.Equal(t, "clone", pre.Name, "PreRun[0].Name")
	assert.Equal(t, "git clone . .", pre.Command, "PreRun[0].Command")
	assert.Equal(t, 120, pre.TimeoutSeconds, "PreRun[0].TimeoutSeconds")

	require.Len(t, rc.Hooks.PostRun, 2)
	post0 := rc.Hooks.PostRun[0]
	assert.Equal(t, "test -f marker", post0.Command, "PostRun[0].Command")
	assert.Equal(t, "success", post0.RunOn, "PostRun[0].RunOn")
	assert.True(t, post0.ContinueOnError, "PostRun[0].ContinueOnError")
	post1 := rc.Hooks.PostRun[1]
	assert.Equal(t, "curl -X POST https://example.com", post1.Command, "PostRun[1].Command")
	assert.Equal(t, "failure", post1.RunOn, "PostRun[1].RunOn")
	assert.False(t, post1.ContinueOnError, "PostRun[1].ContinueOnError")
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

	require.Equal(t, "original", rc.Hooks.PreRun[0].Command,
		"translated HookConfig aliases the proto's backing data")
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

	var decoded pb.RunConfig
	wireRoundTrip(t, original, &decoded)

	rc := runConfigFromProto(&decoded)

	require.Len(t, rc.Tools.MCPServers, 1)
	srv := rc.Tools.MCPServers[0]
	require.Equal(t, []string{"lookup", "query"}, srv.AllowedTools)
	require.Equal(t, []string{"mcp.example.com"}, srv.AllowedMCPHosts)
}

// TestHarnessEventToProto_AudiencePreserved checks that a HarnessEvent's
// Audience survives the internal-to-proto hop unchanged. The control plane
// has no other source for the intended JWT aud claim.
func TestHarnessEventToProto_AudiencePreserved(t *testing.T) {
	e := types.HarnessEvent{
		Type:      "sandbox_token_request",
		RequestID: "req-1",
		Audience:  "https://haybale.internal",
	}

	pe := harnessEventToProto(e)

	assert.Equal(t, "https://haybale.internal", pe.Audience)
	assert.Equal(t, "req-1", pe.RequestId)
}

// TestControlEventFromProto_SandboxTokenFieldsPreserved checks that the
// sandbox token response fields survive translation. The token case goes
// through the wire format so an unset optional ExpiresAt stays nil instead
// of reading back as an explicit zero.
func TestControlEventFromProto_SandboxTokenFieldsPreserved(t *testing.T) {
	t.Run("token and expires_at round-trip", func(t *testing.T) {
		var exp int64 = 1700003600
		original := &pb.ControlEvent{
			Type:      "sandbox_token_response",
			RequestId: "req-1",
			Token:     "eyJhbGciOiJFUzI1NiJ9.eyJzdWIiOiJydW4tMSJ9.sig",
			ExpiresAt: &exp,
		}

		var decoded pb.ControlEvent
		wireRoundTrip(t, original, &decoded)

		e := controlEventFromProto(&decoded)

		require.Equal(t, original.Token, e.Token)
		require.NotNil(t, e.ExpiresAt, "ExpiresAt: got nil, want non-nil pointer")
		require.Equal(t, exp, *e.ExpiresAt)
	})

	t.Run("expires_at unset stays nil", func(t *testing.T) {
		original := &pb.ControlEvent{
			Type:      "sandbox_token_response",
			RequestId: "req-2",
			Token:     "some-token",
		}

		var decoded pb.ControlEvent
		wireRoundTrip(t, original, &decoded)

		e := controlEventFromProto(&decoded)

		require.Nil(t, e.ExpiresAt, "ExpiresAt should be nil when proto field is unset")
	})

	t.Run("is_error and reason round-trip for a decline", func(t *testing.T) {
		original := &pb.ControlEvent{
			Type:      "sandbox_token_response",
			RequestId: "req-3",
			IsError:   &pb.OptionalBool{Value: true},
			Reason:    "no issuer configured",
		}

		e := controlEventFromProto(original)

		require.NotNil(t, e.IsError)
		require.True(t, *e.IsError)
		require.Equal(t, "no issuer configured", e.Reason)
		require.Empty(t, e.Token, "Token should be empty on an error response")
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

	var decoded pb.RunConfig
	wireRoundTrip(t, original, &decoded)

	rc := runConfigFromProto(&decoded)

	require.Len(t, rc.Tools.MCPServers, 1)
	srv := rc.Tools.MCPServers[0]
	assert.Empty(t, srv.AllowedTools, "AllowedTools should be empty when proto field absent")
	assert.Empty(t, srv.AllowedMCPHosts, "AllowedMCPHosts should be empty when proto field absent")
}

// TestRunConfigFromProto_ExecutorSandboxFieldsPreserved pins the executor
// fields registry pinning, workspace export, k8s egress wiring, and the
// sandbox identity and git proxy blocks. The values go through the wire
// format so a field that is never encoded is caught here.
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

	var decoded pb.RunConfig
	wireRoundTrip(t, original, &decoded)

	ec := runConfigFromProto(&decoded).Executor

	assert.Equal(t, "http://egress.agents.svc:3128", ec.K8sEgressProxyURL)
	assert.Equal(t, "gs://stirrup-workspaces/runs", ec.WorkspaceExportTo)
	assert.Equal(t, []string{"ghcr.io/rxbynerd/*", "docker.io/library/*"}, ec.RegistryAllowlist)

	require.NotNil(t, ec.SandboxIdentity, "SandboxIdentity nil after translation")
	assert.Equal(t, "control-plane", ec.SandboxIdentity.Source)
	assert.Equal(t, "https://haybale.internal", ec.SandboxIdentity.Audience)
	assert.Equal(t, "HAYBALE_TOKEN", ec.SandboxIdentity.EnvVar)

	require.NotNil(t, ec.GitProxy, "GitProxy nil after translation")
	assert.Equal(t, "https://haybale.internal", ec.GitProxy.URL)
	assert.Equal(t, []string{"github.com", "gitlab.com"}, ec.GitProxy.Hosts)
	assert.True(t, ec.GitProxy.RewriteSsh, "GitProxy.RewriteSsh not propagated")
	assert.Equal(t, "HAYBALE_TOKEN", ec.GitProxy.TokenEnvVar)
}

// TestRunConfigFromProto_ExecutorSandboxFieldsAbsentStayNil documents the
// safe default: an ExecutorConfig that omits the new sub-messages must
// leave the internal pointers nil, so "operator did not configure" stays
// distinguishable from "configured empty" and the defaults apply.
func TestRunConfigFromProto_ExecutorSandboxFieldsAbsentStayNil(t *testing.T) {
	ec := runConfigFromProto(&pb.RunConfig{
		Executor: &pb.ExecutorConfig{Type: "container"},
	}).Executor

	assert.Nil(t, ec.SandboxIdentity, "SandboxIdentity should be nil when proto field absent")
	assert.Nil(t, ec.GitProxy, "GitProxy should be nil when proto field absent")
	assert.Nil(t, ec.RegistryAllowlist, "RegistryAllowlist should be nil when proto field absent")
	assert.Empty(t, ec.WorkspaceExportTo, "WorkspaceExportTo should be empty when proto field absent")
	assert.Empty(t, ec.K8sEgressProxyURL, "K8sEgressProxyURL should be empty when proto field absent")
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

	assert.Equal(t, "ghcr.io/rxbynerd/*", ec.RegistryAllowlist[0],
		"RegistryAllowlist shares backing array with proto source")
	assert.Equal(t, "github.com", ec.GitProxy.Hosts[0],
		"GitProxy.Hosts shares backing array with proto source")
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

	var decoded pb.RunConfig
	wireRoundTrip(t, original, &decoded)

	sink := runConfigFromProto(&decoded).ResultSink
	require.NotNil(t, sink, "ResultSink nil after translation")
	assert.Equal(t, "gcp-pubsub", sink.Type)
	assert.Equal(t, "stirrup-results", sink.Topic)
	assert.Equal(t, 4096, sink.MaxFinalAssistantTextBytes)
	assert.Equal(t, map[string]string{"tenant": "team-a", "auth": "secret://PUBSUB_TOKEN"}, sink.Attributes)

	decoded.ResultSink.Attributes["tenant"] = "mutated"
	assert.Equal(t, "team-a", sink.Attributes["tenant"], "ResultSink.Attributes shares the proto-owned map")
}

// TestRunConfigFromProto_ResultSinkAbsentStaysNil documents that an absent
// sub-message leaves ResultSink nil — the "sink disabled" state — rather
// than synthesising a block whose empty Type fails validation.
func TestRunConfigFromProto_ResultSinkAbsentStaysNil(t *testing.T) {
	require.Nil(t, runConfigFromProto(&pb.RunConfig{}).ResultSink,
		"ResultSink should be nil when proto field absent")
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
		var decoded pb.RunConfig
		wireRoundTrip(t, original, &decoded)

		esc := runConfigFromProto(&decoded).ToolChoiceEscalation
		require.NotNil(t, esc, "ToolChoiceEscalation nil after translation")
		require.True(t, esc.Enabled, "ToolChoiceEscalation.Enabled not propagated")
		require.Equal(t, 3, esc.MaxRetries)
	})

	t.Run("empty sub-message stays non-nil", func(t *testing.T) {
		esc := runConfigFromProto(&pb.RunConfig{
			ToolChoiceEscalation: &pb.ToolChoiceEscalationConfig{},
		}).ToolChoiceEscalation
		require.NotNil(t, esc, "expected non-nil ToolChoiceEscalation for an empty sub-message")
		require.False(t, esc.Enabled, "empty sub-message must not enable escalation")
		require.Equal(t, 0, esc.MaxRetries, "zero resolves to the default")
	})

	t.Run("absent stays nil", func(t *testing.T) {
		require.Nil(t, runConfigFromProto(&pb.RunConfig{}).ToolChoiceEscalation,
			"ToolChoiceEscalation should be nil when proto field absent")
	})
}

// TestRunConfigFromProto_WindDownPreserved pins the wind-down block across
// a wire round-trip; an absent sub-message stays nil (off).
func TestRunConfigFromProto_WindDownPreserved(t *testing.T) {
	t.Run("enabled", func(t *testing.T) {
		original := &pb.RunConfig{WindDown: &pb.WindDownConfig{Enabled: true}}
		var decoded pb.RunConfig
		wireRoundTrip(t, original, &decoded)

		rc := runConfigFromProto(&decoded)
		require.NotNil(t, rc.WindDown)
		require.True(t, rc.WindDown.Enabled)
		require.True(t, rc.WindDownEnabled())
	})

	t.Run("empty sub-message stays non-nil and off", func(t *testing.T) {
		rc := runConfigFromProto(&pb.RunConfig{WindDown: &pb.WindDownConfig{}})
		require.NotNil(t, rc.WindDown, "expected non-nil WindDown for an empty sub-message")
		require.False(t, rc.WindDownEnabled(), "empty sub-message must not enable wind-down")
	})

	t.Run("absent stays nil", func(t *testing.T) {
		require.Nil(t, runConfigFromProto(&pb.RunConfig{}).WindDown,
			"WindDown should be nil when proto field absent")
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
		var decoded pb.RunConfig
		wireRoundTrip(t, original, &decoded)

		obs := runConfigFromProto(&decoded).Observability
		require.Equal(t, "production", obs.Environment)
		require.Equal(t, "otlp", obs.LogsExport.Type)
		require.Equal(t, "otel-collector.observability.svc:4317", obs.LogsExport.Endpoint)
	})

	t.Run("absent leaves stderr-only zero value", func(t *testing.T) {
		obs := runConfigFromProto(&pb.RunConfig{
			Observability: &pb.ObservabilityConfig{Environment: "staging"},
		}).Observability
		require.Empty(t, obs.LogsExport.Type, "LogsExport.Type should be empty when absent")
		require.Empty(t, obs.LogsExport.Endpoint, "LogsExport.Endpoint should be empty when absent")
	})
}

// TestRunConfigFromProto_TransportIsGRPC pins Transport.Type, which the
// translate layer sets rather than copies. Sandbox identity, rule-of-two
// ask-upstream and batch dispatch all depend on it being "grpc".
func TestRunConfigFromProto_TransportIsGRPC(t *testing.T) {
	require.Equal(t, "grpc", runConfigFromProto(&pb.RunConfig{}).Transport.Type)
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
	require.NoError(t, types.ValidateRunConfig(&rc),
		"wire-delivered sandbox identity config rejected by validation")
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
	require.NoError(t, err, "NewStruct")
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
	require.NoError(t, err, "marshal")
	var decoded pb.RunConfig
	require.NoError(t, proto.Unmarshal(raw, &decoded), "unmarshal")

	rc := runConfigFromProto(&decoded)
	require.Len(t, rc.Tools.ControlPlane, 2)

	first := rc.Tools.ControlPlane[0]
	assert.Equal(t, "search_memory", first.Name, "first entry name")
	assert.Equal(t, "Search long-term memory.", first.Description, "first entry description")
	assert.Equal(t, 30, first.TimeoutSeconds, "first entry timeout")
	assert.True(t, first.RequiresApproval, "first entry requires approval")

	var got map[string]any
	require.NoError(t, json.Unmarshal(first.InputSchema, &got), "inputSchema is not a JSON object: %s", first.InputSchema)
	assert.Equal(t, "object", got["type"], "inputSchema.type")
	props, _ := got["properties"].(map[string]any)
	assert.Contains(t, props, "query", "inputSchema.properties")
	req, _ := got["required"].([]any)
	assert.Equal(t, []any{"query"}, req, "inputSchema.required")

	second := rc.Tools.ControlPlane[1]
	assert.Nil(t, second.InputSchema, "unset input_schema maps to nil")
	assert.Zero(t, second.TimeoutSeconds, "second entry timeout default")
	assert.False(t, second.RequiresApproval, "second entry approval default")
}

// TestRunConfigFromProto_ProtectedWriteTargetsPreserved pins
// ToolsConfig.protected_write_targets across a wire round-trip and checks
// the translated slice does not alias the proto message.
func TestRunConfigFromProto_ProtectedWriteTargetsPreserved(t *testing.T) {
	original := &pb.RunConfig{
		Tools: &pb.ToolsConfig{
			ProtectedWriteTargets: []string{"AGENTS.md", "config/policies/"},
		},
	}
	raw, err := proto.Marshal(original)
	require.NoError(t, err, "marshal")
	var decoded pb.RunConfig
	require.NoError(t, proto.Unmarshal(raw, &decoded), "unmarshal")

	rc := runConfigFromProto(&decoded)
	want := []string{"AGENTS.md", "config/policies/"}
	require.Equal(t, want, rc.Tools.ProtectedWriteTargets)

	decoded.Tools.ProtectedWriteTargets[0] = "mutated"
	assert.Equal(t, "AGENTS.md", rc.Tools.ProtectedWriteTargets[0],
		"translated ProtectedWriteTargets must not alias the proto's backing data")

	empty := runConfigFromProto(&pb.RunConfig{Tools: &pb.ToolsConfig{}})
	assert.Nil(t, empty.Tools.ProtectedWriteTargets, "unset protected_write_targets maps to nil")
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

	require.NotNil(t, rc.MaxCostBudget, "MaxCostBudget dropped in translation")
	assert.InDelta(t, budget, *rc.MaxCostBudget, 1e-9)

	assert.Nil(t, runConfigFromProto(&pb.RunConfig{}).MaxCostBudget, "unset max_cost_budget maps to nil")
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
	require.NoError(t, err, "marshal")
	var decoded pb.RunTrace
	require.NoError(t, proto.Unmarshal(raw, &decoded), "unmarshal")

	g := decoded.GetGit()
	assert.Equal(t, "stirrup/run-handoff", g.GetBranch(), "git branch")
	assert.Equal(t, "abc123", g.GetSha(), "git sha")
	assert.Equal(t, "def456", g.GetBaseSha(), "git base sha")
	assert.True(t, g.GetDirty(), "git dirty")
	assert.Equal(t, "dirty tree", g.GetError(), "git error")

	r := decoded.GetWorkspaceExport()
	assert.Equal(t, "gs://bucket/ws.tar.gz", r.GetUri(), "workspace export uri")
	assert.Equal(t, "failed", r.GetStatus(), "workspace export status")
	assert.True(t, r.GetRequired(), "workspace export required")
	assert.Equal(t, "upload refused", r.GetError(), "workspace export error")
	assert.EqualValues(t, 2048, r.GetBytes(), "workspace export bytes")
	assert.Equal(t, "0123abcd", r.GetSha256(), "workspace export sha256")
	assert.EqualValues(t, 7, r.GetFiles(), "workspace export files")

	assert.Equal(t, "workspace export: upload refused", decoded.GetError(), "run error")

	bare := runTraceToProto(&types.RunTrace{Outcome: "success"})
	assert.Nil(t, bare.Git, "bare summary has no git handoff")
	assert.Nil(t, bare.WorkspaceExport, "bare summary has no workspace export")
	assert.Empty(t, bare.Error, "bare summary has no error")
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
	size := proto.Size(runTraceToProto(tr))
	require.LessOrEqual(t, size, 16<<10, "summary size in bytes")
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
			require.NoError(t, err, "marshal")
			var decoded pb.RunConfig
			require.NoError(t, proto.Unmarshal(raw, &decoded), "unmarshal")

			rc := runConfigFromProto(&decoded)
			got := rc.Executor.WorkspaceExportRequired
			if in == nil {
				require.Nil(t, got, "unset must translate to nil")
			} else {
				require.NotNil(t, got, "explicit value must translate")
				require.Equal(t, *in, *got)
			}
			wantRequired := in == nil || *in
			require.Equal(t, wantRequired, rc.Executor.ExportRequired(), "ExportRequired()")
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
	require.NoError(t, err, "NewStruct")
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
	require.NoError(t, err, "marshal")
	var decoded pb.RunConfig
	require.NoError(t, proto.Unmarshal(raw, &decoded), "unmarshal")
	rc := runConfigFromProto(&decoded)

	var got map[string]any
	require.NoError(t, json.Unmarshal(rc.OutputSchema, &got), "OutputSchema is not JSON: %s", rc.OutputSchema)
	want := map[string]any{
		"type":     "object",
		"required": []any{"title"},
		"properties": map[string]any{
			"title": map[string]any{"type": "string", "maxLength": float64(72)},
			"label": map[string]any{"$ref": "#/$defs/label"},
		},
		"$defs": map[string]any{"label": map[string]any{"enum": []any{"bug", "feature"}}},
	}
	require.Equal(t, want, got, "OutputSchema")
	require.Contains(t, string(rc.OutputSchema), `"maxLength":72`,
		"integer keyword must render as an integral JSON number, got %s", rc.OutputSchema)
	require.NoError(t, types.ValidateRunConfig(&rc), "translated schema must pass validation")
}

func TestRunConfigFromProto_OutputSchemaAbsentStaysNil(t *testing.T) {
	rc := runConfigFromProto(&pb.RunConfig{RunId: "run-free-text"})
	assert.Nil(t, rc.OutputSchema, "OutputSchema is nil when the field is unset")
}

func TestRunConfigFromProto_UnrenderableOutputSchemaIsRejected(t *testing.T) {
	schema := &structpb.Struct{Fields: map[string]*structpb.Value{
		"maximum": structpb.NewNumberValue(math.NaN()),
	}}
	rc := runConfigFromProto(&pb.RunConfig{OutputSchema: schema})
	require.Equal(t, "null", string(rc.OutputSchema), "a schema protojson cannot render translates to JSON null")

	c := types.RunConfig{OutputSchema: rc.OutputSchema}
	err := types.ValidateRunConfig(&c)
	require.ErrorContains(t, err, "outputSchema must be a JSON Schema object")
}

func TestRunTraceToProto_StructuredOutputRoundTrip(t *testing.T) {
	doc := `{"title":"Fix the flaky test","labels":["bug"],"risk":0.25}`
	raw, err := proto.Marshal(runTraceToProto(&types.RunTrace{
		Outcome:          "success",
		StructuredOutput: json.RawMessage(doc),
	}))
	require.NoError(t, err, "marshal")
	var decoded pb.RunTrace
	require.NoError(t, proto.Unmarshal(raw, &decoded), "unmarshal")
	assert.Equal(t, doc, decoded.GetStructuredOutput(), "structured_output")

	bare := runTraceToProto(&types.RunTrace{Outcome: "success"})
	assert.Empty(t, bare.StructuredOutput, "structured_output without a structured answer")
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
	require.NoError(t, err, "marshal")
	var decoded pb.RunConfig
	require.NoError(t, proto.Unmarshal(raw, &decoded), "unmarshal")

	got := runConfigFromProto(&decoded).Tools.CommandOutput
	require.NotNil(t, got.Enabled, "Enabled must stay explicit")
	require.False(t, *got.Enabled, "Enabled must stay explicitly false")
	want := types.CommandOutputConfig{
		Enabled:               got.Enabled,
		FailurePosture:        types.CommandOutputPostureBestEffort,
		InlineMaxBytes:        1 << 14,
		PreviewBytesPerStream: 1 << 11,
		MaxBytesPerStream:     1 << 24,
		MaxBytesPerRun:        1 << 26,
	}
	assert.Equal(t, want, got, "CommandOutput")

	*decoded.Tools.CommandOutput.Enabled = true
	assert.False(t, *got.Enabled, "translated Enabled must not alias the proto message's bool")
}

// TestRunConfigFromProto_CommandOutputUnsetKeepsDefaults pins that an
// absent command_output leaves the zero value, so capture stays enabled
// with the documented defaults.
func TestRunConfigFromProto_CommandOutputUnsetKeepsDefaults(t *testing.T) {
	rc := runConfigFromProto(&pb.RunConfig{Tools: &pb.ToolsConfig{BuiltIn: []string{"run_command"}}})
	assert.Equal(t, types.CommandOutputConfig{}, rc.Tools.CommandOutput, "CommandOutput is the zero value")
	assert.True(t, rc.Tools.CommandOutputCaptureEnabled(), "capture defaults to enabled when command_output is unset")
}

// TestRunConfigFromProto_MaxResultBytesPreserved pins the tools.maxResultBytes
// mirror through the wire format.
func TestRunConfigFromProto_MaxResultBytesPreserved(t *testing.T) {
	raw, err := proto.Marshal(&pb.RunConfig{Tools: &pb.ToolsConfig{MaxResultBytes: 65536}})
	require.NoError(t, err, "marshal")
	var decoded pb.RunConfig
	require.NoError(t, proto.Unmarshal(raw, &decoded), "unmarshal")
	require.Equal(t, 65536, runConfigFromProto(&decoded).Tools.MaxResultBytes, "MaxResultBytes")
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
	require.NoError(t, err, "marshal")
	var decoded pb.RunConfig
	require.NoError(t, proto.Unmarshal(raw, &decoded), "unmarshal")

	rc := runConfigFromProto(&decoded)
	require.Len(t, rc.Tools.MCPServers, 2)
	assert.False(t, rc.Tools.MCPServers[0].Optional, "a server with no optional field translates as required")
	assert.True(t, rc.Tools.MCPServers[1].Optional, "Optional survives translation")
}

func TestRunTraceToProto_DegradedCapabilities(t *testing.T) {
	pt := runTraceToProto(&types.RunTrace{
		ID: "run-degraded",
		DegradedCapabilities: []types.DegradedCapability{
			{Kind: types.DegradedCapabilityMCP, Name: "docs", Error: "connection refused"},
		},
	})

	raw, err := proto.Marshal(pt)
	require.NoError(t, err, "marshal")
	var decoded pb.RunTrace
	require.NoError(t, proto.Unmarshal(raw, &decoded), "unmarshal")
	require.Len(t, decoded.DegradedCapabilities, 1)
	got := decoded.DegradedCapabilities[0]
	assert.Equal(t, "mcp", got.Kind, "degraded kind")
	assert.Equal(t, "docs", got.Name, "degraded name")
	assert.Equal(t, "connection refused", got.Error, "degraded error")

	empty := runTraceToProto(&types.RunTrace{ID: "run-clean"})
	assert.Empty(t, empty.DegradedCapabilities, "a run with nothing degraded carries no entries")
}

// TestRunConfigFromProto_RepositoryGuidancePreserved pins every
// RepositoryGuidanceConfig field across a wire round-trip and checks the
// translated slice and map do not alias the proto-owned values.
func TestRunConfigFromProto_RepositoryGuidancePreserved(t *testing.T) {
	digest := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	original := &pb.RunConfig{
		RepositoryGuidance: &pb.RepositoryGuidanceConfig{
			Files:        []string{"AGENTS.md", "CLAUDE.md"},
			MaxBytes:     4096,
			PinnedSha256: map[string]string{"AGENTS.md": digest},
		},
	}

	raw, err := proto.Marshal(original)
	require.NoError(t, err, "marshal")
	var decoded pb.RunConfig
	require.NoError(t, proto.Unmarshal(raw, &decoded), "unmarshal")

	rg := runConfigFromProto(&decoded).RepositoryGuidance
	require.NotNil(t, rg, "RepositoryGuidance must survive translation")
	want := &types.RepositoryGuidanceConfig{
		Files:        []string{"AGENTS.md", "CLAUDE.md"},
		MaxBytes:     4096,
		PinnedSHA256: map[string]string{"AGENTS.md": digest},
	}
	require.Equal(t, want, rg)

	decoded.RepositoryGuidance.Files[0] = "mutated"
	decoded.RepositoryGuidance.PinnedSha256["AGENTS.md"] = "mutated"
	assert.Equal(t, "AGENTS.md", rg.Files[0], "RepositoryGuidance.Files must not share the proto-owned slice")
	assert.Equal(t, digest, rg.PinnedSHA256["AGENTS.md"], "RepositoryGuidance.PinnedSHA256 must not share the proto-owned map")
}

// TestRunConfigFromProto_CorrelationPreserved pins every CorrelationConfig
// field across a wire round-trip and checks the translated label map does
// not alias the proto-owned map.
func TestRunConfigFromProto_CorrelationPreserved(t *testing.T) {
	original := &pb.RunConfig{
		Correlation: &pb.CorrelationConfig{
			SessionId: "session-42",
			TaskId:    "task-7",
			AttemptId: "attempt-3",
			UserId:    "user-9f2c",
			Url:       "https://cp.example.com/runs/42",
			Labels:    map[string]string{"agent_profile": "reviewer", "execution_mode": "batch"},
		},
	}

	raw, err := proto.Marshal(original)
	require.NoError(t, err, "marshal")
	var decoded pb.RunConfig
	require.NoError(t, proto.Unmarshal(raw, &decoded), "unmarshal")

	got := runConfigFromProto(&decoded).Correlation
	want := &types.CorrelationConfig{
		SessionID: "session-42",
		TaskID:    "task-7",
		AttemptID: "attempt-3",
		UserID:    "user-9f2c",
		URL:       "https://cp.example.com/runs/42",
		Labels:    map[string]string{"agent_profile": "reviewer", "execution_mode": "batch"},
	}
	require.Equal(t, want, got)

	decoded.Correlation.Labels["agent_profile"] = "mutated"
	assert.Equal(t, "reviewer", got.Labels["agent_profile"], "Correlation.Labels must not share the proto-owned map")
}

func TestRunConfigFromProto_CorrelationAbsentStaysNil(t *testing.T) {
	assert.Nil(t, runConfigFromProto(&pb.RunConfig{}).Correlation, "Correlation is nil when the proto field is absent")
}

// TestRunConfigFromProto_RepositoryGuidanceAbsentStaysNil documents that an
// absent sub-message leaves guidance off rather than synthesising an empty
// block that fails validation.
func TestRunConfigFromProto_RepositoryGuidanceAbsentStaysNil(t *testing.T) {
	assert.Nil(t, runConfigFromProto(&pb.RunConfig{}).RepositoryGuidance, "RepositoryGuidance is nil when the proto field is absent")
}

// harnessEventWireRoundTrip translates e and decodes it from the wire, so a
// field the translation drops or the proto lacks reads back as zero.
func harnessEventWireRoundTrip(t *testing.T, e types.HarnessEvent) *pb.HarnessEvent {
	t.Helper()
	raw, err := proto.Marshal(harnessEventToProto(e))
	require.NoError(t, err, "marshal")
	var decoded pb.HarnessEvent
	require.NoError(t, proto.Unmarshal(raw, &decoded), "unmarshal")
	return &decoded
}

func TestHarnessEventToProto_LiveTranscriptFields(t *testing.T) {
	turn := func(n int) *int { return &n }

	t.Run("user_message", func(t *testing.T) {
		got := harnessEventWireRoundTrip(t, types.HarnessEvent{
			Type: "user_message", Text: "fix it", Source: types.UserMessageSourceFollowUpPrompt, RequestID: "req-9",
		})
		require.Equal(t, "fix it", got.GetText(), "text")
		require.Equal(t, "followup_prompt", got.GetSource(), "source")
		require.Equal(t, "req-9", got.GetRequestId(), "request_id")
	})

	t.Run("turn_started keeps turn 0 present", func(t *testing.T) {
		got := harnessEventWireRoundTrip(t, types.HarnessEvent{
			Type: "turn_started", Turn: turn(0), Model: "claude-x", Provider: "anthropic",
		})
		require.NotNil(t, got.Turn, "turn 0 must stay present")
		require.Zero(t, got.GetTurn(), "turn")
		require.Equal(t, "claude-x", got.GetModel(), "model")
		require.Equal(t, "anthropic", got.GetProvider(), "provider")
	})

	t.Run("turn_committed", func(t *testing.T) {
		got := harnessEventWireRoundTrip(t, types.HarnessEvent{
			Type: "turn_committed", Turn: turn(3), StopReason: "tool_use", ToolResultCount: 2,
			Usage: &types.TokenUsage{Input: 100, Output: 20, CacheRead: 5, CacheWrite: 6, Reasoning: 7},
		})
		require.EqualValues(t, 3, got.GetTurn(), "turn")
		require.Equal(t, "tool_use", got.GetStopReason(), "stop_reason")
		require.EqualValues(t, 2, got.GetToolResultCount(), "tool_result_count")

		u := got.GetUsage()
		require.EqualValues(t, 100, u.GetInputTokens(), "input_tokens")
		require.EqualValues(t, 20, u.GetOutputTokens(), "output_tokens")
		require.EqualValues(t, 5, u.GetCacheReadTokens(), "cache_read_tokens")
		require.EqualValues(t, 6, u.GetCacheWriteTokens(), "cache_write_tokens")
		require.EqualValues(t, 7, u.GetReasoningTokens(), "reasoning_tokens")
	})

	t.Run("turn_abandoned", func(t *testing.T) {
		got := harnessEventWireRoundTrip(t, types.HarnessEvent{
			Type: "turn_abandoned", Turn: turn(0), AbandonReason: types.TurnAbandonedUnansweredToolCalls,
		})
		require.NotNil(t, got.Turn, "turn 0 must stay present")
		require.Zero(t, got.GetTurn(), "turn")
		require.Equal(t, types.TurnAbandonedUnansweredToolCalls, got.GetAbandonReason(), "abandon_reason")
	})

	t.Run("tool_result", func(t *testing.T) {
		got := harnessEventWireRoundTrip(t, types.HarnessEvent{
			Type: "tool_result", ToolUseID: "tu-1", Name: "read_file", Content: "boom", IsError: true, DurationMs: 42,
		})
		require.Equal(t, "tu-1", got.GetToolUseId(), "tool_use_id")
		require.Equal(t, "read_file", got.GetName(), "name")
		require.True(t, got.GetIsError(), "is_error")
		require.EqualValues(t, 42, got.GetDurationMs(), "duration_ms")
	})

	t.Run("unset turn and usage stay absent", func(t *testing.T) {
		got := harnessEventWireRoundTrip(t, types.HarnessEvent{Type: "text_delta", Text: "x"})
		require.Nil(t, got.Turn, "turn")
		require.Nil(t, got.Usage, "usage")
	})
}

func TestTraceReferenceToProto_RoundTrip(t *testing.T) {
	ref := &types.TraceReference{
		TraceID:   "4bf92f3577b34da6a3ce929d0e0e4736",
		RunSpanID: "00f067aa0ba902b7",
		Sampled:   true,
	}
	requireRef := func(t *testing.T, got *pb.TraceReference) {
		t.Helper()
		require.NotNil(t, got, "trace_reference")
		require.Equal(t, ref.TraceID, got.GetTraceId(), "trace_id")
		require.Equal(t, ref.RunSpanID, got.GetRunSpanId(), "run_span_id")
		require.Equal(t, ref.Sampled, got.GetSampled(), "sampled")
	}

	t.Run("run_started event", func(t *testing.T) {
		var decoded pb.HarnessEvent
		wireRoundTrip(t, harnessEventToProto(types.HarnessEvent{
			Type: "run_started", ID: "run-ref", TraceReference: ref,
		}), &decoded)
		require.Equal(t, "run_started", decoded.GetType())
		require.Equal(t, "run-ref", decoded.GetId())
		requireRef(t, decoded.GetTraceReference())
	})

	t.Run("done summary", func(t *testing.T) {
		var decoded pb.RunTrace
		wireRoundTrip(t, runTraceToProto(&types.RunTrace{Outcome: "success", TraceReference: ref}), &decoded)
		requireRef(t, decoded.GetTraceReference())
	})

	t.Run("unsampled reference keeps sampled false", func(t *testing.T) {
		got := traceReferenceToProto(&types.TraceReference{TraceID: ref.TraceID, RunSpanID: ref.RunSpanID})
		require.NotNil(t, got, "trace_reference")
		require.False(t, got.GetSampled(), "sampled")
	})

	t.Run("absent stays absent", func(t *testing.T) {
		pe := harnessEventToProto(types.HarnessEvent{Type: "text_delta"})
		require.Nil(t, pe.TraceReference, "event trace_reference")
		pt := runTraceToProto(&types.RunTrace{Outcome: "success"})
		require.Nil(t, pt.TraceReference, "summary trace_reference")
	})
}

func TestTraceContextToProto_ToolResultRequestCarriesTraceContext(t *testing.T) {
	carrier := &types.TraceContext{
		Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		Tracestate:  "vendor=opaque",
	}
	var decoded pb.HarnessEvent
	wireRoundTrip(t, harnessEventToProto(types.HarnessEvent{
		Type: "tool_result_request", RequestID: "req-1", ToolUseID: "tc_1", TraceContext: carrier,
	}), &decoded)
	got := decoded.GetTraceContext()
	require.Equal(t, carrier.Traceparent, got.GetTraceparent(), "traceparent")
	require.Equal(t, carrier.Tracestate, got.GetTracestate(), "tracestate")
	require.Equal(t, "req-1", decoded.GetRequestId())
	require.Equal(t, "tc_1", decoded.GetToolUseId())

	pe := harnessEventToProto(types.HarnessEvent{Type: "tool_result_request"})
	require.Nil(t, pe.TraceContext, "absent trace context")
}

func TestTraceContextFromProto_TaskAssignmentCarriesTraceContext(t *testing.T) {
	var decoded pb.ControlEvent
	wireRoundTrip(t, &pb.ControlEvent{
		Type: "task_assignment",
		Task: &pb.RunConfig{RunId: "run-parented"},
		TraceContext: &pb.TraceContext{
			Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			Tracestate:  "vendor=opaque",
		},
	}, &decoded)
	e := controlEventFromProto(&decoded)
	want := types.TraceContext{
		Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		Tracestate:  "vendor=opaque",
	}
	require.NotNil(t, e.TraceContext, "trace context")
	require.Equal(t, want, *e.TraceContext)
	require.NotNil(t, e.Task, "task")
	require.Equal(t, "run-parented", e.Task.RunID)

	absent := controlEventFromProto(&pb.ControlEvent{Type: "task_assignment"})
	require.Nil(t, absent.TraceContext, "absent trace context")
}

func TestHarnessEventToProto_ReadyCapabilities(t *testing.T) {
	decoded := harnessEventWireRoundTrip(t, types.HarnessEvent{
		Type: "ready", HarnessVersion: "dev", Capabilities: types.HarnessCapabilities(),
	})
	want := types.HarnessCapabilities()
	require.Equal(t, want, decoded.GetCapabilities(), "capabilities")

	bare := harnessEventWireRoundTrip(t, types.HarnessEvent{Type: "ready"})
	require.Empty(t, bare.GetCapabilities(), "ready without capabilities")
}
