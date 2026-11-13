package core

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/sandboxidentity"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// errRetainedOnClose stands in for an executor teardown that kept cluster
// objects.
var errRetainedOnClose = errors.New("tear down pod ns/stirrup-x (run ID \"r\"): pod termination not confirmed; retained egress NetworkPolicy ns/stirrup-x-egress")

// closeErrExecutor is a real executor whose Close reports errRetainedOnClose
// after closing it.
type closeErrExecutor struct {
	executor.Executor
}

func (c closeErrExecutor) Close() error {
	if closer, ok := c.Executor.(io.Closer); ok {
		_ = closer.Close()
	}
	return errRetainedOnClose
}

func (c closeErrExecutor) WriteSandboxIdentityToken(ctx context.Context, token string) error {
	return c.Executor.(executor.SandboxIdentityTokenWriter).WriteSandboxIdentityToken(ctx, token)
}

// closeErrExecutors makes every executor the factory builds report a
// retained object when closed.
func closeErrExecutors(t *testing.T) {
	t.Helper()
	prev := buildExecutorFn
	buildExecutorFn = func(ctx context.Context, cfg types.ExecutorConfig, secrets security.SecretStore, secLogger *security.SecurityLogger, extraEnv []executor.EnvPair, life executor.Lifecycle) (executor.Executor, error) {
		exec, err := prev(ctx, cfg, secrets, secLogger, extraEnv, life)
		if err != nil {
			return nil, err
		}
		return closeErrExecutor{Executor: exec}, nil
	}
	t.Cleanup(func() { buildExecutorFn = prev })
}

func TestBuildLoopWithTransport_FailedBuildReportsRetainedObjects(t *testing.T) {
	t.Run("startup token re-exchange fails", func(t *testing.T) {
		must := require.New(t)
		closeErrExecutors(t)
		sock, capture, cleanupEngine := fakeDockerEngine(t)
		defer cleanupEngine()
		capture.createDelay = 1200 * time.Millisecond
		t.Setenv("DOCKER_HOST", "unix://"+sock)

		tp := &fakeControlPlaneTransport{
			respondToken:       "jwt",
			respondTTL:         time.Second,
			declineFromRequest: 2,
			respondReason:      "issuer unavailable",
		}
		config := sandboxIdentityFailClosedConfig(t, "cleanup-error-reexchange-test")

		_, err := BuildLoopWithTransport(context.Background(), config, tp)

		must.ErrorIs(err, errRetainedOnClose)
		must.ErrorIs(err, sandboxidentity.ErrDeclined)
		must.ErrorContains(err, "could not be re-exchanged")
		must.ErrorContains(err, "; cleanup: ")
	})

	t.Run("later build step fails", func(t *testing.T) {
		must := require.New(t)
		closeErrExecutors(t)
		sock, _, cleanupEngine := fakeDockerEngine(t)
		defer cleanupEngine()
		t.Setenv("DOCKER_HOST", "unix://"+sock)

		tp := &fakeControlPlaneTransport{respondToken: "jwt"}
		config := sandboxIdentityFailClosedConfig(t, "cleanup-error-later-step-test")
		config.TraceEmitter = types.TraceEmitterConfig{
			Type:     "otel",
			Endpoint: "http://127.0.0.1:1/v1/traces",
			Protocol: "http/protobuf",
			Headers:  map[string]string{"Authorization": "secret://STIRRUP_TEST_UNSET_HEADER_SECRET"},
		}

		_, err := BuildLoopWithTransport(context.Background(), config, tp)

		must.ErrorContains(err, "resolve trace emitter headers")
		must.ErrorIs(err, errRetainedOnClose)
	})

	t.Run("successful build is not affected by a failing close", func(t *testing.T) {
		must := require.New(t)
		closeErrExecutors(t)
		sock, _, cleanupEngine := fakeDockerEngine(t)
		defer cleanupEngine()
		t.Setenv("DOCKER_HOST", "unix://"+sock)

		tp := &fakeControlPlaneTransport{respondToken: "jwt"}
		config := sandboxIdentityFailClosedConfig(t, "cleanup-error-success-test")

		loop, err := BuildLoopWithTransport(context.Background(), config, tp)

		must.NoError(err)
		must.ErrorContains(loop.Close(), errRetainedOnClose.Error(), "the close error surfaces at shutdown instead")
	})
}
