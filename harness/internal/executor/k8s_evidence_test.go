package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// podCreateRecord holds the Pod the executor submitted and the Pod or error
// the API server returned for its first Create.
type podCreateRecord struct {
	mu        sync.Mutex
	submitted *corev1.Pod
	created   *corev1.Pod
	createErr error
}

func (r *podCreateRecord) pods() (submitted, created *corev1.Pod) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.submitted, r.created
}

func (r *podCreateRecord) err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.createErr
}

// recordingClientset is a clientset whose Pod Create calls are recorded, so
// a test can keep the exact submitted Pod and Create response the
// constructor works from.
type recordingClientset struct {
	kubernetes.Interface
	record *podCreateRecord
}

func (c recordingClientset) CoreV1() typedcorev1.CoreV1Interface {
	return recordingCoreV1{CoreV1Interface: c.Interface.CoreV1(), record: c.record}
}

type recordingCoreV1 struct {
	typedcorev1.CoreV1Interface
	record *podCreateRecord
}

func (c recordingCoreV1) Pods(namespace string) typedcorev1.PodInterface {
	return recordingPods{PodInterface: c.CoreV1Interface.Pods(namespace), record: c.record}
}

type recordingPods struct {
	typedcorev1.PodInterface
	record *podCreateRecord
}

func (p recordingPods) Create(ctx context.Context, pod *corev1.Pod, opts metav1.CreateOptions) (*corev1.Pod, error) {
	created, err := p.PodInterface.Create(ctx, pod, opts)
	p.record.mu.Lock()
	defer p.record.mu.Unlock()
	if p.record.submitted == nil {
		p.record.submitted = pod.DeepCopy()
		p.record.createErr = err
		// The typed client returns an empty Pod alongside an error.
		if err == nil && created != nil {
			p.record.created = created.DeepCopy()
		}
	}
	return created, err
}

// evidenceDir writes run evidence into the directory named by
// STIRRUP_TEST_EVIDENCE_DIR; with the variable unset it writes nothing.
type evidenceDir string

func evidenceDirFromEnv() evidenceDir { return evidenceDir(os.Getenv("STIRRUP_TEST_EVIDENCE_DIR")) }

// writeJSON writes v as indented JSON to name in the directory.
func (d evidenceDir) writeJSON(t *testing.T, name string, v any) {
	t.Helper()
	if d == "" {
		return
	}
	must := require.New(t)
	data, err := json.MarshalIndent(v, "", "  ")
	must.NoError(err)
	must.NoError(os.MkdirAll(string(d), 0o750))
	must.NoError(os.WriteFile(filepath.Join(string(d), name), append(data, '\n'), 0o600))
	t.Logf("evidence written to %s", filepath.Join(string(d), name))
}

// writeText writes s to name in the directory.
func (d evidenceDir) writeText(t *testing.T, name, s string) {
	t.Helper()
	if d == "" {
		return
	}
	must := require.New(t)
	must.NoError(os.MkdirAll(string(d), 0o750))
	must.NoError(os.WriteFile(filepath.Join(string(d), name), []byte(s), 0o600))
}

// evidenceEnvValue replaces every environment value in a written Pod.
const evidenceEnvValue = "[value omitted from evidence]"

// writePod writes pod as the API object the cluster returned, with
// apiVersion and kind set, and with every env[].value replaced by
// evidenceEnvValue so a value an admission webhook injected never reaches
// the file; names and valueFrom references are kept.
func (d evidenceDir) writePod(t *testing.T, name string, pod *corev1.Pod) {
	t.Helper()
	if pod == nil {
		return
	}
	out := pod.DeepCopy()
	out.APIVersion, out.Kind = "v1", "Pod"
	blank := func(env []corev1.EnvVar) {
		for i := range env {
			if env[i].Value != "" {
				env[i].Value = evidenceEnvValue
			}
		}
	}
	for i := range out.Spec.InitContainers {
		blank(out.Spec.InitContainers[i].Env)
	}
	for i := range out.Spec.Containers {
		blank(out.Spec.Containers[i].Env)
	}
	for i := range out.Spec.EphemeralContainers {
		blank(out.Spec.EphemeralContainers[i].Env)
	}
	d.writeJSON(t, name, out)
}

// writeCreateEvidence writes the recorded submitted Pod as
// submitted-pod.json and, when a create was made, the Create response as
// create-response.json or the Create error as create-error.txt.
func (d evidenceDir) writeCreateEvidence(t *testing.T, record *podCreateRecord) {
	t.Helper()
	submitted, created := record.pods()
	if submitted == nil {
		return
	}
	d.writePod(t, "submitted-pod.json", submitted)
	d.writePod(t, "create-response.json", created)
	if err := record.err(); err != nil {
		d.writeText(t, "create-error.txt", err.Error()+"\n")
	}
}

func TestEvidenceDir_WritesThePodsTheConstructorWorkedFrom(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	t.Setenv("STIRRUP_TEST_EVIDENCE_DIR", dir)
	cs := fake.NewClientset()
	admitPods(cs, nil)
	record := &podCreateRecord{}
	cfg := nitroFixtureCfg()
	calls := 0

	exec, err := newFakeK8sExecutorWithExec(context.Background(), recordingClientset{Interface: cs, record: record}, cfg, guestExec(isolatedGuestDump, nil, &calls))
	must.NoError(err)
	ready, err := cs.CoreV1().Pods(cfg.Namespace).Get(context.Background(), exec.podName, metav1.GetOptions{})
	must.NoError(err)
	evidenceDirFromEnv().writeCreateEvidence(t, record)
	evidenceDirFromEnv().writePod(t, "ready-pod.json", ready)
	evidenceDirFromEnv().writeText(t, "workspace-df.txt", "Filesystem 1K-blocks\n")
	must.NoError(exec.Close())

	for _, name := range []string{"submitted-pod.json", "create-response.json", "ready-pod.json"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		must.NoError(err)
		var pod corev1.Pod
		must.NoError(json.Unmarshal(data, &pod), name)
		must.Equal(exec.podName, pod.Name, name)
		must.Equal("v1", pod.APIVersion, name)
		must.Equal("Pod", pod.Kind, name)
	}
	must.NoFileExists(filepath.Join(dir, "create-error.txt"))
	df, err := os.ReadFile(filepath.Join(dir, "workspace-df.txt"))
	must.NoError(err)
	must.Equal("Filesystem 1K-blocks\n", string(df))
	submitted, created := record.pods()
	must.Empty(submitted.UID, "the submitted Pod is recorded before the API server assigns a UID")
	must.Equal("uid-1", string(created.UID))
	must.Equal(created.Spec.Overhead, ready.Spec.Overhead)
}

func TestEvidenceDir_OmitsEnvironmentValuesAdmissionInjected(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	cs := fake.NewClientset()
	admitPods(cs, func(p *corev1.Pod) {
		p.Spec.Containers[0].Env = append(p.Spec.Containers[0].Env,
			corev1.EnvVar{Name: "INJECTED", Value: "injected-secret-value"},
			corev1.EnvVar{Name: "FROM_SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{Key: "k"}}})
	})
	record := &podCreateRecord{}

	_, err := newFakeK8sExecutorWithExec(context.Background(), recordingClientset{Interface: cs, record: record}, nitroFixtureCfg(), nil)
	must.ErrorContains(err, "spec.containers[agent].env")
	evidenceDir(dir).writeCreateEvidence(t, record)

	data, err := os.ReadFile(filepath.Join(dir, "create-response.json"))
	must.NoError(err)
	must.NotContains(string(data), "injected-secret-value")
	var pod corev1.Pod
	must.NoError(json.Unmarshal(data, &pod))
	must.Equal([]corev1.EnvVar{
		{Name: "INJECTED", Value: evidenceEnvValue},
		{Name: "FROM_SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{Key: "k"}}},
	}, pod.Spec.Containers[0].Env)
	_, created := record.pods()
	must.Equal("injected-secret-value", created.Spec.Containers[0].Env[0].Value, "the record itself is not changed")
}

func TestEvidenceDir_WritesTheCreateError(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	cs := fake.NewClientset()
	failVerb(cs, "create", "pods", errForbiddenPods)
	record := &podCreateRecord{}

	_, err := newFakeK8sExecutorWithExec(context.Background(), recordingClientset{Interface: cs, record: record}, nitroFixtureCfg(), nil)
	must.Error(err)
	evidenceDir(dir).writeCreateEvidence(t, record)

	got, err := os.ReadFile(filepath.Join(dir, "create-error.txt"))
	must.NoError(err)
	must.Equal(errForbiddenPods.Error()+"\n", string(got))
	must.FileExists(filepath.Join(dir, "submitted-pod.json"))
	must.NoFileExists(filepath.Join(dir, "create-response.json"))
}
