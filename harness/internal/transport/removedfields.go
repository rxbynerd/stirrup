package transport

import (
	"log/slog"

	"google.golang.org/protobuf/encoding/protowire"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
)

// removedRegistryAllowlistField is the reserved ExecutorConfig field number
// that once carried registry_allowlist.
const removedRegistryAllowlistField protowire.Number = 14

// warnRemovedTaskFields logs the replacement for a removed RunConfig field
// that a task assignment still sets. The assignment is rejected as setting
// an unknown field; the rejection names only the field number.
func warnRemovedTaskFields(rc *pb.RunConfig) {
	if hasUnknownField(rc.GetExecutor().ProtoReflect().GetUnknown(), removedRegistryAllowlistField) {
		slog.Warn("task_assignment sets executor.registry_allowlist, which this harness does not support; the deployment sets the sandbox image admission policy with STIRRUP_IMAGE_ALLOWLIST (docs/security.md#image-admission-policy)")
	}
}

// hasUnknownField reports whether raw, a message's unknown-field bytes,
// holds field num.
func hasUnknownField(raw []byte, num protowire.Number) bool {
	for len(raw) > 0 {
		n, typ, tagLen := protowire.ConsumeTag(raw)
		if tagLen < 0 {
			return false
		}
		if n == num {
			return true
		}
		valueLen := protowire.ConsumeFieldValue(n, typ, raw[tagLen:])
		if valueLen < 0 {
			return false
		}
		raw = raw[tagLen+valueLen:]
	}
	return false
}
