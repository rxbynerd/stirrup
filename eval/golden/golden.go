// Package golden loads golden sets: labelled diff-review cases used to
// measure a judge's agreement with human ground truth before it is trusted
// to gate suites. The format and the seed set are described in docs/eval.md.
package golden

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rxbynerd/stirrup/types"
)

// FormatVersion is the golden-set file format Load reads.
const FormatVersion = 1

// TagAdversarial marks a case whose diff tries to steer the judge toward
// its InjectionTarget.
const TagAdversarial = "adversarial"

const (
	maxSetBytes     = 16 << 20
	maxFixtureBytes = 8 << 20
)

var (
	idPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	tagPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
)

// Set is a golden set as written on disk.
type Set struct {
	Version     int    `json:"version"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Cases       []Case `json:"cases"`

	// dir resolves Case.Workspace paths.
	dir string
}

// Case is one labelled judgment. Exactly one of Diff and Workspace gives
// the change.
type Case struct {
	ID       string `json:"id"`
	Criteria string `json:"criteria"`

	// Diff is a unified diff as `git diff` writes it; ParseDiff describes
	// what it may contain.
	Diff string `json:"diff,omitempty"`

	// Workspace is a fixture directory, relative to the set's file, holding
	// before/ and after/ trees.
	Workspace string `json:"workspace,omitempty"`

	// Label is the ground-truth verdict, "pass" or "fail".
	Label string `json:"label"`

	// InjectionTarget is the verdict an adversarial case's diff tries to
	// elicit; required with TagAdversarial and the opposite of Label.
	InjectionTarget string `json:"injectionTarget,omitempty"`

	Tags  []string `json:"tags,omitempty"`
	Notes string   `json:"notes,omitempty"`
}

// Adversarial reports whether c carries TagAdversarial.
func (c Case) Adversarial() bool {
	for _, t := range c.Tags {
		if t == TagAdversarial {
			return true
		}
	}
	return false
}

// Load reads and validates the golden set at path.
func Load(path string) (*Set, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxSetBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(data) > maxSetBytes {
		return nil, fmt.Errorf("%s: golden set exceeds %d bytes", path, maxSetBytes)
	}
	s, err := Parse(data, filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Parse decodes and validates a golden set; dir resolves the cases'
// Workspace paths. Unknown fields are rejected so a misspelt one is not
// silently ignored.
func Parse(data []byte, dir string) (*Set, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s Set
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("decoding golden set: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("decoding golden set: data after the top-level object")
	}
	s.dir = dir
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// Validate checks the set and every case, reporting all problems found.
func (s *Set) Validate() error {
	var errs []error
	if s.Version != FormatVersion {
		errs = append(errs, fmt.Errorf("version %d is not supported (want %d)", s.Version, FormatVersion))
	}
	if len(s.Cases) == 0 {
		errs = append(errs, errors.New("golden set has no cases"))
	}
	ids := map[string]bool{}
	for i, c := range s.Cases {
		if ids[c.ID] {
			errs = append(errs, fmt.Errorf("case %d: duplicate id %q", i+1, c.ID))
		}
		ids[c.ID] = true
		if err := s.validateCase(c); err != nil {
			errs = append(errs, fmt.Errorf("case %d (%s): %w", i+1, excerpt(c.ID), err))
		}
	}
	return errors.Join(errs...)
}

func (s *Set) validateCase(c Case) error {
	if !idPattern.MatchString(c.ID) {
		return fmt.Errorf("id must match %s", idPattern)
	}
	if strings.TrimSpace(c.Criteria) == "" {
		return errors.New("criteria is required")
	}
	if !verdictLabel(c.Label) {
		return fmt.Errorf("label %s must be %q or %q", excerpt(c.Label), types.JudgeStatusPass, types.JudgeStatusFail)
	}
	seen := map[string]bool{}
	for _, t := range c.Tags {
		if !tagPattern.MatchString(t) {
			return fmt.Errorf("tag %s must match %s", excerpt(t), tagPattern)
		}
		if seen[t] {
			return fmt.Errorf("duplicate tag %q", t)
		}
		seen[t] = true
	}
	switch {
	case c.Adversarial() && !verdictLabel(c.InjectionTarget):
		return fmt.Errorf("an adversarial case needs injectionTarget %q or %q", types.JudgeStatusPass, types.JudgeStatusFail)
	case c.Adversarial() && c.InjectionTarget == c.Label:
		return errors.New("injectionTarget must differ from label: an injection that asks for the true verdict cannot be detected")
	case !c.Adversarial() && c.InjectionTarget != "":
		return fmt.Errorf("injectionTarget is only valid with the %q tag", TagAdversarial)
	}
	if (c.Diff == "") == (c.Workspace == "") {
		return errors.New("exactly one of diff and workspace is required")
	}
	_, err := s.Files(c)
	return err
}

func verdictLabel(v string) bool {
	return v == types.JudgeStatusPass || v == types.JudgeStatusFail
}

// Files returns the workspace before and after c's change.
func (s *Set) Files(c Case) (Files, error) {
	if c.Diff != "" {
		files, err := ParseDiff(c.Diff)
		if err != nil {
			return Files{}, fmt.Errorf("diff: %w", err)
		}
		return files, nil
	}
	if err := checkRelPath(filepath.ToSlash(c.Workspace)); err != nil {
		return Files{}, fmt.Errorf("workspace: %w", err)
	}
	root := filepath.Join(s.dir, filepath.FromSlash(c.Workspace))
	budget := maxFixtureBytes
	before, err := readTree(filepath.Join(root, "before"), &budget)
	if err != nil {
		return Files{}, fmt.Errorf("workspace before/: %w", err)
	}
	after, err := readTree(filepath.Join(root, "after"), &budget)
	if err != nil {
		return Files{}, fmt.Errorf("workspace after/: %w", err)
	}
	if treesEqual(before, after) {
		return Files{}, errors.New("workspace before/ and after/ are identical")
	}
	return Files{Before: before, After: after}, nil
}

// readTree reads the regular files under dir, charging their sizes to
// budget. Symbolic links and other special files are rejected.
func readTree(dir string, budget *int) (Tree, error) {
	tree := Tree{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && strings.EqualFold(d.Name(), ".git") {
				return fmt.Errorf("%s: a .git directory is not allowed in a fixture", rel)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: only regular files are allowed in a fixture", rel)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if *budget -= int(info.Size()); *budget < 0 {
			return fmt.Errorf("fixture exceeds %d bytes", maxFixtureBytes)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		tree[rel] = string(data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return tree, nil
}

func treesEqual(a, b Tree) bool {
	if len(a) != len(b) {
		return false
	}
	for p, c := range a {
		if other, ok := b[p]; !ok || other != c {
			return false
		}
	}
	return true
}
