package transport

import (
	"slices"
	"sync"

	"google.golang.org/protobuf/reflect/protoreflect"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

var taskAssignmentFieldPaths = sync.OnceValue(func() []string {
	carried := map[string]bool{}
	for _, p := range protoRunConfigFieldPaths() {
		carried[p] = true
	}
	return slices.DeleteFunc(types.ConfigFieldPaths(), func(p string) bool { return !carried[p] })
})

// TaskAssignmentFieldPaths returns the RunConfig field paths, in
// types.ConfigFieldPaths form, that a task_assignment can set: those the
// harness accepts and the compiled-in RunConfig message also carries. A
// field with no protobuf mirror is left out, since this harness drops
// it from a task_assignment whatever a newer proto says.
func TaskAssignmentFieldPaths() []string {
	return slices.Clone(taskAssignmentFieldPaths())
}

// protoRunConfigFieldPaths walks the RunConfig message descriptor with
// the rules of types.ConfigFieldPaths, using protobuf JSON names.
func protoRunConfigFieldPaths() []string {
	var paths []string
	collectProtoFieldPaths("", (&pb.RunConfig{}).ProtoReflect().Descriptor(), &paths, map[protoreflect.FullName]int{})
	return paths
}

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

// isHarnessMessage reports whether a message is one of the harness's own
// rather than a well-known type such as google.protobuf.Struct, which maps
// onto a self-decoding RunConfig field.
func isHarnessMessage(md protoreflect.MessageDescriptor) bool {
	return md.ParentFile().Package() == (&pb.RunConfig{}).ProtoReflect().Descriptor().ParentFile().Package()
}
