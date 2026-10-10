package types

import (
	"bytes"
	"encoding/json"
)

// Kind discriminators for the structured envelopes the built-in tools put in
// ToolResult.Structured. ToolResultWireEnvelope routes on these values.
const (
	ToolResultKindCommandResult      = "command_result"
	ToolResultKindSearchResult       = "search_result"
	ToolResultKindFindResult         = "find_result"
	ToolResultKindFileExcerpt        = "file_excerpt"
	ToolResultKindGitStatus          = "git_status"
	ToolResultKindGitChangedFiles    = "git_changed_files"
	ToolResultKindGitDiff            = "git_diff"
	ToolResultKindGitShow            = "git_show"
	ToolResultKindCommandOutputChunk = "command_output_chunk"
)

// ToolResultWireEnvelope returns the structured payload a provider sends
// next to a tool_result block's text Content. History and trace keep the
// full envelope; this is the view the model receives.
//
// For a built-in kind it is a lean view that drops the body fields the text
// already renders (excerpt lines, matched text, paths, command output, diff
// and show output, status entries) and keeps the scalar metadata, plus the
// per-match columns of a search result, which the text does not show. A
// payload byte-identical to Content yields nil, as does an empty payload.
// An unknown kind, or a payload that does not decode as its kind's object,
// is returned unchanged.
func ToolResultWireEnvelope(b ContentBlock) json.RawMessage {
	if len(b.Structured) == 0 || string(b.Structured) == b.Content {
		return nil
	}
	if trimmed := bytes.TrimSpace(b.Structured); len(trimmed) == 0 || trimmed[0] != '{' {
		return b.Structured
	}
	var (
		lean json.RawMessage
		ok   bool
	)
	switch b.Kind {
	case ToolResultKindFileExcerpt:
		lean, ok = reencode[fileExcerptWire](b.Structured)
	case ToolResultKindSearchResult:
		lean, ok = searchResultWireView(b.Structured)
	case ToolResultKindFindResult:
		lean, ok = reencode[findResultWire](b.Structured)
	case ToolResultKindCommandResult:
		lean, ok = reencode[commandResultWire](b.Structured)
	case ToolResultKindCommandOutputChunk:
		lean, ok = reencode[commandOutputChunkWire](b.Structured)
	case ToolResultKindGitStatus:
		lean, ok = reencode[gitStatusWire](b.Structured)
	case ToolResultKindGitChangedFiles:
		lean, ok = reencode[gitChangedFilesWire](b.Structured)
	case ToolResultKindGitDiff:
		lean, ok = reencode[gitDiffWire](b.Structured)
	case ToolResultKindGitShow:
		lean, ok = reencode[gitShowWire](b.Structured)
	default:
		return b.Structured
	}
	if !ok {
		return b.Structured
	}
	return lean
}

// reencode decodes raw into T, which declares only the fields kept on the
// wire, and marshals it back, so every field T omits is dropped.
func reencode[T any](raw json.RawMessage) (json.RawMessage, bool) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	return out, true
}

func searchResultWireView(raw json.RawMessage) (json.RawMessage, bool) {
	var src struct {
		Matches []struct {
			Column int `json:"column"`
		} `json:"matches"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(raw, &src); err != nil {
		return nil, false
	}
	wire := searchResultWire{Truncated: src.Truncated}
	columns := make([]int, len(src.Matches))
	hasColumn := false
	for i, m := range src.Matches {
		columns[i] = m.Column
		hasColumn = hasColumn || m.Column != 0
	}
	if hasColumn {
		wire.Columns = columns
	}
	out, err := json.Marshal(wire)
	if err != nil {
		return nil, false
	}
	return out, true
}

// The *Wire types mirror the JSON tags of the envelopes in
// harness/internal/tool/builtins/structured.go, minus the body fields.

type fileExcerptWire struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Truncated bool   `json:"truncated"`
	PastEOF   bool   `json:"past_eof,omitempty"`
}

// searchResultWire keeps the truncation flag and, when the search reported
// them, the match columns in the same order as the text's match lines.
type searchResultWire struct {
	Truncated bool  `json:"truncated"`
	Columns   []int `json:"columns,omitempty"`
}

type findResultWire struct {
	Truncated bool `json:"truncated"`
}

type commandResultWire struct {
	ExitCode            int    `json:"exit_code"`
	TimedOut            bool   `json:"timed_out"`
	TimeoutSeconds      int    `json:"timeout_seconds"`
	Spilled             bool   `json:"spilled,omitempty"`
	CaptureComplete     bool   `json:"capture_complete"`
	ArchiveID           string `json:"archive_id,omitempty"`
	StdoutRef           string `json:"stdout_ref,omitempty"`
	StderrRef           string `json:"stderr_ref,omitempty"`
	StdoutRawBytes      int64  `json:"stdout_raw_bytes,omitempty"`
	StderrRawBytes      int64  `json:"stderr_raw_bytes,omitempty"`
	StdoutScrubbedBytes int64  `json:"stdout_scrubbed_bytes,omitempty"`
	StderrScrubbedBytes int64  `json:"stderr_scrubbed_bytes,omitempty"`
	StdoutSHA256        string `json:"stdout_sha256,omitempty"`
	StderrSHA256        string `json:"stderr_sha256,omitempty"`
}

type commandOutputChunkWire struct {
	Reference      string `json:"ref"`
	Stream         string `json:"stream"`
	Offset         int64  `json:"offset"`
	EndOffset      int64  `json:"end_offset"`
	EOF            bool   `json:"eof"`
	RawBytes       int64  `json:"raw_bytes"`
	RawSHA256      string `json:"raw_sha256"`
	ScrubbedBytes  int64  `json:"scrubbed_bytes"`
	ScrubbedSHA256 string `json:"scrubbed_sha256"`
	RedactionCount int    `json:"redaction_count,omitempty"`
}

type gitStatusWire struct {
	Branch    string `json:"branch"`
	Clean     bool   `json:"clean"`
	Truncated bool   `json:"truncated"`
}

type gitChangedFilesWire struct {
	Staged    bool `json:"staged"`
	Truncated bool `json:"truncated"`
}

type gitDiffWire struct {
	Staged    bool   `json:"staged"`
	Path      string `json:"path,omitempty"`
	Truncated bool   `json:"truncated"`
}

type gitShowWire struct {
	Ref       string `json:"ref"`
	Path      string `json:"path,omitempty"`
	Truncated bool   `json:"truncated"`
}
