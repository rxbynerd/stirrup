package security

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// mockSSMClient implements ssmAPI for testing.
type mockSSMClient struct {
	params map[string]string // paramName -> value
	err    error             // if non-nil, GetParameter always returns this
}

func (m *mockSSMClient) GetParameter(_ context.Context, input *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	if m.err != nil {
		return nil, m.err
	}
	name := aws.ToString(input.Name)
	val, ok := m.params[name]
	if !ok {
		return nil, fmt.Errorf("ParameterNotFound: parameter %q not found", name)
	}
	return &ssm.GetParameterOutput{
		Parameter: &ssmtypes.Parameter{
			Name:  aws.String(name),
			Value: aws.String(val),
		},
	}, nil
}

func TestSSMSecretStore_Resolve_ReturnsParamValue(t *testing.T) {
	must := require.New(t)
	client := &mockSSMClient{
		params: map[string]string{
			"/prod/api-key": "super-secret-123",
		},
	}
	store := newSSMSecretStoreWithClient(client)
	ctx := context.Background()

	val, err := store.Resolve(ctx, "secret://ssm:///prod/api-key")
	must.NoError(err)
	must.Equal("super-secret-123", val)
}

func TestSSMSecretStore_Resolve_PrependsLeadingSlash(t *testing.T) {
	must := require.New(t)
	// When the ref is "secret://ssm:///param" the param name is "param" (no leading slash),
	// so the store should prepend one to form "/param".
	client := &mockSSMClient{
		params: map[string]string{
			"/my-param": "value-42",
		},
	}
	store := newSSMSecretStoreWithClient(client)
	ctx := context.Background()

	val, err := store.Resolve(ctx, "secret://ssm:///my-param")
	must.NoError(err)
	must.Equal("value-42", val)
}

func TestSSMSecretStore_Resolve_MissingParamErrors(t *testing.T) {
	must := require.New(t)
	client := &mockSSMClient{params: map[string]string{}}
	store := newSSMSecretStoreWithClient(client)
	ctx := context.Background()

	_, err := store.Resolve(ctx, "secret://ssm:///nonexistent")
	must.Error(err)
}

func TestSSMSecretStore_Resolve_PropagatesSSMError(t *testing.T) {
	must := require.New(t)
	client := &mockSSMClient{err: fmt.Errorf("AccessDeniedException: not authorised")}
	store := newSSMSecretStoreWithClient(client)
	ctx := context.Background()

	_, err := store.Resolve(ctx, "secret://ssm:///some-param")
	must.Error(err)
}

func TestSSMSecretStore_Resolve_EmptyParamNameErrors(t *testing.T) {
	must := require.New(t)
	client := &mockSSMClient{params: map[string]string{}}
	store := newSSMSecretStoreWithClient(client)
	ctx := context.Background()

	_, err := store.Resolve(ctx, "secret://ssm://")
	must.Error(err)
}

func TestSSMSecretStore_Resolve_RejectsNonSSMScheme(t *testing.T) {
	must := require.New(t)
	client := &mockSSMClient{params: map[string]string{}}
	store := newSSMSecretStoreWithClient(client)
	ctx := context.Background()

	_, err := store.Resolve(ctx, "secret://SOME_ENV_VAR")
	must.Error(err)
}

func TestSSMSecretStore_Resolve_RejectsBlankValue(t *testing.T) {
	must := require.New(t)
	client := &mockSSMClient{
		params: map[string]string{
			"/empty-param": "  \n  ",
		},
	}
	store := newSSMSecretStoreWithClient(client)
	ctx := context.Background()

	_, err := store.Resolve(ctx, "secret://ssm:///empty-param")
	must.Error(err)
}

// --- AutoSecretStore tests ---

func TestAutoSecretStore_RoutesToEnvStore(t *testing.T) {
	must := require.New(t)
	const key = "STIRRUP_AUTO_TEST_SECRET"
	t.Setenv(key, "env-value")

	store := &AutoSecretStore{
		env: NewEnvSecretStore(),
		ssm: nil, // no SSM needed
	}
	ctx := context.Background()

	val, err := store.Resolve(ctx, "secret://"+key)
	must.NoError(err)
	must.Equal("env-value", val)
}

func TestAutoSecretStore_RoutesToSSMStore(t *testing.T) {
	must := require.New(t)
	client := &mockSSMClient{
		params: map[string]string{
			"/prod/api-key": "ssm-secret",
		},
	}
	store := &AutoSecretStore{
		env: NewEnvSecretStore(),
		ssm: newSSMSecretStoreWithClient(client),
	}
	ctx := context.Background()

	val, err := store.Resolve(ctx, "secret://ssm:///prod/api-key")
	must.NoError(err)
	must.Equal("ssm-secret", val)
}

func TestAutoSecretStore_SSMRefWithoutSSMStoreErrors(t *testing.T) {
	must := require.New(t)
	store := &AutoSecretStore{
		env: NewEnvSecretStore(),
		ssm: nil,
	}
	ctx := context.Background()

	_, err := store.Resolve(ctx, "secret://ssm:///some-param")
	must.Error(err)
}

func TestAutoSecretStore_RoutesFileRef(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	path := dir + "/secret.txt"
	must.NoError(os.WriteFile(path, []byte("file-secret"), 0o600))

	store := &AutoSecretStore{
		env: NewEnvSecretStore(),
		ssm: nil,
	}
	ctx := context.Background()

	val, err := store.Resolve(ctx, "secret://file://"+path)
	must.NoError(err)
	must.Equal("file-secret", val)
}

func TestNeedsSSM_DetectsSSMReferences(t *testing.T) {
	tests := []struct {
		name   string
		config *types.RunConfig
		want   bool
	}{
		{
			name:   "nil config",
			config: nil,
			want:   false,
		},
		{
			name: "env-only refs",
			config: &types.RunConfig{
				Provider: types.ProviderConfig{APIKeyRef: "secret://ANTHROPIC_KEY"},
			},
			want: false,
		},
		{
			name: "SSM provider ref",
			config: &types.RunConfig{
				Provider: types.ProviderConfig{APIKeyRef: "secret://ssm:///prod/api-key"},
			},
			want: true,
		},
		{
			name: "SSM in VCS backend",
			config: &types.RunConfig{
				Executor: types.ExecutorConfig{
					VcsBackend: &types.VcsBackendConfig{APIKeyRef: "secret://ssm:///gh-token"},
				},
			},
			want: true,
		},
		{
			name: "SSM in MCP server",
			config: &types.RunConfig{
				Tools: types.ToolsConfig{
					MCPServers: []types.MCPServerConfig{
						{Name: "a", APIKeyRef: "secret://ENV_VAR"},
						{Name: "b", APIKeyRef: "secret://ssm:///mcp-key"},
					},
				},
			},
			want: true,
		},
		{
			name: "no refs at all",
			config: &types.RunConfig{
				Provider: types.ProviderConfig{Type: "bedrock"},
			},
			want: false,
		},
		{
			name: "SSM in guardrail apiKeyRef",
			config: &types.RunConfig{
				GuardRail: &types.GuardRailConfig{
					Type:      "shieldstral",
					Endpoint:  "https://api.mistral.ai",
					APIKeyRef: "secret://ssm:///mistral-key",
				},
			},
			want: true,
		},
		{
			name: "SSM in composite guardrail stage",
			config: &types.RunConfig{
				GuardRail: &types.GuardRailConfig{
					Type: "composite",
					Stages: []types.GuardRailConfig{
						{Type: "granite-guardian", Endpoint: "http://localhost:8000"},
						{Type: "shieldstral", Endpoint: "https://api.mistral.ai", APIKeyRef: "secret://ssm:///mistral-key"},
					},
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tt.want, needsSSM(tt.config))
		})
	}
}
