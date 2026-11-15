package executor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/rxbynerd/stirrup/types"
)

var testHarnessPod = &HarnessPod{Namespace: testK8sNamespace, Name: "harness-0", UID: "harness-uid"}

func TestEgressPolicyOwners_NamesHarnessAndSandboxPods(t *testing.T) {
	must := require.New(t)

	owners := egressPolicyOwners(testHarnessPod, testK8sNamespace, testK8sPodName, "sandbox-uid")

	must.Equal([]metav1.OwnerReference{
		{APIVersion: "v1", Kind: "Pod", Name: "harness-0", UID: "harness-uid"},
		{APIVersion: "v1", Kind: "Pod", Name: testK8sPodName, UID: "sandbox-uid"},
	}, owners)
}

func TestEgressPolicyOwners_NoneWithoutAHarnessPodInTheNamespace(t *testing.T) {
	tests := []struct {
		name    string
		harness *HarnessPod
	}{
		{"harness pod unknown", nil},
		{"harness pod in another namespace", &HarnessPod{Namespace: "elsewhere", Name: "harness-0", UID: "harness-uid"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			must.Empty(egressPolicyOwners(tt.harness, testK8sNamespace, testK8sPodName, "sandbox-uid"))
		})
	}
}

func TestNewK8sExecutor_OwnsEgressPolicyByHarnessAndSandboxPods(t *testing.T) {
	for _, mode := range []string{"none", "allowlist"} {
		t.Run(mode, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			cs := fake.NewClientset()
			admitPods(cs, nil)
			cfg := constructorCfg("gvisor")
			cfg.Network = &types.NetworkConfig{Mode: mode}
			cfg.EgressProxyURL = "http://stirrup-egress-proxy:8080"
			cfg.HarnessPod = testHarnessPod

			exec, err := newFakeK8sExecutor(context.Background(), cs, cfg)
			must.NoError(err)

			policy, err := cs.NetworkingV1().NetworkPolicies(testK8sNamespace).Get(context.Background(), exec.networkPolicyName, metav1.GetOptions{})
			must.NoError(err)
			check.Equal([]metav1.OwnerReference{
				{APIVersion: "v1", Kind: "Pod", Name: "harness-0", UID: "harness-uid"},
				{APIVersion: "v1", Kind: "Pod", Name: exec.podName, UID: k8stypes.UID("uid-1")},
			}, policy.OwnerReferences)
			check.Nil(policy.OwnerReferences[0].BlockOwnerDeletion, "foreground deletion of the harness Pod must not wait on or delete the policy")
		})
	}
}

func TestNewK8sExecutor_InstallsEgressPolicyBeforeTheSandboxPodAndOwnsItAfter(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset()
	admitPods(cs, nil)
	cfg := constructorCfg("gvisor")
	cfg.HarnessPod = testHarnessPod

	_, err := newFakeK8sExecutor(context.Background(), cs, cfg)
	must.NoError(err)

	var order []string
	for _, a := range cs.Actions() {
		switch r := a.GetResource().Resource; {
		case r == "networkpolicies" && a.GetVerb() == "create":
			order = append(order, "policy create")
		case r == "pods" && a.GetVerb() == "create":
			order = append(order, "pod create")
		case r == "networkpolicies" && a.GetVerb() == "patch":
			order = append(order, "policy patch")
		}
	}
	must.Equal([]string{"policy create", "pod create", "policy patch"}, order)
}

func TestNewK8sExecutor_LeavesEgressPolicyUnownedWithoutAHarnessPod(t *testing.T) {
	must := require.New(t)
	cs := fake.NewClientset()
	admitPods(cs, nil)

	exec, err := newFakeK8sExecutor(context.Background(), cs, constructorCfg("gvisor"))
	must.NoError(err)

	policy, err := cs.NetworkingV1().NetworkPolicies(testK8sNamespace).Get(context.Background(), exec.networkPolicyName, metav1.GetOptions{})
	must.NoError(err)
	must.Empty(policy.OwnerReferences)
	must.Zero(countVerb(cs, "patch", "networkpolicies"))
}

func TestNewK8sExecutor_ConstructsWhenTheOwnerPatchIsDenied(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cs := fake.NewClientset()
	admitPods(cs, nil)
	failVerb(cs, "patch", "networkpolicies", errForbiddenPods)
	cfg := constructorCfg("gvisor")
	cfg.HarnessPod = testHarnessPod

	exec, logs, err := newLoggedFakeK8sExecutor(context.Background(), cs, cfg)
	must.NoError(err)

	policy, err := cs.NetworkingV1().NetworkPolicies(testK8sNamespace).Get(context.Background(), exec.networkPolicyName, metav1.GetOptions{})
	must.NoError(err)
	check.Empty(policy.OwnerReferences)
	check.Contains(logs.String(), "could not attach owner references to the egress NetworkPolicy")
	check.Contains(logs.String(), exec.networkPolicyName)
}
