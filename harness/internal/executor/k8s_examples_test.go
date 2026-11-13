package executor

import (
	"bytes"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// The reference namespace enforces the restricted Pod Security Standard, so
// the reference harness Job must satisfy it or its Pod is never created.
func TestK8sExamples_HarnessJobSatisfiesTheNamespacePodSecurityLevel(t *testing.T) {
	must := require.New(t)

	var namespace corev1.Namespace
	decodeExample(t, "namespace.yaml", &namespace)
	must.Equal("restricted", namespace.Labels["pod-security.kubernetes.io/enforce"])

	var job batchv1.Job
	decodeExample(t, "harness-job.yaml", &job)
	must.Equal(namespace.Name, job.Namespace)
	pod := job.Spec.Template.Spec

	must.NotNil(pod.SecurityContext, "pod securityContext")
	must.NotNil(pod.SecurityContext.RunAsNonRoot, "runAsNonRoot")
	must.True(*pod.SecurityContext.RunAsNonRoot, "runAsNonRoot")
	must.NotNil(pod.SecurityContext.SeccompProfile, "seccompProfile")
	must.Equal(corev1.SeccompProfileTypeRuntimeDefault, pod.SecurityContext.SeccompProfile.Type)

	must.NotEmpty(pod.Containers)
	for _, container := range pod.Containers {
		sc := container.SecurityContext
		must.NotNil(sc, "container %s securityContext", container.Name)
		must.NotNil(sc.AllowPrivilegeEscalation, "container %s allowPrivilegeEscalation", container.Name)
		must.False(*sc.AllowPrivilegeEscalation, "container %s allowPrivilegeEscalation", container.Name)
		must.NotNil(sc.Capabilities, "container %s capabilities", container.Name)
		must.Equal([]corev1.Capability{"ALL"}, sc.Capabilities.Drop, "container %s", container.Name)
		must.Empty(sc.Capabilities.Add, "container %s", container.Name)
	}
}

func decodeExample(t *testing.T, name string, into any) {
	t.Helper()
	must := require.New(t)
	raw, err := os.ReadFile("../../../examples/k8s/" + name)
	must.NoError(err)
	must.NoError(yaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096).Decode(into), "decode %s", name)
}
