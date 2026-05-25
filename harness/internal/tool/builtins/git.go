package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
)

// Bounds for the read-only VCS tools. Diff and show output is the largest risk
// to model context, so it is capped on both bytes and lines; the status/changed
// lists are capped on entry count. Mirrors list_directory's truncation pattern:
// the slice is cut at the cap and a sentinel makes the truncation visible to
// the model rather than silently dropping data.
const (
	gitDiffMaxBytes        = 256 * 1024 // 256 KB
	gitDiffMaxLines        = 4000
	gitStatusMaxEntries    = 2000
	gitChangedFilesMaxFile = 2000
	gitDiffTruncSentinel   = "\n[diff truncated]"
	gitTimeout             = 30 * time.Second
)

// gitMetacharacters are shell-significant runes rejected outright in ref
// arguments. shellQuote already neutralises them (see runGit), but a ref
// legitimately never contains these, so rejecting up front yields a clear
// error and a smaller trusted surface. `:` is deliberately absent: it is valid
// treeish syntax (`HEAD:path/to/file`), and the path portion after the first
// `:` is validated separately (see git_show) so it cannot smuggle traversal.
const gitMetacharacters = "\x00\n\r;&|`$()<>!*?[]{}\"'\\ \t"

// validateGitRef rejects refs that could inject git options. A leading `-`
// would be read as a flag (e.g. `--upload-pack=...`); shellQuote already stops
// metacharacters from reaching sh as syntax, but a clean ref never carries
// them, so rejecting early gives a deterministic, legible error. An empty ref
// is rejected by the caller before this is reached.
func validateGitRef(ref string) error {
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("ref %q must not begin with '-' (option injection)", ref)
	}
	if strings.ContainsAny(ref, gitMetacharacters) {
		return fmt.Errorf("ref %q contains an unsupported character", ref)
	}
	return nil
}

// validateColonRefPath enforces workspace containment on the path portion of a
// treeish ref of the form `<rev>:<path>` (e.g. `HEAD:.env`). git itself blocks
// OS-level traversal, but a colon-ref with no separate `path` field would
// otherwise skip the validateGitPath layer entirely — so containment must not
// depend on which API field the path arrives in. Refs without a `:` are a
// no-op here. The leading `:./` and `:N:` (stage) prefixes git accepts are not
// supported; such a path simply fails validateGitPath, which is acceptable for
// a read-only inspection tool.
func validateColonRefPath(exec executor.Executor, ref string) error {
	colon := strings.IndexByte(ref, ':')
	if colon < 0 {
		return nil
	}
	pathPart := ref[colon+1:]
	if pathPart == "" {
		return nil
	}
	if _, err := validateGitPath(exec, pathPart); err != nil {
		return fmt.Errorf("ref path component: %w", err)
	}
	return nil
}

// validateGitPath resolves a workspace-relative path against the workspace
// root, rejecting traversal (`..`) and absolute paths that escape it, so a
// path argument cannot reach files outside the workspace. A leading `-` is
// rejected to prevent option injection even though `--` also separates options
// at the call site.
func validateGitPath(exec executor.Executor, path string) (string, error) {
	if strings.HasPrefix(path, "-") {
		return "", fmt.Errorf("path %q must not begin with '-' (option injection)", path)
	}
	if _, err := exec.ResolvePath(path); err != nil {
		return "", fmt.Errorf("invalid path: %w", err)
	}
	// Hand git the original relative form, not the resolved absolute path:
	// git interprets a pathspec relative to the working directory (the
	// workspace root) and re-enforces containment itself, so re-resolving here
	// would only risk a TOCTOU mismatch between the checked and used value for
	// no gain. ResolvePath above is used purely as the containment gate.
	return path, nil
}

// runGit builds a `sh -c`-safe command from a fixed git verb plus already-
// validated arguments and executes it. The injected executor runs every
// command via `sh -c <string>` (executor/local.go:Exec, container.go:Exec), so
// an unquoted `"git " + path` would be a shell-injection vector. Every element
// is single-quoted with shellQuote so a value such as `foo; rm -rf /` is
// treated as literal text by sh, never as syntax. ensureGitRepo must be called
// before any data-returning git invocation so a non-git workspace yields a
// clear error rather than a raw git failure.
func runGit(ctx context.Context, exec executor.Executor, args ...string) (*executor.ExecResult, error) {
	quoted := make([]string, 0, len(args)+1)
	quoted = append(quoted, "git")
	for _, a := range args {
		quoted = append(quoted, shellQuote(a))
	}
	return exec.Exec(ctx, strings.Join(quoted, " "), gitTimeout)
}

// ensureGitRepo returns a clear, deterministic error when the workspace is not
// inside a git work tree, so callers surface that instead of git's own message
// (which differs across versions). It also catches the case where git is not
// installed.
func ensureGitRepo(ctx context.Context, exec executor.Executor) error {
	res, err := runGit(ctx, exec, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return fmt.Errorf("git is not available: %w", err)
	}
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "true" {
		return fmt.Errorf("workspace is not a git repository")
	}
	return nil
}

// currentBranch reports the branch for git_status. A non-zero exit (no commit
// yet) is reported as "(unborn)"; an executor error (e.g. context cancel) is
// distinct and reported as "(unknown)" so the two are not conflated.
func currentBranch(ctx context.Context, exec executor.Executor) string {
	res, err := runGit(ctx, exec, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "(unknown)"
	}
	if res.ExitCode != 0 {
		return "(unborn)"
	}
	name := strings.TrimSpace(res.Stdout)
	if name == "HEAD" {
		return "HEAD (detached)"
	}
	if name == "" {
		return "(unborn)"
	}
	return name
}

var gitStatusSchema = json.RawMessage(`{
	"type": "object",
	"properties": {},
	"additionalProperties": false
}`)

// GitStatusTool returns a read-only tool reporting the working-tree state.
func GitStatusTool(exec executor.Executor) *tool.Tool {
	return &tool.Tool{
		Name: "git_status",
		Description: "Report the git working-tree state (porcelain status) as structured entries with per-path staged/unstaged status letters and the current branch. " +
			"Use this in read-only review or monitoring runs to see what changed without enabling run_command. " +
			"Output is bounded; a large change set is capped at an entry limit and flagged as truncated. " +
			"Example: {}",
		InputExamples:     []json.RawMessage{json.RawMessage(`{}`)},
		InputSchema:       gitStatusSchema,
		WorkspaceMutating: false,
		RequiresApproval:  false,
		StructuredHandler: func(ctx context.Context, input json.RawMessage) (tool.StructuredResult, error) {
			if err := ensureGitRepo(ctx, exec); err != nil {
				return tool.StructuredResult{}, err
			}
			// -z gives NUL-terminated records with verbatim (unquoted) paths,
			// so paths containing spaces or unusual bytes parse unambiguously.
			res, err := runGit(ctx, exec, "status", "--porcelain=v1", "-z")
			if err != nil {
				return tool.StructuredResult{}, err
			}
			if res.ExitCode != 0 {
				return tool.StructuredResult{}, fmt.Errorf("git status failed: %s", gitErrText(res))
			}

			entries, truncated := parsePorcelainZ(res.Stdout, gitStatusMaxEntries)
			result := gitStatusResult{
				Branch:    currentBranch(ctx, exec),
				Clean:     len(entries) == 0,
				Entries:   entries,
				Truncated: truncated,
			}
			return marshalGit(formatGitStatus(result), result, kindGitStatus)
		},
	}
}

var gitChangedFilesSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"staged": {
			"type": "boolean",
			"description": "When true, list staged changes (index vs HEAD). When false or omitted, list unstaged changes (working tree vs index)."
		}
	},
	"additionalProperties": false
}`)

// GitChangedFilesTool returns a read-only tool listing changed paths with their
// name-status letters.
func GitChangedFilesTool(exec executor.Executor) *tool.Tool {
	return &tool.Tool{
		Name: "git_changed_files",
		Description: "List changed file paths with their git name-status letter (A/M/D/R/C/T). " +
			"Use this to enumerate which files differ before reading specific diffs; set staged to inspect the index instead of the working tree. " +
			"Output is bounded; a large change set is capped at an entry limit and flagged as truncated. " +
			"Example: {\"staged\": false}",
		InputExamples:     []json.RawMessage{json.RawMessage(`{"staged": false}`)},
		InputSchema:       gitChangedFilesSchema,
		WorkspaceMutating: false,
		RequiresApproval:  false,
		StructuredHandler: func(ctx context.Context, input json.RawMessage) (tool.StructuredResult, error) {
			var params struct {
				Staged bool `json:"staged"`
			}
			if len(input) > 0 {
				if err := json.Unmarshal(input, &params); err != nil {
					return tool.StructuredResult{}, fmt.Errorf("parse input: %w", err)
				}
			}
			if err := ensureGitRepo(ctx, exec); err != nil {
				return tool.StructuredResult{}, err
			}

			args := []string{"diff", "--name-status", "-z"}
			if params.Staged {
				args = append(args, "--cached")
			}
			res, err := runGit(ctx, exec, args...)
			if err != nil {
				return tool.StructuredResult{}, err
			}
			if res.ExitCode != 0 {
				return tool.StructuredResult{}, fmt.Errorf("git diff failed: %s", gitErrText(res))
			}

			files, truncated := parseNameStatusZ(res.Stdout, gitChangedFilesMaxFile)
			result := gitChangedFilesResult{
				Staged:    params.Staged,
				Files:     files,
				Truncated: truncated,
			}
			return marshalGit(formatGitChangedFiles(result), result, kindGitChangedFiles)
		},
	}
}

var gitDiffSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"path": {
			"type": "string",
			"description": "Optional workspace-relative path to restrict the diff to a single file or directory."
		},
		"staged": {
			"type": "boolean",
			"description": "When true, diff the staged changes (index vs HEAD). When false or omitted, diff the working tree against the index."
		}
	},
	"additionalProperties": false
}`)

// GitDiffTool returns a read-only tool producing a bounded unified diff.
func GitDiffTool(exec executor.Executor) *tool.Tool {
	return &tool.Tool{
		Name: "git_diff",
		Description: "Produce a unified diff of the working tree (or the staged changes when staged is true), optionally restricted to one path. " +
			"Use this to review the exact line-level changes in a read-only run without enabling run_command. " +
			"Output is bounded by byte and line caps; a large diff is truncated and flagged as truncated. " +
			"Example: {\"path\": \"harness/internal/tool/builtins/git.go\", \"staged\": false}",
		InputExamples:     []json.RawMessage{json.RawMessage(`{"path": "harness/internal/tool/builtins/git.go", "staged": false}`)},
		InputSchema:       gitDiffSchema,
		WorkspaceMutating: false,
		RequiresApproval:  false,
		StructuredHandler: func(ctx context.Context, input json.RawMessage) (tool.StructuredResult, error) {
			var params struct {
				Path   string `json:"path"`
				Staged bool   `json:"staged"`
			}
			if len(input) > 0 {
				if err := json.Unmarshal(input, &params); err != nil {
					return tool.StructuredResult{}, fmt.Errorf("parse input: %w", err)
				}
			}
			if err := ensureGitRepo(ctx, exec); err != nil {
				return tool.StructuredResult{}, err
			}

			args := []string{"diff"}
			if params.Staged {
				args = append(args, "--cached")
			}
			if params.Path != "" {
				p, err := validateGitPath(exec, params.Path)
				if err != nil {
					return tool.StructuredResult{}, err
				}
				args = append(args, "--", p)
			}
			res, err := runGit(ctx, exec, args...)
			if err != nil {
				return tool.StructuredResult{}, err
			}
			if res.ExitCode != 0 {
				return tool.StructuredResult{}, fmt.Errorf("git diff failed: %s", gitErrText(res))
			}

			diff, truncated := boundDiff(res.Stdout)
			result := gitDiffResult{
				Diff:      diff,
				Staged:    params.Staged,
				Path:      params.Path,
				Truncated: truncated,
			}
			return marshalGit(diff, result, kindGitDiff)
		},
	}
}

var gitShowSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"ref": {
			"type": "string",
			"description": "The revision to show (commit SHA, tag, branch, or HEAD~N). Required."
		},
		"path": {
			"type": "string",
			"description": "Optional workspace-relative path to restrict the output to a single file."
		}
	},
	"required": ["ref"],
	"additionalProperties": false
}`)

// GitShowTool returns a read-only tool inspecting a specific revision.
func GitShowTool(exec executor.Executor) *tool.Tool {
	return &tool.Tool{
		Name: "git_show",
		Description: "Show a specific git revision (commit metadata and diff, or a file's content at that revision when path is given). " +
			"Use this to inspect a commit or a historical file version in a read-only run; ref must name a revision and must not begin with '-'. " +
			"Output is bounded by byte and line caps; large output is truncated and flagged as truncated. " +
			"Example: {\"ref\": \"HEAD\", \"path\": \"README.md\"}",
		InputExamples:     []json.RawMessage{json.RawMessage(`{"ref": "HEAD", "path": "README.md"}`)},
		InputSchema:       gitShowSchema,
		WorkspaceMutating: false,
		RequiresApproval:  false,
		StructuredHandler: func(ctx context.Context, input json.RawMessage) (tool.StructuredResult, error) {
			var params struct {
				Ref  string `json:"ref"`
				Path string `json:"path"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return tool.StructuredResult{}, fmt.Errorf("parse input: %w", err)
			}
			if params.Ref == "" {
				return tool.StructuredResult{}, fmt.Errorf("ref is required")
			}
			if err := validateGitRef(params.Ref); err != nil {
				return tool.StructuredResult{}, err
			}
			if err := validateColonRefPath(exec, params.Ref); err != nil {
				return tool.StructuredResult{}, err
			}
			if err := ensureGitRepo(ctx, exec); err != nil {
				return tool.StructuredResult{}, err
			}

			args := []string{"show", params.Ref}
			if params.Path != "" {
				p, err := validateGitPath(exec, params.Path)
				if err != nil {
					return tool.StructuredResult{}, err
				}
				args = append(args, "--", p)
			}
			res, err := runGit(ctx, exec, args...)
			if err != nil {
				return tool.StructuredResult{}, err
			}
			if res.ExitCode != 0 {
				return tool.StructuredResult{}, fmt.Errorf("git show failed: %s", gitErrText(res))
			}

			out, truncated := boundDiff(res.Stdout)
			result := gitShowResult{
				Ref:       params.Ref,
				Path:      params.Path,
				Output:    out,
				Truncated: truncated,
			}
			return marshalGit(out, result, kindGitShow)
		},
	}
}

// gitErrText returns the most useful error text from a non-zero git result,
// preferring stderr.
func gitErrText(res *executor.ExecResult) string {
	if s := strings.TrimSpace(res.Stderr); s != "" {
		return s
	}
	if s := strings.TrimSpace(res.Stdout); s != "" {
		return s
	}
	return fmt.Sprintf("exit code %d", res.ExitCode)
}

// marshalGit packages the text fallback and a typed structured payload into a
// StructuredResult, mirroring the run_command/read_file pattern.
func marshalGit(text string, structured any, kind string) (tool.StructuredResult, error) {
	payload, err := json.Marshal(structured)
	if err != nil {
		return tool.StructuredResult{}, fmt.Errorf("marshal structured result: %w", err)
	}
	return tool.StructuredResult{Text: text, Structured: payload, Kind: kind}, nil
}

// parsePorcelainZ parses `git status --porcelain=v1 -z` output into entries,
// capped at max. The porcelain -z format is: two status chars, a space, the
// path, then NUL. For renames/copies the record is `XY <new>\0<orig>\0`, so a
// rename consumes two NUL-terminated fields. The bool reports truncation.
func parsePorcelainZ(out string, max int) ([]gitStatusEntry, bool) {
	entries := make([]gitStatusEntry, 0)
	fields := splitNUL(out)
	for i := 0; i < len(fields); i++ {
		rec := fields[i]
		if len(rec) < 4 {
			// Minimum "XY P" is 4 bytes; skip anything shorter (trailing empty).
			continue
		}
		code := rec[:2]
		path := rec[3:]
		entry := gitStatusEntry{
			Code:     code,
			Staged:   string(code[0]),
			Unstaged: string(code[1]),
			Path:     path,
		}
		// A rename or copy in either column carries the original path in the
		// following NUL-terminated field.
		if code[0] == 'R' || code[0] == 'C' || code[1] == 'R' || code[1] == 'C' {
			if i+1 < len(fields) {
				entry.OrigPath = fields[i+1]
				i++
			}
		}
		if len(entries) >= max {
			return entries, true
		}
		entries = append(entries, entry)
	}
	return entries, false
}

// parseNameStatusZ parses `git diff --name-status -z` output into changed
// files, capped at max. Each record is a status letter (possibly with a rename
// similarity score, e.g. R100) terminated by NUL, followed by the path in the
// next NUL field; renames/copies carry a second path field. The bool reports
// truncation.
func parseNameStatusZ(out string, max int) ([]gitChangedFile, bool) {
	files := make([]gitChangedFile, 0)
	fields := splitNUL(out)
	for i := 0; i < len(fields); i++ {
		status := fields[i]
		if status == "" {
			continue
		}
		letter := status[:1]
		isRenameOrCopy := letter == "R" || letter == "C"
		if i+1 >= len(fields) {
			break
		}
		i++
		path := fields[i]
		file := gitChangedFile{Status: letter, Path: path}
		if isRenameOrCopy {
			// name-status -z for R/C is: status\0 <orig>\0 <new>\0. The path
			// read above is the original; the new path follows.
			file.OrigPath = path
			if i+1 < len(fields) {
				i++
				file.Path = fields[i]
			}
		}
		if len(files) >= max {
			return files, true
		}
		files = append(files, file)
	}
	return files, false
}

// splitNUL splits a NUL-terminated/-separated stream into fields, dropping a
// trailing empty field produced by the terminating NUL.
func splitNUL(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "\x00")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// boundDiff caps diff/show output on both byte and line counts and appends a
// sentinel when either bound trims the content, so a huge worktree cannot blow
// up model context. Mirrors list_directory's visible-sentinel truncation.
//
// The executor already buffers the full command output before returning (the
// local executor caps it post-hoc at 1 MB; see executor/local.go:Exec), so
// boundDiff is the model-context bound, not a memory bound — a pathological
// single-file diff is bounded only after the executor has it in memory.
// Streaming that bound into the local executor would change run_command's
// output path and is tracked as a separate executor-hardening follow-up.
//
// The byte cap trims back to the last newline rather than slicing at an
// arbitrary offset: a raw byte slice can land mid-rune (json.Marshal would
// then emit a silent U+FFFD) and mid-line (producing a ragged, invalid unified
// diff). Trimming to the last \n keeps the output both rune-safe and
// line-complete. When the capped prefix contains no newline at all the result
// is empty, which the sentinel still makes visible.
func boundDiff(s string) (string, bool) {
	truncated := false
	if len(s) > gitDiffMaxBytes {
		s = s[:gitDiffMaxBytes]
		if nl := strings.LastIndexByte(s, '\n'); nl >= 0 {
			s = s[:nl+1]
		} else {
			s = ""
		}
		truncated = true
	}
	lines := strings.Split(s, "\n")
	if len(lines) > gitDiffMaxLines {
		lines = lines[:gitDiffMaxLines]
		s = strings.Join(lines, "\n")
		truncated = true
	}
	if truncated {
		s += gitDiffTruncSentinel
	}
	return s, truncated
}

// formatGitStatus renders the canonical text fallback for git_status.
func formatGitStatus(r gitStatusResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "On branch %s\n", r.Branch)
	if r.Clean {
		b.WriteString("working tree clean")
		return b.String()
	}
	for _, e := range r.Entries {
		if e.OrigPath != "" {
			fmt.Fprintf(&b, "%s %s -> %s\n", e.Code, e.OrigPath, e.Path)
		} else {
			fmt.Fprintf(&b, "%s %s\n", e.Code, e.Path)
		}
	}
	if r.Truncated {
		fmt.Fprintf(&b, "[status truncated at %d entries]", gitStatusMaxEntries)
		return b.String()
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatGitChangedFiles renders the canonical text fallback for
// git_changed_files.
func formatGitChangedFiles(r gitChangedFilesResult) string {
	if len(r.Files) == 0 {
		return "no changed files"
	}
	var b strings.Builder
	for _, f := range r.Files {
		if f.OrigPath != "" {
			fmt.Fprintf(&b, "%s\t%s -> %s\n", f.Status, f.OrigPath, f.Path)
		} else {
			fmt.Fprintf(&b, "%s\t%s\n", f.Status, f.Path)
		}
	}
	if r.Truncated {
		fmt.Fprintf(&b, "[changed-file list truncated at %d entries]", gitChangedFilesMaxFile)
		return b.String()
	}
	return strings.TrimRight(b.String(), "\n")
}
