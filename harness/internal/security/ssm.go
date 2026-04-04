package security

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/rxbynerd/stirrup/types"
)

const ssmSchemePrefix = "secret://ssm://"

// ssmAPI is a minimal interface over the SSM client, enabling test doubles.
type ssmAPI interface {
	GetParameter(ctx context.Context, params *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

// SSMSecretStore resolves secret references of the form "secret://ssm:///param-name"
// via AWS SSM Parameter Store with decryption enabled.
type SSMSecretStore struct {
	client ssmAPI
}

// NewSSMSecretStore constructs an SSMSecretStore using the default AWS config.
func NewSSMSecretStore(ctx context.Context) (*SSMSecretStore, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config for SSM: %w", err)
	}
	return &SSMSecretStore{client: ssm.NewFromConfig(cfg)}, nil
}

// newSSMSecretStoreWithClient constructs an SSMSecretStore with an injected
// client, used for testing.
func newSSMSecretStoreWithClient(client ssmAPI) *SSMSecretStore {
	return &SSMSecretStore{client: client}
}

// Resolve fetches a parameter from SSM Parameter Store. The ref must have
// the format "secret://ssm:///parameter-name".
func (s *SSMSecretStore) Resolve(ctx context.Context, ref string) (string, error) {
	if !strings.HasPrefix(ref, ssmSchemePrefix) {
		return "", fmt.Errorf("SSMSecretStore: unsupported reference scheme: %q", ref)
	}

	paramName := strings.TrimPrefix(ref, ssmSchemePrefix)
	if paramName == "" {
		return "", fmt.Errorf("SSMSecretStore: empty parameter name in reference: %q", ref)
	}

	// Prepend leading slash for SSM path convention if not already present.
	if !strings.HasPrefix(paramName, "/") {
		paramName = "/" + paramName
	}

	out, err := s.client.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(paramName),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return "", fmt.Errorf("SSM GetParameter %q: %w", paramName, err)
	}

	if out.Parameter == nil || out.Parameter.Value == nil {
		return "", fmt.Errorf("SSM parameter %q returned nil value", paramName)
	}

	value := strings.TrimSpace(*out.Parameter.Value)
	if value == "" {
		return "", fmt.Errorf("SSM parameter %q is empty", paramName)
	}

	return value, nil
}

// AutoSecretStore routes secret references to the appropriate backend based
// on the reference scheme:
//   - "secret://ssm:///..." -> SSMSecretStore
//   - everything else       -> EnvSecretStore
type AutoSecretStore struct {
	env *EnvSecretStore
	ssm *SSMSecretStore // nil if no SSM refs are needed
}

// NewAutoSecretStore creates a composite secret store. It scans all secret
// references in the RunConfig; if any use the "ssm://" scheme, it initialises
// an SSM client. Otherwise it returns a plain EnvSecretStore wrapper (avoiding
// unnecessary AWS SDK initialisation).
func NewAutoSecretStore(ctx context.Context, config *types.RunConfig) (*AutoSecretStore, error) {
	store := &AutoSecretStore{
		env: NewEnvSecretStore(),
	}

	if needsSSM(config) {
		ssmStore, err := NewSSMSecretStore(ctx)
		if err != nil {
			return nil, fmt.Errorf("init SSM secret store: %w", err)
		}
		store.ssm = ssmStore
	}

	return store, nil
}

// Resolve dispatches to the appropriate backend based on the ref scheme.
func (a *AutoSecretStore) Resolve(ctx context.Context, ref string) (string, error) {
	if strings.HasPrefix(ref, ssmSchemePrefix) {
		if a.ssm == nil {
			return "", fmt.Errorf("SSM secret reference %q but SSM store not initialised", ref)
		}
		return a.ssm.Resolve(ctx, ref)
	}
	return a.env.Resolve(ctx, ref)
}

// needsSSM scans all secret references in the config and returns true if any
// use the "ssm://" scheme.
func needsSSM(config *types.RunConfig) bool {
	if config == nil {
		return false
	}
	refs := collectSecretRefs(config)
	for _, ref := range refs {
		if strings.HasPrefix(ref, ssmSchemePrefix) {
			return true
		}
	}
	return false
}

// collectSecretRefs extracts all secret reference strings from a RunConfig.
func collectSecretRefs(config *types.RunConfig) []string {
	var refs []string
	if config.Provider.APIKeyRef != "" {
		refs = append(refs, config.Provider.APIKeyRef)
	}
	for _, provider := range config.Providers {
		if provider.APIKeyRef != "" {
			refs = append(refs, provider.APIKeyRef)
		}
	}
	if config.Executor.VcsBackend != nil && config.Executor.VcsBackend.APIKeyRef != "" {
		refs = append(refs, config.Executor.VcsBackend.APIKeyRef)
	}
	for _, srv := range config.Tools.MCPServers {
		if srv.APIKeyRef != "" {
			refs = append(refs, srv.APIKeyRef)
		}
	}
	return refs
}
