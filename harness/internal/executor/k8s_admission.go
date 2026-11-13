package executor

import (
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// admissionMismatch is one field where an observed Pod departs from the Pod
// it is compared against.
type admissionMismatch struct {
	field    string
	expected string
	observed string
}

// admissionCheck compares one part of an observed Pod against an expected
// Pod. Checks report mismatches rather than errors so one failure names every
// departure at once.
type admissionCheck func(expected, observed *corev1.Pod) []admissionMismatch

// baseAdmissionChecks apply to every runtime, at both stages.
var baseAdmissionChecks = []admissionCheck{checkRuntimeClassKept, checkSelectorLabelsKept}

// podAdmission verifies the Pod contract in two stages. At creation it
// compares the submitted Pod with the Pod Create returned, which shows what
// admission did. During the readiness wait it compares that admitted Pod, as
// the baseline, with each Pod observed, which catches a replaced Pod or a
// later writer. Drift is caught only until the Pod is Ready, and the check
// runs after creation, so it cannot prevent a mutated Pod from starting.
type podAdmission struct {
	submitted    *corev1.Pod
	admitted     *corev1.Pod
	createChecks []admissionCheck
	pollChecks   []admissionCheck
}

// newPodAdmission returns the two-stage contract for a Pod submitted as
// submitted and returned by Create as admitted: the base checks, plus the
// whole-Pod admission checks of strictAdmissionChecks at both stages when the
// runtime profile sets strictAdmission. Ephemeral containers cannot be set at
// creation, so the whole-Pod admission check sees one only if it is added
// before the Pod is Ready.
func newPodAdmission(submitted, admitted *corev1.Pod, profile k8sRuntimeProfile) podAdmission {
	checks := slices.Clone(baseAdmissionChecks)
	if profile.strictAdmission {
		checks = append(checks, strictAdmissionChecks(profile)...)
	}
	return podAdmission{
		submitted:    submitted,
		admitted:     admitted,
		createChecks: checks,
		pollChecks:   slices.Clone(checks),
	}
}

// checkCreate compares the submitted Pod with the Create response.
func (a podAdmission) checkCreate() error {
	var mismatches []admissionMismatch
	if a.admitted.UID == "" {
		mismatches = append(mismatches, admissionMismatch{field: "metadata.uid", expected: "a server-assigned UID", observed: "none"})
	}
	for _, check := range a.createChecks {
		mismatches = append(mismatches, check(a.submitted, a.admitted)...)
	}
	return mismatchError(fmt.Sprintf("admitted pod %s/%s does not match the submitted pod", a.submitted.Namespace, a.submitted.Name), mismatches)
}

// checkPoll compares a Pod observed during the readiness wait with the
// Create response.
func (a podAdmission) checkPoll(observed *corev1.Pod) error {
	var mismatches []admissionMismatch
	if observed.UID != a.admitted.UID {
		mismatches = append(mismatches, admissionMismatch{field: "metadata.uid", expected: string(a.admitted.UID), observed: string(observed.UID)})
	}
	for _, check := range a.pollChecks {
		mismatches = append(mismatches, check(a.admitted, observed)...)
	}
	return mismatchError(fmt.Sprintf("observed pod %s/%s no longer matches the admitted pod", a.submitted.Namespace, a.submitted.Name), mismatches)
}

// admissionMismatchError is an admission contract failure at either stage.
type admissionMismatchError struct {
	prefix     string
	mismatches []admissionMismatch
}

func (e *admissionMismatchError) Error() string {
	parts := make([]string, len(e.mismatches))
	for i, m := range e.mismatches {
		parts[i] = fmt.Sprintf("%s: expected %s, observed %s", m.field, m.expected, clusterText(m.observed))
	}
	return fmt.Sprintf("%s: %s", e.prefix, strings.Join(parts, "; "))
}

func mismatchError(prefix string, mismatches []admissionMismatch) error {
	if len(mismatches) == 0 {
		return nil
	}
	return &admissionMismatchError{prefix: prefix, mismatches: mismatches}
}

// checkRuntimeClassKept requires an expected RuntimeClass to be unchanged. A
// class admission fills in when none was requested is a cluster default, not
// a violation, and becomes the baseline for the readiness wait.
func checkRuntimeClassKept(expected, observed *corev1.Pod) []admissionMismatch {
	if expected.Spec.RuntimeClassName == nil {
		return nil
	}
	want := *expected.Spec.RuntimeClassName
	if observed.Spec.RuntimeClassName != nil && *observed.Spec.RuntimeClassName == want {
		return nil
	}
	return []admissionMismatch{{
		field:    "spec.runtimeClassName",
		expected: fmt.Sprintf("%q", want),
		observed: optionalQuoted(observed.Spec.RuntimeClassName),
	}}
}

// checkSelectorLabelsKept requires the labels the egress NetworkPolicy selects
// on, and the sandbox marker, to be unchanged: a rewritten selector label
// would leave the Pod outside its own policy. Other labels may be added.
func checkSelectorLabelsKept(expected, observed *corev1.Pod) []admissionMismatch {
	var out []admissionMismatch
	for _, key := range []string{k8sPodNameLabel, k8sSandboxLabel} {
		want, wantOK := expected.Labels[key]
		got, gotOK := observed.Labels[key]
		if want == got && wantOK == gotOK {
			continue
		}
		out = append(out, admissionMismatch{
			field:    "metadata.labels[" + key + "]",
			expected: labelValue(want, wantOK),
			observed: labelValue(got, gotOK),
		})
	}
	return out
}

func labelValue(v string, ok bool) string {
	if !ok {
		return "absent"
	}
	return fmt.Sprintf("%q", v)
}

func optionalQuoted(s *string) string {
	if s == nil {
		return "unset"
	}
	return fmt.Sprintf("%q", *s)
}
