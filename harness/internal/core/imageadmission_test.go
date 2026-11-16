package core

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

var pinnedTestImage = "ghcr.io/acme/sandbox@sha256:" + strings.Repeat("a", 64)

func mustImagePolicy(t *testing.T, patterns ...string) executor.ImageAdmissionPolicy {
	t.Helper()
	policy, err := executor.ParseImageAdmissionPolicy(patterns)
	require.New(t).NoError(err)
	return policy
}

func TestAdmitExecutorImage_EveryImageExecutorIsChecked(t *testing.T) {
	policy := mustImagePolicy(t, "us-docker.pkg.dev/acme/")
	for _, executorType := range []string{"container", "k8s", "k8s-sandbox"} {
		t.Run(executorType, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			var buf bytes.Buffer
			sec := security.NewSecurityLogger(&buf, "run")

			err := admitExecutorImage(types.ExecutorConfig{Type: executorType, Image: "docker.io/someuser/app"}, policy, sec)
			must.ErrorIs(err, executor.ErrImageNotAdmitted)
			must.ErrorContains(err, "executor.image")
			check.Equal([]string{"image_admission_denied"}, securityEventNames(t, &buf))

			buf.Reset()
			must.NoError(admitExecutorImage(types.ExecutorConfig{Type: executorType, Image: "us-docker.pkg.dev/acme/sandboxes/go:1"}, policy, sec))
			check.Empty(securityEventNames(t, &buf), "an admitted image under a configured policy emits nothing")
		})
	}
}

func TestAdmitExecutorImage_OtherExecutorsAreNotChecked(t *testing.T) {
	policy := mustImagePolicy(t, "us-docker.pkg.dev/acme/")
	for _, executorType := range []string{"", "local", "api", "none"} {
		t.Run(executorType, func(t *testing.T) {
			must := require.New(t)
			var buf bytes.Buffer
			sec := security.NewSecurityLogger(&buf, "run")
			must.NoError(admitExecutorImage(types.ExecutorConfig{Type: executorType}, policy, sec))
			must.Empty(buf.String())
		})
	}
}

func TestAdmitExecutorImage_UnconfiguredPolicyWarnsOnUnpinnedImage(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	sec := security.NewSecurityLogger(&buf, "run")

	must.NoError(admitExecutorImage(types.ExecutorConfig{Type: "k8s", Image: "docker.io/someuser/app:latest"}, executor.ImageAdmissionPolicy{}, sec))
	must.Equal([]string{"image_unpinned_without_admission_policy"}, securityEventNames(t, &buf))

	var ev security.SecurityEvent
	must.NoError(json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &ev))
	must.Equal("warn", ev.Level)
	must.Equal("k8s", ev.Data["executor"])
	must.Equal("docker.io/someuser/app:latest", ev.Data["image"])
}

func TestAdmitExecutorImage_UnconfiguredPolicyIsSilentOnPinnedImage(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	sec := security.NewSecurityLogger(&buf, "run")
	must.NoError(admitExecutorImage(types.ExecutorConfig{Type: "container", Image: pinnedTestImage}, executor.ImageAdmissionPolicy{}, sec))
	must.Empty(buf.String())
}

func TestAdmitExecutorImage_MalformedImageIsAValidationFailure(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	sec := security.NewSecurityLogger(&buf, "run")
	err := admitExecutorImage(types.ExecutorConfig{Type: "container", Image: "ghcr.io/acme/"}, executor.ImageAdmissionPolicy{}, sec)
	must.ErrorContains(err, "not a valid image reference")
	must.NotErrorIs(err, executor.ErrImageNotAdmitted)
	must.Equal([]string{"config_validation_failed"}, securityEventNames(t, &buf))
}

func TestBuildLoop_ImageAdmissionDeniesBeforeExecutorIsBuilt(t *testing.T) {
	must := require.New(t)
	prev := buildExecutorFn
	built := false
	buildExecutorFn = func(ctx context.Context, cfg types.ExecutorConfig, secrets security.SecretStore, secLogger *security.SecurityLogger, extraEnv []executor.EnvPair, life executor.Lifecycle) (executor.Executor, error) {
		built = true
		return prev(ctx, cfg, secrets, secLogger, extraEnv, life)
	}
	t.Cleanup(func() { buildExecutorFn = prev })

	config := buildTestConfig()
	config.Executor = types.ExecutorConfig{Type: "container", Image: "docker.io/someuser/app", Workspace: t.TempDir()}

	_, err := BuildLoop(context.Background(), config, WithImageAdmission(mustImagePolicy(t, "ghcr.io/acme/")))
	must.ErrorIs(err, executor.ErrImageNotAdmitted)
	must.False(built, "no sandbox may be created for a refused image")
}

func TestPreflight_ImageAdmissionStep(t *testing.T) {
	tests := []struct {
		name       string
		image      string
		policy     []string
		wantStatus PreflightStatus
		wantDetail string
	}{
		{"denied by policy", "docker.io/someuser/app", []string{"ghcr.io/acme/"}, PreflightFail, "not admitted"},
		{"admitted by policy", "ghcr.io/acme/team/sandbox:1", []string{"ghcr.io/acme/"}, PreflightOK, "admitted"},
		{"unpinned without policy", "ubuntu:26.04", nil, PreflightWarn, "not pinned by digest"},
		{"pinned without policy", pinnedTestImage, nil, PreflightOK, "pinned by digest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
			srv, _ := metadataOnlyServer(t)
			defer srv.Close()

			cfg := preflightTestConfig(t, srv.URL+"/v1")
			cfg.Executor = types.ExecutorConfig{Type: "container", Image: tt.image}
			report, err := Preflight(context.Background(), cfg, PreflightOptions{
				SkipExecutor:   true,
				ImageAdmission: mustImagePolicy(t, tt.policy...),
			})
			must.NoError(err)

			st, ok := stepStatus(report, "image-admission")
			must.True(ok, "no image-admission step; steps: %+v", report.Steps)
			must.Equal(tt.wantStatus, st)
			detail, _ := stepDetail(report, "image-admission")
			must.Contains(detail, tt.wantDetail)
			must.Equal(tt.wantStatus != PreflightFail, report.OK)
		})
	}
}

func TestPreflight_ImageAdmissionStepAbsentForLocalExecutor(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	report, err := Preflight(context.Background(), preflightTestConfig(t, srv.URL+"/v1"), PreflightOptions{
		ImageAdmission: mustImagePolicy(t, "ghcr.io/acme/"),
	})
	must.NoError(err)
	_, ok := stepStatus(report, "image-admission")
	must.False(ok)
}
