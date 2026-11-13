package executor

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// submittedPod returns the sandbox Pod the executor would submit for
// testK8sPodName with the given RuntimeClass.
func submittedPod(runtimeClass string) *corev1.Pod {
	cfg := baseSandboxCfg()
	cfg.Namespace = testK8sNamespace
	cfg.RuntimeClassName = runtimeClass
	cfg.Lifecycle.RunID = testK8sRunID
	return buildSandboxPod(cfg, testK8sPodName, nil)
}

// admittedCopy returns submitted as the API server would return it, with a
// UID assigned.
func admittedCopy(submitted *corev1.Pod) *corev1.Pod {
	admitted := submitted.DeepCopy()
	admitted.UID = "uid-1"
	return admitted
}

func TestPodAdmission_AcceptsUnmodifiedPod(t *testing.T) {
	must := require.New(t)
	submitted := submittedPod("gvisor")
	admitted := admittedCopy(submitted)
	// Ordinary API defaulting is not a contract violation.
	admitted.Spec.SchedulerName = "default-scheduler"
	admitted.Labels["added-by-webhook"] = "true"
	must.NoError(podAdmission{submitted: submitted, uid: "uid-1", checks: baseAdmissionChecks}.check(admitted))
}

func TestPodAdmission_RejectsChangedRuntimeClass(t *testing.T) {
	cases := map[string]*string{
		"rewritten": ptr.To("runc"),
		"removed":   nil,
	}
	for name, admittedClass := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			submitted := submittedPod("gvisor")
			admitted := admittedCopy(submitted)
			admitted.Spec.RuntimeClassName = admittedClass

			err := podAdmission{submitted: submitted, checks: baseAdmissionChecks}.check(admitted)

			must.Error(err)
			must.ErrorContains(err, `spec.runtimeClassName: expected "gvisor", observed `+optionalQuoted(admittedClass))
			must.ErrorContains(err, testK8sNamespace+"/"+testK8sPodName)
		})
	}
}

func TestPodAdmission_AcceptsDefaultedRuntimeClassWhenNoneRequested(t *testing.T) {
	must := require.New(t)
	submitted := submittedPod("")
	admitted := admittedCopy(submitted)
	admitted.Spec.RuntimeClassName = ptr.To("cluster-default")
	must.NoError(podAdmission{submitted: submitted, checks: baseAdmissionChecks}.check(admitted))
}

func TestPodAdmission_RejectsChangedSelectorLabels(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(labels map[string]string)
		wantErr string
	}{
		{
			name:    "pod name label rewritten",
			mutate:  func(l map[string]string) { l[k8sPodNameLabel] = "other" },
			wantErr: `metadata.labels[stirrup.dev/pod]: expected "` + testK8sPodName + `", observed "other"`,
		},
		{
			name:    "pod name label removed",
			mutate:  func(l map[string]string) { delete(l, k8sPodNameLabel) },
			wantErr: `metadata.labels[stirrup.dev/pod]: expected "` + testK8sPodName + `", observed absent`,
		},
		{
			name:    "sandbox marker rewritten",
			mutate:  func(l map[string]string) { l[k8sSandboxLabel] = "false" },
			wantErr: `metadata.labels[stirrup-sandbox]: expected "true", observed "false"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			submitted := submittedPod("gvisor")
			admitted := admittedCopy(submitted)
			tc.mutate(admitted.Labels)

			err := podAdmission{submitted: submitted, checks: baseAdmissionChecks}.check(admitted)
			must.ErrorContains(err, tc.wantErr)
		})
	}
}

func TestPodAdmission_ReportsEveryMismatch(t *testing.T) {
	must := require.New(t)
	submitted := submittedPod("gvisor")
	admitted := admittedCopy(submitted)
	admitted.Spec.RuntimeClassName = ptr.To("runc")
	admitted.Labels[k8sPodNameLabel] = "other"

	err := podAdmission{submitted: submitted, checks: baseAdmissionChecks}.check(admitted)
	must.Error(err)
	must.ErrorContains(err, "spec.runtimeClassName")
	must.ErrorContains(err, "metadata.labels[stirrup.dev/pod]")
}

func TestPodAdmission_RequiresServerAssignedMatchingUID(t *testing.T) {
	t.Run("missing uid", func(t *testing.T) {
		must := require.New(t)
		submitted := submittedPod("gvisor")
		err := podAdmission{submitted: submitted, checks: baseAdmissionChecks}.check(submitted.DeepCopy())
		must.ErrorContains(err, "metadata.uid: expected a server-assigned UID, observed none")
	})

	t.Run("different uid", func(t *testing.T) {
		must := require.New(t)
		submitted := submittedPod("gvisor")
		admitted := admittedCopy(submitted)
		admitted.UID = "uid-2"
		err := podAdmission{submitted: submitted, uid: "uid-1", checks: baseAdmissionChecks}.check(admitted)
		must.ErrorContains(err, "metadata.uid: expected uid-1, observed uid-2")
	})
}

func TestPodAdmission_BoundsObservedValues(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	submitted := submittedPod("gvisor")
	admitted := admittedCopy(submitted)
	admitted.Spec.RuntimeClassName = ptr.To("evil\n" + strings.Repeat("x", 4*k8sClusterTextMax))

	err := podAdmission{submitted: submitted, checks: baseAdmissionChecks}.check(admitted)
	must.Error(err)
	check.NotContains(err.Error(), "\n")
	check.Less(len(err.Error()), k8sClusterTextMax+300)
}
