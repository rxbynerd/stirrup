// Package spec parses eval suite definitions from HCLv2 source files
// into the canonical types.EvalSuite shape used by the runner. HCL is
// the preferred authoring format for suites; the legacy JSON loader
// still works and is dispatched by file extension in the eval CLI.
//
// The package mirrors the EvalSuite / EvalTask / EvalJudge shape one
// for one but keeps `hcl:` tags on internal structs in this package so
// types/eval.go stays free of optional-dependency tags.
package spec

import (
	"bytes"
	"fmt"
	"os"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"

	"github.com/rxbynerd/stirrup/types"
)

// validJudgeTypes mirrors the set accepted by eval/judge.Evaluate so that
// authoring-time validation rejects misspelt types early.
var validJudgeTypes = map[string]struct{}{
	"test-command":  {},
	"file-exists":   {},
	"file-contains": {},
	"diff-review":   {},
	"composite":     {},
}

// maxJudgeDepth caps recursive composite nesting in convertJudge so that
// a pathologically nested fixture returns a clear validation error rather
// than exhausting the goroutine stack and panicking.
const maxJudgeDepth = 10

// LoadSuiteHCL parses an HCL file at path and returns a types.EvalSuite.
//
// The HCL surface mirrors the existing types one for one:
//
//	suite "id" {
//	  description = "..."
//	  task "task-id" {
//	    description = "..."
//	    repo        = ""
//	    ref         = ""
//	    mode        = "execution"
//	    prompt      = <<-EOT
//	      multi-line
//	    EOT
//	    judge { ... }
//	  }
//	}
//
// Composite judges nest `judge` blocks recursively; all other judge
// fields are HCL attributes.
//
// Diagnostics from the parser are surfaced as a single wrapped error
// formatted via hcl.NewDiagnosticTextWriter so callers see file/line
// context for each problem.
func LoadSuiteHCL(path string) (types.EvalSuite, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return types.EvalSuite{}, fmt.Errorf("reading suite file: %w", err)
	}

	parser := hclparse.NewParser()
	file, diags := parser.ParseHCL(src, path)
	if diags.HasErrors() {
		return types.EvalSuite{}, formatDiagnostics(parser, diags)
	}

	// Walk the top-level body explicitly so we can reject anything that
	// is not a `suite` block with a clear "reserved for future use"
	// error rather than a generic gohcl complaint.
	if err := rejectUnsupportedTopLevel(file.Body); err != nil {
		return types.EvalSuite{}, err
	}

	var root rootSpec
	if d := gohcl.DecodeBody(file.Body, nil, &root); d.HasErrors() {
		return types.EvalSuite{}, formatDiagnostics(parser, d)
	}

	switch len(root.Suites) {
	case 0:
		return types.EvalSuite{}, fmt.Errorf("no suite block found in %s", path)
	case 1:
		// ok
	default:
		return types.EvalSuite{}, fmt.Errorf("expected exactly one suite block, found %d", len(root.Suites))
	}

	return convertSuite(root.Suites[0])
}

// rootSpec is the top level of the HCL grammar.
type rootSpec struct {
	Suites []suiteSpec `hcl:"suite,block"`
}

type suiteSpec struct {
	ID          string     `hcl:"id,label"`
	Description string     `hcl:"description,optional"`
	Tasks       []taskSpec `hcl:"task,block"`
}

type taskSpec struct {
	ID          string    `hcl:"id,label"`
	Description string    `hcl:"description,optional"`
	Repo        string    `hcl:"repo,optional"`
	Ref         string    `hcl:"ref,optional"`
	Mode        string    `hcl:"mode,optional"`
	Prompt      string    `hcl:"prompt,optional"`
	Judge       judgeSpec `hcl:"judge,block"`
}

type judgeSpec struct {
	Type     string      `hcl:"type"`
	Command  string      `hcl:"command,optional"`
	Paths    []string    `hcl:"paths,optional"`
	Path     string      `hcl:"path,optional"`
	Pattern  string      `hcl:"pattern,optional"`
	Criteria string      `hcl:"criteria,optional"`
	Require  string      `hcl:"require,optional"`
	Judges   []judgeSpec `hcl:"judge,block"`
}

// rejectUnsupportedTopLevel inspects the file body and returns a clear
// error if it contains any top-level construct other than `suite`
// blocks. variable / locals / for_each are reserved for future use; we
// reject them now so authors don't write suites that depend on a
// grammar we haven't committed to.
//
// We use PartialContent with a wildcard schema rather than `,remain`
// in rootSpec so the error message names the offending block precisely.
func rejectUnsupportedTopLevel(body hcl.Body) error {
	// Only `suite` is a recognised top-level block; there are no
	// recognised top-level attributes. Anything else lands in `leftover`.
	schema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "suite", LabelNames: []string{"id"}},
		},
	}
	_, leftover, diags := body.PartialContent(schema)
	if diags.HasErrors() {
		// A wrong label arity on `suite` etc. would surface here; bubble
		// it up via the same diagnostics path as a parse error.
		return fmt.Errorf("hcl: %s", diags.Error())
	}
	if leftover == nil {
		return nil
	}

	// The HCL syntax body type exposes JustAttributes / blocks that we
	// can probe for unrecognised content. We try a wide net of plausible
	// top-level keywords; whichever appears is reported.
	probe := []hcl.BlockHeaderSchema{
		{Type: "variable", LabelNames: []string{"name"}},
		{Type: "locals"},
		{Type: "for_each"},
		{Type: "task", LabelNames: []string{"id"}},
		{Type: "judge"},
		{Type: "include", LabelNames: []string{"path"}},
		{Type: "import", LabelNames: []string{"path"}},
		{Type: "module", LabelNames: []string{"name"}},
	}
	content, _, _ := leftover.PartialContent(&hcl.BodySchema{Blocks: probe})
	for _, b := range content.Blocks {
		return fmt.Errorf(
			"unsupported top-level block %q at %s (variable/locals/for_each are reserved for future use)",
			b.Type, b.DefRange.String(),
		)
	}

	// Check for any leftover top-level attributes (e.g. `name = "x"`
	// floating outside a suite block).
	if attrs, attrDiags := leftover.JustAttributes(); !attrDiags.HasErrors() {
		for name, attr := range attrs {
			return fmt.Errorf(
				"unsupported top-level attribute %q at %s (only `suite` blocks are allowed)",
				name, attr.Range.String(),
			)
		}
	}
	return nil
}

// convertSuite translates the parsed HCL spec into the canonical
// types.EvalSuite shape and runs authoring-time validation.
func convertSuite(s suiteSpec) (types.EvalSuite, error) {
	if s.ID == "" {
		return types.EvalSuite{}, fmt.Errorf("suite ID is required")
	}
	if len(s.Tasks) == 0 {
		return types.EvalSuite{}, fmt.Errorf("suite %q must contain at least one task", s.ID)
	}

	tasks := make([]types.EvalTask, 0, len(s.Tasks))
	for i, t := range s.Tasks {
		if t.ID == "" {
			return types.EvalSuite{}, fmt.Errorf("task[%d] in suite %q is missing an id label", i, s.ID)
		}
		j, err := convertJudge(t.Judge, fmt.Sprintf("task %q", t.ID), 0)
		if err != nil {
			return types.EvalSuite{}, err
		}
		tasks = append(tasks, types.EvalTask{
			ID:          t.ID,
			Description: t.Description,
			Repo:        t.Repo,
			Ref:         t.Ref,
			Prompt:      t.Prompt,
			Mode:        t.Mode,
			Judge:       j,
		})
	}

	return types.EvalSuite{
		ID:          s.ID,
		Description: s.Description,
		Tasks:       tasks,
	}, nil
}

// convertJudge recursively translates a judgeSpec into types.EvalJudge,
// validating type and require values along the way. context describes
// the enclosing scope (e.g. `task "foo"`) for error messages. depth
// guards against pathologically nested composite judges; see
// maxJudgeDepth.
func convertJudge(j judgeSpec, context string, depth int) (types.EvalJudge, error) {
	if depth > maxJudgeDepth {
		return types.EvalJudge{}, fmt.Errorf(
			"%s: judge nesting exceeds maximum depth of %d",
			context, maxJudgeDepth,
		)
	}
	if _, ok := validJudgeTypes[j.Type]; !ok {
		return types.EvalJudge{}, fmt.Errorf(
			"%s: invalid judge.type %q (must be one of test-command, file-exists, file-contains, diff-review, composite)",
			context, j.Type,
		)
	}

	// gohcl decodes nested `judge` blocks into judgeSpec.Judges
	// regardless of the parent type. Only composite consumes them; on
	// any other type, silently dropping them would mask a common
	// authoring mistake (forgetting to set `type = "composite"` after
	// adding child judges). Reject the construct loudly instead.
	if j.Type != "composite" && len(j.Judges) > 0 {
		return types.EvalJudge{}, fmt.Errorf(
			"%s: judge.type %q does not support nested judge blocks (use type \"composite\")",
			context, j.Type,
		)
	}

	out := types.EvalJudge{
		Type:     j.Type,
		Command:  j.Command,
		Paths:    j.Paths,
		Path:     j.Path,
		Pattern:  j.Pattern,
		Criteria: j.Criteria,
		Require:  j.Require,
	}

	if j.Type == "composite" {
		require := j.Require
		if require == "" {
			require = "all"
		}
		if require != "all" && require != "any" {
			return types.EvalJudge{}, fmt.Errorf(
				"%s: composite judge.require must be \"all\" or \"any\", got %q",
				context, j.Require,
			)
		}
		out.Require = require

		children := make([]types.EvalJudge, 0, len(j.Judges))
		for i, sub := range j.Judges {
			converted, err := convertJudge(sub, fmt.Sprintf("%s > judge[%d]", context, i), depth+1)
			if err != nil {
				return types.EvalJudge{}, err
			}
			children = append(children, converted)
		}
		out.Judges = children

		if len(out.Judges) == 0 {
			// A bare composite block (likely a typo where the author
			// forgot to add child judges) would otherwise evaluate as
			// vacuous-pass via the `all` of zero — producing misleading
			// CI greens. Force at least one child.
			return types.EvalJudge{}, fmt.Errorf(
				"%s: composite judge must have at least one nested judge block",
				context,
			)
		}
	}

	return out, nil
}

// formatDiagnostics renders an hcl.Diagnostics into a single wrapped
// error using NewDiagnosticTextWriter so authors see file/line context.
func formatDiagnostics(parser *hclparse.Parser, diags hcl.Diagnostics) error {
	var buf bytes.Buffer
	w := hcl.NewDiagnosticTextWriter(&buf, parser.Files(), 0, false)
	_ = w.WriteDiagnostics(diags)
	msg := buf.String()
	if msg == "" {
		msg = diags.Error()
	}
	return fmt.Errorf("hcl: %s", msg)
}
