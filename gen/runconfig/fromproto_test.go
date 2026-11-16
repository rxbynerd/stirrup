package runconfig

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

// wireRoundTrip encodes in and decodes the bytes into out, so a test reads
// values that have passed through the wire format.
func wireRoundTrip(t *testing.T, in, out proto.Message) {
	t.Helper()
	must := require.New(t)
	raw, err := proto.Marshal(in)
	must.NoError(err, "marshal")
	must.NoError(proto.Unmarshal(raw, out), "unmarshal")
}

// TestRunConfigFromProto_SessionNamePropagates ensures SessionName set on
// the proto RunConfig is copied into the internal types.RunConfig.
func TestRunConfigFromProto_SessionNamePropagates(t *testing.T) {
	must := require.New(t)
	name := "nightly-eval"

	rc := runConfigFromProto(&pb.RunConfig{SessionName: &name})

	must.Equal("nightly-eval", rc.SessionName)
}

// TestRunConfigFromProto_PromptBuilderFieldsPreserved confirms
// prompt_builder.template and prompt_builder.prompt_model survive
// translation.
func TestRunConfigFromProto_PromptBuilderFieldsPreserved(t *testing.T) {
	must := require.New(t)
	pc := &pb.RunConfig{
		PromptBuilder: &pb.PromptBuilderConfig{
			Type:        "composed",
			Template:    `Custom agent.{{if eq .Tier "frontier"}} Act when ready.{{end}}`,
			PromptModel: "claude-fable-5",
		},
	}

	rc := runConfigFromProto(pc)

	must.Equal("composed", rc.PromptBuilder.Type)
	must.Equal(pc.PromptBuilder.Template, rc.PromptBuilder.Template)
	must.Equal("claude-fable-5", rc.PromptBuilder.PromptModel)
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
		must := require.New(t)
		wire, err := proto.Marshal(&pb.RuleOfTwoConfig{})
		must.NoError(err)

		var decoded pb.RuleOfTwoConfig
		must.NoError(proto.Unmarshal(wire, &decoded))
		must.Nil(decoded.Enforce)

		rc := runConfigFromProto(&pb.RunConfig{RuleOfTwo: &decoded})
		must.NotNil(rc.RuleOfTwo, "RuleOfTwo must be set when the proto sub-message is present")
		must.Nil(rc.RuleOfTwo.Enforce)
	})

	t.Run("explicit false round-trips as &false", func(t *testing.T) {
		must := require.New(t)
		enforce := false
		wire, err := proto.Marshal(&pb.RuleOfTwoConfig{Enforce: &enforce})
		must.NoError(err)

		var decoded pb.RuleOfTwoConfig
		must.NoError(proto.Unmarshal(wire, &decoded))
		must.NotNil(decoded.Enforce)
		must.False(*decoded.Enforce)
	})

	t.Run("explicit true round-trips as &true", func(t *testing.T) {
		must := require.New(t)
		enforce := true
		wire, err := proto.Marshal(&pb.RuleOfTwoConfig{Enforce: &enforce})
		must.NoError(err)

		var decoded pb.RuleOfTwoConfig
		must.NoError(proto.Unmarshal(wire, &decoded))
		must.NotNil(decoded.Enforce)
		must.True(*decoded.Enforce)
	})
}

// TestRuleOfTwoProto_RuntimeTranslation pins the runtime sub-message
// translation. The nil-vs-present distinction matters the same way it
// does for enforce: an absent proto Runtime must stay nil on the
// internal config so the factory's default arming applies, while a
// present-but-empty one must surface as a present-but-empty block.
func TestRuleOfTwoProto_RuntimeTranslation(t *testing.T) {
	t.Run("absent runtime stays nil", func(t *testing.T) {
		must := require.New(t)
		rc := runConfigFromProto(&pb.RunConfig{RuleOfTwo: &pb.RuleOfTwoConfig{}})

		must.NotNil(rc.RuleOfTwo)
		must.Nil(rc.RuleOfTwo.Runtime)
	})

	t.Run("empty runtime surfaces as present-but-empty", func(t *testing.T) {
		must := require.New(t)
		rc := runConfigFromProto(&pb.RunConfig{
			RuleOfTwo: &pb.RuleOfTwoConfig{Runtime: &pb.RuleOfTwoRuntimeConfig{}},
		})

		must.NotNil(rc.RuleOfTwo)
		must.NotNil(rc.RuleOfTwo.Runtime)
		must.Empty(rc.RuleOfTwo.Runtime.Classifier)
		must.Empty(rc.RuleOfTwo.Runtime.OnDetect)
		must.Nil(rc.RuleOfTwo.Runtime.GuardCriteria)
	})

	t.Run("populated runtime translates fields", func(t *testing.T) {
		must := require.New(t)
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
		must.NotNil(rt)
		must.Equal("patterns", rt.Classifier)
		must.Equal("block-external", rt.OnDetect)
		must.Equal([]string{"sensitive_data", "pii"}, rt.GuardCriteria)

		// GuardCriteria must be copied, not aliased, so mutating the
		// source after translation cannot reach the translated copy.
		src.RuleOfTwo.Runtime.GuardCriteria[0] = "mutated"
		must.Equal("sensitive_data", rt.GuardCriteria[0], "GuardCriteria shares backing array with proto source")
	})
}

// TestRunConfigFromProto_TranslatesNewSafetyFields confirms
// executor.runtime, ruleOfTwo, codeScanner, and
// permissionPolicy.policy_file/fallback populate the internal RunConfig.
func TestRunConfigFromProto_TranslatesNewSafetyFields(t *testing.T) {
	must := require.New(t)
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

	must.Equal("runsc", rc.Executor.Runtime)
	must.Equal("/etc/stirrup/policy.cedar", rc.PermissionPolicy.PolicyFile)
	must.Equal("deny-side-effects", rc.PermissionPolicy.Fallback)
	must.NotNil(rc.RuleOfTwo)
	must.NotNil(rc.RuleOfTwo.Enforce)
	must.False(*rc.RuleOfTwo.Enforce)
	must.NotNil(rc.CodeScanner)
	must.Equal("composite", rc.CodeScanner.Type)
	must.Len(rc.CodeScanner.Scanners, 2)
	must.True(rc.CodeScanner.BlockOnWarn)
}

// TestRunConfigFromProto_TranslatesK8sExecutorFields pins that the k8s
// executor's namespace, kubeconfig, nodeSelector, and serviceAccount
// fields survive the gRPC translate layer.
func TestRunConfigFromProto_TranslatesK8sExecutorFields(t *testing.T) {
	must := require.New(t)
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

	must.Equal("k8s", rc.Executor.Type)
	must.Equal("gvisor", rc.Executor.Runtime)
	must.Equal("agents", rc.Executor.K8sNamespace)
	must.Equal("/home/u/.kube/config", rc.Executor.K8sKubeconfig)
	must.Equal("agent-sa", rc.Executor.K8sServiceAccount)
	must.Equal("ssd", rc.Executor.K8sNodeSelector["disktype"])
}

// TestRunConfigFromProto_TraceEmitterGCSFieldsPreserved pins that the
// gcs trace emitter's Bucket and ObjectPrefix survive translation.
func TestRunConfigFromProto_TraceEmitterGCSFieldsPreserved(t *testing.T) {
	must := require.New(t)
	pc := &pb.RunConfig{
		TraceEmitter: &pb.TraceEmitterConfig{
			Type:         "gcs",
			Bucket:       "stirrup-results",
			ObjectPrefix: "traces/",
		},
	}

	rc := runConfigFromProto(pc)

	must.Equal("gcs", rc.TraceEmitter.Type)
	must.Equal("stirrup-results", rc.TraceEmitter.Bucket)
	must.Equal("traces/", rc.TraceEmitter.ObjectPrefix)
}

// TestRunConfigFromProto_SessionNameAbsentWhenNil documents the safe default:
// when the proto omits SessionName (the field is a proto3 optional, so the
// zero value is a nil pointer), the internal RunConfig must surface the empty
// string rather than panicking on a nil dereference.
func TestRunConfigFromProto_SessionNameAbsentWhenNil(t *testing.T) {
	must := require.New(t)
	rc := runConfigFromProto(&pb.RunConfig{})

	must.Empty(rc.SessionName)
	must.Nil(rc.RuleOfTwo)
	must.Nil(rc.CodeScanner)

	rcUnset := runConfigFromProto(&pb.RunConfig{RuleOfTwo: &pb.RuleOfTwoConfig{}})
	must.NotNil(rcUnset.RuleOfTwo)
	must.Nil(rcUnset.RuleOfTwo.Enforce)
}

// TestRunConfigFromProto_GuardRailFieldsPreserved asserts that every field on
// the proto GuardRailConfig is preserved by runConfigFromProto, including
// the recursive Stages case.
func TestRunConfigFromProto_GuardRailFieldsPreserved(t *testing.T) {
	must := require.New(t)
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

	must.NotNil(rc.GuardRail, "GuardRail dropped during proto translation")
	must.Equal("composite", rc.GuardRail.Type)
	must.Equal([]string{"pre_turn", "post_turn"}, rc.GuardRail.Phases)
	must.Equal(1500, rc.GuardRail.TimeoutMs)
	must.True(rc.GuardRail.FailOpen)
	must.Equal(256, rc.GuardRail.MinChunkChars)
	must.Len(rc.GuardRail.Stages, 3)

	gg := rc.GuardRail.Stages[0]
	must.Equal("granite-guardian", gg.Type)
	must.Equal("http://classifier.local:9999", gg.Endpoint)
	must.Equal("ibm-granite/granite-guardian-4.1-8b", gg.Model)
	must.Equal([]string{"harm", "jailbreak"}, gg.Criteria)
	must.Equal("no profanity", gg.CustomCriteria["my_rule"])
	must.NotNil(gg.Think)
	must.True(*gg.Think)

	cj := rc.GuardRail.Stages[1]
	must.Equal("cloud-judge", cj.Type)
	must.Equal("claude-haiku-5-5", cj.Model)
	must.Equal(5000, cj.TimeoutMs)

	ss := rc.GuardRail.Stages[2]
	must.Equal("shieldstral", ss.Type)
	must.Equal("https://api.mistral.ai", ss.Endpoint)
	must.Equal("mistralai/Shieldstral-1.0-3B", ss.Model)
	must.Equal("secret://MISTRAL_API_KEY", ss.APIKeyRef)
	must.Equal(3000, ss.TimeoutMs)
}

func TestRunConfigProtoRoundTrip_ClearToolResultsFields(t *testing.T) {
	must := require.New(t)
	keep, clearAtLeast := int32(0), int32(20000)
	original := &pb.RunConfig{ContextStrategy: &pb.ContextStrategyConfig{
		Type:         "clear-tool-results",
		MaxTokens:    100000,
		Keep:         &keep,
		ClearAtLeast: &clearAtLeast,
		ExcludeTools: []string{"run_command", "web_fetch"},
	}}
	wire, err := proto.Marshal(original)
	must.NoError(err)

	var decoded pb.RunConfig
	must.NoError(proto.Unmarshal(wire, &decoded))

	got := runConfigFromProto(&decoded).ContextStrategy
	must.Equal("clear-tool-results", got.Type)
	must.Equal(100000, got.MaxTokens)
	must.NotNil(got.Keep, "keep must survive as an explicit 0, not unset")
	must.Equal(0, *got.Keep)
	must.NotNil(got.ClearAtLeast)
	must.Equal(20000, *got.ClearAtLeast)
	must.Equal([]string{"run_command", "web_fetch"}, got.ExcludeTools)

	decoded.ContextStrategy.ExcludeTools[0] = "mutated"
	must.Equal("run_command", got.ExcludeTools[0], "ExcludeTools aliases the proto slice")
}

func TestRunConfigFromProto_ClearToolResultsFieldsAbsentStayUnset(t *testing.T) {
	must := require.New(t)
	got := runConfigFromProto(&pb.RunConfig{ContextStrategy: &pb.ContextStrategyConfig{Type: "sliding-window"}}).ContextStrategy

	must.Nil(got.Keep)
	must.Nil(got.ClearAtLeast)
	must.Nil(got.ExcludeTools)
}

// TestRunConfigFromProto_GuardRailAbsentStaysNil documents that an
// absent guard_rail proto field translates to a nil GuardRail (the
// validator then applies the default — guards are opt-in).
func TestRunConfigFromProto_GuardRailAbsentStaysNil(t *testing.T) {
	must := require.New(t)
	rc := runConfigFromProto(&pb.RunConfig{})

	must.Nil(rc.GuardRail)
}

// TestRunConfigFromProto_ObservabilityPropagates ensures an
// ObservabilityConfig set on the proto RunConfig is copied into the
// internal types.RunConfig.
func TestRunConfigFromProto_ObservabilityPropagates(t *testing.T) {
	must := require.New(t)
	pc := &pb.RunConfig{
		Observability: &pb.ObservabilityConfig{
			ServiceName:      "agent-a",
			Environment:      "staging",
			ServiceNamespace: "eval",
		},
	}

	rc := runConfigFromProto(pc)

	must.Equal("agent-a", rc.Observability.ServiceName)
	must.Equal("staging", rc.Observability.Environment)
	must.Equal("eval", rc.Observability.ServiceNamespace)
}

// TestRunConfigFromProto_ObservabilityAbsentWhenNil documents the safe
// default: when the proto omits the Observability sub-message, the internal
// RunConfig surfaces a zero-value ObservabilityConfig rather than panicking
// on a nil dereference.
func TestRunConfigFromProto_ObservabilityAbsentWhenNil(t *testing.T) {
	must := require.New(t)
	rc := runConfigFromProto(&pb.RunConfig{})

	must.Empty(rc.Observability.Environment)
	must.Empty(rc.Observability.ServiceNamespace)
}

// TestRunConfigFromProto_AzureWIFFieldsPreserved guards the Azure
// Workload Identity Federation fields (azure_tenant_id, azure_client_id,
// azure_scope) against silent drop in credentialConfigFromProto.
func TestRunConfigFromProto_AzureWIFFieldsPreserved(t *testing.T) {
	must := require.New(t)
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
	must.NotNil(cred, "Credential dropped during proto translation")
	must.Equal("azure-workload-identity", cred.Type)
	must.Equal("11111111-2222-3333-4444-555555555555", cred.AzureTenantID)
	must.Equal("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", cred.AzureClientID)
	must.Equal("https://cognitiveservices.azure.com/.default", cred.AzureScope)
	must.Equal("https://login.microsoftonline.us/11111111-2222-3333-4444-555555555555/oauth2/v2.0/token", cred.AzureTokenURL)
	must.NotNil(cred.TokenSource, "TokenSource dropped during proto translation")
	must.Equal("/var/run/secrets/azure/token", cred.TokenSource.Path)
}

// TestRunConfigFromProto_CredentialWIFFieldsPreserved guards the GCP
// credential federation proto fields (audience, serviceAccount, and the
// nested TokenSourceConfig's resource/clientId) against silent drop.
func TestRunConfigFromProto_CredentialWIFFieldsPreserved(t *testing.T) {
	must := require.New(t)
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
	must.NotNil(cred, "Credential dropped during proto translation")
	must.Equal("gcp-workload-identity-federation", cred.Type)
	must.Equal("//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/azure-pool/providers/azure-provider", cred.Audience)
	must.Equal("vertex@my-project.iam.gserviceaccount.com", cred.ServiceAccount)

	ts := cred.TokenSource
	must.NotNil(ts, "TokenSource dropped during proto translation")
	must.Equal("azure-imds", ts.Type)
	must.Equal("api://AzureADTokenExchange", ts.Resource)
	must.Equal("11111111-1111-1111-1111-111111111111", ts.ClientID)
}

// TestRunConfigFromProto_AnthropicWIFFieldsPreserved guards the four
// Anthropic Workload Identity Federation proto fields against silent
// drop in credentialConfigFromProto.
func TestRunConfigFromProto_AnthropicWIFFieldsPreserved(t *testing.T) {
	must := require.New(t)
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
	must.NotNil(cred, "Credential dropped during proto translation")
	must.Equal("anthropic-wif", cred.Type)
	must.Equal("fdrl_abc123", cred.FederationRuleID)
	must.Equal("550e8400-e29b-41d4-a716-446655440000", cred.OrganizationID)
	must.Equal("svac_xyz789", cred.ServiceAccountID)
	must.Equal("default", cred.WorkspaceID)
}

// TestRunConfigFromProto_OpenAIWIFFieldsPreserved guards the three
// OpenAI Workload Identity Federation proto fields against silent drop
// in credentialConfigFromProto.
func TestRunConfigFromProto_OpenAIWIFFieldsPreserved(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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

	must.NotNil(rc.Provider.Credential, "Credential dropped during proto translation")
	check.Equal("openai-wif", rc.Provider.Credential.Type, "Credential.Type")
	check.Equal("idp_abc123", rc.Provider.Credential.OpenAIIdentityProviderID, "Credential.OpenAIIdentityProviderID")
	check.Equal("sa_xyz789", rc.Provider.Credential.OpenAIServiceAccountID, "Credential.OpenAIServiceAccountID")
	check.Equal("urn:ietf:params:oauth:token-type:jwt", rc.Provider.Credential.OpenAISubjectTokenType, "Credential.OpenAISubjectTokenType")
	must.NotNil(rc.Provider.Credential.TokenSource, "TokenSource dropped")
	check.Equal("https://api.openai.com/v1", rc.Provider.Credential.TokenSource.Audience, "TokenSource.Audience")
}

// TestRunConfigFromProto_TraceEmitterProtocolAndHeadersPreserved guards
// TraceEmitterConfig's Protocol and Headers fields against silent drop
// in runConfigFromProto.
func TestRunConfigFromProto_TraceEmitterProtocolAndHeadersPreserved(t *testing.T) {
	check := assert.New(t)
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

	check.Equal("http/protobuf", rc.TraceEmitter.Protocol, "TraceEmitter.Protocol")
	check.Equal("https://otlp-gateway-prod-us-east-0.grafana.net/otlp", rc.TraceEmitter.Endpoint, "TraceEmitter.Endpoint")
	check.Equal("Basic xxx", rc.TraceEmitter.Headers["Authorization"], "TraceEmitter.Headers[Authorization]")
	check.Equal("team-a", rc.TraceEmitter.Headers["X-Tenant"], "TraceEmitter.Headers[X-Tenant]")
}

// TestRunConfigFromProto_TraceEmitterMetricsTemporalityPreserved guards
// TraceEmitterConfig.MetricsTemporality against silent drop in
// runConfigFromProto.
func TestRunConfigFromProto_TraceEmitterMetricsTemporalityPreserved(t *testing.T) {
	must := require.New(t)
	rc := runConfigFromProto(&pb.RunConfig{
		TraceEmitter: &pb.TraceEmitterConfig{Type: "otel", MetricsTemporality: "delta"},
	})
	must.Equal("delta", rc.TraceEmitter.MetricsTemporality)
}

// TestRunConfigFromProto_TraceEmitterCaptureContentMaxBytesPreserved
// guards TraceEmitterConfig.CaptureContentMaxBytes against silent drop in
// runConfigFromProto.
func TestRunConfigFromProto_TraceEmitterCaptureContentMaxBytesPreserved(t *testing.T) {
	check := assert.New(t)
	rc := runConfigFromProto(&pb.RunConfig{
		TraceEmitter: &pb.TraceEmitterConfig{Type: "otel", CaptureContent: true, CaptureContentMaxBytes: 2048},
	})
	check.True(rc.TraceEmitter.CaptureContent, "TraceEmitter.CaptureContent dropped")
	check.Equal(2048, rc.TraceEmitter.CaptureContentMaxBytes, "TraceEmitter.CaptureContentMaxBytes")
}

// TestRunConfigFromProto_TraceEmitterPlatformPreserved guards
// TraceEmitterConfig.Platform and PlatformOptions against silent drop in
// runConfigFromProto.
func TestRunConfigFromProto_TraceEmitterPlatformPreserved(t *testing.T) {
	check := assert.New(t)
	rc := runConfigFromProto(&pb.RunConfig{
		TraceEmitter: &pb.TraceEmitterConfig{
			Type:            "otel",
			Platform:        "example-platform",
			PlatformOptions: map[string]string{"region": "eu-west-2", "project": "agents"},
		},
	})
	check.Equal("example-platform", rc.TraceEmitter.Platform, "TraceEmitter.Platform")
	check.Equal(map[string]string{"region": "eu-west-2", "project": "agents"}, rc.TraceEmitter.PlatformOptions, "TraceEmitter.PlatformOptions")
}

// TestRunConfigFromProto_ProviderRetryConfigPreserved guards the
// ProviderRetryConfig wire fields against silent drop. The non-nil case
// checks all four fields round-trip; the nil case checks that a
// wire-absent retry block produces a nil pointer so the validator's
// defaulter runs.
func TestRunConfigFromProto_ProviderRetryConfigPreserved(t *testing.T) {
	t.Run("non-nil retry round-trips all four fields", func(t *testing.T) {
		must := require.New(t)
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

		must.NotNil(rc.Provider.Retry, "Retry dropped during proto translation")
		must.Equal(4, rc.Provider.Retry.MaxAttempts)
		must.Equal(250, rc.Provider.Retry.InitialDelayMs)
		must.Equal(12000, rc.Provider.Retry.MaxDelayMs)
		must.Equal(60000, rc.Provider.Retry.WallClockBudgetMs)
	})

	t.Run("nil retry stays nil on the Go side", func(t *testing.T) {
		must := require.New(t)
		pc := &pb.RunConfig{
			Provider: &pb.ProviderConfig{
				Type: "openai-compatible",
			},
		}

		rc := runConfigFromProto(pc)

		must.Nil(rc.Provider.Retry, "Retry should be nil when proto omits the field")
	})
}

// TestRunConfigFromProto_ToolDispatchMaxParallelPreserved guards the
// translate hop that copies tool_dispatch.max_parallel from the wire
// format into types.RunConfig.ToolDispatch.MaxParallel.
func TestRunConfigFromProto_ToolDispatchMaxParallelPreserved(t *testing.T) {
	must := require.New(t)
	pc := &pb.RunConfig{
		ToolDispatch: &pb.ToolDispatchConfig{MaxParallel: 8},
	}

	rc := runConfigFromProto(pc)

	must.NotNil(rc.ToolDispatch, "ToolDispatch sub-config dropped: got nil, want populated")
	must.Equal(8, rc.ToolDispatch.MaxParallel)
}

// TestRunConfigFromProto_BatchProviderConfigPreserved pins that the
// proto's ProviderConfig.batch round-trips into the internal
// types.ProviderConfig.Batch with every field preserved and the
// nil/non-nil distinction on MaxWaitSeconds intact.
func TestRunConfigFromProto_BatchProviderConfigPreserved(t *testing.T) {
	t.Run("non-nil batch round-trips all six fields", func(t *testing.T) {
		must := require.New(t)
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
		must.NoError(err, "marshal")
		var decoded pb.RunConfig
		must.NoError(proto.Unmarshal(bytes, &decoded), "unmarshal")

		rc := runConfigFromProto(&decoded)

		must.NotNil(rc.Provider.Batch, "Batch dropped during proto translation")
		must.True(rc.Provider.Batch.Enabled, "Batch.Enabled")
		must.NotNil(rc.Provider.Batch.MaxWaitSeconds, "Batch.MaxWaitSeconds")
		must.Equal(3600, *rc.Provider.Batch.MaxWaitSeconds, "Batch.MaxWaitSeconds")
		must.True(rc.Provider.Batch.HarnessSidePolling, "Batch.HarnessSidePolling")
		must.True(rc.Provider.Batch.FallbackOnTimeout, "Batch.FallbackOnTimeout")
		must.True(rc.Provider.Batch.CancelBundleOnRunCancel, "Batch.CancelBundleOnRunCancel")
		must.True(rc.Provider.Batch.AllowInteractiveModes, "Batch.AllowInteractiveModes")
	})

	t.Run("nil batch stays nil on the Go side", func(t *testing.T) {
		must := require.New(t)
		pc := &pb.RunConfig{
			Provider: &pb.ProviderConfig{Type: "anthropic"},
		}

		rc := runConfigFromProto(pc)

		must.Nil(rc.Provider.Batch, "Batch should be nil when proto omits the field")
	})

	t.Run("batch present with MaxWaitSeconds unset preserves nil", func(t *testing.T) {
		must := require.New(t)
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
		must.NoError(err, "marshal")
		var decoded pb.RunConfig
		must.NoError(proto.Unmarshal(bytes, &decoded), "unmarshal")

		rc := runConfigFromProto(&decoded)
		must.NotNil(rc.Provider.Batch, "Batch should be present when proto sub-message is set")
		must.Nil(rc.Provider.Batch.MaxWaitSeconds, "MaxWaitSeconds should be nil when proto field is unset")
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
			must := require.New(t)
			pc := &pb.RunConfig{
				Provider: &pb.ProviderConfig{
					Type:          "openai-compatible",
					CompatProfile: tc.profile,
				},
			}
			rc := runConfigFromProto(pc)
			must.Equal(tc.wantProfile, rc.Provider.CompatProfile)
		})
	}
}

// TestRunConfigFromProto_EndpointProfileRoundTrip pins the wire-to-internal
// copy of ProviderConfig.EndpointProfile on the default and named providers.
// ValidateRunConfig, not the translate layer, rejects unknown values.
func TestRunConfigFromProto_EndpointProfileRoundTrip(t *testing.T) {
	for _, profile := range []string{"", "openai", "azure-openai", "bedrock-mantle", "generic", "future-profile"} {
		t.Run("profile "+profile, func(t *testing.T) {
			must := require.New(t)
			pc := &pb.RunConfig{
				Provider: &pb.ProviderConfig{Type: "openai-responses", EndpointProfile: profile},
				Providers: map[string]*pb.ProviderConfig{
					"secondary": {Type: "openai-compatible", EndpointProfile: profile},
				},
			}
			rc := runConfigFromProto(pc)
			must.Equal(profile, rc.Provider.EndpointProfile)
			must.Equal(profile, rc.Providers["secondary"].EndpointProfile)
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
			must := require.New(t)
			pc := &pb.RunConfig{Temperature: tc.set}
			rc := runConfigFromProto(pc)
			if tc.set == nil {
				must.Nil(rc.Temperature)
				return
			}
			must.NotNil(rc.Temperature)
			must.InDelta(*tc.set, *rc.Temperature, 1e-9)
		})
	}
}

// TestRunConfigFromProto_ReasoningEffortCarried pins the new
// provider-neutral reasoning knob across the wire boundary: a set value
// must survive verbatim and an absent one must stay empty (say nothing
// on the wire downstream).
func TestRunConfigFromProto_ReasoningEffortCarried(t *testing.T) {
	check := assert.New(t)
	check.Equal("high", runConfigFromProto(&pb.RunConfig{ReasoningEffort: "high"}).ReasoningEffort, "set value")
	check.Empty(runConfigFromProto(&pb.RunConfig{}).ReasoningEffort, "absent value")
}

func TestRunConfigFromProto_ReasoningSummaryCarried(t *testing.T) {
	check := assert.New(t)
	check.Equal("off", runConfigFromProto(&pb.RunConfig{ReasoningSummary: "off"}).ReasoningSummary, "set value")
	check.Empty(runConfigFromProto(&pb.RunConfig{}).ReasoningSummary, "absent value")
}

// TestRunConfigFromProto_FollowUpHistoryCarried pins follow_up_history
// across the wire boundary: without the copy a control plane asking for
// "continue" would silently get the reset default.
func TestRunConfigFromProto_FollowUpHistoryCarried(t *testing.T) {
	check := assert.New(t)
	check.Equal(types.FollowUpHistoryContinue, runConfigFromProto(&pb.RunConfig{FollowUpHistory: types.FollowUpHistoryContinue}).FollowUpHistory, "set value")
	check.Empty(runConfigFromProto(&pb.RunConfig{}).FollowUpHistory, "absent value")
}

// TestRunConfigFromProto_HeartbeatSecondsCarried pins heartbeat_seconds
// across the wire boundary: without the copy a control plane's cadence
// would silently fall back to the 30 s default.
func TestRunConfigFromProto_HeartbeatSecondsCarried(t *testing.T) {
	check := assert.New(t)
	check.Equal(15, runConfigFromProto(&pb.RunConfig{HeartbeatSeconds: 15}).HeartbeatSeconds, "set value")
	check.Zero(runConfigFromProto(&pb.RunConfig{}).HeartbeatSeconds, "absent value")
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
	check := assert.New(t)
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
	check.Equal("proj-1", cfg.GCPProject, "GCPProject")
	check.Equal("us-central1", cfg.GCPLocation, "GCPLocation")
	check.Equal("/etc/sa.json", cfg.GCPCredentialsFile, "GCPCredentialsFile")
	want := []types.GeminiSafetySetting{
		{Category: "HARM_CATEGORY_HATE_SPEECH", Threshold: "BLOCK_ONLY_HIGH"},
		{Category: "HARM_CATEGORY_HARASSMENT", Threshold: "BLOCK_NONE"},
	}
	check.Equal(want, cfg.GeminiSafetySettings, "GeminiSafetySettings")
}

// The gRPC path applies no CLI key default, so a provider with no
// api_key_ref reaches the factory with an empty reference and no
// credential block; the factory decides whether that is allowed.
func TestProviderConfigFromProto_EmptyAPIKeyRefGetsNoDefault(t *testing.T) {
	check := assert.New(t)
	cfg := providerConfigFromProto(&pb.ProviderConfig{Type: "anthropic"})
	check.Empty(cfg.APIKeyRef)
	check.Nil(cfg.Credential)
}

// TestRunConfigFromProto_HooksAbsentStaysNil pins the nil/absent case:
// a TaskAssignment with no hooks sub-message must not synthesise a
// non-nil types.HooksConfig.
func TestRunConfigFromProto_HooksAbsentStaysNil(t *testing.T) {
	must := require.New(t)
	rc := runConfigFromProto(&pb.RunConfig{})
	must.Nil(rc.Hooks, "Hooks should be nil when proto field absent")
}

// TestRunConfigFromProto_HooksEmptyStaysNonNil pins the companion case:
// an explicit-but-empty proto HooksConfig{} (e.g. a control plane that
// always sets the sub-message) must translate to a non-nil, empty
// types.HooksConfig — distinguishable from "field absent" the same way
// ToolDispatch preserves the unset/explicit-zero distinction.
func TestRunConfigFromProto_HooksEmptyStaysNonNil(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	rc := runConfigFromProto(&pb.RunConfig{Hooks: &pb.HooksConfig{}})
	must.NotNil(rc.Hooks, "Hooks should be non-nil for an explicit-but-empty proto sub-message")
	check.Empty(rc.Hooks.PreRun, "PreRun")
	check.Empty(rc.Hooks.PostRun, "PostRun")
}

// TestRunConfigFromProto_HooksFieldsPreserved guards the translate hop
// that copies every HookConfig field from the wire format into
// types.HookConfig, exercising the actual Marshal -> Unmarshal wire path.
func TestRunConfigFromProto_HooksFieldsPreserved(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NotNil(rc.Hooks, "Hooks dropped across the wire round-trip")

	must.Len(rc.Hooks.PreRun, 1)
	pre := rc.Hooks.PreRun[0]
	check.Equal("command", pre.Type, "PreRun[0].Type")
	check.Equal("clone", pre.Name, "PreRun[0].Name")
	check.Equal("git clone . .", pre.Command, "PreRun[0].Command")
	check.Equal(120, pre.TimeoutSeconds, "PreRun[0].TimeoutSeconds")

	must.Len(rc.Hooks.PostRun, 2)
	post0 := rc.Hooks.PostRun[0]
	check.Equal("test -f marker", post0.Command, "PostRun[0].Command")
	check.Equal("success", post0.RunOn, "PostRun[0].RunOn")
	check.True(post0.ContinueOnError, "PostRun[0].ContinueOnError")
	post1 := rc.Hooks.PostRun[1]
	check.Equal("curl -X POST https://example.com", post1.Command, "PostRun[1].Command")
	check.Equal("failure", post1.RunOn, "PostRun[1].RunOn")
	check.False(post1.ContinueOnError, "PostRun[1].ContinueOnError")
}

// TestRunConfigFromProto_HooksNotAliased pins that mutating the source
// *pb.RunConfig after translation does not reach the translated
// types.RunConfig.
func TestRunConfigFromProto_HooksNotAliased(t *testing.T) {
	must := require.New(t)
	pc := &pb.RunConfig{
		Hooks: &pb.HooksConfig{
			PreRun: []*pb.HookConfig{{Command: "original"}},
		},
	}

	rc := runConfigFromProto(pc)

	pc.Hooks.PreRun[0].Command = "mutated-after-translate"

	must.Equal("original", rc.Hooks.PreRun[0].Command,
		"translated HookConfig aliases the proto's backing data")
}

// TestRunConfigFromProto_MCPTrustFieldsPreserved guards the MCP trust
// fields (AllowedTools, AllowedMCPHosts) against silent drop in
// toolsConfigFromProto. Marshal/Unmarshal through the actual wire format
// so a future field-number collision on MCPServerConfig shows up here.
func TestRunConfigFromProto_MCPTrustFieldsPreserved(t *testing.T) {
	must := require.New(t)
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

	must.Len(rc.Tools.MCPServers, 1)
	srv := rc.Tools.MCPServers[0]
	must.Equal([]string{"lookup", "query"}, srv.AllowedTools)
	must.Equal([]string{"mcp.example.com"}, srv.AllowedMCPHosts)
}

// TestRunConfigFromProto_MCPTrustFieldsAbsentWhenNil documents the
// backward-compatible default: an MCP server config that omits the
// trust fields entirely must translate to nil/empty slices, not a
// spurious default allowlist or host pin.
func TestRunConfigFromProto_MCPTrustFieldsAbsentWhenNil(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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

	must.Len(rc.Tools.MCPServers, 1)
	srv := rc.Tools.MCPServers[0]
	check.Empty(srv.AllowedTools, "AllowedTools should be empty when proto field absent")
	check.Empty(srv.AllowedMCPHosts, "AllowedMCPHosts should be empty when proto field absent")
}

// TestRunConfigFromProto_ExecutorSandboxFieldsPreserved pins the executor
// fields registry pinning, workspace export, k8s egress wiring, and the
// sandbox identity and git proxy blocks. The values go through the wire
// format so a field that is never encoded is caught here.
func TestRunConfigFromProto_ExecutorSandboxFieldsPreserved(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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

	check.Equal("http://egress.agents.svc:3128", ec.K8sEgressProxyURL)
	check.Equal("gs://stirrup-workspaces/runs", ec.WorkspaceExportTo)
	check.Equal([]string{"ghcr.io/rxbynerd/*", "docker.io/library/*"}, ec.RegistryAllowlist)

	must.NotNil(ec.SandboxIdentity, "SandboxIdentity nil after translation")
	check.Equal("control-plane", ec.SandboxIdentity.Source)
	check.Equal("https://haybale.internal", ec.SandboxIdentity.Audience)
	check.Equal("HAYBALE_TOKEN", ec.SandboxIdentity.EnvVar)

	must.NotNil(ec.GitProxy, "GitProxy nil after translation")
	check.Equal("https://haybale.internal", ec.GitProxy.URL)
	check.Equal([]string{"github.com", "gitlab.com"}, ec.GitProxy.Hosts)
	check.True(ec.GitProxy.RewriteSsh, "GitProxy.RewriteSsh not propagated")
	check.Equal("HAYBALE_TOKEN", ec.GitProxy.TokenEnvVar)
}

// TestRunConfigFromProto_ExecutorSandboxFieldsAbsentStayNil documents the
// safe default: an ExecutorConfig that omits the new sub-messages must
// leave the internal pointers nil, so "operator did not configure" stays
// distinguishable from "configured empty" and the defaults apply.
func TestRunConfigFromProto_ExecutorSandboxFieldsAbsentStayNil(t *testing.T) {
	check := assert.New(t)
	ec := runConfigFromProto(&pb.RunConfig{
		Executor: &pb.ExecutorConfig{Type: "container"},
	}).Executor

	check.Nil(ec.SandboxIdentity, "SandboxIdentity should be nil when proto field absent")
	check.Nil(ec.GitProxy, "GitProxy should be nil when proto field absent")
	check.Nil(ec.RegistryAllowlist, "RegistryAllowlist should be nil when proto field absent")
	check.Empty(ec.WorkspaceExportTo, "WorkspaceExportTo should be empty when proto field absent")
	check.Empty(ec.K8sEgressProxyURL, "K8sEgressProxyURL should be empty when proto field absent")
}

// TestRunConfigFromProto_ExecutorSlicesNotAliased confirms the repeated
// executor fields are copied rather than aliased, so mutating the wire
// payload after translation cannot reach internal config state.
func TestRunConfigFromProto_ExecutorSlicesNotAliased(t *testing.T) {
	check := assert.New(t)
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

	check.Equal("ghcr.io/rxbynerd/*", ec.RegistryAllowlist[0],
		"RegistryAllowlist shares backing array with proto source")
	check.Equal("github.com", ec.GitProxy.Hosts[0],
		"GitProxy.Hosts shares backing array with proto source")
}

// TestRunConfigFromProto_ResultSinkPreserved pins every ResultSinkConfig
// field across a wire round-trip. The sink's type discriminator decides
// whether the run's answer is delivered at all, so a silent drop here
// looks like a working run that produced nothing.
func TestRunConfigFromProto_ResultSinkPreserved(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	original := &pb.RunConfig{
		ResultSink: &pb.ResultSinkConfig{
			Type:                       "gcp-pubsub",
			Topic:                      "projects/acme-prod/topics/stirrup-results",
			Attributes:                 map[string]string{"tenant": "team-a", "auth": "secret://PUBSUB_TOKEN"},
			MaxFinalAssistantTextBytes: 4096,
			Bucket:                     "stirrup-results",
			ObjectPrefix:               "results/",
			OrderingKey:                "tenant-a",
			Credential:                 &pb.CredentialConfig{Type: "gcp-default"},
		},
	}

	var decoded pb.RunConfig
	wireRoundTrip(t, original, &decoded)

	sink := runConfigFromProto(&decoded).ResultSink
	must.NotNil(sink, "ResultSink nil after translation")
	check.Equal("gcp-pubsub", sink.Type)
	check.Equal("projects/acme-prod/topics/stirrup-results", sink.Topic)
	check.Equal(4096, sink.MaxFinalAssistantTextBytes)
	check.Equal("stirrup-results", sink.Bucket)
	check.Equal("results/", sink.ObjectPrefix)
	check.Equal("tenant-a", sink.OrderingKey)
	must.NotNil(sink.Credential, "ResultSink.Credential nil after translation")
	check.Equal("gcp-default", sink.Credential.Type)
	check.Equal(map[string]string{"tenant": "team-a", "auth": "secret://PUBSUB_TOKEN"}, sink.Attributes)

	decoded.ResultSink.Attributes["tenant"] = "mutated"
	check.Equal("team-a", sink.Attributes["tenant"], "ResultSink.Attributes shares the proto-owned map")
}

// TestRunConfigFromProto_ResultSinkAbsentStaysNil documents that an absent
// sub-message leaves ResultSink nil — the "sink disabled" state — rather
// than synthesising a block whose empty Type fails validation.
func TestRunConfigFromProto_ResultSinkAbsentStaysNil(t *testing.T) {
	must := require.New(t)
	must.Nil(runConfigFromProto(&pb.RunConfig{}).ResultSink,
		"ResultSink should be nil when proto field absent")
}

// TestRunConfigFromProto_ToolChoiceEscalationPreserved pins the escalation
// block across a wire round-trip, including the empty-sub-message case:
// present-but-unset must stay non-nil so it is distinguishable from absent.
func TestRunConfigFromProto_ToolChoiceEscalationPreserved(t *testing.T) {
	t.Run("populated", func(t *testing.T) {
		must := require.New(t)
		original := &pb.RunConfig{
			ToolChoiceEscalation: &pb.ToolChoiceEscalationConfig{
				Enabled:    true,
				MaxRetries: 3,
			},
		}
		var decoded pb.RunConfig
		wireRoundTrip(t, original, &decoded)

		esc := runConfigFromProto(&decoded).ToolChoiceEscalation
		must.NotNil(esc, "ToolChoiceEscalation nil after translation")
		must.True(esc.Enabled, "ToolChoiceEscalation.Enabled not propagated")
		must.Equal(3, esc.MaxRetries)
	})

	t.Run("empty sub-message stays non-nil", func(t *testing.T) {
		must := require.New(t)
		esc := runConfigFromProto(&pb.RunConfig{
			ToolChoiceEscalation: &pb.ToolChoiceEscalationConfig{},
		}).ToolChoiceEscalation
		must.NotNil(esc, "expected non-nil ToolChoiceEscalation for an empty sub-message")
		must.False(esc.Enabled, "empty sub-message must not enable escalation")
		must.Equal(0, esc.MaxRetries, "zero resolves to the default")
	})

	t.Run("absent stays nil", func(t *testing.T) {
		must := require.New(t)
		must.Nil(runConfigFromProto(&pb.RunConfig{}).ToolChoiceEscalation,
			"ToolChoiceEscalation should be nil when proto field absent")
	})
}

// TestRunConfigFromProto_WindDownPreserved pins the wind-down block across
// a wire round-trip; an absent sub-message stays nil (off).
func TestRunConfigFromProto_WindDownPreserved(t *testing.T) {
	t.Run("enabled", func(t *testing.T) {
		must := require.New(t)
		original := &pb.RunConfig{WindDown: &pb.WindDownConfig{Enabled: true}}
		var decoded pb.RunConfig
		wireRoundTrip(t, original, &decoded)

		rc := runConfigFromProto(&decoded)
		must.NotNil(rc.WindDown)
		must.True(rc.WindDown.Enabled)
		must.True(rc.WindDownEnabled())
	})

	t.Run("empty sub-message stays non-nil and off", func(t *testing.T) {
		must := require.New(t)
		rc := runConfigFromProto(&pb.RunConfig{WindDown: &pb.WindDownConfig{}})
		must.NotNil(rc.WindDown, "expected non-nil WindDown for an empty sub-message")
		must.False(rc.WindDownEnabled(), "empty sub-message must not enable wind-down")
	})

	t.Run("absent stays nil", func(t *testing.T) {
		must := require.New(t)
		must.Nil(runConfigFromProto(&pb.RunConfig{}).WindDown,
			"WindDown should be nil when proto field absent")
	})
}

// TestRunConfigFromProto_LogsExportPreserved pins observability.logs_export,
// which sits one level below a sub-message that was already translated —
// exactly the shape a field-by-field mirror is most likely to miss.
func TestRunConfigFromProto_LogsExportPreserved(t *testing.T) {
	t.Run("populated", func(t *testing.T) {
		must := require.New(t)
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
		must.Equal("production", obs.Environment)
		must.Equal("otlp", obs.LogsExport.Type)
		must.Equal("otel-collector.observability.svc:4317", obs.LogsExport.Endpoint)
	})

	t.Run("absent leaves stderr-only zero value", func(t *testing.T) {
		must := require.New(t)
		obs := runConfigFromProto(&pb.RunConfig{
			Observability: &pb.ObservabilityConfig{Environment: "staging"},
		}).Observability
		must.Empty(obs.LogsExport.Type, "LogsExport.Type should be empty when absent")
		must.Empty(obs.LogsExport.Endpoint, "LogsExport.Endpoint should be empty when absent")
	})
}

// TestRunConfigFromProto_TransportIsGRPC pins Transport.Type, which the
// translate layer sets rather than copies. Sandbox identity, rule-of-two
// ask-upstream and batch dispatch all depend on it being "grpc".
func TestRunConfigFromProto_TransportIsGRPC(t *testing.T) {
	must := require.New(t)
	must.Equal("grpc", runConfigFromProto(&pb.RunConfig{}).Transport.Type)
}

// TestRunConfigFromProto_SandboxIdentityPassesValidation drives the mirrored
// fields through ValidateRunConfig, the gate BuildLoopWithTransport applies
// to every wire-delivered assignment. Translation alone is not enough: the
// sandbox identity rules require transport "grpc", so a config that
// translates cleanly but fails validation would leave the feature just as
// unreachable as having no proto field at all.
func TestRunConfigFromProto_SandboxIdentityPassesValidation(t *testing.T) {
	must := require.New(t)
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
	must.NoError(types.ValidateRunConfig(&rc),
		"wire-delivered sandbox identity config rejected by validation")
}

// TestRunConfigFromProto_ControlPlaneToolsPreserved pins the wire mapping
// of ToolsConfig.control_plane, including the Struct → JSON projection of
// input_schema, through a real marshal/unmarshal cycle.
func TestRunConfigFromProto_ControlPlaneToolsPreserved(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	schema, err := structpb.NewStruct(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string"},
			"limit": map[string]any{"type": "integer"},
		},
		"required": []any{"query"},
	})
	must.NoError(err, "NewStruct")
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
	must.NoError(err, "marshal")
	var decoded pb.RunConfig
	must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")

	rc := runConfigFromProto(&decoded)
	must.Len(rc.Tools.ControlPlane, 2)

	first := rc.Tools.ControlPlane[0]
	check.Equal("search_memory", first.Name, "first entry name")
	check.Equal("Search long-term memory.", first.Description, "first entry description")
	check.Equal(30, first.TimeoutSeconds, "first entry timeout")
	check.True(first.RequiresApproval, "first entry requires approval")

	var got map[string]any
	must.NoError(json.Unmarshal(first.InputSchema, &got), "inputSchema is not a JSON object: %s", first.InputSchema)
	check.Equal("object", got["type"], "inputSchema.type")
	props, _ := got["properties"].(map[string]any)
	check.Contains(props, "query", "inputSchema.properties")
	req, _ := got["required"].([]any)
	check.Equal([]any{"query"}, req, "inputSchema.required")

	second := rc.Tools.ControlPlane[1]
	check.Nil(second.InputSchema, "unset input_schema maps to nil")
	check.Zero(second.TimeoutSeconds, "second entry timeout default")
	check.False(second.RequiresApproval, "second entry approval default")
}

// TestRunConfigFromProto_ProtectedWriteTargetsPreserved pins
// ToolsConfig.protected_write_targets across a wire round-trip and checks
// the translated slice does not alias the proto message.
func TestRunConfigFromProto_ProtectedWriteTargetsPreserved(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	original := &pb.RunConfig{
		Tools: &pb.ToolsConfig{
			ProtectedWriteTargets: []string{"AGENTS.md", "config/policies/"},
		},
	}
	raw, err := proto.Marshal(original)
	must.NoError(err, "marshal")
	var decoded pb.RunConfig
	must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")

	rc := runConfigFromProto(&decoded)
	want := []string{"AGENTS.md", "config/policies/"}
	must.Equal(want, rc.Tools.ProtectedWriteTargets)

	decoded.Tools.ProtectedWriteTargets[0] = "mutated"
	check.Equal("AGENTS.md", rc.Tools.ProtectedWriteTargets[0],
		"translated ProtectedWriteTargets must not alias the proto's backing data")

	empty := runConfigFromProto(&pb.RunConfig{Tools: &pb.ToolsConfig{}})
	check.Nil(empty.Tools.ProtectedWriteTargets, "unset protected_write_targets maps to nil")
}

func TestRunConfigFromProto_LazyLoadingPreserved(t *testing.T) {
	t.Run("every field", func(t *testing.T) {
		must := require.New(t)
		original := &pb.RunConfig{Tools: &pb.ToolsConfig{LazyLoading: &pb.LazyToolLoadingConfig{
			Enabled:    true,
			Strategy:   types.LazyToolStrategyHarness,
			Eager:      []string{"read_file", "mcp_github_create_issue"},
			MaxResults: 12,
		}}}
		raw, err := proto.Marshal(original)
		must.NoError(err, "marshal")
		var decoded pb.RunConfig
		must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")

		rc := runConfigFromProto(&decoded)
		must.Equal(types.LazyToolLoadingConfig{
			Enabled:    true,
			Strategy:   types.LazyToolStrategyHarness,
			Eager:      []string{"read_file", "mcp_github_create_issue"},
			MaxResults: 12,
		}, rc.Tools.LazyLoading)

		decoded.Tools.LazyLoading.Eager[0] = "mutated"
		must.Equal("read_file", rc.Tools.LazyLoading.Eager[0], "translated Eager must not alias the proto's backing data")
	})

	t.Run("absent message leaves lazy loading off", func(t *testing.T) {
		must := require.New(t)
		rc := runConfigFromProto(&pb.RunConfig{Tools: &pb.ToolsConfig{}})
		must.Zero(rc.Tools.LazyLoading)
	})

	t.Run("empty eager stays nil", func(t *testing.T) {
		must := require.New(t)
		rc := runConfigFromProto(&pb.RunConfig{Tools: &pb.ToolsConfig{LazyLoading: &pb.LazyToolLoadingConfig{Enabled: true}}})
		must.True(rc.Tools.LazyLoading.Enabled)
		must.Nil(rc.Tools.LazyLoading.Eager)
	})
}

func TestRunConfigFromProto_NoBuiltInPreserved(t *testing.T) {
	t.Run("marker survives the wire", func(t *testing.T) {
		must := require.New(t)
		raw, err := proto.Marshal(&pb.RunConfig{Tools: &pb.ToolsConfig{NoBuiltIn: true}})
		must.NoError(err, "marshal")
		var decoded pb.RunConfig
		must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")

		rc := runConfigFromProto(&decoded)
		must.True(rc.Tools.NoBuiltIn)
		must.Empty(rc.Tools.BuiltIn)
		must.False(rc.Tools.BuiltInEnabled("web_fetch"))
	})

	t.Run("an empty built_in list decodes as absent", func(t *testing.T) {
		must := require.New(t)
		raw, err := proto.Marshal(&pb.RunConfig{Tools: &pb.ToolsConfig{BuiltIn: []string{}}})
		must.NoError(err, "marshal")
		var decoded pb.RunConfig
		must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")

		rc := runConfigFromProto(&decoded)
		must.False(rc.Tools.NoBuiltIn)
		must.True(rc.Tools.BuiltInEnabled("web_fetch"), "an empty list selects every built-in, so it cannot mean none")
	})
}

func TestRunConfigFromProto_TraceArchivePreserved(t *testing.T) {
	t.Run("every field", func(t *testing.T) {
		must := require.New(t)
		raw, err := proto.Marshal(&pb.RunConfig{TraceEmitter: &pb.TraceEmitterConfig{
			Type: "gcs",
			Archive: &pb.TraceArchiveConfig{
				Type:         "gcs",
				FilePath:     "unused.tar.gz",
				Bucket:       "archive-bucket",
				ObjectPrefix: "runs/",
			},
		}})
		must.NoError(err, "marshal")
		var decoded pb.RunConfig
		must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")

		must.Equal(&types.TraceArchiveConfig{
			Type:         "gcs",
			FilePath:     "unused.tar.gz",
			Bucket:       "archive-bucket",
			ObjectPrefix: "runs/",
		}, runConfigFromProto(&decoded).TraceEmitter.Archive)
	})

	t.Run("absent message leaves the derived default", func(t *testing.T) {
		must := require.New(t)
		rc := runConfigFromProto(&pb.RunConfig{TraceEmitter: &pb.TraceEmitterConfig{Type: "jsonl"}})
		must.Nil(rc.TraceEmitter.Archive)
	})
}

func TestRunConfigFromProto_ToolsProfilePreserved(t *testing.T) {
	must := require.New(t)
	raw, err := proto.Marshal(&pb.RunConfig{Tools: &pb.ToolsConfig{Profile: "coding-classic"}})
	must.NoError(err, "marshal")
	var decoded pb.RunConfig
	must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")

	must.Equal("coding-classic", runConfigFromProto(&decoded).Tools.Profile)
}

// TestRunConfigFromProto_WorkspaceExportRequiredTriState pins that an
// unset workspace_export_required stays nil (so the destination default
// applies) and that both explicit values survive the wire and translate.
func TestRunConfigFromProto_WorkspaceExportRequiredTriState(t *testing.T) {
	for name, in := range map[string]*bool{"unset": nil, "true": proto.Bool(true), "false": proto.Bool(false)} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			raw, err := proto.Marshal(&pb.RunConfig{Executor: &pb.ExecutorConfig{
				Type:                    "local",
				WorkspaceExportTo:       "gs://bucket/ws.tar.gz",
				WorkspaceExportRequired: in,
			}})
			must.NoError(err, "marshal")
			var decoded pb.RunConfig
			must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")

			rc := runConfigFromProto(&decoded)
			got := rc.Executor.WorkspaceExportRequired
			if in == nil {
				must.Nil(got, "unset must translate to nil")
			} else {
				must.NotNil(got, "explicit value must translate")
				must.Equal(*in, *got)
			}
			wantRequired := in == nil || *in
			must.Equal(wantRequired, rc.Executor.ExportRequired(), "ExportRequired()")
		})
	}
}

// TestRunConfigFromProto_OutputSchemaRoundTrip pins the wire mapping of
// output_schema through a real marshal/unmarshal cycle, and that a
// translated schema passes validation.
func TestRunConfigFromProto_OutputSchemaRoundTrip(t *testing.T) {
	must := require.New(t)
	schema, err := structpb.NewStruct(map[string]any{
		"type":     "object",
		"required": []any{"title"},
		"properties": map[string]any{
			"title": map[string]any{"type": "string", "maxLength": 72},
			"label": map[string]any{"$ref": "#/$defs/label"},
		},
		"$defs": map[string]any{"label": map[string]any{"enum": []any{"bug", "feature"}}},
	})
	must.NoError(err, "NewStruct")
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
	must.NoError(err, "marshal")
	var decoded pb.RunConfig
	must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")
	rc := runConfigFromProto(&decoded)

	var got map[string]any
	must.NoError(json.Unmarshal(rc.OutputSchema, &got), "OutputSchema is not JSON: %s", rc.OutputSchema)
	want := map[string]any{
		"type":     "object",
		"required": []any{"title"},
		"properties": map[string]any{
			"title": map[string]any{"type": "string", "maxLength": float64(72)},
			"label": map[string]any{"$ref": "#/$defs/label"},
		},
		"$defs": map[string]any{"label": map[string]any{"enum": []any{"bug", "feature"}}},
	}
	must.Equal(want, got, "OutputSchema")
	must.Contains(string(rc.OutputSchema), `"maxLength":72`,
		"integer keyword must render as an integral JSON number, got %s", rc.OutputSchema)
	must.NoError(types.ValidateRunConfig(&rc), "translated schema must pass validation")
}

func TestRunConfigFromProto_OutputSchemaAbsentStaysNil(t *testing.T) {
	check := assert.New(t)
	rc := runConfigFromProto(&pb.RunConfig{RunId: "run-free-text"})
	check.Nil(rc.OutputSchema, "OutputSchema is nil when the field is unset")
}

func TestRunConfigFromProto_UnrenderableOutputSchemaIsRejected(t *testing.T) {
	must := require.New(t)
	schema := &structpb.Struct{Fields: map[string]*structpb.Value{
		"maximum": structpb.NewNumberValue(math.NaN()),
	}}
	rc := runConfigFromProto(&pb.RunConfig{OutputSchema: schema})
	must.Equal("null", string(rc.OutputSchema), "a schema protojson cannot render translates to JSON null")

	c := types.RunConfig{OutputSchema: rc.OutputSchema}
	err := types.ValidateRunConfig(&c)
	must.ErrorContains(err, "outputSchema must be a JSON Schema object")
}

// TestRunConfigFromProto_CommandOutputPreserved pins every
// tools.commandOutput field through the wire format so a config arriving
// over task_assignment keeps its compliance-capture bounds.
func TestRunConfigFromProto_CommandOutputPreserved(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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
	must.NoError(err, "marshal")
	var decoded pb.RunConfig
	must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")

	got := runConfigFromProto(&decoded).Tools.CommandOutput
	must.NotNil(got.Enabled, "Enabled must stay explicit")
	must.False(*got.Enabled, "Enabled must stay explicitly false")
	want := types.CommandOutputConfig{
		Enabled:               got.Enabled,
		FailurePosture:        types.CommandOutputPostureBestEffort,
		InlineMaxBytes:        1 << 14,
		PreviewBytesPerStream: 1 << 11,
		MaxBytesPerStream:     1 << 24,
		MaxBytesPerRun:        1 << 26,
	}
	check.Equal(want, got, "CommandOutput")

	*decoded.Tools.CommandOutput.Enabled = true
	check.False(*got.Enabled, "translated Enabled must not alias the proto message's bool")
}

// TestRunConfigFromProto_CommandOutputUnsetKeepsDefaults pins that an
// absent command_output leaves the zero value, so capture stays enabled
// with the documented defaults.
func TestRunConfigFromProto_CommandOutputUnsetKeepsDefaults(t *testing.T) {
	check := assert.New(t)
	rc := runConfigFromProto(&pb.RunConfig{Tools: &pb.ToolsConfig{BuiltIn: []string{"run_command"}}})
	check.Equal(types.CommandOutputConfig{}, rc.Tools.CommandOutput, "CommandOutput is the zero value")
	check.True(rc.Tools.CommandOutputCaptureEnabled(), "capture defaults to enabled when command_output is unset")
}

// TestRunConfigFromProto_MaxResultBytesPreserved pins the tools.maxResultBytes
// mirror through the wire format.
func TestRunConfigFromProto_MaxResultBytesPreserved(t *testing.T) {
	must := require.New(t)
	raw, err := proto.Marshal(&pb.RunConfig{Tools: &pb.ToolsConfig{MaxResultBytes: 65536}})
	must.NoError(err, "marshal")
	var decoded pb.RunConfig
	must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")
	must.Equal(65536, runConfigFromProto(&decoded).Tools.MaxResultBytes, "MaxResultBytes")
}

// TestRunConfigFromProto_MCPOptionalPreserved pins the optional flag across
// the wire format and the translate step; the zero value must stay
// "required".
func TestRunConfigFromProto_MCPOptionalPreserved(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	original := &pb.RunConfig{
		Tools: &pb.ToolsConfig{
			McpServers: []*pb.MCPServerConfig{
				{Name: "required", Uri: "https://mcp.example.com/v1"},
				{Name: "optional", Uri: "https://mcp.example.com/v2", Optional: true},
			},
		},
	}

	raw, err := proto.Marshal(original)
	must.NoError(err, "marshal")
	var decoded pb.RunConfig
	must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")

	rc := runConfigFromProto(&decoded)
	must.Len(rc.Tools.MCPServers, 2)
	check.False(rc.Tools.MCPServers[0].Optional, "a server with no optional field translates as required")
	check.True(rc.Tools.MCPServers[1].Optional, "Optional survives translation")
}

// TestRunConfigFromProto_RepositoryGuidancePreserved pins every
// RepositoryGuidanceConfig field across a wire round-trip and checks the
// translated slice and map do not alias the proto-owned values.
func TestRunConfigFromProto_RepositoryGuidancePreserved(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	digest := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	original := &pb.RunConfig{
		RepositoryGuidance: &pb.RepositoryGuidanceConfig{
			Files:        []string{"AGENTS.md", "CLAUDE.md"},
			MaxBytes:     4096,
			PinnedSha256: map[string]string{"AGENTS.md": digest},
		},
	}

	raw, err := proto.Marshal(original)
	must.NoError(err, "marshal")
	var decoded pb.RunConfig
	must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")

	rg := runConfigFromProto(&decoded).RepositoryGuidance
	must.NotNil(rg, "RepositoryGuidance must survive translation")
	want := &types.RepositoryGuidanceConfig{
		Files:        []string{"AGENTS.md", "CLAUDE.md"},
		MaxBytes:     4096,
		PinnedSHA256: map[string]string{"AGENTS.md": digest},
	}
	must.Equal(want, rg)

	decoded.RepositoryGuidance.Files[0] = "mutated"
	decoded.RepositoryGuidance.PinnedSha256["AGENTS.md"] = "mutated"
	check.Equal("AGENTS.md", rg.Files[0], "RepositoryGuidance.Files must not share the proto-owned slice")
	check.Equal(digest, rg.PinnedSHA256["AGENTS.md"], "RepositoryGuidance.PinnedSHA256 must not share the proto-owned map")
}

// TestRunConfigFromProto_CorrelationPreserved pins every CorrelationConfig
// field across a wire round-trip and checks the translated label map does
// not alias the proto-owned map.
func TestRunConfigFromProto_CorrelationPreserved(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err, "marshal")
	var decoded pb.RunConfig
	must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")

	got := runConfigFromProto(&decoded).Correlation
	want := &types.CorrelationConfig{
		SessionID: "session-42",
		TaskID:    "task-7",
		AttemptID: "attempt-3",
		UserID:    "user-9f2c",
		URL:       "https://cp.example.com/runs/42",
		Labels:    map[string]string{"agent_profile": "reviewer", "execution_mode": "batch"},
	}
	must.Equal(want, got)

	decoded.Correlation.Labels["agent_profile"] = "mutated"
	check.Equal("reviewer", got.Labels["agent_profile"], "Correlation.Labels must not share the proto-owned map")
}

func TestRunConfigFromProto_CorrelationAbsentStaysNil(t *testing.T) {
	check := assert.New(t)
	check.Nil(runConfigFromProto(&pb.RunConfig{}).Correlation, "Correlation is nil when the proto field is absent")
}

// TestRunConfigFromProto_RepositoryGuidanceAbsentStaysNil documents that an
// absent sub-message leaves guidance off rather than synthesising an empty
// block that fails validation.
func TestRunConfigFromProto_RepositoryGuidanceAbsentStaysNil(t *testing.T) {
	check := assert.New(t)
	check.Nil(runConfigFromProto(&pb.RunConfig{}).RepositoryGuidance, "RepositoryGuidance is nil when the proto field is absent")
}

// TestProviderConfigFromProto_ReplayFileCarried pins that the replay
// provider's recording path survives the proto boundary; without it a
// TaskAssignment selecting the replay provider fails ValidateRunConfig
// ("replayFile is required") although the wire payload named one.
func TestProviderConfigFromProto_ReplayFileCarried(t *testing.T) {
	check := assert.New(t)
	cfg := providerConfigFromProto(&pb.ProviderConfig{
		Type:       "replay",
		ReplayFile: "/recordings/run.jsonl",
	})
	check.Equal("replay", cfg.Type, "Type")
	check.Equal("/recordings/run.jsonl", cfg.ReplayFile, "ReplayFile")
}
func TestRunConfigFromProto_PostEditPreserved(t *testing.T) {
	must := require.New(t)
	original := &pb.RunConfig{
		Tools: &pb.ToolsConfig{
			PostEdit: []*pb.PostEditCheck{
				{Glob: "*.go", Command: "gofmt -l {path}", TimeoutSeconds: 30},
				{Glob: "cmd/*.go", Command: "go vet ./cmd/..."},
			},
		},
	}
	raw, err := proto.Marshal(original)
	must.NoError(err)
	var decoded pb.RunConfig
	must.NoError(proto.Unmarshal(raw, &decoded))

	got := runConfigFromProto(&decoded).Tools.PostEdit
	want := []types.PostEditCheckConfig{
		{Glob: "*.go", Command: "gofmt -l {path}", TimeoutSeconds: 30},
		{Glob: "cmd/*.go", Command: "go vet ./cmd/..."},
	}
	must.Equal(want, got)

	absent := runConfigFromProto(&pb.RunConfig{Tools: &pb.ToolsConfig{}}).Tools.PostEdit
	must.Nil(absent, "absent post_edit should stay nil")
}

// The proto JSON mirror of RunConfig must accept the Go-side JSON shape
// for tools.postEdit unchanged.
func TestRunConfigFromProto_PostEditJSONMirrorMatchesGoShape(t *testing.T) {
	must := require.New(t)
	goSide := types.ToolsConfig{PostEdit: []types.PostEditCheckConfig{
		{Glob: "*.go", Command: "gofmt -l {path}", TimeoutSeconds: 45},
	}}
	raw, err := json.Marshal(map[string]any{"postEdit": goSide.PostEdit})
	must.NoError(err)
	var pc pb.ToolsConfig
	must.NoError(protojson.Unmarshal(raw, &pc), "proto JSON mirror rejected Go JSON %s", raw)
	must.Equal(goSide.PostEdit, toolsConfigFromProto(&pc).PostEdit)
}
