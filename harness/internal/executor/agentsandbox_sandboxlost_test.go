package executor

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// failingExecs makes every pods/exec of the executor fail as it does once
// the container is gone.
func failingExecs(exec *AgentSandboxExecutor) {
	exec.lossSettle = time.Millisecond
	exec.execFn = func(context.Context, []string, io.Reader, io.Writer, io.Writer) error {
		return errContainerNotFound
	}
}

func TestAgentSandboxExecutor_Exec_ReportsReplacedBackingPodAsLost(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	f := newFakeAgentSandbox(t)
	f.admitSandboxes(nil)
	exec, _, err := f.build(t, fastK8sTiming)
	must.NoError(err)
	failingExecs(exec)

	replacement, err := f.cs.CoreV1().Pods(testK8sNamespace).Get(context.Background(), exec.podName, metav1.GetOptions{})
	must.NoError(err)
	replacement.UID = "uid-replacement"
	must.NoError(f.cs.Tracker().Update(podsResource, replacement, testK8sNamespace))

	_, err = exec.Exec(context.Background(), "true", time.Second)
	var lost *SandboxLostError
	must.ErrorAs(err, &lost)
	check.Equal("sandbox pod "+testK8sNamespace+"/"+exec.podName+" was replaced by another pod with the same name", lost.State)
	must.Equal(lost, exec.SandboxLost())
}

func TestAgentSandboxExecutor_Exec_ReportsEndedBackingPodAsLost(t *testing.T) {
	must := require.New(t)
	f := newFakeAgentSandbox(t)
	f.admitSandboxes(nil)
	exec, _, err := f.build(t, fastK8sTiming)
	must.NoError(err)
	failingExecs(exec)

	ended := endedPod(corev1.PodFailed, "", "", "OOMKilled", 137)
	ended.Name = exec.podName
	ended.UID = "uid-backing"
	must.NoError(f.cs.Tracker().Update(podsResource, ended, testK8sNamespace))

	_, err = exec.Exec(context.Background(), "true", time.Second)
	var lost *SandboxLostError
	must.ErrorAs(err, &lost)
	must.Equal("sandbox pod "+testK8sNamespace+"/"+exec.podName+" has failed: container agent was OOMKilled (exit code 137)", lost.State)
}
