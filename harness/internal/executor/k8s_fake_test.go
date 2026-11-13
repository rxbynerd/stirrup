package executor

import (
	"bytes"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

const (
	testK8sNamespace = "stirrup-sandbox"
	testK8sPodName   = "stirrup-0123456789ab"
	testK8sRunID     = "run-1"
)

var (
	podsResource            = corev1.SchemeGroupVersion.WithResource("pods")
	networkPoliciesResource = networkingv1.SchemeGroupVersion.WithResource("networkpolicies")
)

// fastK8sTiming keeps cluster waits in unit tests to milliseconds.
var fastK8sTiming = k8sTiming{
	readyTimeout:    200 * time.Millisecond,
	pollPeriod:      time.Millisecond,
	teardownTimeout: 100 * time.Millisecond,
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testLogger returns a logger whose text output the test can inspect.
func testLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// readyPod returns a sandbox Pod named testK8sPodName with the given UID that
// is Running with the agent container Ready.
func readyPod(uid k8stypes.UID) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testK8sPodName,
			Namespace: testK8sNamespace,
			UID:       uid,
			Labels:    podLabels(testK8sPodName, testK8sRunID),
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: k8sAgentContainer, Ready: true}},
		},
	}
}

// testPolicy returns the deny-all egress policy for testK8sPodName.
func testPolicy() *networkingv1.NetworkPolicy {
	return denyAllEgressPolicy(testK8sNamespace, testK8sPodName, testK8sRunID)
}

// testTeardown returns a podTeardown over cs for testK8sPodName.
func testTeardown(cs *fake.Clientset, uid k8stypes.UID, logger *slog.Logger) podTeardown {
	return podTeardown{
		clientset:  cs,
		namespace:  testK8sNamespace,
		podName:    testK8sPodName,
		podUID:     uid,
		policyName: networkPolicyName(testK8sPodName),
		runID:      testK8sRunID,
		logger:     logger,
		timing:     fastK8sTiming,
	}
}

// trackerHas reports whether the fake's tracker holds the named object in
// testK8sNamespace.
func trackerHas(t *testing.T, cs *fake.Clientset, gvr schema.GroupVersionResource, name string) bool {
	t.Helper()
	_, err := cs.Tracker().Get(gvr, testK8sNamespace, name)
	if apierrors.IsNotFound(err) {
		return false
	}
	require.New(t).NoError(err, "tracker get %s %s", gvr.Resource, name)
	return true
}

// actionsOf returns the verbs recorded against resource, in order.
func actionsOf(cs *fake.Clientset, resource string) []clienttesting.Action {
	var out []clienttesting.Action
	for _, a := range cs.Actions() {
		if a.GetResource().Resource == resource {
			out = append(out, a)
		}
	}
	return out
}

// podDeletes returns the DeleteOptions of every Pod delete the fake saw.
func podDeletes(cs *fake.Clientset) []metav1.DeleteOptions {
	var out []metav1.DeleteOptions
	for _, a := range actionsOf(cs, "pods") {
		if d, ok := a.(clienttesting.DeleteActionImpl); ok {
			out = append(out, d.DeleteOptions)
		}
	}
	return out
}

// countVerb counts the actions with verb against resource.
func countVerb(cs *fake.Clientset, verb, resource string) int {
	n := 0
	for _, a := range actionsOf(cs, resource) {
		if a.GetVerb() == verb {
			n++
		}
	}
	return n
}

// ignorePodDeletes makes every Pod delete succeed without removing the
// object, as a delete does while the kubelet has not confirmed termination.
func ignorePodDeletes(cs *fake.Clientset) {
	cs.PrependReactor("delete", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})
}

// failVerb makes every verb on resource fail with err.
func failVerb(cs *fake.Clientset, verb, resource string, err error) {
	cs.PrependReactor(verb, resource, func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, err
	})
}

var errForbiddenPods = apierrors.NewForbidden(podsResource.GroupResource(), testK8sPodName, errors.New("denied by RBAC"))
