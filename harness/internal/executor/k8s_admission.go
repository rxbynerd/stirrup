package executor

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

// admissionMismatch is one field where the admitted Pod departs from the
// contract the executor submitted.
type admissionMismatch struct {
	field    string
	expected string
	observed string
}

// admissionCheck compares one part of an admitted Pod against the submitted
// Pod. Checks report mismatches rather than errors so one failure names every
// departure at once.
type admissionCheck func(submitted, admitted *corev1.Pod) []admissionMismatch

// baseAdmissionChecks apply to every runtime.
var baseAdmissionChecks = []admissionCheck{checkAdmittedRuntimeClass, checkAdmittedSelectorLabels}

// podAdmission verifies that the Pod the API server admitted, and the Pod the
// readiness wait later observes, still honour the submitted contract. The
// check runs after creation: it detects mutation by admission or by a later
// writer and stops the run, but it cannot prevent the Pod from starting.
type podAdmission struct {
	submitted *corev1.Pod
	// uid is the UID Create returned; every later observation must match.
	uid    k8stypes.UID
	checks []admissionCheck
}

// check returns an error naming every expected and observed value that
// differs, or nil when the admitted Pod honours the contract.
func (a podAdmission) check(admitted *corev1.Pod) error {
	var mismatches []admissionMismatch
	switch {
	case admitted.UID == "":
		mismatches = append(mismatches, admissionMismatch{field: "metadata.uid", expected: "a server-assigned UID", observed: "none"})
	case a.uid != "" && admitted.UID != a.uid:
		mismatches = append(mismatches, admissionMismatch{field: "metadata.uid", expected: string(a.uid), observed: string(admitted.UID)})
	}
	for _, check := range a.checks {
		mismatches = append(mismatches, check(a.submitted, admitted)...)
	}
	if len(mismatches) == 0 {
		return nil
	}
	parts := make([]string, len(mismatches))
	for i, m := range mismatches {
		parts[i] = fmt.Sprintf("%s: expected %s, observed %s", m.field, m.expected, clusterText(m.observed))
	}
	return fmt.Errorf("admitted pod %s/%s does not match the submitted pod: %s",
		a.submitted.Namespace, a.submitted.Name, strings.Join(parts, "; "))
}

// checkAdmittedRuntimeClass requires a requested RuntimeClass to survive
// admission unchanged. A class admission fills in when none was requested is
// a cluster default, not a contract violation.
func checkAdmittedRuntimeClass(submitted, admitted *corev1.Pod) []admissionMismatch {
	if submitted.Spec.RuntimeClassName == nil {
		return nil
	}
	want := *submitted.Spec.RuntimeClassName
	if admitted.Spec.RuntimeClassName != nil && *admitted.Spec.RuntimeClassName == want {
		return nil
	}
	return []admissionMismatch{{
		field:    "spec.runtimeClassName",
		expected: fmt.Sprintf("%q", want),
		observed: optionalQuoted(admitted.Spec.RuntimeClassName),
	}}
}

// checkAdmittedSelectorLabels requires the labels the egress NetworkPolicy
// selects on, and the sandbox marker, to survive unchanged: a rewritten
// selector label would leave the Pod outside its own policy.
func checkAdmittedSelectorLabels(submitted, admitted *corev1.Pod) []admissionMismatch {
	var out []admissionMismatch
	for _, key := range []string{k8sPodNameLabel, k8sSandboxLabel} {
		want, wantOK := submitted.Labels[key]
		got, gotOK := admitted.Labels[key]
		if want == got && wantOK == gotOK {
			continue
		}
		observed := "absent"
		if gotOK {
			observed = fmt.Sprintf("%q", got)
		}
		out = append(out, admissionMismatch{
			field:    "metadata.labels[" + key + "]",
			expected: fmt.Sprintf("%q", want),
			observed: observed,
		})
	}
	return out
}

func optionalQuoted(s *string) string {
	if s == nil {
		return "unset"
	}
	return fmt.Sprintf("%q", *s)
}
