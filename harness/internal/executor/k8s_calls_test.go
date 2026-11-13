package executor

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1unstructured "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// observedAccess converts recorded fake-client actions to the permissions
// they need, in k8sAccess.String form.
func observedAccess(actions ...[]clienttesting.Action) map[string]bool {
	seen := map[string]bool{}
	for _, list := range actions {
		for _, a := range list {
			gvr := a.GetResource()
			access := k8sAccess{group: gvr.Group, resource: gvr.Resource, subresource: a.GetSubresource(), verb: a.GetVerb()}
			seen[access.String()] = true
		}
	}
	return seen
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// requireProbeMatchesCalls asserts the probe checks exactly the permissions
// the executor used. pods/exec is the one entry no fake records, because exec
// is a REST request on the clientset's RESTClient (see
// TestExecutorSource_UsesOnlyTheExecSubresource); it is therefore expected in
// the probe list and absent from the observed calls.
func requireProbeMatchesCalls(t *testing.T, executorType string, observed map[string]bool) {
	t.Helper()
	must := require.New(t)

	probed := map[string]bool{}
	for _, access := range k8sRequiredAccess(executorType) {
		probed[access.String()] = true
	}
	delete(probed, k8sExecAccess.String())

	var uncovered []string
	for _, call := range sortedKeys(observed) {
		if !probed[call] {
			uncovered = append(uncovered, call)
		}
	}
	must.Empty(uncovered, "the %s executor makes calls the access probe does not check", executorType)

	var unused []string
	for _, entry := range sortedKeys(probed) {
		if !observed[entry] {
			unused = append(unused, entry)
		}
	}
	must.Empty(unused, "the access probe checks permissions the %s executor never used in these scenarios", executorType)
}

func TestK8sAccessProbe_CoversEveryCallTheK8sExecutorMakes(t *testing.T) {
	must := require.New(t)
	ambiguous := apierrors.NewServerTimeout(podsResource.GroupResource(), "create", 1)
	var recorded [][]clienttesting.Action

	t.Run("construct and close", func(t *testing.T) {
		must := require.New(t)
		cs := fake.NewClientset()
		admitPods(cs, nil)
		lingerOnDelete(t, cs, 3)

		exec, err := newFakeK8sExecutor(context.Background(), cs, constructorCfg("gvisor"))
		must.NoError(err)
		must.NoError(exec.Close())
		recorded = append(recorded, cs.Actions())
	})

	t.Run("ambiguous create error reconciles through a pod lookup", func(t *testing.T) {
		must := require.New(t)
		cs := fake.NewClientset()
		createPersistedThenFail(t, cs, ambiguous, nil)
		lingerOnDelete(t, cs, 3)

		_, err := newFakeK8sExecutor(context.Background(), cs, constructorCfg("gvisor"))
		must.ErrorIs(err, ambiguous)
		must.Positive(countVerb(cs, "get", "pods"), "the reconcile did not look the pod up")
		recorded = append(recorded, cs.Actions())
	})

	t.Run("ambiguous create error with no pod retains the policy", func(t *testing.T) {
		cs := fake.NewClientset()
		failVerb(cs, "create", "pods", ambiguous)

		_, err := newFakeK8sExecutor(context.Background(), cs, constructorCfg("gvisor"))
		require.New(t).ErrorIs(err, ambiguous)
		recorded = append(recorded, cs.Actions())
	})

	must.Len(recorded, 3, "a scenario did not run")
	requireProbeMatchesCalls(t, "k8s", observedAccess(recorded...))
}

func TestK8sAccessProbe_CoversEveryCallTheK8sSandboxExecutorMakes(t *testing.T) {
	must := require.New(t)
	ambiguous := apierrors.NewServerTimeout(sandboxGVR.GroupResource(), "create", 1)
	var recorded [][]clienttesting.Action

	t.Run("construct and close", func(t *testing.T) {
		must := require.New(t)
		f := newFakeAgentSandbox(t)
		f.admitSandboxes(nil)
		f.lingerOnDelete(t, 3)

		exec, _, err := f.build(t, fastK8sTiming)
		must.NoError(err)
		must.NoError(exec.Close())
		recorded = append(recorded, f.cs.Actions(), f.dyn.Actions())
	})

	t.Run("ambiguous create error reconciles through a sandbox lookup", func(t *testing.T) {
		must := require.New(t)
		f := newFakeAgentSandbox(t)
		f.dyn.PrependReactor("create", "sandboxes", func(action clienttesting.Action) (bool, runtime.Object, error) {
			obj := action.(clienttesting.CreateAction).GetObject().(*metav1unstructured.Unstructured).DeepCopy()
			must.NoError(f.dyn.Tracker().Create(sandboxGVR, obj, testK8sNamespace))
			return true, nil, ambiguous
		})
		f.lingerOnDelete(t, 3)

		_, _, err := f.build(t, fastK8sTiming)
		must.ErrorIs(err, ambiguous)
		recorded = append(recorded, f.cs.Actions(), f.dyn.Actions())
	})

	t.Run("ambiguous create error with no sandbox retains the policy", func(t *testing.T) {
		f := newFakeAgentSandbox(t)
		f.dyn.PrependReactor("create", "sandboxes", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, ambiguous
		})

		_, _, err := f.build(t, fastK8sTiming)
		require.New(t).ErrorIs(err, ambiguous)
		recorded = append(recorded, f.cs.Actions(), f.dyn.Actions())
	})

	must.Len(recorded, 6, "a scenario did not run")
	requireProbeMatchesCalls(t, "k8s-sandbox", observedAccess(recorded...))
}

// TestExecutorSource_UsesOnlyTheExecSubresource covers pods/exec, which the
// fake-client tests cannot record: exec is a REST request built on the
// clientset's RESTClient. The probe checks it as pods/exec create, so any
// other subresource or any RESTClient use outside the exec chokepoint would
// be an unchecked call.
func TestExecutorSource_UsesOnlyTheExecSubresource(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	files, err := filepath.Glob("*.go")
	must.NoError(err)
	fset := token.NewFileSet()
	var subresourceUses int
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		must.NoError(err, file)
		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "RESTClient":
				check.Equal("k8s_execcore.go", file, "RESTClient used outside the exec chokepoint at %s", fset.Position(call.Pos()))
			case "SubResource":
				subresourceUses++
				lit, isLit := call.Args[0].(*ast.BasicLit)
				name := ""
				if isLit {
					name, _ = strconv.Unquote(lit.Value)
				}
				check.Equal("exec", name, "unchecked subresource at %s", fset.Position(call.Pos()))
			}
			return true
		})
	}
	check.Equal(1, subresourceUses, "the exec request is expected to be the only subresource call")
}
