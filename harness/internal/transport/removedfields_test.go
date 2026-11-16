package transport

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
)

func taskAssignmentWithExecutorWire(t *testing.T, withRegistryAllowlist bool) *pb.ControlEvent {
	t.Helper()
	var raw []byte
	raw = protowire.AppendTag(raw, 1, protowire.BytesType)
	raw = protowire.AppendString(raw, "container")
	if withRegistryAllowlist {
		raw = protowire.AppendTag(raw, removedRegistryAllowlistField, protowire.BytesType)
		raw = protowire.AppendString(raw, "ghcr.io/acme/*")
	}
	raw = protowire.AppendTag(raw, 4, protowire.BytesType)
	raw = protowire.AppendString(raw, "ghcr.io/acme/base")
	var ec pb.ExecutorConfig
	require.NoError(t, proto.Unmarshal(raw, &ec))
	return &pb.ControlEvent{Type: "task_assignment", Task: &pb.RunConfig{Executor: &ec}}
}

func captureDefaultLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestControlEventFromProto_RemovedRegistryAllowlistWarnsAndStaysUnknown(t *testing.T) {
	must := require.New(t)
	logs := captureDefaultLog(t)

	ev := controlEventFromProto(taskAssignmentWithExecutorWire(t, true))

	must.NotNil(ev.Task)
	must.Equal("ghcr.io/acme/base", ev.Task.Executor.Image)
	must.NotEmpty(ev.UnknownTaskFields)
	must.Contains(logs.String(), "registry_allowlist")
	must.Contains(logs.String(), "STIRRUP_IMAGE_ALLOWLIST")
}

func TestControlEventFromProto_SilentWithoutRemovedField(t *testing.T) {
	must := require.New(t)
	logs := captureDefaultLog(t)

	ev := controlEventFromProto(taskAssignmentWithExecutorWire(t, false))

	must.Empty(ev.UnknownTaskFields)
	must.Empty(logs.String())
}

func TestHasUnknownField_StopsOnTruncatedInput(t *testing.T) {
	must := require.New(t)
	raw := protowire.AppendTag(nil, 3, protowire.BytesType)
	raw = append(raw, 0x05, 'a')
	must.False(hasUnknownField(raw, removedRegistryAllowlistField))
}
