package transport

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

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

	// The roots of the "Features not on the wire" table in
	// docs/integration-guide.md; a change here belongs in that table too.
	unmirroredRoots := []string{"tools.profile", "traceEmitter.archive", "traceEmitter.credential", "transport"}
	advertised := map[string]bool{}
	for _, p := range got {
		advertised[p] = true
	}
	var omittedRoots []string
	for _, p := range types.ConfigFieldPaths() {
		if advertised[p] {
			continue
		}
		parent, nested := parentPath(p)
		if !nested || advertised[parent] {
			omittedRoots = append(omittedRoots, p)
		}
	}
	check.Equal(unmirroredRoots, omittedRoots)

	got[0] = "mutated"
	check.NotEqual("mutated", TaskAssignmentFieldPaths()[0], "mutating the returned slice must not change later calls")
}

// TestRunConfigFromProto_CopiesEveryAdvertisedField fills every RunConfig
// proto field with a non-zero value, translates it, and walks the result
// with the integration guide's negotiation recipe: every advertised path
// must survive, so a listed field is never one the translation drops.
func TestRunConfigFromProto_CopiesEveryAdvertisedField(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	pc := &pb.RunConfig{}
	fillProtoMessage(t, pc.ProtoReflect(), map[protoreflect.FullName]int{})
	raw, err := json.Marshal(runConfigFromProto(pc))
	must.NoError(err)
	var doc any
	must.NoError(json.Unmarshal(raw, &doc))

	advertised := TaskAssignmentFieldPaths()
	listed := map[string]bool{}
	for _, p := range advertised {
		listed[p] = true
	}
	present := map[string]bool{}
	collectSetFieldPaths("", doc, listed, present)
	for _, p := range advertised {
		check.True(present[p], "advertised field %q is lost in translation", p)
	}
}

// fillProtoMessage sets every field of m, the first member of each oneof
// only, to a non-zero value, going one level into recursive messages as
// the advertised list does.
func fillProtoMessage(t *testing.T, m protoreflect.Message, active map[protoreflect.FullName]int) {
	t.Helper()
	md := m.Descriptor()
	if md.FullName() == "google.protobuf.Struct" {
		s, err := structpb.NewStruct(map[string]any{"type": "object"})
		require.New(t).NoError(err)
		s.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			m.Set(fd, v)
			return true
		})
		return
	}
	if active[md.FullName()] >= types.ConfigFieldTypeNesting {
		return
	}
	active[md.FullName()]++
	defer func() { active[md.FullName()]-- }()
	fields := md.Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		if oneof := fd.ContainingOneof(); oneof != nil && !oneof.IsSynthetic() && oneof.Fields().Get(0) != fd {
			continue
		}
		switch {
		case fd.IsMap():
			entries := m.Mutable(fd).Map()
			v := entries.NewValue()
			if fd.MapValue().Kind() == protoreflect.MessageKind {
				fillProtoMessage(t, v.Message(), active)
			} else {
				v = nonZeroScalar(t, fd.MapValue())
			}
			entries.Set(protoreflect.ValueOfString("key").MapKey(), v)
		case fd.IsList():
			list := m.Mutable(fd).List()
			if fd.Kind() == protoreflect.MessageKind {
				v := list.NewElement()
				fillProtoMessage(t, v.Message(), active)
				list.Append(v)
			} else {
				list.Append(nonZeroScalar(t, fd))
			}
		case fd.Kind() == protoreflect.MessageKind:
			v := m.NewField(fd)
			fillProtoMessage(t, v.Message(), active)
			m.Set(fd, v)
		default:
			m.Set(fd, nonZeroScalar(t, fd))
		}
	}
}

func nonZeroScalar(t *testing.T, fd protoreflect.FieldDescriptor) protoreflect.Value {
	t.Helper()
	switch fd.Kind() {
	case protoreflect.StringKind:
		return protoreflect.ValueOfString("x")
	case protoreflect.BoolKind:
		return protoreflect.ValueOfBool(true)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return protoreflect.ValueOfInt32(1)
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return protoreflect.ValueOfInt64(1)
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return protoreflect.ValueOfUint32(1)
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return protoreflect.ValueOfUint64(1)
	case protoreflect.FloatKind:
		return protoreflect.ValueOfFloat32(0.5)
	case protoreflect.DoubleKind:
		return protoreflect.ValueOfFloat64(0.5)
	case protoreflect.BytesKind:
		return protoreflect.ValueOfBytes([]byte("x"))
	case protoreflect.EnumKind:
		return protoreflect.ValueOfEnum(1)
	}
	require.New(t).FailNow("unhandled proto kind", "%s", fd.Kind())
	return protoreflect.Value{}
}

// collectSetFieldPaths is the integration guide's walk: a key under a map
// takes the "*" segment, array elements share the array's path, and the
// walk stops at a path no listed path extends.
func collectSetFieldPaths(prefix string, v any, listed, out map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			path := k
			if prefix != "" {
				path = prefix + "." + k
				if listed[prefix+"."+types.ConfigFieldWildcard] {
					path = prefix + "." + types.ConfigFieldWildcard
				}
			}
			out[path] = true
			if listedBelow(path, listed) {
				collectSetFieldPaths(path, child, listed, out)
			}
		}
	case []any:
		for _, e := range x {
			collectSetFieldPaths(prefix, e, listed, out)
		}
	}
}

// parentPath returns the path p's last segment hangs from, if any.
func parentPath(p string) (string, bool) {
	i := strings.LastIndex(p, ".")
	if i < 0 {
		return "", false
	}
	return p[:i], true
}

func listedBelow(path string, listed map[string]bool) bool {
	for p := range listed {
		if strings.HasPrefix(p, path+".") {
			return true
		}
	}
	return false
}
