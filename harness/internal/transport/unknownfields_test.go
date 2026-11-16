package transport

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
)

// appendUnknownVarint appends a varint field a newer schema would carry.
func appendUnknownVarint(b []byte, num protowire.Number, v uint64) []byte {
	b = protowire.AppendTag(b, num, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}

// fieldNumber is the number of the named field of md, which must exist.
func fieldNumber(t *testing.T, md protoreflect.MessageDescriptor, name protoreflect.Name) protowire.Number {
	t.Helper()
	fd := md.Fields().ByName(name)
	require.New(t).NotNil(fd, "%s has no field %s", md.FullName(), name)
	return fd.Number()
}

// unknownTo asserts that num is not a field of md, so the test's extra
// field stays unknown however the schema grows.
func unknownTo(t *testing.T, md protoreflect.MessageDescriptor, num protowire.Number) protowire.Number {
	t.Helper()
	require.New(t).Nil(md.Fields().ByNumber(num), "%s now carries field %d", md.FullName(), num)
	return num
}

// newerSchemaAssignment encodes a task_assignment as a control plane built
// against a newer schema would: the RunConfig gains a top-level field and
// its executor gains one too.
func newerSchemaAssignment(t *testing.T) []byte {
	t.Helper()
	must := require.New(t)
	rcDesc := (&pb.RunConfig{}).ProtoReflect().Descriptor()
	topLevel := unknownTo(t, rcDesc, 9001)
	nested := unknownTo(t, (&pb.ExecutorConfig{}).ProtoReflect().Descriptor(), 9002)

	executor, err := proto.Marshal(&pb.ExecutorConfig{Type: "container", Network: &pb.NetworkConfig{Mode: "none"}})
	must.NoError(err)
	executor = appendUnknownVarint(executor, nested, 1)

	task, err := proto.Marshal(&pb.RunConfig{RunId: "run-newer-schema", Mode: "execution", MaxTurns: 3})
	must.NoError(err)
	task = protowire.AppendTag(task, fieldNumber(t, rcDesc, "executor"), protowire.BytesType)
	task = protowire.AppendBytes(task, executor)
	task = appendUnknownVarint(task, topLevel, 1)

	ceDesc := (&pb.ControlEvent{}).ProtoReflect().Descriptor()
	event, err := proto.Marshal(&pb.ControlEvent{Type: "task_assignment"})
	must.NoError(err)
	event = protowire.AppendTag(event, fieldNumber(t, ceDesc, "task"), protowire.BytesType)
	return protowire.AppendBytes(event, task)
}

func TestControlEventFromProto_ReportsUnknownTaskFieldsFromNewerSchema(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var pe pb.ControlEvent
	must.NoError(proto.Unmarshal(newerSchemaAssignment(t), &pe))

	ev := controlEventFromProto(&pe)

	must.NotNil(ev.Task)
	check.Equal("run-newer-schema", ev.Task.RunID, "the known fields still decode")
	check.Equal("none", ev.Task.Executor.Network.Mode)
	check.Equal([]string{"field 9001 at the top level", "field 9002 under executor"}, ev.UnknownTaskFields)
}

func TestControlEventFromProto_KnownSchemaHasNoUnknownTaskFields(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	schema, err := structpb.NewStruct(map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}})
	must.NoError(err)
	sent := &pb.ControlEvent{Type: "task_assignment", Task: &pb.RunConfig{
		RunId:        "run-known",
		Executor:     &pb.ExecutorConfig{Type: "local", K8SNodeSelector: map[string]string{"pool": "a"}},
		Providers:    map[string]*pb.ProviderConfig{"fast": {Type: "anthropic"}},
		Tools:        &pb.ToolsConfig{McpServers: []*pb.MCPServerConfig{{Name: "a"}, {Name: "b"}}},
		OutputSchema: schema,
	}}
	wire, err := proto.Marshal(sent)
	must.NoError(err)
	var pe pb.ControlEvent
	must.NoError(proto.Unmarshal(wire, &pe))

	check.Nil(controlEventFromProto(&pe).UnknownTaskFields)
	check.Nil(controlEventFromProto(&pb.ControlEvent{Type: "cancel"}).UnknownTaskFields, "an event without a task has none")
}

func TestUnknownRunConfigFields_ReachesRepeatedMapAndStructValues(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	extra := appendUnknownVarint(nil, 9003, 1)
	withExtra := func(m proto.Message) {
		m.ProtoReflect().SetUnknown(extra)
	}

	first, second := &pb.MCPServerConfig{Name: "a"}, &pb.MCPServerConfig{Name: "b"}
	withExtra(first)
	withExtra(second)
	fast := &pb.ProviderConfig{Type: "anthropic"}
	withExtra(fast)
	stage := &pb.GuardRailConfig{Type: "granite-guardian"}
	withExtra(stage)
	schema, err := structpb.NewStruct(map[string]any{"type": "object"})
	must.NoError(err)
	withExtra(schema.Fields["type"])

	rc := &pb.RunConfig{
		Tools:        &pb.ToolsConfig{McpServers: []*pb.MCPServerConfig{first, second}},
		Providers:    map[string]*pb.ProviderConfig{"secret-looking-key": fast},
		GuardRail:    &pb.GuardRailConfig{Type: "composite", Stages: []*pb.GuardRailConfig{stage}},
		OutputSchema: schema,
	}

	check.Equal([]string{
		"field 9003 under guardRail.stages",
		"field 9003 under outputSchema.fields.*",
		"field 9003 under providers.*",
		"field 9003 under tools.mcpServers",
	}, unknownRunConfigFields(rc), "sorted by path, one entry per path and number, no map keys")
}

func TestUnknownRunConfigFields_ReportsEveryNumberInOneMessage(t *testing.T) {
	check := assert.New(t)
	rc := &pb.RunConfig{}
	rc.ProtoReflect().SetUnknown(appendUnknownVarint(appendUnknownVarint(appendUnknownVarint(nil, 9005, 1), 9004, 1), 9005, 2))

	check.Equal([]string{"field 9004 at the top level", "field 9005 at the top level"}, unknownRunConfigFields(rc))
}

func TestUnknownRunConfigFields_UnparseableUnknownDataIsReported(t *testing.T) {
	check := assert.New(t)
	executor := &pb.ExecutorConfig{Type: "local"}
	executor.ProtoReflect().SetUnknown([]byte{0xff})
	rc := &pb.RunConfig{Executor: executor}

	check.Equal([]string{"unparseable field data under executor"}, unknownRunConfigFields(rc))
	check.Nil(unknownRunConfigFields(nil))
}

func TestUnknownRunConfigFields_RetiredFieldIsUnknown(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	rcDesc := (&pb.RunConfig{}).ProtoReflect().Descriptor()
	check.True(rcDesc.ReservedNames().Has("max_cost_budget"))
	var rc pb.RunConfig
	must.NoError(proto.Unmarshal(appendUnknownVarint(nil, unknownTo(t, rcDesc, 18), 5), &rc))

	check.Equal([]string{"field 18 at the top level"}, unknownRunConfigFields(&rc))
}
