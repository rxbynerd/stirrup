package types

// EvalSuite is a collection of tasks with reproducible starting states
// and outcome judges.
type EvalSuite struct {
	ID          string     `json:"id"`
	Description string     `json:"description"`
	Tasks       []EvalTask `json:"tasks"`

	// RunConfig carries an optional suite-level RunConfig baseline that
	// every task inherits. The runner merges per-task RunConfigOverrides
	// on top of it before invoking `stirrup harness --config`. Nil means
	// "no suite-level baseline configured", which preserves the legacy
	// runner behaviour of relying entirely on the harness binary's own
	// defaults / flags.
	RunConfig *RunConfigSource `json:"runConfig,omitempty"`
}

// EvalTask describes a single evaluation task.
type EvalTask struct {
	ID          string    `json:"id"`
	Description string    `json:"description"`
	Repo        string    `json:"repo"`
	Ref         string    `json:"ref"`
	Prompt      string    `json:"prompt"`
	Mode        string    `json:"mode"`
	Judge       EvalJudge `json:"judge"`

	// RunConfigOverrides carries optional sparse RunConfig overrides
	// applied on top of the suite-level baseline (if any) when the
	// runner materialises the per-task RunConfig. Nil means "no per-task
	// overrides", in which case the suite-level baseline (if any) is
	// used as-is.
	RunConfigOverrides *RunConfigOverrides `json:"runConfigOverrides,omitempty"`
}

// RunConfigSource describes where a suite's RunConfig baseline comes
// from. Exactly one of File or Inline is expected to be set; the spec
// loader enforces mutual exclusion at parse time. The carrier itself
// keeps both fields so downstream consumers can branch on whichever is
// populated without a second tagged enum.
type RunConfigSource struct {
	// File is a filesystem path to a RunConfig JSON document of the same
	// shape consumed by `stirrup harness --config`. Resolved relative to
	// the suite file's directory by the loader.
	File string `json:"file,omitempty"`

	// Inline is a sparse, parsed RunConfig baseline expressed inline in
	// the suite definition. Same shape as a per-task RunConfigOverrides
	// — only the fields the suite cares about need to be set.
	Inline *RunConfigOverrides `json:"inline,omitempty"`
}

// EvalJudge describes how to judge a run's outcome.
type EvalJudge struct {
	Type     string      `json:"type"` // "test-command" | "file-exists" | "file-contains" | "diff-review" | "composite"
	Command  string      `json:"command,omitempty"`
	Paths    []string    `json:"paths,omitempty"`
	Path     string      `json:"path,omitempty"`
	Pattern  string      `json:"pattern,omitempty"`
	Criteria string      `json:"criteria,omitempty"`
	Judges   []EvalJudge `json:"judges,omitempty"`
	Require  string      `json:"require,omitempty"` // "all" | "any"
}

// Experiment holds one or more variables constant while varying others.
type Experiment struct {
	ID             string              `json:"id"`
	Description    string              `json:"description"`
	Suite          string              `json:"suite"`
	BaseConfig     RunConfigOverrides  `json:"baseConfig"`
	Variants       []ExperimentVariant `json:"variants"`
	RunsPerVariant int                 `json:"runsPerVariant"`
}

// ExperimentVariant names a set of RunConfig overrides.
type ExperimentVariant struct {
	Name      string             `json:"name"`
	Overrides RunConfigOverrides `json:"overrides"`
}

// RunConfigOverrides holds optional RunConfig fields for experiment variants.
type RunConfigOverrides struct {
	Mode            string                 `json:"mode,omitempty"`
	Provider        *ProviderConfig        `json:"provider,omitempty"`
	ModelRouter     *ModelRouterConfig     `json:"modelRouter,omitempty"`
	ContextStrategy *ContextStrategyConfig `json:"contextStrategy,omitempty"`
	EditStrategy    *EditStrategyConfig    `json:"editStrategy,omitempty"`
	Verifier        *VerifierConfig        `json:"verifier,omitempty"`
	MaxTurns        *int                   `json:"maxTurns,omitempty"`
}
