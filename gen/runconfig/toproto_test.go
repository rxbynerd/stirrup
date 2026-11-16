package runconfig

import (
	"encoding/json"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

func TestToProto_NilRunConfigIsAnError(t *testing.T) {
	must := require.New(t)
	pc, err := ToProto(nil)
	must.Error(err)
	must.Nil(pc)
}

func TestFromProto_NilMessageIsNil(t *testing.T) {
	must := require.New(t)
	must.Nil(FromProto(nil))
}

func TestToProto_RejectsFieldsWithNoWireField(t *testing.T) {
	cases := []struct {
		name string
		root string
		path string
		set  func(*types.RunConfig)
	}{
		{"stdio transport", "transport", "transport.type", func(rc *types.RunConfig) { rc.Transport.Type = "stdio" }},
		{"transport address", "transport", "transport.address", func(rc *types.RunConfig) {
			rc.Transport.Address = "cp.example.com:443"
		}},
		{"trace emitter credential", "traceEmitter.credential", "traceEmitter.credential", func(rc *types.RunConfig) {
			rc.TraceEmitter.Credential = &types.CredentialConfig{Type: "gcp-workload-identity"}
		}},
		{"trace archive credential", "traceEmitter.archive.credential", "traceEmitter.archive.credential", func(rc *types.RunConfig) {
			rc.TraceEmitter.Archive = &types.TraceArchiveConfig{Type: "gcs", Credential: &types.CredentialConfig{Type: "gcp-workload-identity"}}
		}},
	}
	covered := map[string]bool{}
	for _, tc := range cases {
		covered[tc.root] = true
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			rc := grpcRunConfig()
			tc.set(rc)
			pc, err := ToProto(rc)
			must.ErrorIs(err, ErrNotOnWire)
			must.ErrorContains(err, tc.path+":")
			must.Nil(pc)
		})
	}
	for _, path := range notOnWirePaths {
		assert.New(t).True(covered[path], "no rejection case for %q", path)
	}
}

// grpcRunConfig returns an otherwise empty RunConfig with the transport
// ToProto requires.
func grpcRunConfig() *types.RunConfig {
	return &types.RunConfig{Transport: types.TransportConfig{Type: "grpc"}}
}

func TestToProto_RequiresTheGRPCTransport(t *testing.T) {
	t.Run("grpc is accepted", func(t *testing.T) {
		must := require.New(t)
		_, err := ToProto(grpcRunConfig())
		must.NoError(err)
	})
	t.Run("unset is rejected with the value to set", func(t *testing.T) {
		must := require.New(t)
		_, err := ToProto(&types.RunConfig{})
		must.ErrorContains(err, `transport.type: unset; set it to "grpc"`)
		must.NotErrorIs(err, ErrNotOnWire)
	})
}

func TestToProto_RejectsIntegersOutsideTheWireRange(t *testing.T) {
	must := require.New(t)
	tooBig := math.MaxInt32 + 1
	rc := grpcRunConfig()
	rc.MaxTurns = tooBig
	rc.MaxTokenBudget = &tooBig
	rc.Executor.Resources = &types.ResourceLimits{MemoryMB: math.MinInt32 - 1}
	_, err := ToProto(rc)
	must.Error(err)
	for _, path := range []string{"maxTurns:", "maxTokenBudget:", "executor.resources.memoryMb:"} {
		must.ErrorContains(err, path)
	}
}

func TestToProto_RejectsSchemasThatAreNotJSONObjects(t *testing.T) {
	cases := []struct {
		name string
		path string
		set  func(*types.RunConfig, json.RawMessage)
	}{
		{"output schema", "outputSchema:", func(rc *types.RunConfig, raw json.RawMessage) { rc.OutputSchema = raw }},
		{"control-plane tool input schema", "tools.controlPlane[0].inputSchema:", func(rc *types.RunConfig, raw json.RawMessage) {
			rc.Tools.ControlPlane = []types.ControlPlaneToolConfig{{Name: "lookup", InputSchema: raw}}
		}},
	}
	for _, tc := range cases {
		for _, body := range []struct{ name, raw string }{
			{"null", `null`}, {"array", `[]`}, {"string", `"schema"`}, {"truncated", `{`},
		} {
			t.Run(tc.name+" "+body.name, func(t *testing.T) {
				must := require.New(t)
				rc := grpcRunConfig()
				tc.set(rc, json.RawMessage(body.raw))
				_, err := ToProto(rc)
				must.ErrorContains(err, tc.path)
			})
		}
	}
}

func TestToProto_ReportsEveryErrorAtOnce(t *testing.T) {
	must := require.New(t)
	rc := &types.RunConfig{
		Transport:    types.TransportConfig{Type: "stdio"},
		MaxTurns:     math.MaxInt32 + 1,
		OutputSchema: json.RawMessage(`[]`),
	}
	_, err := ToProto(rc)
	for _, line := range []string{"runconfig: transport.type:", "runconfig: maxTurns:", "runconfig: outputSchema:"} {
		must.ErrorContains(err, line)
	}
}

// TestToProto_PreservesBlockPresence pins that a nil pointer block stays
// absent on the wire and an explicit empty block stays present, since
// the harness treats the two differently for several blocks.
func TestToProto_PreservesBlockPresence(t *testing.T) {
	t.Run("nil blocks are absent", func(t *testing.T) {
		must := require.New(t)
		pc, err := ToProto(grpcRunConfig())
		must.NoError(err)
		must.True(proto.Equal(&pb.RunConfig{}, pc), "an empty RunConfig translates to an empty message, got %v", pc)
	})
	t.Run("explicit empty blocks are present", func(t *testing.T) {
		must := require.New(t)
		rc := &types.RunConfig{
			Transport:            types.TransportConfig{Type: "grpc"},
			RuleOfTwo:            &types.RuleOfTwoConfig{},
			Hooks:                &types.HooksConfig{},
			ToolDispatch:         &types.ToolDispatchConfig{},
			ToolChoiceEscalation: &types.ToolChoiceEscalationConfig{},
			ResultSink:           &types.ResultSinkConfig{},
			Provider:             types.ProviderConfig{Type: "anthropic", Retry: &types.ProviderRetryConfig{}},
		}
		pc, err := ToProto(rc)
		must.NoError(err)
		must.NotNil(pc.RuleOfTwo)
		must.Nil(pc.RuleOfTwo.Enforce, "an unset enforce stays unset so the validator applies the secure default")
		must.NotNil(pc.Hooks)
		must.NotNil(pc.ToolDispatch)
		must.NotNil(pc.ToolChoiceEscalation)
		must.NotNil(pc.ResultSink)
		must.NotNil(pc.Provider.Retry)

		back := FromProto(pc)
		must.Equal(rc.RuleOfTwo, back.RuleOfTwo)
		must.Equal(rc.Hooks, back.Hooks)
		must.Equal(rc.Provider.Retry, back.Provider.Retry)
	})
}

// TestToProto_ValidatedConfigReachesTheHarnessUnchanged pins the
// documented control-plane sequence: a config validated and sent with
// ToProto decodes to the same config, which validates again unchanged.
func TestToProto_ValidatedConfigReachesTheHarnessUnchanged(t *testing.T) {
	must := require.New(t)
	rc := &types.RunConfig{
		RunID:            "parity",
		Mode:             "planning",
		Prompt:           "Summarise the repository.",
		Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "anthropic", Model: "claude-haiku-5-5"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"read_file", "list_directory"}},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "deny-side-effects"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: "/workspace"},
		Transport:        types.TransportConfig{Type: "grpc"},
		MaxTurns:         5,
		Timeout:          new(120),
	}
	must.NoError(types.ValidateRunConfig(rc))

	pc, err := ToProto(rc)
	must.NoError(err)
	got := FromProto(pc)
	must.NoError(types.ValidateRunConfig(got))
	must.Equal(rc, got)
}

// TestToProto_RangeChecksEveryInt32Field sets each int field of a full
// RunConfig, one at a time, just outside and exactly at the int32
// bounds, and checks ToProto rejects the first and accepts the second.
func TestToProto_RangeChecksEveryInt32Field(t *testing.T) {
	paths := intFieldPaths(t)
	must := require.New(t)
	must.Contains(paths, "heartbeatSeconds")
	must.Contains(paths, "hooks.preRun.timeoutSeconds")
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			for _, bound := range []int{math.MaxInt32, math.MinInt32} {
				must := require.New(t)
				outside := bound + 1
				if bound == math.MinInt32 {
					outside = bound - 1
				}
				rc := filledRunConfig(t)
				setIntField(t, rc, path, bound)
				_, err := ToProto(rc)
				must.NoError(err, "%s = %d is in range", path, bound)

				rc = filledRunConfig(t)
				setIntField(t, rc, path, outside)
				_, err = ToProto(rc)
				must.Error(err, "%s = %d is out of range", path, outside)
				must.Contains(normalisedErrorPaths(err), path)
			}
		})
	}
}

// intFieldPaths returns the JSON path of every int-typed field in a full
// RunConfig, the fields ToProto narrows to int32.
func intFieldPaths(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	walkIntFields(reflect.ValueOf(filledRunConfig(t)).Elem(), "", func(path string, _ reflect.Value) {
		seen[path] = true
	})
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	return paths
}

// setIntField sets every int field at path in rc to v.
func setIntField(t *testing.T, rc *types.RunConfig, path string, v int) {
	t.Helper()
	n := 0
	walkIntFields(reflect.ValueOf(rc).Elem(), "", func(p string, field reflect.Value) {
		if p == path {
			field.SetInt(int64(v))
			n++
		}
	})
	require.New(t).Positive(n, "no int field at %s", path)
}

// walkIntFields calls fn for every settable int field reachable from v,
// with map values replaced so their fields can be set.
func walkIntFields(v reflect.Value, path string, fn func(string, reflect.Value)) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			walkIntFields(v.Elem(), path, fn)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if f := v.Type().Field(i); f.IsExported() {
				walkIntFields(v.Field(i), joinPath(path, jsonName(f)), fn)
			}
		}
	case reflect.Slice:
		if v.Type() == rawMessageType {
			return
		}
		for i := range v.Len() {
			walkIntFields(v.Index(i), path, fn)
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			elem := reflect.New(v.Type().Elem()).Elem()
			elem.Set(iter.Value())
			walkIntFields(elem, path+"."+types.ConfigFieldWildcard, fn)
			v.SetMapIndex(iter.Key(), elem)
		}
	case reflect.Int:
		fn(path, v)
	}
}

// normalisedErrorPaths returns the field path of each line of a ToProto
// error with list indexes dropped and map keys replaced by
// types.ConfigFieldWildcard, so it compares with a JSON path.
func normalisedErrorPaths(err error) []string {
	var out []string
	for _, line := range strings.Split(err.Error(), "\n") {
		path, _, _ := strings.Cut(strings.TrimPrefix(line, "runconfig: "), ":")
		path = listIndex.ReplaceAllString(path, "")
		path = fillerMapKey.ReplaceAllString(path, "."+types.ConfigFieldWildcard)
		out = append(out, path)
	}
	return out
}

var (
	listIndex    = regexp.MustCompile(`\[\d+\]`)
	fillerMapKey = regexp.MustCompile(`\.key[12]\b`)
)

// TestToProto_OmitsEmptyNestedBlocks pins that a value-typed block left at
// its zero value inside a block that is set does not reach the wire, so
// PopulatedFieldPaths of the message lists only what the config sets.
func TestToProto_OmitsEmptyNestedBlocks(t *testing.T) {
	must := require.New(t)
	rc := grpcRunConfig()
	rc.Tools.BuiltIn = []string{"read_file"}
	rc.Observability.ServiceName = "control-plane"
	pc, err := ToProto(rc)
	must.NoError(err)
	must.Nil(pc.Tools.LazyLoading)
	must.Nil(pc.Tools.CommandOutput)
	must.Nil(pc.Observability.LogsExport)
	must.Equal([]string{"observability", "observability.serviceName", "tools", "tools.builtIn"}, PopulatedFieldPaths(pc))
}
