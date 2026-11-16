package runconfig

import (
	"slices"

	"google.golang.org/protobuf/reflect/protoreflect"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

// PopulatedFieldPaths returns the path of every field pc populates, in
// the form of ready.config_fields, sorted and without duplicates. A path
// missing from a harness's config_fields is a field that harness does not
// know. Map keys appear as types.ConfigFieldWildcard, repeated fields
// list their elements' fields under their own path, a
// google.protobuf.Struct is not descended into, and a recursive message
// is walked one level inside itself, as config_fields is.
func PopulatedFieldPaths(pc *pb.RunConfig) []string {
	if pc == nil {
		return nil
	}
	seen := map[string]struct{}{}
	collectPopulated("", pc.ProtoReflect(), seen, map[protoreflect.FullName]int{})
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	return paths
}

func collectPopulated(prefix string, m protoreflect.Message, seen map[string]struct{}, active map[protoreflect.FullName]int) {
	md := m.Descriptor()
	if active[md.FullName()] >= types.ConfigFieldTypeNesting {
		return
	}
	active[md.FullName()]++
	defer func() { active[md.FullName()]-- }()
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		path := fd.JSONName()
		if prefix != "" {
			path = prefix + "." + path
		}
		seen[path] = struct{}{}
		switch {
		case fd.IsMap():
			keyPath := path + "." + types.ConfigFieldWildcard
			seen[keyPath] = struct{}{}
			if isHarnessMessage(fd.MapValue()) {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					collectPopulated(keyPath, mv.Message(), seen, active)
					return true
				})
			}
		case fd.IsList():
			if isHarnessMessage(fd) {
				list := v.List()
				for i := range list.Len() {
					collectPopulated(path, list.Get(i).Message(), seen, active)
				}
			}
		case isHarnessMessage(fd):
			collectPopulated(path, v.Message(), seen, active)
		}
		return true
	})
}

// isHarnessMessage reports whether fd holds one of the harness's own
// messages rather than a scalar or a well-known type such as
// google.protobuf.Struct, whose content is data.
func isHarnessMessage(fd protoreflect.FieldDescriptor) bool {
	return fd.Kind() == protoreflect.MessageKind &&
		fd.Message().ParentFile().Package() == (&pb.RunConfig{}).ProtoReflect().Descriptor().ParentFile().Package()
}
