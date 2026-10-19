package trace

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

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
			require.True(t, ok, "emitter does not implement RepositoryGuidanceRecorder")

			e.Start("run-1", &types.RunConfig{RunID: "run-1"})
			recorder.RecordRepositoryGuidance(testGuidanceRecords())
			got, err := e.Finish(context.Background(), "success")
			require.NoError(t, err)
			require.Equal(t, testGuidanceRecords(), got.RepositoryGuidance)

			e.Start("run-2", &types.RunConfig{RunID: "run-2"})
			got, err = e.Finish(context.Background(), "success")
			require.NoError(t, err)
			require.Nil(t, got.RepositoryGuidance, "guidance after restart")
		})
	}
}

// Guidance metadata carries no content, so it lands on the root span with
// content capture off (the test emitter's default).
func TestOTelTraceEmitter_RecordRepositoryGuidanceSetsRootAttributes(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()
	emitter.Start("run-guidance", nil)
	emitter.RecordRepositoryGuidance(testGuidanceRecords())
	_, err := emitter.Finish(context.Background(), "success")
	require.NoError(t, err)
	root := findSpanByName(t, exporter.GetSpans(), "invoke_agent stirrup")
	attrs := spanAttrs(root)

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
		t.Run(key, func(t *testing.T) {
			got, ok := attrs[key]
			require.True(t, ok, "attribute not found on root span")
			require.Equal(t, wantVal, got)
		})
	}
}

func TestOTelTraceEmitter_RecordRepositoryGuidanceBeforeStart(t *testing.T) {
	emitter, _ := newTestOTelEmitter()
	require.NotPanics(t, func() {
		emitter.RecordRepositoryGuidance(testGuidanceRecords())
	})
}
