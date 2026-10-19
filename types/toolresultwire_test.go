package types

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestToolResultWireEnvelope_LeanViewPerKind(t *testing.T) {
	cases := []struct {
		name       string
		kind       string
		structured string
		want       string
	}{
		{
			name:       "file excerpt drops lines",
			kind:       ToolResultKindFileExcerpt,
			structured: `{"path":"a.go","start_line":3,"end_line":4,"truncated":true,"lines":["x","y"]}`,
			want:       `{"path":"a.go","start_line":3,"end_line":4,"truncated":true}`,
		},
		{
			name:       "file excerpt past EOF keeps the flag",
			kind:       ToolResultKindFileExcerpt,
			structured: `{"path":"a.go","start_line":9,"end_line":2,"truncated":false,"past_eof":true,"lines":[]}`,
			want:       `{"path":"a.go","start_line":9,"end_line":2,"truncated":false,"past_eof":true}`,
		},
		{
			name:       "search result keeps truncation and columns",
			kind:       ToolResultKindSearchResult,
			structured: `{"matches":[{"path":"a.go","line":1,"column":5,"text":"foo"},{"path":"b.go","line":2,"column":1,"text":"bar"}],"truncated":true}`,
			want:       `{"truncated":true,"columns":[5,1]}`,
		},
		{
			name:       "search result without columns omits them",
			kind:       ToolResultKindSearchResult,
			structured: `{"matches":[{"path":"a.go","line":1,"text":"foo"}],"truncated":false}`,
			want:       `{"truncated":false}`,
		},
		{
			name:       "find result drops paths",
			kind:       ToolResultKindFindResult,
			structured: `{"paths":["a.go","b.go"],"truncated":true}`,
			want:       `{"truncated":true}`,
		},
		{
			name:       "command result drops stdout and stderr",
			kind:       ToolResultKindCommandResult,
			structured: `{"stdout":"out","stderr":"err","exit_code":2,"timed_out":false,"timeout_seconds":30,"capture_complete":true,"stdout_ref":"stirrup://command-output/r/stdout","stdout_raw_bytes":3}`,
			want:       `{"exit_code":2,"timed_out":false,"timeout_seconds":30,"capture_complete":true,"stdout_ref":"stirrup://command-output/r/stdout","stdout_raw_bytes":3}`,
		},
		{
			name:       "command output chunk drops content",
			kind:       ToolResultKindCommandOutputChunk,
			structured: `{"ref":"stirrup://command-output/r/stdout","stream":"stdout","offset":0,"end_offset":5,"eof":true,"content":"hello","raw_bytes":5,"raw_sha256":"a","scrubbed_bytes":5,"scrubbed_sha256":"b"}`,
			want:       `{"ref":"stirrup://command-output/r/stdout","stream":"stdout","offset":0,"end_offset":5,"eof":true,"raw_bytes":5,"raw_sha256":"a","scrubbed_bytes":5,"scrubbed_sha256":"b"}`,
		},
		{
			name:       "git status drops entries",
			kind:       ToolResultKindGitStatus,
			structured: `{"branch":"main","clean":false,"entries":[{"code":" M","staged":" ","unstaged":"M","path":"a.go"}],"truncated":false}`,
			want:       `{"branch":"main","clean":false,"truncated":false}`,
		},
		{
			name:       "git changed files drops files",
			kind:       ToolResultKindGitChangedFiles,
			structured: `{"staged":true,"files":[{"status":"M","path":"a.go"}],"truncated":false}`,
			want:       `{"staged":true,"truncated":false}`,
		},
		{
			name:       "git diff drops the diff",
			kind:       ToolResultKindGitDiff,
			structured: `{"diff":"--- a\n+++ b\n","staged":false,"path":"a.go","truncated":true}`,
			want:       `{"staged":false,"path":"a.go","truncated":true}`,
		},
		{
			name:       "git show drops the output",
			kind:       ToolResultKindGitShow,
			structured: `{"ref":"HEAD","output":"commit abc","truncated":false}`,
			want:       `{"ref":"HEAD","truncated":false}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			block := ContentBlock{Type: "tool_result", Content: "text rendering", Kind: tc.kind, Structured: json.RawMessage(tc.structured)}
			got := ToolResultWireEnvelope(block)
			require.Equal(t, tc.want, string(got))
			require.Equal(t, tc.structured, string(block.Structured), "block's structured payload was modified")
		})
	}
}

func TestToolResultWireEnvelope_PassThroughAndEmpty(t *testing.T) {
	cases := []struct {
		name       string
		kind       string
		content    string
		structured string
		want       string
	}{
		{name: "no envelope", kind: ToolResultKindCommandResult, content: "ok", structured: "", want: ""},
		{name: "envelope identical to text", kind: ToolResultKindCommandOutputChunk, content: `{"ref":"r","content":"x"}`, structured: `{"ref":"r","content":"x"}`, want: ""},
		{name: "unknown kind is unchanged", kind: "mcp_tool_result", content: "ok", structured: `{"rows":[1,2,3]}`, want: `{"rows":[1,2,3]}`},
		{name: "empty kind is unchanged", kind: "", content: "ok", structured: `{"a":1}`, want: `{"a":1}`},
		{name: "known kind with a non-object payload is unchanged", kind: ToolResultKindFileExcerpt, content: "ok", structured: `"redacted"`, want: `"redacted"`},
		{name: "known kind with a mistyped field is unchanged", kind: ToolResultKindFileExcerpt, content: "ok", structured: `{"path":7}`, want: `{"path":7}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var structured json.RawMessage
			if tc.structured != "" {
				structured = json.RawMessage(tc.structured)
			}
			got := ToolResultWireEnvelope(ContentBlock{Type: "tool_result", Content: tc.content, Kind: tc.kind, Structured: structured})
			require.Equal(t, tc.want, string(got))
		})
	}
}
