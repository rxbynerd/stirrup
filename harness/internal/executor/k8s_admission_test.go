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
	// Ordinary API defaulting and added labels are not contract violations.
	admitted.Spec.SchedulerName = "default-scheduler"
	admitted.Labels["added-by-webhook"] = "true"
	admission := newPodAdmission(submitted, admitted)
	must.NoError(admission.checkCreate())

	observed := admitted.DeepCopy()
	observed.Spec.NodeName = "node-1"
	must.NoError(admission.checkPoll(observed))
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

			err := newPodAdmission(submitted, admitted).checkCreate()

			must.Error(err)
			must.ErrorContains(err, `spec.runtimeClassName: expected "gvisor", observed `+optionalQuoted(admittedClass))
			must.ErrorContains(err, "admitted pod "+testK8sNamespace+"/"+testK8sPodName+" does not match the submitted pod")
		})
	}
}

func TestPodAdmission_AcceptsDefaultedRuntimeClassWhenNoneRequested(t *testing.T) {
	must := require.New(t)
	submitted := submittedPod("")
	admitted := admittedCopy(submitted)
	admitted.Spec.RuntimeClassName = ptr.To("cluster-default")
	admission := newPodAdmission(submitted, admitted)
	must.NoError(admission.checkCreate())

	observed := admitted.DeepCopy()
	observed.Spec.RuntimeClassName = ptr.To("runc")
	must.ErrorContains(admission.checkPoll(observed), `spec.runtimeClassName: expected "cluster-default", observed "runc"`,
		"the admitted class is the baseline for the readiness wait")
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

			must.ErrorContains(newPodAdmission(submitted, admitted).checkCreate(), tc.wantErr)
		})
	}
}

func TestPodAdmission_RendersMissingExpectedLabelAsAbsent(t *testing.T) {
	must := require.New(t)
	submitted := submittedPod("gvisor")
	delete(submitted.Labels, k8sSandboxLabel)
	admitted := admittedCopy(submitted)
	admitted.Labels[k8sSandboxLabel] = "true"

	must.ErrorContains(newPodAdmission(submitted, admitted).checkCreate(), `metadata.labels[stirrup-sandbox]: expected absent, observed "true"`)
}

func TestPodAdmission_ReportsEveryMismatch(t *testing.T) {
	must := require.New(t)
	submitted := submittedPod("gvisor")
	admitted := admittedCopy(submitted)
	admitted.Spec.RuntimeClassName = ptr.To("runc")
	admitted.Labels[k8sPodNameLabel] = "other"

	err := newPodAdmission(submitted, admitted).checkCreate()
	must.Error(err)
	must.ErrorContains(err, "spec.runtimeClassName")
	must.ErrorContains(err, "metadata.labels[stirrup.dev/pod]")
}

func TestPodAdmission_ChecksUIDAtEachStage(t *testing.T) {
	t.Run("create response without a uid", func(t *testing.T) {
		must := require.New(t)
		submitted := submittedPod("gvisor")
		err := newPodAdmission(submitted, submitted.DeepCopy()).checkCreate()
		must.ErrorContains(err, "metadata.uid: expected a server-assigned UID, observed none")
	})

	t.Run("observed pod with a different uid", func(t *testing.T) {
		must := require.New(t)
		submitted := submittedPod("gvisor")
		admitted := admittedCopy(submitted)
		observed := admitted.DeepCopy()
		observed.UID = "uid-2"
		err := newPodAdmission(submitted, admitted).checkPoll(observed)
		must.ErrorContains(err, "observed pod "+testK8sNamespace+"/"+testK8sPodName+" no longer matches the admitted pod")
		must.ErrorContains(err, "metadata.uid: expected uid-1, observed uid-2")
	})
}

func TestPodAdmission_RunsEachStageAgainstItsBaseline(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	submitted := submittedPod("gvisor")
	admitted := admittedCopy(submitted)
	observed := admitted.DeepCopy()

	var createSaw, pollSaw [][2]*corev1.Pod
	admission := newPodAdmission(submitted, admitted)
	admission.createChecks = []admissionCheck{func(expected, got *corev1.Pod) []admissionMismatch {
		createSaw = append(createSaw, [2]*corev1.Pod{expected, got})
		return nil
	}}
	admission.pollChecks = []admissionCheck{func(expected, got *corev1.Pod) []admissionMismatch {
		pollSaw = append(pollSaw, [2]*corev1.Pod{expected, got})
		return nil
	}}

	must.NoError(admission.checkCreate())
	must.Len(createSaw, 1)
	check.Empty(pollSaw, "a poll-time check ran at creation")
	check.Same(submitted, createSaw[0][0], "the create-time baseline is the submitted pod")
	check.Same(admitted, createSaw[0][1])

	must.NoError(admission.checkPoll(observed))
	must.Len(pollSaw, 1)
	check.Len(createSaw, 1, "a create-time check ran during the readiness wait")
	check.Same(admitted, pollSaw[0][0], "the poll-time baseline is the Create response")
	check.Same(observed, pollSaw[0][1])
}

func TestNewPodAdmission_CopiesTheBaseChecks(t *testing.T) {
	must := require.New(t)
	submitted := submittedPod("gvisor")
	admission := newPodAdmission(submitted, admittedCopy(submitted))
	admission.createChecks[0] = nil
	must.NotNil(baseAdmissionChecks[0], "changing one admission's checks changed the shared base list")
}

func TestPodAdmission_BoundsObservedValues(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	submitted := submittedPod("gvisor")
	admitted := admittedCopy(submitted)
	admitted.Spec.RuntimeClassName = ptr.To("evil\n" + strings.Repeat("x", 4*k8sClusterTextMax))

	err := newPodAdmission(submitted, admitted).checkCreate()
	must.Error(err)
	check.NotContains(err.Error(), "\n")
	check.Less(len(err.Error()), k8sClusterTextMax+300)
}
