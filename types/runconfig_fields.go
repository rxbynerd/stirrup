package types

import (
	"encoding"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"sync"
)

// ConfigFieldWildcard is the path segment that stands for a map key in
// the paths ConfigFieldPaths returns.
const ConfigFieldWildcard = "*"

var configFieldPaths = sync.OnceValue(func() []string {
	seen := map[string]struct{}{}
	collectConfigFieldPaths("", reflect.TypeFor[RunConfig](), seen, map[reflect.Type]int{})
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	return paths
})

// ConfigFieldPaths returns the dotted JSON path of every RunConfig field
// this binary accepts, sorted and without duplicates, as a new slice on
// every call. A struct-valued field is listed along with each of its
// fields; an array contributes its element's fields under its own path;
// a map contributes a ConfigFieldWildcard segment for its keys, followed
// by its value's fields. Types that decode themselves are not descended.
// A type that contains itself (a composite guardrail's stages, a
// composite verifier's sub-verifiers) is walked one nesting level deep:
// its own field at the second level is listed, but nothing beneath it.
func ConfigFieldPaths() []string {
	return slices.Clone(configFieldPaths())
}

// ConfigFieldTypeNesting is how many times one struct type may appear
// on a descent, so a recursive type is walked one level inside itself.
const ConfigFieldTypeNesting = 2

var (
	jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// collectConfigFieldPaths records the paths beneath prefix for a value of
// type t, following encoding/json's field naming. active counts the
// struct types on the current descent so a recursive type ends the walk
// after ConfigFieldTypeNesting levels instead of looping.
func collectConfigFieldPaths(prefix string, t reflect.Type, seen map[string]struct{}, active map[reflect.Type]int) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if decodesItself(t) {
		return
	}
	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		collectConfigFieldPaths(prefix, t.Elem(), seen, active)
	case reflect.Map:
		path := joinConfigFieldPath(prefix, ConfigFieldWildcard)
		seen[path] = struct{}{}
		collectConfigFieldPaths(path, t.Elem(), seen, active)
	case reflect.Struct:
		if active[t] >= ConfigFieldTypeNesting {
			return
		}
		active[t]++
		defer func() { active[t]-- }()
		for i := range t.NumField() {
			collectStructField(prefix, t.Field(i), seen, active)
		}
	}
}

func collectStructField(prefix string, f reflect.StructField, seen map[string]struct{}, active map[reflect.Type]int) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return
	}
	name, _, _ := strings.Cut(tag, ",")
	if f.Anonymous && name == "" {
		embedded := f.Type
		if embedded.Kind() == reflect.Pointer {
			embedded = embedded.Elem()
		}
		if embedded.Kind() == reflect.Struct {
			collectConfigFieldPaths(prefix, embedded, seen, active)
			return
		}
	}
	if !f.IsExported() {
		return
	}
	if name == "" {
		name = f.Name
	}
	path := joinConfigFieldPath(prefix, name)
	seen[path] = struct{}{}
	collectConfigFieldPaths(path, f.Type, seen, active)
}

func decodesItself(t reflect.Type) bool {
	pt := reflect.PointerTo(t)
	return pt.Implements(jsonUnmarshalerType) || pt.Implements(textUnmarshalerType)
}

func joinConfigFieldPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}
