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
	// Resolve a suite-level `run_config_file` reference relative to the
	// suite file's directory so authors can use intuitive relative paths
	// (e.g. `configs/base.json` next to the suite). Absolute paths are
	// preserved verbatim. Doing the resolution here keeps the runner
	// pure: it never needs to know where the suite file lived.
	if suite.RunConfig != nil && suite.RunConfig.File != "" && !filepath.IsAbs(suite.RunConfig.File) {
		base := filepath.Dir(path)
		suite.RunConfig.File = filepath.Join(base, suite.RunConfig.File)
	}
	return suite, nil
}

// rootSpec is the top level of the HCL grammar.
type rootSpec struct {
	Suites []suiteSpec `hcl:"suite,block"`
}

type suiteSpec struct {
	ID            string                  `hcl:"id,label"`
	Description   string                  `hcl:"description,optional"`
	RunConfigFile *string                 `hcl:"run_config_file,optional"`
	RunConfig     *runConfigOverridesSpec `hcl:"run_config,block"`
	Tasks         []taskSpec              `hcl:"task,block"`
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
	Type     string      `hcl:"type"`
	Command  string      `hcl:"command,optional"`
	Paths    []string    `hcl:"paths,optional"`
	Path     string      `hcl:"path,optional"`
	Pattern  string      `hcl:"pattern,optional"`
	Criteria string      `hcl:"criteria,optional"`
	Require  string      `hcl:"require,optional"`
	Judges   []judgeSpec `hcl:"judge,block"`
}

// runConfigOverridesSpec is the HCL shape of types.RunConfigOverrides.
// Used for both the suite-level inline `run_config` block and the
// per-task `run_config_overrides` block — both produce a
// *types.RunConfigOverrides.
//
// The HCL field naming is snake_case throughout to match the rest of
// the suite grammar (`paths`, `pattern`, `command`); each block maps to
// the corresponding JSON field on types.RunConfigOverrides and its
// nested structs in types/runconfig.go.
//
// Surfaced fields (chunk A): every field on types.RunConfigOverrides
// (Provider, ModelRouter, ContextStrategy, EditStrategy, Verifier,
// MaxTurns) plus the commonly-set sub-fields of their nested configs.
// Less-common sub-fields (e.g. Gemini safety settings, dynamic-router
// thresholds, composite verifier children) can be added in follow-ups
// without changing the carrier shape.
//
// Mode is intentionally absent from the HCL grammar even though the
// Go type types.RunConfigOverrides.Mode exists. The runner forwards
// task.Mode through the harness's --mode flag, which always wins over
// the --config payload; exposing `mode = "..."` inside a run_config
// block would be an authoring trap (parsed and either silently
// ignored at the task level, or only effective at the suite level
// when no task pins its own Mode). Authors set mode at the task or
// suite level via the existing `mode = "..."` attribute. Experiments
// that consume RunConfigOverrides directly still use the Go field.
type runConfigOverridesSpec struct {
	MaxTurns        *int                       `hcl:"max_turns,optional"`
	Provider        *providerConfigSpec        `hcl:"provider,block"`
	ModelRouter     *modelRouterConfigSpec     `hcl:"model_router,block"`
	ContextStrategy *contextStrategyConfigSpec `hcl:"context_strategy,block"`
	EditStrategy    *editStrategyConfigSpec    `hcl:"edit_strategy,block"`
	Verifier        *verifierConfigSpec        `hcl:"verifier,block"`
}

// providerConfigSpec covers the most-common ProviderConfig fields.
// Credential federation, Gemini safety settings, and per-vendor
// extensions can be added in follow-ups; this set is enough to
// distinguish provider type + auth + endpoint, which is what eval
// suites today need to enforce.
type providerConfigSpec struct {
	Type         string            `hcl:"type"`
	APIKeyRef    string            `hcl:"api_key_ref,optional"`
	Region       string            `hcl:"region,optional"`
	Profile      string            `hcl:"profile,optional"`
	BaseURL      string            `hcl:"base_url,optional"`
	APIKeyHeader string            `hcl:"api_key_header,optional"`
	QueryParams  map[string]string `hcl:"query_params,optional"`
	GCPProject   string            `hcl:"gcp_project,optional"`
	GCPLocation  string            `hcl:"gcp_location,optional"`
}

type modelRouterConfigSpec struct {
	Type       string            `hcl:"type"`
	Provider   string            `hcl:"provider,optional"`
	Model      string            `hcl:"model,optional"`
	ModeModels map[string]string `hcl:"mode_models,optional"`
}

type contextStrategyConfigSpec struct {
	Type      string `hcl:"type"`
	MaxTokens *int   `hcl:"max_tokens,optional"`
}

type editStrategyConfigSpec struct {
	Type           string   `hcl:"type"`
	FuzzyThreshold *float64 `hcl:"fuzzy_threshold,optional"`
}

type verifierConfigSpec struct {
	Type     string `hcl:"type"`
	Command  string `hcl:"command,optional"`
	Timeout  *int   `hcl:"timeout,optional"`
	Criteria string `hcl:"criteria,optional"`
	Model    string `hcl:"model,optional"`
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

	// Suite-level run-config baseline: file path and inline block are
	// mutually exclusive — a suite that sets both leaves the merge order
	// genuinely ambiguous, so reject loudly rather than silently picking
	// a winner.
	var runCfgSource *types.RunConfigSource
	if s.RunConfigFile != nil && s.RunConfig != nil {
		return types.EvalSuite{}, fmt.Errorf(
			"suite %q: run_config_file and run_config block are mutually exclusive — set only one",
			s.ID,
		)
	}
	switch {
	case s.RunConfigFile != nil:
		runCfgSource = &types.RunConfigSource{File: *s.RunConfigFile}
	case s.RunConfig != nil:
		inline := convertRunConfigOverrides(s.RunConfig)
		runCfgSource = &types.RunConfigSource{Inline: inline}
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
			ID:                 t.ID,
			Description:        t.Description,
			Repo:               t.Repo,
			Ref:                t.Ref,
			Prompt:             t.Prompt,
			Mode:               t.Mode,
			Judge:              j,
			RunConfigOverrides: convertRunConfigOverrides(t.RunConfigOverrides),
		})
	}

	return types.EvalSuite{
		ID:          s.ID,
		Description: s.Description,
		Tasks:       tasks,
		RunConfig:   runCfgSource,
	}, nil
}

// convertRunConfigOverrides walks a parsed runConfigOverridesSpec into
// a *types.RunConfigOverrides. Returns nil when src is nil so callers
// can distinguish "no override block was authored" from "override
// block was authored but every field happened to be a zero value".
func convertRunConfigOverrides(src *runConfigOverridesSpec) *types.RunConfigOverrides {
	if src == nil {
		return nil
	}
	// Mode is not surfaced in the HCL grammar (see runConfigOverridesSpec
	// doc-comment); the runner reads task.Mode directly via the task-level
	// `mode = "..."` attribute. Leaving Mode at its zero value here keeps
	// the merged RunConfig the runner builds free of a phantom mode that
	// would silently shadow the task-level value.
	out := &types.RunConfigOverrides{
		MaxTurns: src.MaxTurns,
	}
	if src.Provider != nil {
		out.Provider = &types.ProviderConfig{
			Type:         src.Provider.Type,
			APIKeyRef:    src.Provider.APIKeyRef,
			Region:       src.Provider.Region,
			Profile:      src.Provider.Profile,
			BaseURL:      src.Provider.BaseURL,
			APIKeyHeader: src.Provider.APIKeyHeader,
			QueryParams:  src.Provider.QueryParams,
			GCPProject:   src.Provider.GCPProject,
			GCPLocation:  src.Provider.GCPLocation,
		}
	}
	if src.ModelRouter != nil {
		out.ModelRouter = &types.ModelRouterConfig{
			Type:       src.ModelRouter.Type,
			Provider:   src.ModelRouter.Provider,
			Model:      src.ModelRouter.Model,
			ModeModels: src.ModelRouter.ModeModels,
		}
	}
	if src.ContextStrategy != nil {
		cs := &types.ContextStrategyConfig{Type: src.ContextStrategy.Type}
		if src.ContextStrategy.MaxTokens != nil {
			cs.MaxTokens = *src.ContextStrategy.MaxTokens
		}
		out.ContextStrategy = cs
	}
	if src.EditStrategy != nil {
		out.EditStrategy = &types.EditStrategyConfig{
			Type:           src.EditStrategy.Type,
			FuzzyThreshold: src.EditStrategy.FuzzyThreshold,
		}
	}
	if src.Verifier != nil {
		v := &types.VerifierConfig{
			Type:     src.Verifier.Type,
			Command:  src.Verifier.Command,
			Criteria: src.Verifier.Criteria,
			Model:    src.Verifier.Model,
		}
		if src.Verifier.Timeout != nil {
			v.Timeout = *src.Verifier.Timeout
		}
		out.Verifier = v
	}
	return out
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
