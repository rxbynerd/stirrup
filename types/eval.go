package types

// EvalSuite is a collection of tasks with reproducible starting states
// and outcome judges.
type EvalSuite struct {
	ID          string     `json:"id"`
	Description string     `json:"description"`
	Tasks       []EvalTask `json:"tasks"`

	// RunConfigFile is an optional path to a RunConfig JSON file that
	// acts as the suite-level baseline applied to every task before any
	// per-task overrides. The format matches what `stirrup harness
	// --config` accepts. Empty means "not set". Mutually exclusive with
	// RunConfig; the HCL parser rejects suites that set both.
	RunConfigFile string `json:"runConfigFile,omitempty"`

	// RunConfig is an optional inline RunConfig that acts as the
	// suite-level baseline applied to every task before any per-task
	// overrides. Nil means "not set". Mutually exclusive with
	// RunConfigFile; the HCL parser rejects suites that set both.
	RunConfig *RunConfig `json:"runConfig,omitempty"`
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

	// RunConfigOverrides is a sparse per-task overlay applied on top of
	// the suite-level RunConfig baseline (EvalSuite.RunConfigFile or
	// EvalSuite.RunConfig). Nil means "no per-task overrides"; only set
	// fields are layered onto the baseline.
	RunConfigOverrides *RunConfigOverrides `json:"runConfigOverrides,omitempty"`
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
