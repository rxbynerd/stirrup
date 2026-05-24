// Package builtins provides the built-in tools available to the coding agent.
package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
)

// readFileSchema is the JSON Schema for the read_file tool input. start_line
// and limit are optional; when both are omitted the tool returns the entire
// file. start_line is 1-indexed to match the line numbers in the returned
// output. limit caps the number of lines returned and is bounded so a model
// cannot accidentally page in a huge file.
var readFileSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"path": {
			"type": "string",
			"description": "Path to the file to read, relative to the workspace root."
		},
		"start_line": {
			"type": "integer",
			"minimum": 1,
			"description": "Optional 1-indexed line to start reading from. Defaults to 1."
		},
		"limit": {
			"type": "integer",
			"minimum": 1,
			"maximum": 5000,
			"description": "Optional maximum number of lines to return. Defaults to 2000."
		}
	},
	"required": ["path"],
	"additionalProperties": false
}`)

// writeFileSchema is the JSON Schema for the write_file tool input.
var writeFileSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"path": {
			"type": "string",
			"description": "Path to the file to write, relative to the workspace root. Parent directories are created automatically."
		},
		"content": {
			"type": "string",
			"description": "The content to write to the file."
		}
	},
	"required": ["path", "content"],
	"additionalProperties": false
}`)

// listDirectorySchema is the JSON Schema for the list_directory tool input.
// recursive defaults to false (current behaviour). max_depth and max_entries
// bound the output so a model cannot accidentally enumerate a huge tree.
var listDirectorySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"path": {
			"type": "string",
			"description": "Path to the directory to list, relative to the workspace root."
		},
		"recursive": {
			"type": "boolean",
			"description": "Optional. If true, recurse into subdirectories up to max_depth. Defaults to false."
		},
		"max_depth": {
			"type": "integer",
			"minimum": 0,
			"maximum": 10,
			"description": "Optional. Maximum recursion depth when recursive is true. 0 means only the named directory; defaults to 3."
		},
		"max_entries": {
			"type": "integer",
			"minimum": 1,
			"maximum": 10000,
			"description": "Optional. Maximum number of entries to return. Defaults to 1000."
		}
	},
	"required": ["path"],
	"additionalProperties": false
}`)

// Defaults and bounds for read_file and list_directory. Kept as package-level
// constants so tests can pin the values without re-parsing the schemas.
const (
	readFileDefaultLimit      = 2000
	readFileMaxLimit          = 5000
	listDirDefaultMaxDepth    = 3
	listDirAbsoluteMaxDepth   = 10
	listDirDefaultMaxEntries  = 1000
	listDirAbsoluteMaxEntries = 10000
	listDirTruncationSentinel = "[truncated: max_entries reached]"
	readFilePastEOFNoticeFmt  = "[empty: start_line %d is past end of file (file has %d lines)]"
)

// ReadFileTool returns a tool that reads a file from the workspace. The
// returned content is line-numbered ("123\tcontent") so the model can refer to
// specific lines on subsequent edit calls. start_line past EOF returns a
// non-error notice rather than failing — models often guess line numbers and a
// hard error here trains them to over-read whole files instead of probing.
func ReadFileTool(exec executor.Executor) *tool.Tool {
	return &tool.Tool{
		Name: "read_file",
		Description: "Read the contents of a file from the workspace. Returns line-numbered output (\"123\\tcontent\") so subsequent edits can refer to specific lines. " +
			"Use this when the exact content of a known file is needed; prefer grep_files when searching for a string across many files. " +
			"start_line is 1-indexed and limit caps the lines returned (default 2000, max 5000). " +
			"When start_line is past end-of-file the tool returns a notice rather than an error, so probing with a guessed start_line is safe. " +
			"Example: {\"path\": \"path/to/file.go\", \"start_line\": 100, \"limit\": 50}",
		InputSchema:       readFileSchema,
		WorkspaceMutating: false,
		RequiresApproval:  false,
		StructuredHandler: func(ctx context.Context, input json.RawMessage) (string, json.RawMessage, error) {
			var params struct {
				Path      string `json:"path"`
				StartLine *int   `json:"start_line,omitempty"`
				Limit     *int   `json:"limit,omitempty"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", nil, fmt.Errorf("parse input: %w", err)
			}
			if params.Path == "" {
				return "", nil, fmt.Errorf("path is required")
			}
			startLine := 1
			if params.StartLine != nil {
				if *params.StartLine < 1 {
					return "", nil, fmt.Errorf("start_line must be >= 1, got %d", *params.StartLine)
				}
				startLine = *params.StartLine
			}
			limit := readFileDefaultLimit
			if params.Limit != nil {
				if *params.Limit < 1 {
					return "", nil, fmt.Errorf("limit must be >= 1, got %d", *params.Limit)
				}
				if *params.Limit > readFileMaxLimit {
					return "", nil, fmt.Errorf("limit must be <= %d, got %d", readFileMaxLimit, *params.Limit)
				}
				limit = *params.Limit
			}

			content, err := exec.ReadFile(ctx, params.Path)
			if err != nil {
				return "", nil, err
			}
			text := formatReadFile(content, startLine, limit)
			excerpt := readFileExcerpt(params.Path, content, startLine, limit)
			structured, marshalErr := json.Marshal(excerpt)
			if marshalErr != nil {
				return "", nil, fmt.Errorf("marshal structured result: %w", marshalErr)
			}
			return text, structured, nil
		},
	}
}

// readFileExcerpt computes the typed structured payload for read_file from the
// same inputs formatReadFile uses, mirroring its line-window arithmetic so the
// structured fields agree with the text rendering exactly (issue #231). Lines
// holds the excerpt content without the line-number prefixes the text adds.
// PastEOF is set when start_line is beyond the file, matching the past-EOF
// notice the text returns; Lines is empty in that case. Truncated reports that
// the window stopped short of the end of the file.
func readFileExcerpt(path, content string, startLine, limit int) fileExcerpt {
	// Mirror formatReadFile's trailing-newline handling so the line count and
	// the EOF boundary match the text rendering byte-for-byte.
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	totalLines := len(lines)
	if startLine > totalLines {
		return fileExcerpt{
			Path:      path,
			StartLine: startLine,
			EndLine:   totalLines,
			PastEOF:   true,
			Lines:     []string{},
		}
	}
	endLine := startLine + limit - 1
	if endLine > totalLines {
		endLine = totalLines
	}
	excerptLines := make([]string, 0, endLine-startLine+1)
	for i := startLine; i <= endLine; i++ {
		excerptLines = append(excerptLines, lines[i-1])
	}
	return fileExcerpt{
		Path:      path,
		StartLine: startLine,
		EndLine:   endLine,
		Truncated: endLine < totalLines,
		Lines:     excerptLines,
	}
}

// formatReadFile slices the file content to the requested 1-indexed line
// window and prefixes each line with its line number followed by a tab.
// start_line past EOF returns the past-EOF notice instead of an empty string
// so the model can distinguish "empty file" from "start_line out of range".
func formatReadFile(content string, startLine, limit int) string {
	// Splitting on "\n" turns a trailing newline into a final empty string.
	// We drop that synthetic empty line so the line count matches what an
	// editor would report — otherwise a 3-line file ending in "\n" would
	// report 4 lines.
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	totalLines := len(lines)
	if startLine > totalLines {
		return fmt.Sprintf(readFilePastEOFNoticeFmt, startLine, totalLines)
	}
	endLine := startLine + limit - 1
	if endLine > totalLines {
		endLine = totalLines
	}
	// Pre-compute a width for line-number padding so the columns line up
	// when the slice spans more than one digit-width.
	widthMax := endLine
	width := len(strconv.Itoa(widthMax))
	var b strings.Builder
	for i := startLine; i <= endLine; i++ {
		num := strconv.Itoa(i)
		// Right-align line numbers within `width` for stable column alignment.
		for j := len(num); j < width; j++ {
			b.WriteByte(' ')
		}
		b.WriteString(num)
		b.WriteByte('\t')
		b.WriteString(lines[i-1])
		if i < endLine {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// WriteFileTool returns a tool that writes content to a file in the workspace.
func WriteFileTool(exec executor.Executor) *tool.Tool {
	return &tool.Tool{
		Name: "write_file",
		Description: "Create or overwrite a file in the workspace, writing content verbatim. Parent directories are created automatically. " +
			"Use this when authoring a new file or when a wholesale rewrite is simpler than a targeted change. " +
			"Do not use for small edits to existing files — prefer edit_file with operation 'replace' or 'patch' so unrelated lines stay untouched. " +
			"Example: {\"path\": \"cmd/cli/main.go\", \"content\": \"package main\\n\\nfunc main() {}\\n\"}",
		InputSchema:       writeFileSchema,
		WorkspaceMutating: true,
		RequiresApproval:  true,
		Handler: func(ctx context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("parse input: %w", err)
			}
			if params.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			if err := exec.WriteFile(ctx, params.Path, params.Content); err != nil {
				return "", err
			}
			return fmt.Sprintf("Successfully wrote %d bytes to %s", len(params.Content), params.Path), nil
		},
	}
}

// ListDirectoryTool returns a tool that lists the contents of a directory.
// When recursive=true the tool walks subdirectories up to max_depth and
// returns paths relative to the listed directory; directories continue to
// carry a trailing slash so models can spot them at a glance.
func ListDirectoryTool(exec executor.Executor) *tool.Tool {
	return &tool.Tool{
		Name: "list_directory",
		Description: "List the contents of a directory in the workspace. Directory entries carry a trailing slash so they are visually distinguishable from files. " +
			"Use this to discover the shape of an unfamiliar tree; prefer find_files when the goal is to locate files by name across the workspace. " +
			"Set recursive=true to walk subdirectories up to max_depth (default 3, max 10). Results are capped at max_entries (default 1000, max 10000) and a truncation sentinel is appended when the cap is hit. " +
			"Example: {\"path\": \"harness/internal\", \"recursive\": true, \"max_depth\": 2}",
		InputSchema:       listDirectorySchema,
		WorkspaceMutating: false,
		RequiresApproval:  false,
		Handler: func(ctx context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Path       string `json:"path"`
				Recursive  bool   `json:"recursive,omitempty"`
				MaxDepth   *int   `json:"max_depth,omitempty"`
				MaxEntries *int   `json:"max_entries,omitempty"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("parse input: %w", err)
			}
			if params.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			maxDepth := listDirDefaultMaxDepth
			if params.MaxDepth != nil {
				if *params.MaxDepth < 0 {
					return "", fmt.Errorf("max_depth must be >= 0, got %d", *params.MaxDepth)
				}
				if *params.MaxDepth > listDirAbsoluteMaxDepth {
					return "", fmt.Errorf("max_depth must be <= %d, got %d", listDirAbsoluteMaxDepth, *params.MaxDepth)
				}
				maxDepth = *params.MaxDepth
			}
			maxEntries := listDirDefaultMaxEntries
			if params.MaxEntries != nil {
				if *params.MaxEntries < 1 {
					return "", fmt.Errorf("max_entries must be >= 1, got %d", *params.MaxEntries)
				}
				if *params.MaxEntries > listDirAbsoluteMaxEntries {
					return "", fmt.Errorf("max_entries must be <= %d, got %d", listDirAbsoluteMaxEntries, *params.MaxEntries)
				}
				maxEntries = *params.MaxEntries
			}
			if !params.Recursive {
				// Backwards-compatible non-recursive path: just return the
				// directory's direct entries (still bounded by maxEntries).
				entries, err := exec.ListDirectory(ctx, params.Path)
				if err != nil {
					return "", err
				}
				if len(entries) > maxEntries {
					entries = append(entries[:maxEntries:maxEntries], listDirTruncationSentinel)
				}
				return strings.Join(entries, "\n"), nil
			}
			entries, truncated, err := listDirectoryRecursive(ctx, exec, params.Path, maxDepth, maxEntries)
			if err != nil {
				return "", err
			}
			if truncated {
				entries = append(entries, listDirTruncationSentinel)
			}
			return strings.Join(entries, "\n"), nil
		},
	}
}

// listDirectoryRecursive performs a breadth-first walk of `root` to `maxDepth`,
// collecting at most `maxEntries` entries. Paths are returned relative to
// `root` with directories suffixed by "/". The walk uses the executor's
// ListDirectory rather than the OS filesystem so containerised executors
// continue to work — this is slower than a single os.WalkDir but stays inside
// the sandbox boundary established by the executor.
//
// Returned entries are in BFS order (siblings before descendants), which gives
// models a stable, predictable view of the tree.
func listDirectoryRecursive(ctx context.Context, exec executor.Executor, root string, maxDepth, maxEntries int) ([]string, bool, error) {
	type frame struct {
		rel   string
		depth int
	}
	queue := []frame{{rel: "", depth: 0}}
	var out []string
	for len(queue) > 0 {
		head := queue[0]
		queue = queue[1:]

		dirPath := root
		if head.rel != "" {
			dirPath = root + "/" + head.rel
		}
		entries, err := exec.ListDirectory(ctx, dirPath)
		if err != nil {
			// If a subdirectory becomes inaccessible mid-walk (permissions,
			// race with deletion) we surface the error for the root call but
			// continue past it for descendants so a single bad child does
			// not abort the whole listing.
			if head.depth == 0 {
				return nil, false, err
			}
			continue
		}
		for _, entry := range entries {
			rel := entry
			if head.rel != "" {
				rel = head.rel + "/" + entry
			}
			out = append(out, rel)
			if len(out) >= maxEntries {
				return out, true, nil
			}
			if strings.HasSuffix(entry, "/") && head.depth < maxDepth {
				// Strip the trailing slash before recursing into the child
				// path; the slash is purely a presentation marker.
				childRel := strings.TrimSuffix(rel, "/")
				queue = append(queue, frame{rel: childRel, depth: head.depth + 1})
			}
		}
	}
	return out, false, nil
}
