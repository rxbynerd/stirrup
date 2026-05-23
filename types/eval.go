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

	// QuarantineFlags marks suites mined from production data that
	// carry raw conversation content and may have privacy / safety
	// implications. A non-empty list means the suite was mined from
	// classified or oversized recordings (see QuarantineFlag for the
	// enumeration). The runner refuses to execute a quarantined
	// suite without --accept-quarantine and CI should treat the
	// presence of this field as a code-review smell. See #115 for
	// the design rationale.
	QuarantineFlags []QuarantineFlag `json:"quarantineFlags,omitempty"`
}

// QuarantineFlag classifies why a mined suite carries
// privacy / safety implications. Values are open enums — operators
// may add policy-specific flags upstream — but the canonical
// set below is what the FileStore-side miner emits.
type QuarantineFlag string

const (
	// QuarantineUnscrubbedSecretEvent indicates a source recording
	// triggered SecretRedactedInOutput during the harness run. The
	// upstream scrubber redacted the matched substring, but the
	// surrounding context that *almost* leaked may still be in the
	// recording — a precautionary flag rather than a "secret is on
	// disk" claim.
	QuarantineUnscrubbedSecretEvent QuarantineFlag = "unscrubbed_secret_event"

	// QuarantineLargePayload indicates a source recording carries a
	// turn or tool-call payload above the configurable byte limit
	// (see DefaultLargePayloadBytes). Large payloads are a privacy
	// risk by sheer surface area: an attacker who exfiltrates a
	// quarantined suite gets megabytes of model-context per task.
	QuarantineLargePayload QuarantineFlag = "large_payload"

	// QuarantinePIIClassification indicates a source recording was
	// classified `restricted` by the upstream PII pipeline. v0.1
	// does not implement a classifier — the flag is reserved so
	// future control-plane scoring can populate it without a
	// schema change.
	QuarantinePIIClassification QuarantineFlag = "pii_classification"
)

// DefaultLargePayloadBytes is the per-recording-payload byte
// threshold above which QuarantineLargePayload fires. 256 KiB is a
// conservative pick: a turn carrying that much content is large
// enough to imply file-shaped data (config dumps, log captures) was
// pulled into the conversation. Operators can override per-policy
// in a future iteration.
const DefaultLargePayloadBytes = 256 * 1024

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
