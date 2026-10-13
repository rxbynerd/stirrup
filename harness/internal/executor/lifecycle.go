package executor

import (
	"math"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

const (
	// SandboxGCMargin is added to the longest time a session can legitimately
	// run to give a sandbox's own deadline, so a graceful Close always wins
	// and the deadline only matters after an unclean harness exit.
	SandboxGCMargin = 10 * time.Minute

	// Container labels identifying a sandbox and the run that created it, so
	// a leaked container can be found with `--filter label=...`.
	containerLabelSandbox = "stirrup.sandbox"
	containerLabelRunID   = "stirrup.run-id"

	// k8sRunIDLabel carries the run ID on the sandbox Pod and its egress
	// NetworkPolicy, so both can be selected together.
	k8sRunIDLabel = "stirrup.dev/run-id"

	// k8sLabelValueMax is the longest label value the API server accepts.
	k8sLabelValueMax = 63

	// Downward-API environment variables naming the pod the harness itself
	// runs in. All three must be set for the sandbox Pod to carry an owner
	// reference.
	EnvPodName      = "STIRRUP_POD_NAME"
	EnvPodUID       = "STIRRUP_POD_UID"
	EnvPodNamespace = "STIRRUP_POD_NAMESPACE"
)

// Lifecycle identifies a sandbox's run and bounds how long it survives an
// unclean harness exit.
type Lifecycle struct {
	// RunID is stamped on the sandbox as a label. Follow-up runs in the same
	// session reuse the sandbox, so it holds the session's first run ID.
	RunID string
	// MaxLifetime caps the sandbox's life from creation. Zero leaves it
	// unbounded.
	MaxLifetime time.Duration
}

// seconds returns MaxLifetime rounded up to whole seconds, and false when no
// bound is set.
func (l Lifecycle) seconds() (int64, bool) {
	if l.MaxLifetime <= 0 {
		return 0, false
	}
	return int64(math.Ceil(l.MaxLifetime.Seconds())), true
}

// k8sLabelValue returns runID as a valid Kubernetes label value: truncated to
// 63 characters with trailing separators trimmed. RunConfig validation already
// restricts RunID to [a-zA-Z0-9_-] with an alphanumeric first character.
func k8sLabelValue(runID string) string {
	if len(runID) > k8sLabelValueMax {
		runID = runID[:k8sLabelValueMax]
	}
	return strings.TrimRight(runID, "-_.")
}

// HarnessPod names the pod the harness runs in. A sandbox Pod in the same
// namespace is owned by it so deleting the harness pod deletes the sandbox.
type HarnessPod struct {
	Namespace string
	Name      string
	UID       string
}

// HarnessPodFromEnv reads the downward-API variables EnvPodName, EnvPodUID and
// EnvPodNamespace. It returns nil unless all three are set.
func HarnessPodFromEnv(getenv func(string) string) *HarnessPod {
	p := &HarnessPod{
		Namespace: getenv(EnvPodNamespace),
		Name:      getenv(EnvPodName),
		UID:       getenv(EnvPodUID),
	}
	if p.Namespace == "" || p.Name == "" || p.UID == "" {
		return nil
	}
	return p
}

// ownerReferences returns the owner reference for an object created in
// namespace, or nil when the harness pod is unknown or in another namespace:
// owner references cannot cross namespaces.
func (p *HarnessPod) ownerReferences(namespace string) []metav1.OwnerReference {
	if p == nil || p.Namespace != namespace {
		return nil
	}
	return []metav1.OwnerReference{{
		APIVersion: "v1",
		Kind:       "Pod",
		Name:       p.Name,
		UID:        k8stypes.UID(p.UID),
	}}
}
