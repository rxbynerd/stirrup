package transport

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func TestHarnessEventToProto_ReadyConfigFieldsAndSemver(t *testing.T) {
	must := require.New(t)
	decoded := harnessEventWireRoundTrip(t, types.HarnessEvent{
		Type:          "ready",
		HarnessSemver: "0.1.0-rc.1",
		ConfigFields:  TaskAssignmentFieldPaths(),
	})
	must.Equal(TaskAssignmentFieldPaths(), decoded.GetConfigFields(), "config fields")
	must.Equal("0.1.0-rc.1", decoded.GetHarnessSemver(), "harness semver")

	bare := harnessEventWireRoundTrip(t, types.HarnessEvent{Type: "ready"})
	must.Empty(bare.GetConfigFields(), "ready without config fields")
	must.Empty(bare.GetHarnessSemver(), "ready without a semantic version")
}

// TestConfigFieldPaths_CoversEveryProtoRunConfigField pins that a control
// plane filtering a task_assignment against ready.config_fields never
// drops a field the RunConfig proto can carry: every proto field path,
// spelled with its protobuf JSON name, must be one the harness accepts.
func TestConfigFieldPaths_CoversEveryProtoRunConfigField(t *testing.T) {
	check := assert.New(t)
	accepted := map[string]bool{}
	for _, p := range types.ConfigFieldPaths() {
		accepted[p] = true
	}
	protoPaths := protoRunConfigFieldPaths()
	check.Subset(protoPaths, []string{"providers.*.endpointProfile", "tools.mcpServers.name", "tools.lazyLoading.eager", "guardRail.stages.type", "verifier.verifiers.command"},
		"walker reaches maps, repeated and nested messages, and one level into recursive messages")
	for _, p := range protoPaths {
		check.True(accepted[p], "proto RunConfig field %q is not in types.ConfigFieldPaths", p)
	}
}

func TestTaskAssignmentFieldPaths_OmitsFieldsWithoutProtoMirror(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	got := TaskAssignmentFieldPaths()
	must.ElementsMatch(protoRunConfigFieldPaths(), got)
	check.Subset(got, []string{"provider.endpointProfile", "tools.commandOutput.enabled", "outputSchema"})
	for _, p := range got {
		check.NotEqual("transport", p)
		check.False(strings.HasPrefix(p, "transport."), "unmirrored field advertised: %s", p)
		check.False(strings.HasPrefix(p, "traceEmitter.credential"), "unmirrored field advertised: %s", p)
		check.False(strings.HasPrefix(p, "traceEmitter.archive"), "unmirrored field advertised: %s", p)
		check.NotEqual("tools.profile", p)
	}
	check.Contains(types.ConfigFieldPaths(), "tools.profile", "the full schema still lists file-only fields")

	got[0] = "mutated"
	check.NotEqual("mutated", TaskAssignmentFieldPaths()[0], "mutating the returned slice must not change later calls")
}
