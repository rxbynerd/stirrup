package core

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// TestBuildProvider_UnusableHostFilesReturnHostFileError pins that every
// failure to use a provider file from the harness host is reported as a
// HostFileError naming its field, whatever the cause.
func TestBuildProvider_UnusableHostFilesReturnHostFileError(t *testing.T) {
	dir := t.TempDir()
	notJSON := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(notJSON, []byte("not json\n"), 0o600))
	cases := []struct {
		name      string
		cfg       types.ProviderConfig
		wantField string
	}{
		{name: "replay file missing", cfg: types.ProviderConfig{Type: "replay", ReplayFile: filepath.Join(dir, "absent.jsonl")}, wantField: "replayFile"},
		{name: "replay file is a directory", cfg: types.ProviderConfig{Type: "replay", ReplayFile: dir}, wantField: "replayFile"},
		{name: "replay file is not a trace", cfg: types.ProviderConfig{Type: "replay", ReplayFile: notJSON}, wantField: "replayFile"},
		{
			name: "gcp credentials file missing",
			cfg: types.ProviderConfig{
				Type: "gemini", GCPProject: "p", GCPLocation: "global",
				GCPCredentialsFile: filepath.Join(dir, "absent.json"),
				Credential:         &types.CredentialConfig{Type: "gcp-service-account"},
			},
			wantField: "gcpCredentialsFile",
		},
		{
			name: "gcp credentials file is not a key",
			cfg: types.ProviderConfig{
				Type: "gemini", GCPProject: "p", GCPLocation: "global",
				GCPCredentialsFile: notJSON,
				Credential:         &types.CredentialConfig{Type: "gcp-service-account"},
			},
			wantField: "gcpCredentialsFile",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			_, err := buildProvider(context.Background(), tc.cfg, &stubSecretStore{})
			var hostFile *HostFileError
			must.ErrorAs(err, &hostFile)
			must.Equal(tc.wantField, hostFile.Field)
			must.Equal(tc.wantField+" unavailable on the harness host", hostFile.Redacted())
			must.NotContains(hostFile.Redacted(), dir, "the redacted text must not echo the path")
		})
	}
}

// TestBuildPermissionPolicy_UnusablePolicyFileReturnsHostFileError pins
// that a policy file the harness cannot read or parse is a HostFileError,
// while a parsed policy the linter rejects keeps its own error so the
// finding still reaches the operator.
func TestBuildPermissionPolicy_UnusablePolicyFileReturnsHostFileError(t *testing.T) {
	dir := t.TempDir()
	notCedar := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(notCedar, []byte("not cedar {\n"), 0o600))
	deadClause := filepath.Join(dir, "dead.cedar")
	require.NoError(t, os.WriteFile(deadClause, []byte(`permit (principal, action, resource) when { principal.runID == "x" };`+"\n"), 0o600))
	registry := buildToolRegistry(&registryExecutor{
		caps: executor.ExecutorCapabilities{CanRead: true},
	}, edit.NewWholeFileStrategy(), types.ToolsConfig{}, nil)
	build := func(path string) error {
		rc := &types.RunConfig{
			RunID: "test-run",
			Mode:  "execution",
			PermissionPolicy: types.PermissionPolicyConfig{
				Type:       "policy-engine",
				PolicyFile: path,
				Fallback:   "deny-all",
			},
		}
		_, err := buildPermissionPolicy(rc, registry, nil, security.NewSecurityLogger(io.Discard, "test-run"))
		return err
	}

	for name, path := range map[string]string{
		"missing":   filepath.Join(dir, "absent.cedar"),
		"directory": dir,
		"not cedar": notCedar,
	} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			var hostFile *HostFileError
			must.ErrorAs(build(path), &hostFile)
			must.Equal("permissionPolicy.policyFile", hostFile.Field)
		})
	}
	t.Run("lint failure", func(t *testing.T) {
		must := require.New(t)
		err := build(deadClause)
		must.ErrorContains(err, "unknown-principal-attribute")
		var hostFile *HostFileError
		must.False(errors.As(err, &hostFile), "a lint finding is not a host-file failure: %v", err)
	})
}
