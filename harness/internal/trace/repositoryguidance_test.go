package trace

import (
	"context"
	"reflect"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

func testGuidanceRecords() []types.RepositoryGuidanceRecord {
	return []types.RepositoryGuidanceRecord{
		{Path: "AGENTS.md", Tier: types.RepositoryGuidanceTierPinned, Bytes: 120, SHA256: "abc123", RenderedBytes: 120},
		{Path: "CLAUDE.md", Tier: types.RepositoryGuidanceTierAdvisory, Missing: true},
	}
}

func TestEmitters_RecordRepositoryGuidanceReachesRunTrace(t *testing.T) {
	for name, build := range stopDetailsEmitterBuilders(t) {
		if name == "nested" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			e := build(t)
			recorder, ok := e.(RepositoryGuidanceRecorder)
			if !ok {
				t.Fatalf("%s emitter does not implement RepositoryGuidanceRecorder", name)
			}

			e.Start("run-1", &types.RunConfig{RunID: "run-1"})
			recorder.RecordRepositoryGuidance(testGuidanceRecords())
			got, err := e.Finish(context.Background(), "success")
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if !reflect.DeepEqual(got.RepositoryGuidance, testGuidanceRecords()) {
				t.Errorf("RepositoryGuidance = %+v, want %+v", got.RepositoryGuidance, testGuidanceRecords())
			}

			e.Start("run-2", &types.RunConfig{RunID: "run-2"})
			got, err = e.Finish(context.Background(), "success")
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if got.RepositoryGuidance != nil {
				t.Errorf("RepositoryGuidance after restart = %+v, want nil", got.RepositoryGuidance)
			}
		})
	}
}

// Guidance metadata carries no content, so it lands on the root span with
// content capture off (the test emitter's default).
func TestOTelTraceEmitter_RecordRepositoryGuidanceSetsRootAttributes(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()
	emitter.Start("run-guidance", nil)
	emitter.RecordRepositoryGuidance(testGuidanceRecords())
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	root := findSpanByName(t, exporter.GetSpans(), "invoke_agent stirrup")

	want := map[string]any{
		repositoryGuidancePathsKey:         []string{"AGENTS.md", "CLAUDE.md"},
		repositoryGuidanceTiersKey:         []string{"pinned", "advisory"},
		repositoryGuidanceSHA256Key:        []string{"abc123", ""},
		repositoryGuidanceMissingKey:       []bool{false, true},
		repositoryGuidanceTruncatedKey:     []bool{false, false},
		repositoryGuidanceBytesKey:         []int64{120, 0},
		repositoryGuidanceRenderedBytesKey: []int64{120, 0},
	}
	for key, wantVal := range want {
		found := false
		for _, attr := range root.Attributes {
			if string(attr.Key) == key {
				found = true
				if got := attr.Value.AsInterface(); !reflect.DeepEqual(got, wantVal) {
					t.Errorf("attribute %q = %#v, want %#v", key, got, wantVal)
				}
			}
		}
		if !found {
			t.Errorf("attribute %q not found on root span", key)
		}
	}
}

func TestOTelTraceEmitter_RecordRepositoryGuidanceBeforeStart(t *testing.T) {
	emitter, _ := newTestOTelEmitter()
	emitter.RecordRepositoryGuidance(testGuidanceRecords())
}
