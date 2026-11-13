package executor

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
)

// admissionFieldRule says how the strict admission tier treats one field.
type admissionFieldRule int

const (
	// fieldCompared must be semantically equal to the Pod it is compared
	// against, so 1Gi matches a submitted 1024Mi.
	fieldCompared admissionFieldRule = iota + 1
	// fieldPlatformOwned is filled in or changed by API defaulting,
	// scheduling or the RuntimeClass, and is ignored.
	fieldPlatformOwned
	// fieldSubmittedSubset may gain entries but must keep every submitted
	// one.
	fieldSubmittedSubset
)

// admissionField classifies one field; why records the reason for every
// field that is not plainly compared.
type admissionField struct {
	rule admissionFieldRule
	why  string
}

var comparedField = admissionField{rule: fieldCompared}

// strictPodSpecFields classifies every corev1.PodSpec field by Go name. A
// test fails when the struct gains a field this table does not name, so a
// client-go upgrade forces a decision. An unnamed field is compared.
var strictPodSpecFields = map[string]admissionField{
	"Volumes":                       {fieldCompared, "an injected volume (an IRSA or Pod Identity token, a webhook's mount) is a change to what the guest sees; no service-account token volume is added because automountServiceAccountToken is false"},
	"InitContainers":                comparedField,
	"Containers":                    {fieldCompared, "compared container by container under strictContainerFields"},
	"EphemeralContainers":           {fieldCompared, "cannot be set at creation; one added later is seen only until readiness"},
	"RestartPolicy":                 comparedField,
	"TerminationGracePeriodSeconds": comparedField,
	"ActiveDeadlineSeconds":         comparedField,
	"DNSPolicy":                     {fieldPlatformOwned, "API defaulting sets ClusterFirst; the guest has no network in the supported mode"},
	"NodeSelector":                  {fieldSubmittedSubset, "the RuntimeClass's scheduling selector is merged in"},
	"ServiceAccountName":            comparedField,
	"DeprecatedServiceAccount":      {fieldPlatformOwned, "API defaulting mirrors serviceAccountName into it"},
	"AutomountServiceAccountToken":  comparedField,
	"NodeName":                      {fieldPlatformOwned, "set by the scheduler"},
	"HostNetwork":                   comparedField,
	"HostPID":                       comparedField,
	"HostIPC":                       comparedField,
	"ShareProcessNamespace":         comparedField,
	"SecurityContext":               {fieldCompared, "the harness sets fsGroup only; no API default is applied to the other Pod security fields"},
	"ImagePullSecrets":              {fieldPlatformOwned, "the ServiceAccount admission plugin copies the service account's pull secrets; they reach the kubelet's image pull, not the guest"},
	"Hostname":                      comparedField,
	"Subdomain":                     comparedField,
	"Affinity":                      comparedField,
	"SchedulerName":                 {fieldPlatformOwned, "API defaulting sets default-scheduler"},
	"Tolerations":                   {fieldSubmittedSubset, "the DefaultTolerationSeconds admission plugin adds not-ready and unreachable tolerations"},
	"HostAliases":                   comparedField,
	"PriorityClassName":             {fieldPlatformOwned, "the Priority admission plugin fills in a cluster's global default class"},
	"Priority":                      {fieldPlatformOwned, "the Priority admission plugin resolves it from the class"},
	"DNSConfig":                     comparedField,
	"ReadinessGates":                comparedField,
	"RuntimeClassName":              comparedField,
	"EnableServiceLinks":            {fieldPlatformOwned, "API defaulting sets true"},
	"PreemptionPolicy":              {fieldPlatformOwned, "the Priority admission plugin sets it from the class"},
	"Overhead":                      {fieldPlatformOwned, "the RuntimeClass admission plugin sets the RuntimeClass's overhead"},
	"TopologySpreadConstraints":     comparedField,
	"SetHostnameAsFQDN":             comparedField,
	"OS":                            comparedField,
	"HostUsers":                     comparedField,
	"SchedulingGates":               comparedField,
	"ResourceClaims":                comparedField,
	"Resources":                     comparedField,
	"HostnameOverride":              comparedField,
	"SchedulingGroup":               comparedField,
	"EvictionResponders":            comparedField,
}

// strictContainerFields classifies every corev1.Container field by Go name,
// with the same completeness test as strictPodSpecFields.
var strictContainerFields = map[string]admissionField{
	"Name":                     comparedField,
	"Image":                    {fieldCompared, "an admission policy that rewrites image references fails the run"},
	"Command":                  comparedField,
	"Args":                     comparedField,
	"WorkingDir":               comparedField,
	"Ports":                    comparedField,
	"EnvFrom":                  comparedField,
	"Env":                      {fieldCompared, "compared by name, value and source; values are never printed"},
	"Resources":                {fieldCompared, "requests equal limits as submitted, so defaulting requests from limits changes nothing; a LimitRange default fails the run"},
	"ResizePolicy":             comparedField,
	"RestartPolicy":            comparedField,
	"RestartPolicyRules":       comparedField,
	"VolumeMounts":             comparedField,
	"VolumeDevices":            comparedField,
	"LivenessProbe":            comparedField,
	"ReadinessProbe":           comparedField,
	"StartupProbe":             comparedField,
	"Lifecycle":                comparedField,
	"TerminationMessagePath":   {fieldPlatformOwned, "API defaulting sets /dev/termination-log"},
	"TerminationMessagePolicy": {fieldPlatformOwned, "API defaulting sets File"},
	"ImagePullPolicy":          {fieldPlatformOwned, "API defaulting sets it from the image tag"},
	"SecurityContext":          {fieldCompared, "no API default is applied to the fields the harness leaves unset"},
	"Stdin":                    comparedField,
	"StdinOnce":                comparedField,
	"TTY":                      comparedField,
}

// strictAdmissionChecks are the whole-Pod checks a runtime profile with
// strictAdmission adds at both admission stages. noNetworkAnnotations are
// the annotations that must keep their submitted value.
func strictAdmissionChecks(noNetworkAnnotations map[string]string) []admissionCheck {
	keys := make([]string, 0, len(noNetworkAnnotations))
	for k := range noNetworkAnnotations {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return []admissionCheck{
		func(expected, observed *corev1.Pod) []admissionMismatch {
			return checkAnnotationsKept(keys, expected, observed)
		},
		checkSubmittedLabelsKept,
		checkOwnerReferencesKept,
		checkStrictPodSpec,
	}
}

// checkAnnotationsKept requires each named annotation the expected Pod
// carries to be present on the observed Pod with the same value.
func checkAnnotationsKept(keys []string, expected, observed *corev1.Pod) []admissionMismatch {
	var out []admissionMismatch
	for _, key := range keys {
		want, ok := expected.Annotations[key]
		if !ok {
			continue
		}
		got, gotOK := observed.Annotations[key]
		if gotOK && got == want {
			continue
		}
		out = append(out, admissionMismatch{
			field:    "metadata.annotations[" + key + "]",
			expected: labelValue(want, true),
			observed: labelValue(got, gotOK),
		})
	}
	return out
}

// checkSubmittedLabelsKept requires every expected label to be kept; labels
// may be added. The policy-selector labels are left to checkSelectorLabelsKept.
func checkSubmittedLabelsKept(expected, observed *corev1.Pod) []admissionMismatch {
	keys := make([]string, 0, len(expected.Labels))
	for k := range expected.Labels {
		if k == k8sPodNameLabel || k == k8sSandboxLabel {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []admissionMismatch
	for _, key := range keys {
		want := expected.Labels[key]
		got, ok := observed.Labels[key]
		if ok && got == want {
			continue
		}
		out = append(out, admissionMismatch{field: "metadata.labels[" + key + "]", expected: labelValue(want, true), observed: labelValue(got, ok)})
	}
	return out
}

func checkOwnerReferencesKept(expected, observed *corev1.Pod) []admissionMismatch {
	if apiequality.Semantic.DeepEqual(expected.OwnerReferences, observed.OwnerReferences) {
		return nil
	}
	return []admissionMismatch{{
		field:    "metadata.ownerReferences",
		expected: renderAdmissionValue(reflect.ValueOf(expected.OwnerReferences)),
		observed: renderAdmissionValue(reflect.ValueOf(observed.OwnerReferences)),
	}}
}

// checkStrictPodSpec reports the first spec field, in struct order, where
// the observed Pod departs from the expected one under strictPodSpecFields.
func checkStrictPodSpec(expected, observed *corev1.Pod) []admissionMismatch {
	if m, ok := firstStructMismatch("spec", reflect.ValueOf(expected.Spec), reflect.ValueOf(observed.Spec), strictPodSpecFields); ok {
		return []admissionMismatch{m}
	}
	return nil
}

// firstStructMismatch walks want and got, two values of the same struct
// type, under table.
func firstStructMismatch(prefix string, want, got reflect.Value, table map[string]admissionField) (admissionMismatch, bool) {
	t := want.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		path := prefix + "." + jsonFieldName(f)
		w, g := want.Field(i), got.Field(i)
		rule, ok := table[f.Name]
		if !ok {
			rule = comparedField
		}
		switch {
		case rule.rule == fieldPlatformOwned:
			continue
		case f.Type == reflect.TypeOf([]corev1.Container(nil)):
			if m, found := firstContainersMismatch(path, w.Interface().([]corev1.Container), g.Interface().([]corev1.Container)); found {
				return m, true
			}
		case f.Type == reflect.TypeOf([]corev1.EnvVar(nil)):
			if m, found := firstEnvMismatch(path, w.Interface().([]corev1.EnvVar), g.Interface().([]corev1.EnvVar)); found {
				return m, true
			}
		case rule.rule == fieldSubmittedSubset:
			if !isSubmittedSubset(w, g) {
				return admissionMismatch{field: path, expected: "at least " + renderAdmissionValue(w), observed: renderAdmissionValue(g)}, true
			}
		default:
			if !apiequality.Semantic.DeepEqual(w.Interface(), g.Interface()) {
				return admissionMismatch{field: path, expected: renderAdmissionValue(w), observed: renderAdmissionValue(g)}, true
			}
		}
	}
	return admissionMismatch{}, false
}

// firstContainersMismatch requires exactly the expected containers, by name
// and in order, and compares each under strictContainerFields.
func firstContainersMismatch(path string, want, got []corev1.Container) (admissionMismatch, bool) {
	if len(want) != len(got) || !sameContainerNames(want, got) {
		return admissionMismatch{field: path, expected: containerNames(want), observed: containerNames(got)}, true
	}
	for i := range want {
		if m, ok := firstStructMismatch(fmt.Sprintf("%s[%s]", path, want[i].Name), reflect.ValueOf(want[i]), reflect.ValueOf(got[i]), strictContainerFields); ok {
			return m, true
		}
	}
	return admissionMismatch{}, false
}

func sameContainerNames(want, got []corev1.Container) bool {
	for i := range want {
		if want[i].Name != got[i].Name {
			return false
		}
	}
	return true
}

// firstEnvMismatch compares environment variables in order. Only names are
// reported: a value can be a credential.
func firstEnvMismatch(path string, want, got []corev1.EnvVar) (admissionMismatch, bool) {
	for i := 0; i < len(want) || i < len(got); i++ {
		switch {
		case i >= len(got):
			return admissionMismatch{field: path, expected: fmt.Sprintf("variable %q", want[i].Name), observed: "missing"}, true
		case i >= len(want):
			return admissionMismatch{field: path, expected: "no further variables", observed: fmt.Sprintf("added variable %q", got[i].Name)}, true
		case want[i].Name != got[i].Name:
			return admissionMismatch{field: fmt.Sprintf("%s[%d].name", path, i), expected: fmt.Sprintf("%q", want[i].Name), observed: fmt.Sprintf("%q", got[i].Name)}, true
		case want[i].Value != got[i].Value:
			return admissionMismatch{field: fmt.Sprintf("%s[%s].value", path, want[i].Name), expected: "the submitted value", observed: "a different value"}, true
		case !apiequality.Semantic.DeepEqual(want[i].ValueFrom, got[i].ValueFrom):
			return admissionMismatch{field: fmt.Sprintf("%s[%s].valueFrom", path, want[i].Name), expected: "the submitted source", observed: "a different source"}, true
		}
	}
	return admissionMismatch{}, false
}

// isSubmittedSubset reports whether every entry of want, a map or a slice,
// is in got.
func isSubmittedSubset(want, got reflect.Value) bool {
	switch want.Kind() {
	case reflect.Map:
		for _, key := range want.MapKeys() {
			g := got.MapIndex(key)
			if !g.IsValid() || !apiequality.Semantic.DeepEqual(want.MapIndex(key).Interface(), g.Interface()) {
				return false
			}
		}
		return true
	case reflect.Slice:
		for i := 0; i < want.Len(); i++ {
			found := false
			for j := 0; j < got.Len() && !found; j++ {
				found = apiequality.Semantic.DeepEqual(want.Index(i).Interface(), got.Index(j).Interface())
			}
			if !found {
				return false
			}
		}
		return true
	default:
		return apiequality.Semantic.DeepEqual(want.Interface(), got.Interface())
	}
}

// renderAdmissionValue renders a field value for a mismatch message, bounded
// and scrubbed by clusterText. Container lists render as names only, since a
// container carries environment values.
func renderAdmissionValue(v reflect.Value) string {
	switch c := v.Interface().(type) {
	case []corev1.Container:
		return containerNames(c)
	case []corev1.EphemeralContainer:
		names := make([]string, len(c))
		for i := range c {
			names[i] = c[i].Name
		}
		return clusterText("containers [" + strings.Join(names, ", ") + "]")
	}
	if (v.Kind() == reflect.Pointer || v.Kind() == reflect.Slice || v.Kind() == reflect.Map) && v.IsNil() {
		return "unset"
	}
	b, err := json.Marshal(v.Interface())
	if err != nil {
		return "unrenderable value"
	}
	return clusterText(string(b))
}

func containerNames(cs []corev1.Container) string {
	names := make([]string, len(cs))
	for i := range cs {
		names[i] = cs[i].Name
	}
	return clusterText("containers [" + strings.Join(names, ", ") + "]")
}

// jsonFieldName is a struct field's JSON name, the name a field path uses.
func jsonFieldName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if name == "" {
		return f.Name
	}
	return name
}
