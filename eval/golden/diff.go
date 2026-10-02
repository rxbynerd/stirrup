package golden

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Tree maps slash-separated relative paths to file contents.
type Tree map[string]string

// Files are the workspace states a case's change moves between.
type Files struct {
	Before Tree
	After  Tree
}

// fileDiff is one file's section of a unified diff.
type fileDiff struct {
	oldPath, newPath string // "" for /dev/null
	hunks            []hunk
}

type hunk struct {
	oldStart, oldCount int
	newStart, newCount int
	lines              []string // each prefixed ' ', '-', '+' or '\\'
	header             string
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

const devNull = "/dev/null"

// ParseDiff reconstructs the files a unified diff, as `git diff` writes it,
// changes. Unchanged lines a hunk does not show are filled with empty lines,
// identical before and after, so the reconstruction's own diff shows the same
// changes. Renames, copies, binary patches and sections without hunks (mode
// changes, empty files) are rejected rather than approximated.
func ParseDiff(diff string) (Files, error) {
	sections, err := splitDiff(diff)
	if err != nil {
		return Files{}, err
	}
	if len(sections) == 0 {
		return Files{}, errors.New("diff has no file sections")
	}
	files := Files{Before: Tree{}, After: Tree{}}
	seen := map[string]bool{}
	changed := false
	for _, s := range sections {
		p := s.newPath
		if p == "" {
			p = s.oldPath
		}
		if seen[p] {
			return Files{}, fmt.Errorf("%s: more than one section for the file", p)
		}
		seen[p] = true
		before, after, err := s.reconstruct()
		if err != nil {
			return Files{}, fmt.Errorf("%s: %w", p, err)
		}
		if before != nil {
			files.Before[p] = *before
		}
		if after != nil {
			files.After[p] = *after
		}
		if (before == nil) != (after == nil) || (before != nil && *before != *after) {
			changed = true
		}
	}
	if !changed {
		return Files{}, errors.New("diff changes nothing")
	}
	return files, nil
}

// splitDiff parses diff into file sections.
func splitDiff(diff string) ([]fileDiff, error) {
	lines := strings.Split(diff, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var sections []fileDiff
	inGitHeader := false
	for i := 0; i < len(lines); {
		line := lines[i]
		switch {
		case strings.HasPrefix(line, "diff --git "):
			if inGitHeader {
				return nil, fmt.Errorf("line %d: the previous section has no ---/+++ lines; mode-only and empty-file changes are not supported", i+1)
			}
			inGitHeader = true
			i++
		case inGitHeader && unsupportedHeader(line):
			return nil, fmt.Errorf("line %d: %s: renames, copies and binary patches are not supported", i+1, excerpt(line))
		case inGitHeader && extendedHeader(line):
			i++
		case strings.HasPrefix(line, "--- "):
			s, next, err := parseSection(lines, i)
			if err != nil {
				return nil, err
			}
			sections = append(sections, s)
			inGitHeader = false
			i = next
		case line == "" && !inGitHeader:
			i++
		default:
			return nil, fmt.Errorf("line %d: unexpected %s outside a hunk", i+1, excerpt(line))
		}
	}
	if inGitHeader {
		return nil, errors.New("the last section has no ---/+++ lines; mode-only and empty-file changes are not supported")
	}
	return sections, nil
}

func extendedHeader(line string) bool {
	for _, p := range []string{"index ", "new file mode ", "deleted file mode ", "old mode ", "new mode "} {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}

func unsupportedHeader(line string) bool {
	for _, p := range []string{"similarity index ", "dissimilarity index ", "rename from ", "rename to ", "copy from ", "copy to ", "Binary files ", "GIT binary patch"} {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}

// parseSection parses the ---/+++ lines at lines[i] and the hunks after
// them, returning the index of the first line past the section.
func parseSection(lines []string, i int) (fileDiff, int, error) {
	if i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "+++ ") {
		return fileDiff{}, 0, fmt.Errorf("line %d: --- is not followed by +++", i+1)
	}
	oldPath, err := diffPath(lines[i][4:], "a/")
	if err != nil {
		return fileDiff{}, 0, fmt.Errorf("line %d: %w", i+1, err)
	}
	newPath, err := diffPath(lines[i+1][4:], "b/")
	if err != nil {
		return fileDiff{}, 0, fmt.Errorf("line %d: %w", i+2, err)
	}
	switch {
	case oldPath == "" && newPath == "":
		return fileDiff{}, 0, fmt.Errorf("line %d: both sides are %s", i+1, devNull)
	case oldPath != "" && newPath != "" && oldPath != newPath:
		return fileDiff{}, 0, fmt.Errorf("line %d: %s becomes %s; renames are not supported", i+1, oldPath, newPath)
	}
	s := fileDiff{oldPath: oldPath, newPath: newPath}
	i += 2
	for i < len(lines) && strings.HasPrefix(lines[i], "@@ ") {
		h, next, err := parseHunk(lines, i)
		if err != nil {
			return fileDiff{}, 0, err
		}
		s.hunks = append(s.hunks, h)
		i = next
	}
	if len(s.hunks) == 0 {
		return fileDiff{}, 0, fmt.Errorf("line %d: the section for %s has no hunks", i, s.path())
	}
	return s, i, nil
}

func (s fileDiff) path() string {
	if s.newPath != "" {
		return s.newPath
	}
	return s.oldPath
}

// diffPath returns the path a ---/+++ line names, without its prefix, or ""
// for /dev/null.
func diffPath(field, prefix string) (string, error) {
	if tab := strings.IndexByte(field, '\t'); tab >= 0 {
		field = field[:tab]
	}
	if field == devNull {
		return "", nil
	}
	if strings.HasPrefix(field, `"`) {
		return "", fmt.Errorf("quoted path %s is not supported", excerpt(field))
	}
	p, ok := strings.CutPrefix(field, prefix)
	if !ok {
		return "", fmt.Errorf("path %s does not start with %q", excerpt(field), prefix)
	}
	if err := checkRelPath(p); err != nil {
		return "", err
	}
	return p, nil
}

// checkRelPath accepts a clean relative slash path that stays inside the
// workspace and avoids git's own directory.
func checkRelPath(p string) error {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p || p == "." || strings.Contains(p, "\\") {
		return fmt.Errorf("path %s must be a clean relative path", excerpt(p))
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || strings.EqualFold(part, ".git") {
			return fmt.Errorf("path %s must stay inside the workspace and outside .git", excerpt(p))
		}
	}
	return nil
}

// parseHunk parses the hunk whose header is lines[i], consuming exactly the
// lines its header counts plus any "\ No newline at end of file" markers.
func parseHunk(lines []string, i int) (hunk, int, error) {
	m := hunkHeader.FindStringSubmatch(lines[i])
	if m == nil {
		return hunk{}, 0, fmt.Errorf("line %d: malformed hunk header %s", i+1, excerpt(lines[i]))
	}
	h := hunk{header: m[0]}
	var err error
	if h.oldStart, h.oldCount, err = hunkRange(m[1], m[2]); err != nil {
		return hunk{}, 0, fmt.Errorf("line %d: %w", i+1, err)
	}
	if h.newStart, h.newCount, err = hunkRange(m[3], m[4]); err != nil {
		return hunk{}, 0, fmt.Errorf("line %d: %w", i+1, err)
	}
	oldLeft, newLeft := h.oldCount, h.newCount
	i++
	for ; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(line, `\`) {
			if len(h.lines) == 0 || strings.HasPrefix(h.lines[len(h.lines)-1], `\`) {
				return hunk{}, 0, fmt.Errorf("line %d: misplaced %s", i+1, excerpt(line))
			}
			h.lines = append(h.lines, `\`)
			continue
		}
		if oldLeft == 0 && newLeft == 0 {
			break
		}
		if line == "" {
			line = " "
		}
		switch line[0] {
		case ' ':
			oldLeft--
			newLeft--
		case '-':
			oldLeft--
		case '+':
			newLeft--
		default:
			return hunk{}, 0, fmt.Errorf("line %d: hunk %s ends early at %s", i+1, h.header, excerpt(line))
		}
		if oldLeft < 0 || newLeft < 0 {
			return hunk{}, 0, fmt.Errorf("line %d: hunk %s has more lines than its header counts", i+1, h.header)
		}
		h.lines = append(h.lines, line)
	}
	if oldLeft != 0 || newLeft != 0 {
		return hunk{}, 0, fmt.Errorf("hunk %s ends before the lines its header counts", h.header)
	}
	return h, i, nil
}

func hunkRange(start, count string) (int, int, error) {
	s, err := strconv.Atoi(start)
	if err != nil {
		return 0, 0, fmt.Errorf("hunk start %q: %w", start, err)
	}
	c := 1
	if count != "" {
		if c, err = strconv.Atoi(count); err != nil {
			return 0, 0, fmt.Errorf("hunk count %q: %w", count, err)
		}
	}
	if s == 0 && c != 0 {
		return 0, 0, errors.New("a hunk range starting at line 0 must be empty")
	}
	return s, c, nil
}

// side accumulates one side of a file's reconstruction.
type side struct {
	lines   []string
	noEOL   bool
	present bool
}

func (s *side) add(line string) error {
	if s.noEOL {
		return errors.New(`"\ No newline at end of file" marks a line that is not the file's last`)
	}
	s.lines = append(s.lines, line)
	return nil
}

func (s *side) content() *string {
	if !s.present {
		return nil
	}
	c := strings.Join(s.lines, "\n")
	if len(s.lines) > 0 && !s.noEOL {
		c += "\n"
	}
	return &c
}

// reconstruct rebuilds the file's content before and after the change; nil
// means the file is absent on that side.
func (s fileDiff) reconstruct() (before, after *string, err error) {
	old := &side{present: s.oldPath != ""}
	cur := &side{present: s.newPath != ""}
	for _, h := range s.hunks {
		oldFirst, newFirst := h.oldStart, h.newStart
		if h.oldCount == 0 {
			oldFirst++
		}
		if h.newCount == 0 {
			newFirst++
		}
		switch {
		case !old.present && h.oldCount != 0:
			return nil, nil, fmt.Errorf("hunk %s removes lines from a file the diff creates", h.header)
		case !cur.present && h.newCount != 0:
			return nil, nil, fmt.Errorf("hunk %s adds lines to a file the diff deletes", h.header)
		case oldFirst < len(old.lines)+1:
			return nil, nil, fmt.Errorf("hunk %s overlaps or precedes the hunk before it", h.header)
		}
		for len(old.lines)+1 < oldFirst {
			if err := old.add(""); err != nil {
				return nil, nil, err
			}
			if err := cur.add(""); err != nil {
				return nil, nil, err
			}
		}
		if newFirst != len(cur.lines)+1 {
			return nil, nil, fmt.Errorf("hunk %s starts at new line %d, but the hunks before it put it at %d", h.header, newFirst, len(cur.lines)+1)
		}
		var last byte
		for _, line := range h.lines {
			var err error
			switch line[0] {
			case ' ':
				if err = old.add(line[1:]); err == nil {
					err = cur.add(line[1:])
				}
			case '-':
				err = old.add(line[1:])
			case '+':
				err = cur.add(line[1:])
			case '\\':
				switch last {
				case ' ':
					old.noEOL, cur.noEOL = true, true
				case '-':
					old.noEOL = true
				case '+':
					cur.noEOL = true
				}
			}
			if err != nil {
				return nil, nil, fmt.Errorf("hunk %s: %w", h.header, err)
			}
			last = line[0]
		}
	}
	return old.content(), cur.content(), nil
}

// excerpt bounds text quoted in an error.
func excerpt(s string) string {
	const limit = 80
	if len(s) > limit {
		s = s[:limit] + "..."
	}
	return strconv.Quote(s)
}
