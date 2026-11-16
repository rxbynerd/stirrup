package executor

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// The kubelet reports a killed container up to a couple of seconds after the
// runtime removed it, so the default settle window must outlast that.
func TestPodExecCore_Exec_DefaultSettleOutlastsTheKubeletStatusLag(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	core, cs, _ := lostTestCore(nil, errContainerNotFound)
	core.lossSettle = 0
	var reads atomic.Int32
	cs.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		if reads.Add(1) < 3 {
			return true, readyPod("uid-1"), nil
		}
		return true, endedPod(corev1.PodFailed, "", "", "OOMKilled", 137), nil
	})

	_, err := core.Exec(context.Background(), "true", time.Minute)
	var lost *SandboxLostError
	must.ErrorAs(err, &lost)
	check.Contains(lost.State, "OOMKilled (exit code 137)")
	check.Equal(int32(3), reads.Load())
}

func TestWaitForExecReachable_FailedProbeDoesNotReadThePodOrRecordLoss(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	logger, _ := testLogger()
	cs := fake.NewClientset(endedPod(corev1.PodFailed, "", "", "OOMKilled", 137))
	var probes atomic.Int32
	core := &podExecCore{
		clientset: cs,
		namespace: testK8sNamespace,
		podName:   testK8sPodName,
		execFn: func(context.Context, []string, io.Reader, io.Writer, io.Writer) error {
			probes.Add(1)
			return errContainerNotFound
		},
	}

	must.NoError(waitForExecReachable(context.Background(), core, fastK8sTiming, logger))

	check.Equal(int32(1), probes.Load())
	check.Equal(0, countVerb(cs, "get", "pods"), "the readiness wait does not read the Pod to classify a probe failure")
	must.NoError(core.SandboxLost())
}
