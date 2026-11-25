package executor

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1unstructured "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// sandboxPodTemplate is the Pod the harness puts in a Sandbox's
// spec.podTemplate. It is the submitted side of the backing Pod's admission
// check, since the controller, not the harness, creates the Pod.
func sandboxPodTemplate(namespace, name, runID string, spec corev1.PodSpec) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    podLabels(name, runID),
		},
		Spec: spec,
	}
}

// sandboxReadinessWait is what a Sandbox readiness wait last observed of the
// Sandbox and of its backing Pod.
type sandboxReadinessWait struct {
	dyn       dynamic.Interface
	gvr       schema.GroupVersionResource
	template  *corev1.Pod
	logger    *slog.Logger
	pod       podReadinessPoll
	admission *podAdmission

	sawSandbox bool
	sandboxUID k8stypes.UID
	condition  string
	lastAPIErr error
}

// waitForSandboxReady polls the Sandbox and its backing Pod until the Sandbox
// reports Ready and the Pod is Running with the agent container Ready. The
// backing Pod is read only once the Sandbox has been. The first Pod observed
// is checked against template (see newSandboxPodAdmission) and becomes the
// baseline for every later observation. The Pod's terminal states, a
// mismatch, a Pod or Sandbox that disappears after being observed, a Sandbox
// naming another backing Pod and an RBAC denial fail at once; a Pod not yet
// created and other API errors are waited out.
func waitForSandboxReady(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, clientset kubernetes.Interface, template *corev1.Pod, timing k8sTiming, logger *slog.Logger) (*corev1.Pod, error) {
	w := &sandboxReadinessWait{
		dyn:      dyn,
		gvr:      gvr,
		template: template,
		logger:   logger,
		pod: podReadinessPoll{
			clientset:     clientset,
			namespace:     template.Namespace,
			name:          template.Name,
			logger:        logger,
			awaitCreation: true,
		},
	}
	var ready *corev1.Pod
	attrs := []any{"namespace", template.Namespace, "sandbox", template.Name}
	err := pollReadiness(ctx, timing, logger, attrs, w.summary, w.pod.seen.classify, func(ctx context.Context) (bool, error) {
		sandboxIsReady, err := w.pollSandbox(ctx)
		if err != nil || !w.sawSandbox {
			return false, err
		}
		pod, err := w.pod.poll(ctx, w.checkAdmission)
		if err != nil {
			return false, err
		}
		ready = pod
		return sandboxIsReady && pod != nil, nil
	})
	if err != nil {
		return nil, err
	}
	return ready, nil
}

// pollSandbox reads the Sandbox once and reports whether its Ready condition
// is True.
func (w *sandboxReadinessWait) pollSandbox(ctx context.Context) (bool, error) {
	namespace, name := w.template.Namespace, w.template.Name
	sandbox, err := w.dyn.Resource(w.gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		w.sawSandbox = true
		w.sandboxUID = sandbox.GetUID()
		w.lastAPIErr = nil
		w.condition = describeSandboxReadyCondition(sandbox)
		// Cold-create only: an adopted (warm-pool) Sandbox's Pod name would not
		// match, and the per-pod egress NetworkPolicy would bind to nothing.
		// See docs/executors/k8s-agent-sandbox.md.
		if podName := resolveSandboxPodName(sandbox); podName != name {
			return false, fmt.Errorf("%w: resolved to backing pod %q: warm-pool adoption is not supported by the agent sandbox executor (its per-pod egress policy would not bind)", errPodNotReady, clusterText(podName))
		}
		return sandboxReady(sandbox), nil
	case apierrors.IsNotFound(err) && w.sawSandbox:
		return false, fmt.Errorf("%w: the sandbox was deleted during the readiness wait", errPodNotReady)
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return false, fmt.Errorf("%w: get sandbox: %w", errPodNotReady, boundClusterErr(err))
	case ctx.Err() == nil:
		w.lastAPIErr = err
		w.logger.Debug("transient error polling sandbox readiness, retrying", "namespace", namespace, "sandbox", name, "err", err)
	}
	return false, nil
}

// checkAdmission compares the first observed backing Pod with the template,
// then every observation with that first one.
func (w *sandboxReadinessWait) checkAdmission(observed *corev1.Pod) error {
	if w.admission == nil {
		admission := newSandboxPodAdmission(w.template, observed, w.sandboxUID)
		if err := admission.checkCreate(); err != nil {
			return err
		}
		w.admission = &admission
	}
	return w.admission.checkPoll(observed)
}

// newSandboxPodAdmission is the admission contract for a backing Pod the
// harness did not create, so a Pod under the Sandbox's name proves nothing
// by itself. Beyond the base checks, the first observation must be
// controlled by the Sandbox with sandboxUID, and every observation's spec
// must match the template under the whole-Pod field rules.
func newSandboxPodAdmission(template, first *corev1.Pod, sandboxUID k8stypes.UID) podAdmission {
	checks := append(slices.Clone(baseAdmissionChecks), checkStrictPodSpec)
	controlledBySandbox := func(_, observed *corev1.Pod) []admissionMismatch {
		return checkControlledBySandbox(sandboxUID, observed)
	}
	return podAdmission{
		submitted:    template,
		admitted:     first,
		createChecks: append(slices.Clone(checks), controlledBySandbox),
		pollChecks:   checks,
	}
}

// checkControlledBySandbox requires the Pod's controller reference to name
// the Sandbox with sandboxUID.
func checkControlledBySandbox(sandboxUID k8stypes.UID, observed *corev1.Pod) []admissionMismatch {
	ref := metav1.GetControllerOfNoCopy(observed)
	if ref != nil && sandboxUID != "" && ref.UID == sandboxUID && ref.Kind == "Sandbox" {
		if gv, err := schema.ParseGroupVersion(ref.APIVersion); err == nil && gv.Group == sandboxGroup {
			return nil
		}
	}
	got := "none"
	if ref != nil {
		got = fmt.Sprintf("%s %s %q (UID %s)", ref.APIVersion, ref.Kind, ref.Name, ref.UID)
	}
	return []admissionMismatch{{
		field:    "metadata.ownerReferences[controller]",
		expected: fmt.Sprintf("Sandbox %q (UID %s)", observed.Name, sandboxUID),
		observed: got,
	}}
}

// summary renders the last observation for the progress line and the timeout
// error.
func (w *sandboxReadinessWait) summary() string {
	sandbox := "the sandbox was never observed"
	if w.sawSandbox {
		sandbox = "sandbox " + w.condition
	}
	if w.lastAPIErr != nil {
		sandbox += "; last sandbox API error: " + clusterText(w.lastAPIErr.Error())
	}
	return sandbox + "; backing pod: " + w.pod.seen.summary()
}

// describeSandboxReadyCondition renders the Sandbox's Ready condition with
// its reason and message.
func describeSandboxReadyCondition(sandbox *metav1unstructured.Unstructured) string {
	cond, ok := sandboxReadyCondition(sandbox)
	if !ok {
		return "has no Ready condition"
	}
	status, _ := cond["status"].(string)
	if status == "" {
		status = "unknown"
	}
	out := "Ready " + clusterText(status)
	reason, _ := cond["reason"].(string)
	message, _ := cond["message"].(string)
	if reason != "" || message != "" {
		out += fmt.Sprintf(" (%s: %s)", clusterText(reason), clusterText(message))
	}
	return out
}
