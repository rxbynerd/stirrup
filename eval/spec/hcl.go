// Package spec parses eval suite definitions from HCLv2 source files
// into the canonical types.EvalSuite shape used by the runner. HCL is
// the only accepted authoring format for suites; the legacy JSON
// loader was removed once HCL became canonical.
//
// The package mirrors the EvalSuite / EvalTask / EvalJudge shape one
// for one but keeps `hcl:` tags on internal structs in this package so
// types/eval.go stays free of optional-dependency tags.
package spec

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"

	"github.com/rxbynerd/stirrup/eval/judge"
	"github.com/rxbynerd/stirrup/types"
)

// validJudgeTypes mirrors the set accepted by eval/judge.Evaluate so
// that authoring-time validation rejects misspelt types early. The
// canonical list lives in eval/judge.KnownJudgeTypes; we materialise a
// lookup map at package init so this stays trivially in sync.
var validJudgeTypes = func() map[string]struct{} {
	m := make(map[string]struct{}, len(judge.KnownJudgeTypes()))
	for _, t := range judge.KnownJudgeTypes() {
		m[t] = struct{}{}
	}
	return m
}()

// maxJudgeDepth caps recursive composite nesting in convertJudge so that
// a pathologically nested fixture returns a clear validation error rather
// than exhausting the goroutine stack and panicking.
const maxJudgeDepth = 10

// MaxSuiteBytes is the upper bound for any single suite file the loader
// will read. It mirrors the harness's loadRunConfigFile cap so a
// misconfigured glob matching a build artefact returns a clear error
// instead of OOMing the runner.
const MaxSuiteBytes = 4 << 20 // 4 MiB

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
	if info, err := os.Stat(path); err == nil && info.Size() > MaxSuiteBytes {
		return types.EvalSuite{}, fmt.Errorf(
			"suite file %s is %d bytes, exceeds limit of %d",
			path, info.Size(), MaxSuiteBytes,
		)
	}
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

	suite, err := convertSuite(root.Suites[0])
	if err != nil {
		return types.EvalSuite{}, err
	}
	// Resolve a suite-level run_config_file relative to the suite file's
	// directory so authors can write intuitive relative paths
	// (`configs/base.json` next to the suite) without depending on the
	// caller's working directory. Absolute paths pass through unchanged.
	// Done here rather than in the runner so the runner stays pure with
	// respect to filesystem layout.
	if suite.RunConfigFile != "" && !filepath.IsAbs(suite.RunConfigFile) {
		suite.RunConfigFile = filepath.Join(filepath.Dir(path), suite.RunConfigFile)
	}
	return suite, nil
}

// rootSpec is the top level of the HCL grammar.
type rootSpec struct {
	Suites []suiteSpec `hcl:"suite,block"`
}

type suiteSpec struct {
	ID              string         `hcl:"id,label"`
	Description     string         `hcl:"description,optional"`
	RunConfigFile   string         `hcl:"run_config_file,optional"`
	RunConfig       *runConfigSpec `hcl:"run_config,block"`
	QuarantineFlags []string       `hcl:"quarantine_flags,optional"`
	Tasks           []taskSpec     `hcl:"task,block"`
}

type taskSpec struct {
	ID                 string                  `hcl:"id,label"`
	Description        string                  `hcl:"description,optional"`
	Repo               string                  `hcl:"repo,optional"`
	Ref                string                  `hcl:"ref,optional"`
	Mode               string                  `hcl:"mode,optional"`
	Prompt             string                  `hcl:"prompt,optional"`
	RunConfigOverrides *runConfigOverridesSpec `hcl:"run_config_overrides,block"`
	Judge              judgeSpec               `hcl:"judge,block"`
}

type judgeSpec struct {
	Type      string         `hcl:"type"`
	Command   string         `hcl:"command,optional"`
	Paths     []string       `hcl:"paths,optional"`
	Path      string         `hcl:"path,optional"`
	Pattern   string         `hcl:"pattern,optional"`
	Criteria  string         `hcl:"criteria,optional"`
	Require   string         `hcl:"require,optional"`
	ToolTrace *toolTraceSpec `hcl:"tool_trace,block"`
	Judges    []judgeSpec    `hcl:"judge,block"`
}

// toolTraceSpec mirrors types.ToolTraceCriteria for the "tool-trace" judge.
// sequence is the ordered list of internal tool names that must appear in
// that relative order; each `call` block is a per-tool count / success
// expectation. forbid_unknown asserts in-loop recovery from a renamed- or
// unknown-tool miss.
type toolTraceSpec struct {
	Sequence      []string             `hcl:"sequence,optional"`
	ForbidUnknown bool                 `hcl:"forbid_unknown,optional"`
	Calls         []toolCallExpectSpec `hcl:"call,block"`
}

type toolCallExpectSpec struct {
	Name         string `hcl:"name,label"`
	MinCalls     int    `hcl:"min_calls,optional"`
	MaxCalls     *int   `hcl:"max_calls,optional"`
	AllSucceeded bool   `hcl:"all_succeeded,optional"`
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
	// We do a single PartialContent against `suite` plus a probe set of
	// reserved-for-future-use block names. Whatever falls out of probe
	// is then enumerated via the underlying hclsyntax.Body for a
	// generic catch-all.
	probe := []hcl.BlockHeaderSchema{
		{Type: "suite", LabelNames: []string{"id"}},
		{Type: "variable", LabelNames: []string{"name"}},
		{Type: "locals"},
		{Type: "for_each"},
		{Type: "task", LabelNames: []string{"id"}},
		{Type: "judge"},
		{Type: "include", LabelNames: []string{"path"}},
		{Type: "import", LabelNames: []string{"path"}},
		{Type: "module", LabelNames: []string{"name"}},
	}
	content, leftover, diags := body.PartialContent(&hcl.BodySchema{Blocks: probe})
	if diags.HasErrors() {
		// A wrong label arity on `suite` etc. would surface here; bubble
		// it up via the same diagnostics path as a parse error.
		return fmt.Errorf("hcl: %s", diags.Error())
	}

	// Report the first probe-matched non-`suite` block with a "reserved
	// for future use" hint.
	for _, b := range content.Blocks {
		if b.Type == "suite" {
			continue
		}
		return fmt.Errorf(
			"unsupported top-level block %q at %s (variable/locals/for_each are reserved for future use)",
			b.Type, b.DefRange.String(),
		)
	}

	if leftover == nil {
		return nil
	}

	// Catch-all for any block type outside the probe list (e.g.
	// `output`, `resource`, `data`). The downstream gohcl.DecodeBody
	// would surface a generic strict-mode error for these, but with no
	// "reserved for future use" hint; this branch closes that gap with
	// a uniform message and a precise file/line range. The hclsyntax
	// Body type exposes its raw Blocks slice (including those already
	// matched by PartialContent above), so we filter by name.
	if syntaxBody, ok := leftover.(*hclsyntax.Body); ok {
		for _, b := range syntaxBody.Blocks {
			if isProbedTopLevelBlock(b.Type) {
				continue
			}
			return fmt.Errorf(
				"unsupported top-level block %q at %s (only `suite` blocks are allowed)",
				b.Type, b.DefRange().String(),
			)
		}
	}

	// Check for any leftover top-level attributes (e.g. `name = "x"`
	// floating outside a suite block). JustAttributes returns
	// diagnostics whenever the body still contains blocks, so we
	// ignore the gate and walk attrs directly. Iterating in sorted
	// order keeps the error deterministic when more than one stray
	// attribute is present.
	attrs, _ := leftover.JustAttributes()
	if len(attrs) > 0 {
		names := make([]string, 0, len(attrs))
		for name := range attrs {
			names = append(names, name)
		}
		sort.Strings(names)
		first := attrs[names[0]]
		return fmt.Errorf(
			"unsupported top-level attribute %q at %s (only `suite` blocks are allowed)",
			names[0], first.Range.String(),
		)
	}
	return nil
}

// isProbedTopLevelBlock reports whether name was already handled by the
// probe schema in rejectUnsupportedTopLevel. Used as a defensive belt
// for the *hclsyntax.Body catch-all walk.
func isProbedTopLevelBlock(name string) bool {
	switch name {
	case "suite", "variable", "locals", "for_each", "task", "judge",
		"include", "import", "module":
		return true
	}
	return false
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
	if s.RunConfigFile != "" && s.RunConfig != nil {
		return types.EvalSuite{}, fmt.Errorf(
			"suite %q sets both `run_config_file` and `run_config` block; these are mutually exclusive — pick one",
			s.ID,
		)
	}

	suiteRunConfig := runConfigSpecToType(s.RunConfig)
	if err := validateInlineAPIKeyRefs(suiteRunConfig, nil); err != nil {
		return types.EvalSuite{}, fmt.Errorf("suite %q: %w", s.ID, err)
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
		taskOverrides := runConfigOverridesSpecToType(t.RunConfigOverrides)
		if err := validateInlineAPIKeyRefs(nil, taskOverrides); err != nil {
			return types.EvalSuite{}, fmt.Errorf("task %q: %w", t.ID, err)
		}
		tasks = append(tasks, types.EvalTask{
			ID:                 t.ID,
			Description:        t.Description,
			Repo:               t.Repo,
			Ref:                t.Ref,
			Prompt:             t.Prompt,
			Mode:               t.Mode,
			Judge:              j,
			RunConfigOverrides: taskOverrides,
		})
	}

	var quarantineFlags []types.QuarantineFlag
	if len(s.QuarantineFlags) > 0 {
		quarantineFlags = make([]types.QuarantineFlag, len(s.QuarantineFlags))
		for i, f := range s.QuarantineFlags {
			quarantineFlags[i] = types.QuarantineFlag(f)
		}
	}

	return types.EvalSuite{
		ID:              s.ID,
		Description:     s.Description,
		Tasks:           tasks,
		RunConfigFile:   s.RunConfigFile,
		RunConfig:       suiteRunConfig,
		QuarantineFlags: quarantineFlags,
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
			"%s: invalid judge.type %q (must be one of %s)",
			context, j.Type, strings.Join(judge.KnownJudgeTypes(), ", "),
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

	// Same reasoning for the tool_trace block: only the tool-trace judge
	// consumes it. A tool_trace block on any other type is an authoring
	// mistake (a misplaced block or a forgotten `type = "tool-trace"`),
	// so reject it rather than silently dropping the assertions.
	if j.Type != "tool-trace" && j.ToolTrace != nil {
		return types.EvalJudge{}, fmt.Errorf(
			"%s: judge.type %q does not support a tool_trace block (use type \"tool-trace\")",
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

	if j.Type == "tool-trace" {
		if j.ToolTrace == nil {
			return types.EvalJudge{}, fmt.Errorf(
				"%s: tool-trace judge requires a tool_trace block",
				context,
			)
		}
		out.ToolTrace = toolTraceSpecToType(j.ToolTrace)
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

// toolTraceSpecToType materialises a parsed toolTraceSpec into the
// canonical types.ToolTraceCriteria. The HCL `call` blocks carry the
// matched internal tool name as a block label; everything else is an
// optional attribute.
func toolTraceSpecToType(s *toolTraceSpec) *types.ToolTraceCriteria {
	out := &types.ToolTraceCriteria{
		Sequence:      s.Sequence,
		ForbidUnknown: s.ForbidUnknown,
	}
	if len(s.Calls) > 0 {
		out.Calls = make([]types.ToolCallExpectation, 0, len(s.Calls))
		for _, c := range s.Calls {
			out.Calls = append(out.Calls, types.ToolCallExpectation{
				Name:         c.Name,
				MinCalls:     c.MinCalls,
				MaxCalls:     c.MaxCalls,
				AllSucceeded: c.AllSucceeded,
			})
		}
	}
	return out
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
