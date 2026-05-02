package transport

import (
	"testing"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
)

// TestRunConfigFromProto_SessionNamePropagates ensures that a SessionName set
// on the proto RunConfig (the wire format used by the gRPC / K8s job path) is
// copied into the internal types.RunConfig. Without this, every job dispatched
// via the control plane would silently lose its session label.
func TestRunConfigFromProto_SessionNamePropagates(t *testing.T) {
	name := "nightly-eval"
	pc := &pb.RunConfig{
		SessionName: &name,
	}

	rc := runConfigFromProto(pc)

	if rc.SessionName != "nightly-eval" {
		t.Fatalf("SessionName not propagated: got %q, want %q", rc.SessionName, "nightly-eval")
	}
}

// TestRunConfigFromProto_SessionNameAbsentWhenNil documents the safe default:
// when the proto omits SessionName (the field is a proto3 optional, so the
// zero value is a nil pointer), the internal RunConfig must surface the empty
// string rather than panicking on a nil dereference.
func TestRunConfigFromProto_SessionNameAbsentWhenNil(t *testing.T) {
	pc := &pb.RunConfig{}

	rc := runConfigFromProto(pc)

	if rc.SessionName != "" {
		t.Fatalf("SessionName should be empty when proto field is nil, got %q", rc.SessionName)
	}
}
