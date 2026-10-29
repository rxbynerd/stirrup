package transport

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

func TestHarnessEventToProto_ReadyConfigFieldsAndSemver(t *testing.T) {
	must := require.New(t)
	decoded := harnessEventWireRoundTrip(t, types.HarnessEvent{
		Type:          "ready",
		HarnessSemver: "0.1.0-rc.1",
		ConfigFields:  types.ConfigFieldPaths(),
	})
	must.Equal(types.ConfigFieldPaths(), decoded.GetConfigFields(), "config fields")
	must.Equal("0.1.0-rc.1", decoded.GetHarnessSemver(), "harness semver")

	bare := harnessEventWireRoundTrip(t, types.HarnessEvent{Type: "ready"})
	must.Empty(bare.GetConfigFields(), "ready without config fields")
	must.Empty(bare.GetHarnessSemver(), "ready without a semantic version")
}

// TestConfigFieldPaths_CoversEveryProtoRunConfigField pins that a control
// plane filtering a task_assignment against ready.config_fields never
// drops a field the RunConfig proto can carry: every proto field path,
// spelled with its protobuf JSON name, must be advertised.
func TestConfigFieldPaths_CoversEveryProtoRunConfigField(t *testing.T) {
	check := assert.New(t)
	advertised := map[string]bool{}
	for _, p := range types.ConfigFieldPaths() {
		advertised[p] = true
	}
	var protoPaths []string
	collectProtoFieldPaths("", (&pb.RunConfig{}).ProtoReflect().Descriptor(), &protoPaths, map[protoreflect.FullName]int{})
	check.Subset(protoPaths, []string{"providers.*.endpointProfile", "tools.mcpServers.name", "tools.lazyLoading.eager", "guardRail.stages.type", "verifier.verifiers.command"},
		"walker reaches maps, repeated and nested messages, and one level into recursive messages")
	for _, p := range protoPaths {
		check.True(advertised[p], "proto RunConfig field %q is not in ConfigFieldPaths", p)
	}
}

// collectProtoFieldPaths mirrors types.ConfigFieldPaths' walk, including
// its one nesting level inside a recursive message.
func collectProtoFieldPaths(prefix string, md protoreflect.MessageDescriptor, out *[]string, active map[protoreflect.FullName]int) {
	if active[md.FullName()] >= 2 {
		return
	}
	active[md.FullName()]++
	defer func() { active[md.FullName()]-- }()
	fields := md.Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		path := fd.JSONName()
		if prefix != "" {
			path = prefix + "." + path
		}
		*out = append(*out, path)
		switch {
		case fd.IsMap():
			keyPath := path + "." + types.ConfigFieldWildcard
			*out = append(*out, keyPath)
			if v := fd.MapValue(); v.Kind() == protoreflect.MessageKind && isHarnessMessage(v.Message()) {
				collectProtoFieldPaths(keyPath, v.Message(), out, active)
			}
		case fd.Kind() == protoreflect.MessageKind && isHarnessMessage(fd.Message()):
			collectProtoFieldPaths(path, fd.Message(), out, active)
		}
	}
}

// isHarnessMessage reports whether a message is one of this package's own
// rather than a well-known type such as google.protobuf.Struct, which maps
// onto a self-decoding RunConfig field.
func isHarnessMessage(md protoreflect.MessageDescriptor) bool {
	return md.ParentFile().Package() == (&pb.RunConfig{}).ProtoReflect().Descriptor().ParentFile().Package()
}
