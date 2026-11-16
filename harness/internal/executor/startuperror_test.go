package executor

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/rxbynerd/stirrup/types"
)

// startupReason returns the class err carries, or "" when it has none.
func startupReason(err error) string {
	var classified *StartupError
	if errors.As(err, &classified) {
		return classified.Reason
	}
	return ""
}

func TestStartupError_KeepsTextAndFirstReason(t *testing.T) {
	must := require.New(t)
	cause := errors.New("pull access denied")

	err := startupErr(types.BuildFailureSandboxImage, cause)
	again := startupErr(types.BuildFailureSandboxPlatform, err)

	must.Equal("pull access denied", err.Error(), "the text is the wrapped error's")
	must.ErrorIs(again, cause)
	must.Equal(types.BuildFailureSandboxImage, startupReason(again), "the first class wins")
	must.NoError(startupErr(types.BuildFailureSandboxImage, nil))
}

func TestClassifyK8sStartup_ClassifiesByErrorType(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"admission mismatch", &admissionMismatchError{prefix: "admitted pod", mismatches: []admissionMismatch{{field: "spec.runtimeClassName"}}}, types.BuildFailureAdmissionMismatch},
		{"runtime diagnostic", &k8sRuntimeDiagnosticError{check: "dmi_product_name", reason: "not an enclave"}, types.BuildFailureRuntimeDiagnosticFailed},
		{"diagnostic exec failed", &k8sRuntimeDiagnosticError{check: diagnosticCheckExec, reason: "dial backend"}, types.BuildFailureSandboxNotReady},
		{"API rejection", boundClusterErr(apierrors.NewForbidden(podsResource.GroupResource(), "p", errors.New("rbac"))), types.BuildFailureSandboxPlatform},
		{"anything else", errors.Join(errPodNotReady, context.DeadlineExceeded), types.BuildFailureUnclassified},
		{"already classified", startupErr(types.BuildFailureSandboxImage, errPodNotReady), types.BuildFailureSandboxImage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, startupReason(classifyK8sStartup(tc.err, types.BuildFailureUnclassified)))
		})
	}
}

func TestWaitForPodReady_ClassifiesImageFailures(t *testing.T) {
	terminated := []corev1.ContainerStatus{{
		Name:  k8sAgentContainer,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 127}},
	}}
	oomKilled := []corev1.ContainerStatus{{
		Name:  k8sAgentContainer,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}},
	}}
	cases := []struct {
		name     string
		statuses []corev1.ContainerStatus
		timing   k8sTiming
		want     string
	}{
		{"pull back-off until the timeout", agentWaiting("ImagePullBackOff", "Back-off pulling image"), fastK8sTiming, types.BuildFailureSandboxImage},
		{"crash loop until the timeout", agentWaiting("CrashLoopBackOff", "back-off restarting"), fastK8sTiming, types.BuildFailureSandboxImage},
		{"invalid image name", agentWaiting("InvalidImageName", "bad"), failFastTiming, types.BuildFailureSandboxImage},
		{"agent container exited", terminated, failFastTiming, types.BuildFailureSandboxImage},
		{"agent container killed for memory", oomKilled, failFastTiming, ""},
		{"still creating at the timeout", agentWaiting("ContainerCreating", ""), fastK8sTiming, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			pod := readyPod("uid-1")
			pod.Status.Phase = corev1.PodPending
			pod.Status.ContainerStatuses = tc.statuses
			cs := fake.NewClientset(pod)

			_, _, err := waitReady(t, cs, tc.timing)

			must.ErrorIs(err, errPodNotReady)
			must.Equal(tc.want, startupReason(err), "readiness leaves other failures to its caller")
		})
	}
}

func TestNewK8sExecutor_ClassifiesStartupFailures(t *testing.T) {
	t.Run("pod create rejected", func(t *testing.T) {
		must := require.New(t)
		cs := fake.NewClientset()
		failVerb(cs, "create", "pods", apierrors.NewForbidden(podsResource.GroupResource(), "p", errors.New("exceeded quota")))

		_, err := newFakeK8sExecutor(context.Background(), cs, constructorCfg("gvisor"))

		must.Equal(types.BuildFailureSandboxPlatform, startupReason(err))
	})

	t.Run("diagnostics failed", func(t *testing.T) {
		must := require.New(t)
		cs := fake.NewClientset()
		admitPods(cs, nil)
		cfg, _ := nitroConstructorCfg()
		calls := 0

		_, err := newFakeK8sExecutorWithExec(context.Background(), cs, cfg, guestExec("", nil, &calls))

		must.Equal(types.BuildFailureRuntimeDiagnosticFailed, startupReason(err))
	})
}

func TestClassifyContainerCreate_OnlyAMissingImageIsTheImages(t *testing.T) {
	must := require.New(t)
	missing := &apiStatusError{status: http.StatusNotFound, msg: "No such image: ghcr.io/x/y:1"}
	missingNetwork := &apiStatusError{status: http.StatusNotFound, msg: "network stirrup-egress not found"}
	conflict := &apiStatusError{status: http.StatusConflict, msg: "name in use"}

	must.Equal(types.BuildFailureSandboxImage, startupReason(classifyContainerCreate(missing)))
	must.Equal(types.BuildFailureSandboxPlatform, startupReason(classifyContainerCreate(missingNetwork)))
	must.Equal(types.BuildFailureSandboxPlatform, startupReason(classifyContainerCreate(conflict)))
	must.Equal(types.BuildFailureSandboxPlatform, startupReason(classifyContainerCreate(errors.New("dial unix: no such file"))))
}

func TestNewContainerExecutor_DisallowedImageIsAnImageFailure(t *testing.T) {
	must := require.New(t)

	_, err := NewContainerExecutorWithContext(context.Background(), ContainerExecutorConfig{
		Image:             "registry.invalid/sandbox:1",
		HostDir:           t.TempDir(),
		RegistryAllowlist: []string{"ghcr.io/"},
	})

	must.Error(err)
	must.Equal(types.BuildFailureSandboxImage, startupReason(err))
}
