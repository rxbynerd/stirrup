package builtins

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// TestStructuredEnvelopes_WireViewKeepsEveryNonBodyField pins the contract
// between these envelopes and types.ToolResultWireEnvelope: the wire view
// drops exactly the listed body fields and carries every other field through
// unchanged. A field added to an envelope fails here until the wire view
// either keeps it or lists it as body.
func TestStructuredEnvelopes_WireViewKeepsEveryNonBodyField(t *testing.T) {
	tests := []struct {
		kind    string
		payload any
		body    []string
		derived []string
	}{
		{
			kind:    kindFileExcerpt,
			payload: fileExcerpt{Path: "a.go", StartLine: 2, EndLine: 3, Truncated: true, PastEOF: true, Lines: []string{"x", "y"}},
			body:    []string{"lines"},
		},
		{
			kind:    kindSearchResult,
			payload: searchResult{Matches: []searchMatch{{Path: "a.go", Line: 4, Column: 7, Text: "match"}}, Truncated: true},
			body:    []string{"matches"},
			derived: []string{"columns"},
		},
		{
			kind:    kindFindResult,
			payload: findResult{Paths: []string{"a.go"}, Truncated: true},
			body:    []string{"paths"},
		},
		{
			kind: kindCommandResult,
			payload: commandResult{
				Stdout: "out", Stderr: "err", ExitCode: 1, TimedOut: true, TimeoutSeconds: 30,
				Spilled: true, CaptureComplete: true, ArchiveID: "arch",
				StdoutRef: "stirrup://command-output/r/stdout", StderrRef: "stirrup://command-output/r/stderr",
				StdoutRawBytes: 1, StderrRawBytes: 2, StdoutScrubbedBytes: 3, StderrScrubbedBytes: 4,
				StdoutSHA256: "s1", StderrSHA256: "s2",
			},
			body: []string{"stdout", "stderr"},
		},
		{
			kind: kindCommandOutputChunk,
			payload: commandOutputChunkResult{
				Reference: "stirrup://command-output/r/stdout", Stream: "stdout", Offset: 1, EndOffset: 2, EOF: true,
				Content: "chunk", RawBytes: 3, RawSHA256: "r", ScrubbedBytes: 4, ScrubbedSHA256: "s", RedactionCount: 5,
			},
			body: []string{"content"},
		},
		{
			kind:    kindGitStatus,
			payload: gitStatusResult{Branch: "main", Clean: true, Entries: []gitStatusEntry{{Code: " M", Staged: " ", Unstaged: "M", Path: "a.go"}}, Truncated: true},
			body:    []string{"entries"},
		},
		{
			kind:    kindGitChangedFiles,
			payload: gitChangedFilesResult{Staged: true, Files: []gitChangedFile{{Status: "M", Path: "a.go"}}, Truncated: true},
			body:    []string{"files"},
		},
		{
			kind:    kindGitDiff,
			payload: gitDiffResult{Diff: "+x\n", Staged: true, Path: "a.go", Truncated: true},
			body:    []string{"diff"},
		},
		{
			kind:    kindGitShow,
			payload: gitShowResult{Ref: "HEAD", Path: "a.go", Output: "commit", Truncated: true},
			body:    []string{"output"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			must := require.New(t)
			full, err := json.Marshal(tt.payload)
			must.NoError(err, "marshal payload")
			wire := types.ToolResultWireEnvelope(types.ContentBlock{
				Type: "tool_result", Content: "text rendering", Kind: tt.kind, Structured: full,
			})

			var fullFields, wireFields map[string]json.RawMessage
			must.NoError(json.Unmarshal(full, &fullFields), "decode full envelope")
			must.NoError(json.Unmarshal(wire, &wireFields), "decode wire view %s", wire)

			for _, name := range jsonFieldNames(tt.payload) {
				must.Contains(fullFields, name,
					"fixture leaves field %q unset, so the wire view is not checked for it", name)
			}

			isBody := map[string]bool{}
			for _, k := range tt.body {
				isBody[k] = true
				must.Contains(fullFields, k, "body field %q is not in the full envelope", k)
			}
			for k, v := range fullFields {
				got, ok := wireFields[k]
				if isBody[k] {
					must.False(ok, "body field %q reached the wire view", k)
					continue
				}
				must.True(ok, "field %q is missing from the wire view %s", k, wire)
				must.Equal(string(v), string(got), "field %q on the wire", k)
			}

			isDerived := map[string]bool{}
			for _, k := range tt.derived {
				isDerived[k] = true
			}
			for k := range wireFields {
				if isDerived[k] {
					continue
				}
				must.Contains(fullFields, k, "wire view has field %q the envelope lacks", k)
			}
		})
	}
}

// jsonFieldNames returns the JSON names of v's exported top-level fields.
func jsonFieldNames(v any) []string {
	rt := reflect.TypeOf(v)
	var names []string
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = f.Name
		}
		names = append(names, name)
	}
	return names
}
