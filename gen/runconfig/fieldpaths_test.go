package runconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

func TestPopulatedFieldPaths_EmptyAndNilMessagesSetNothing(t *testing.T) {
	check := assert.New(t)
	check.Empty(PopulatedFieldPaths(nil))
	check.Empty(PopulatedFieldPaths(&pb.RunConfig{}))
}

// TestPopulatedFieldPaths_MatchesTheIntegrationGuideExample pins the
// worked example in docs/integration-guide.md#negotiating-runconfig-fields.
func TestPopulatedFieldPaths_MatchesTheIntegrationGuideExample(t *testing.T) {
	must := require.New(t)
	pc := &pb.RunConfig{}
	must.NoError(protojson.Unmarshal([]byte(`{
  "provider": {"type": "openai-compatible", "endpointProfile": "azure-openai"},
  "providers": {"fast": {"type": "anthropic"}},
  "outputSchema": {"type": "object", "properties": {"summary": {"type": "string"}}},
  "tools": {"mcpServers": [{"name": "docs", "uri": "https://mcp.example.com"}]}
}`), pc))
	must.Equal([]string{
		"outputSchema",
		"provider",
		"provider.endpointProfile",
		"provider.type",
		"providers",
		"providers.*",
		"providers.*.type",
		"tools",
		"tools.mcpServers",
		"tools.mcpServers.name",
		"tools.mcpServers.uri",
	}, PopulatedFieldPaths(pc))
}

func TestPopulatedFieldPaths_WalksARecursiveMessageOneLevelInsideItself(t *testing.T) {
	check := assert.New(t)
	pc := &pb.RunConfig{Verifier: &pb.VerifierConfig{
		Type: "composite",
		Verifiers: []*pb.VerifierConfig{{
			Type:      "composite",
			Verifiers: []*pb.VerifierConfig{{Type: "test-runner"}},
		}},
	}}
	got := PopulatedFieldPaths(pc)
	check.Subset(got, []string{"verifier.type", "verifier.verifiers", "verifier.verifiers.type", "verifier.verifiers.verifiers"})
	check.NotContains(got, "verifier.verifiers.verifiers.type")
}

func TestPopulatedFieldPaths_ListsOnlyFieldsTheHarnessAccepts(t *testing.T) {
	check := assert.New(t)
	got := PopulatedFieldPaths(filledProto(t))
	check.Subset(types.ConfigFieldPaths(), got)
	check.Contains(got, "executor.k8sNodeSelector.*")
	check.Contains(got, "guardRail.stages.stages")
}
