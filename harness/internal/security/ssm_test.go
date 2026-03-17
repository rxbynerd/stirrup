package security

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
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

func TestSSMSecretStore_ResolveValidParam(t *testing.T) {
	client := &mockSSMClient{
		params: map[string]string{
			"/prod/api-key": "super-secret-123",
		},
	}
	store := newSSMSecretStoreWithClient(client)
	ctx := context.Background()

	val, err := store.Resolve(ctx, "secret://ssm:///prod/api-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "super-secret-123" {
		t.Errorf("got %q, want %q", val, "super-secret-123")
	}
}

func TestSSMSecretStore_ResolveWithoutLeadingSlash(t *testing.T) {
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "value-42" {
		t.Errorf("got %q, want %q", val, "value-42")
	}
}

func TestSSMSecretStore_ResolveMissingParam(t *testing.T) {
	client := &mockSSMClient{params: map[string]string{}}
	store := newSSMSecretStoreWithClient(client)
	ctx := context.Background()

	_, err := store.Resolve(ctx, "secret://ssm:///nonexistent")
	if err == nil {
		t.Fatal("expected error for missing parameter, got nil")
	}
}

func TestSSMSecretStore_ResolveSSMError(t *testing.T) {
	client := &mockSSMClient{err: fmt.Errorf("AccessDeniedException: not authorised")}
	store := newSSMSecretStoreWithClient(client)
	ctx := context.Background()

	_, err := store.Resolve(ctx, "secret://ssm:///some-param")
	if err == nil {
		t.Fatal("expected error from SSM, got nil")
	}
}

func TestSSMSecretStore_ResolveEmptyParamName(t *testing.T) {
	client := &mockSSMClient{params: map[string]string{}}
	store := newSSMSecretStoreWithClient(client)
	ctx := context.Background()

	_, err := store.Resolve(ctx, "secret://ssm://")
	if err == nil {
		t.Fatal("expected error for empty parameter name, got nil")
	}
}

func TestSSMSecretStore_ResolveWrongScheme(t *testing.T) {
	client := &mockSSMClient{params: map[string]string{}}
	store := newSSMSecretStoreWithClient(client)
	ctx := context.Background()

	_, err := store.Resolve(ctx, "secret://SOME_ENV_VAR")
	if err == nil {
		t.Fatal("expected error for non-SSM scheme, got nil")
	}
}

func TestSSMSecretStore_ResolveEmptyValue(t *testing.T) {
	client := &mockSSMClient{
		params: map[string]string{
			"/empty-param": "  \n  ",
		},
	}
	store := newSSMSecretStoreWithClient(client)
	ctx := context.Background()

	_, err := store.Resolve(ctx, "secret://ssm:///empty-param")
	if err == nil {
		t.Fatal("expected error for empty SSM value, got nil")
	}
}

// --- AutoSecretStore tests ---

func TestAutoSecretStore_RoutesToEnvStore(t *testing.T) {
	const key = "STIRRUP_AUTO_TEST_SECRET"
	t.Setenv(key, "env-value")

	store := &AutoSecretStore{
		env: NewEnvSecretStore(),
		ssm: nil, // no SSM needed
	}
	ctx := context.Background()

	val, err := store.Resolve(ctx, "secret://"+key)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "env-value" {
		t.Errorf("got %q, want %q", val, "env-value")
	}
}

func TestAutoSecretStore_RoutesToSSMStore(t *testing.T) {
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "ssm-secret" {
		t.Errorf("got %q, want %q", val, "ssm-secret")
	}
}

func TestAutoSecretStore_SSMRefWithoutSSMStore(t *testing.T) {
	store := &AutoSecretStore{
		env: NewEnvSecretStore(),
		ssm: nil,
	}
	ctx := context.Background()

	_, err := store.Resolve(ctx, "secret://ssm:///some-param")
	if err == nil {
		t.Fatal("expected error when SSM store is nil, got nil")
	}
}

func TestAutoSecretStore_FileRefStillWorks(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/secret.txt"
	if err := os.WriteFile(path, []byte("file-secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	store := &AutoSecretStore{
		env: NewEnvSecretStore(),
		ssm: nil,
	}
	ctx := context.Background()

	val, err := store.Resolve(ctx, "secret://file://"+path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "file-secret" {
		t.Errorf("got %q, want %q", val, "file-secret")
	}
}

func TestNeedsSSM(t *testing.T) {
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := needsSSM(tt.config)
			if got != tt.want {
				t.Errorf("needsSSM() = %v, want %v", got, tt.want)
			}
		})
	}
}
