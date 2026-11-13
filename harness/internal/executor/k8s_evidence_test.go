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

// podCreateRecord holds the Pod the executor submitted and the Pod the API
// server returned for its first Create.
type podCreateRecord struct {
	mu        sync.Mutex
	submitted *corev1.Pod
	created   *corev1.Pod
}

func (r *podCreateRecord) pods() (submitted, created *corev1.Pod) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.submitted, r.created
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
		if created != nil {
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

// writeCreateEvidence writes the recorded submitted Pod and Create response
// as submitted-pod.json and create-response.json, when a create was made.
func (d evidenceDir) writeCreateEvidence(t *testing.T, record *podCreateRecord) {
	t.Helper()
	submitted, created := record.pods()
	if submitted == nil {
		return
	}
	d.writeJSON(t, "submitted-pod.json", submitted)
	d.writeJSON(t, "create-response.json", created)
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
	evidenceDirFromEnv().writeJSON(t, "ready-pod.json", ready)
	evidenceDirFromEnv().writeText(t, "workspace-df.txt", "Filesystem 1K-blocks\n")
	must.NoError(exec.Close())

	for _, name := range []string{"submitted-pod.json", "create-response.json", "ready-pod.json"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		must.NoError(err)
		var pod corev1.Pod
		must.NoError(json.Unmarshal(data, &pod), name)
		must.Equal(exec.podName, pod.Name, name)
	}
	df, err := os.ReadFile(filepath.Join(dir, "workspace-df.txt"))
	must.NoError(err)
	must.Equal("Filesystem 1K-blocks\n", string(df))
	submitted, created := record.pods()
	must.Empty(submitted.UID, "the submitted Pod is recorded before the API server assigns a UID")
	must.Equal("uid-1", string(created.UID))
	must.Equal(created.Spec.Overhead, ready.Spec.Overhead)
}
