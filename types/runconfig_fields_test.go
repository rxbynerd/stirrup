package types

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigFieldPaths_ListsKnownPaths(t *testing.T) {
	check := assert.New(t)
	got := ConfigFieldPaths()
	for _, want := range []string{
		"runId",
		"provider",
		"provider.endpointProfile",
		"provider.credential.tokenSource.type",
		"providers.*",
		"providers.*.type",
		"dynamicContext.*.value",
		"tools.lazyLoading",
		"tools.lazyLoading.eager",
		"tools.mcpServers.allowedMcpHosts",
		"outputSchema",
		"guardRail.stages.type",
		"guardRail.stages.stages",
		"verifier.verifiers.command",
		"verifier.verifiers.verifiers",
	} {
		check.Contains(got, want)
	}
}

func TestConfigFieldPaths_SortedUniqueAndStable(t *testing.T) {
	must := require.New(t)
	got := ConfigFieldPaths()
	must.NotEmpty(got)
	must.True(slices.IsSorted(got), "paths are sorted")
	must.Len(slices.Compact(slices.Clone(got)), len(got), "paths are unique")
	must.Equal(got, ConfigFieldPaths(), "repeated calls return the same paths")

	got[0] = "mutated"
	must.NotEqual("mutated", ConfigFieldPaths()[0], "mutating the returned slice must not change later calls")
}

func TestConfigFieldPaths_SelfDecodingFieldsAreLeaves(t *testing.T) {
	check := assert.New(t)
	for _, p := range ConfigFieldPaths() {
		check.False(strings.HasPrefix(p, "outputSchema."), "raw JSON field descended into: %s", p)
	}
}

func TestConfigFieldPaths_RecursiveTypesStopAfterOneNestingLevel(t *testing.T) {
	check := assert.New(t)
	for _, p := range ConfigFieldPaths() {
		check.False(strings.HasPrefix(p, "guardRail.stages.stages."), "guardrail walked past one nesting level: %s", p)
		check.False(strings.HasPrefix(p, "verifier.verifiers.verifiers."), "verifier walked past one nesting level: %s", p)
	}
}

func TestConfigFieldPaths_TopLevelKeysMatchStrictDecoder(t *testing.T) {
	must := require.New(t)
	for _, p := range ConfigFieldPaths() {
		if strings.Contains(p, ".") {
			continue
		}
		must.NoError(decodeStrict(`{"`+p+`":null}`), "listed top-level field %q", p)
	}
	must.Error(decodeStrict(`{"notARunConfigField":null}`), "an unlisted field is rejected")
}

type fieldWalkInner struct {
	Leaf string `json:"leaf"`
}

type fieldWalkEmbedded struct {
	Promoted string `json:"promoted"`
}

type fieldWalkRecursive struct {
	Next *fieldWalkRecursive `json:"next"`
}

type fieldWalkSubject struct {
	fieldWalkEmbedded
	Named     string `json:"named,omitempty"`
	Untagged  string
	Skipped   string                    `json:"-"`
	hidden    string                    //nolint:unused // exercises skipping of unexported fields
	Inner     *fieldWalkInner           `json:"inner"`
	List      []fieldWalkInner          `json:"list"`
	ByKey     map[string]fieldWalkInner `json:"byKey"`
	Labels    map[string]string         `json:"labels"`
	Raw       json.RawMessage           `json:"raw"`
	Bytes     []byte                    `json:"bytes"`
	Recursive fieldWalkRecursive        `json:"recursive"`
}

func TestCollectConfigFieldPaths_FollowsEncodingJSONNaming(t *testing.T) {
	must := require.New(t)
	seen := map[string]struct{}{}
	collectConfigFieldPaths("", reflect.TypeFor[fieldWalkSubject](), seen, map[reflect.Type]int{})
	got := make([]string, 0, len(seen))
	for p := range seen {
		got = append(got, p)
	}
	slices.Sort(got)
	want := []string{
		"Untagged",
		"byKey",
		"byKey.*",
		"byKey.*.leaf",
		"bytes",
		"inner",
		"inner.leaf",
		"labels",
		"labels.*",
		"list",
		"list.leaf",
		"named",
		"promoted",
		"raw",
		"recursive",
		"recursive.next",
		"recursive.next.next",
	}
	must.Equal(want, got)
}

func decodeStrict(doc string) error {
	var cfg RunConfig
	dec := json.NewDecoder(bytes.NewReader([]byte(doc)))
	dec.DisallowUnknownFields()
	return dec.Decode(&cfg)
}
