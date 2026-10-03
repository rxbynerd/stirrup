package types

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

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

	// QuarantineFlags marks suites mined from production data that carry
	// raw conversation content and may have privacy / safety implications.
	// The runner refuses to execute a quarantined suite without
	// --accept-quarantine. See docs/eval.md#quarantine-envelope.
	QuarantineFlags []QuarantineFlag `json:"quarantineFlags,omitempty"`
}

// QuarantineFlag classifies why a mined suite carries
// privacy / safety implications. Values are open enums — operators
// may add policy-specific flags upstream — but the canonical
// set below is what the FileStore-side miner emits.
type QuarantineFlag string

const (
	// QuarantineUnscrubbedSecretEvent indicates a source recording
	// triggered SecretRedactedInOutput: a precautionary flag, not a claim
	// that a secret is on disk.
	QuarantineUnscrubbedSecretEvent QuarantineFlag = "unscrubbed_secret_event"

	// QuarantineLargePayload indicates a source recording carries a turn or
	// tool-call payload above DefaultLargePayloadBytes.
	QuarantineLargePayload QuarantineFlag = "large_payload"

	// QuarantinePIIClassification indicates a source recording was
	// classified `restricted` by the upstream PII pipeline. Reserved: v0.1
	// does not implement a classifier.
	QuarantinePIIClassification QuarantineFlag = "pii_classification"
)

// DefaultLargePayloadBytes is the per-recording-payload byte threshold
// above which QuarantineLargePayload fires.
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

	// Files seeds the per-task workspace before the harness runs. Keys are
	// workspace-relative paths (may include subdirectories); values are the
	// file contents. The runner writes them after any repo clone and before
	// invoking the harness. This lets a task operate on pre-existing files
	// — e.g. "read README.md and summarise it" — without depending on a
	// cloned repo or on artefacts that happen to leak into the workspace.
	Files map[string]string `json:"files,omitempty"`

	// RunConfigOverrides is a sparse per-task overlay applied on top of
	// the suite-level RunConfig baseline (EvalSuite.RunConfigFile or
	// EvalSuite.RunConfig). Nil means "no per-task overrides"; only set
	// fields are layered onto the baseline.
	RunConfigOverrides *RunConfigOverrides `json:"runConfigOverrides,omitempty"`
}

// EvalJudge describes how to judge a run's outcome.
type EvalJudge struct {
	Type     string      `json:"type"` // "test-command" | "file-exists" | "file-contains" | "diff-review" | "tool-trace" | "composite"
	Command  string      `json:"command,omitempty"`
	Paths    []string    `json:"paths,omitempty"`
	Path     string      `json:"path,omitempty"`
	Pattern  string      `json:"pattern,omitempty"`
	Criteria string      `json:"criteria,omitempty"`
	Judges   []EvalJudge `json:"judges,omitempty"`
	Require  string      `json:"require,omitempty"` // "all" | "any"

	// ToolTrace carries the parameters for the "tool-trace" judge type,
	// which inspects the run's RunTrace.ToolCalls rather than the
	// workspace filesystem. Nil for every other judge type, so the field
	// is omitted from the wire shape of the existing file/command judges.
	ToolTrace *ToolTraceCriteria `json:"toolTrace,omitempty"`

	// LLM configures the model behind the "diff-review" judge. Nil means
	// "use the invocation defaults, then the built-in default". It is
	// rejected on every other judge type by the HCL parser.
	LLM *JudgeLLMConfig `json:"llm,omitempty"`

	// Shadow marks a composite sub-judge that is evaluated and recorded in
	// the composite's details but never counts toward its outcome. A
	// task's top-level judge cannot be a shadow; see ValidateShadow.
	Shadow bool `json:"shadow,omitempty"`
}

// shadowSafeJudgeTypes are the judge types that only read the workspace
// and trace, so evaluating one as a shadow cannot change what a deciding
// judge sees.
var shadowSafeJudgeTypes = []string{"file-exists", "file-contains", "diff-review", "tool-trace", "composite"}

// ValidateShadow checks the shadow rules of the judge tree rooted at j;
// topLevel is true for a task's own judge. A top-level judge decides its
// task's outcome, so it cannot be a shadow, and every composite needs at
// least one sub-judge that is not a shadow. A shadow, and every judge
// beneath one, must be of a type without side effects, which excludes
// test-command. A judge whose llm block names the uncalibrated decision
// provider must be a shadow.
func (j EvalJudge) ValidateShadow(topLevel bool) error {
	return j.validateShadow(topLevel, false)
}

func (j EvalJudge) validateShadow(topLevel, inShadow bool) error {
	if topLevel && j.Shadow {
		return errors.New("shadow is only valid on a composite sub-judge; a task's top-level judge always decides the outcome")
	}
	shadowed := inShadow || j.Shadow
	if shadowed && !slices.Contains(shadowSafeJudgeTypes, j.Type) {
		return fmt.Errorf("a %q judge cannot be a shadow or sit inside one: it could change the workspace the deciding judges inspect", j.Type)
	}
	if j.LLM != nil && j.LLM.EffectiveProvider() == JudgeProviderDecision && !j.Shadow {
		return fmt.Errorf("llm provider %q is usable only on a shadow judge (set shadow = true inside a composite) or by stirrup-eval judge-calibrate: its verdicts are uncalibrated and never gate a task", JudgeProviderDecision)
	}
	deciding := 0
	for i, sub := range j.Judges {
		if err := sub.validateShadow(false, shadowed); err != nil {
			return fmt.Errorf("sub-judge %d: %w", i+1, err)
		}
		if !sub.Shadow {
			deciding++
		}
	}
	if j.Type == "composite" && len(j.Judges) > 0 && deciding == 0 {
		return errors.New("composite judge needs at least one sub-judge that is not a shadow")
	}
	return nil
}

// Judge LLM providers accepted by JudgeLLMConfig.Provider.
const (
	JudgeProviderAnthropic        = "anthropic"
	JudgeProviderOpenAICompatible = "openai-compatible"

	// JudgeProviderDecision speaks the /v1/systemone decision-model
	// protocol, which answers with option probabilities rather than text.
	// Suites may use it only on shadow judges; see ValidateShadow.
	JudgeProviderDecision = "decision"
)

// Structured-output modes accepted by JudgeLLMConfig.StructuredOutput.
const (
	// JudgeStructuredJSONSchema asks the provider to constrain the
	// response to the verdict schema.
	JudgeStructuredJSONSchema = "json_schema"

	// JudgeStructuredPromptOnly relies on the prompt alone, for endpoints
	// without schema-constrained decoding.
	JudgeStructuredPromptOnly = "prompt_only"
)

// Judge LLM defaults and bounds applied by the Effective* accessors.
const (
	JudgeDefaultTimeoutSeconds = 30
	JudgeMaxTimeoutSeconds     = 300
	JudgeDefaultMaxInputBytes  = 64 * 1024
	JudgeDefaultMaxTokens      = 1024
)

// JudgeLLMConfig selects and tunes the model used by an LLM-backed judge.
// Credentials are never carried inline: APIKeyRef is a "secret://"
// reference resolved when the judge runs.
type JudgeLLMConfig struct {
	// Provider is "anthropic", "openai-compatible" or "decision". Empty
	// means "anthropic".
	Provider string `json:"provider,omitempty"`

	// Model is the provider-side model identifier.
	Model string `json:"model,omitempty"`

	// BaseURL is the API root. Required for "openai-compatible" (the
	// "/chat/completions" path is appended); optional for "anthropic"
	// (default https://api.anthropic.com, "/v1/messages" is appended) and
	// "decision" (default https://api.typesafe.ai, "/v1/systemone" is
	// appended).
	BaseURL string `json:"baseUrl,omitempty"`

	// APIKeyRef is a "secret://" reference to the API key.
	APIKeyRef string `json:"apiKeyRef,omitempty"`

	// TimeoutSeconds bounds each model call. Zero means
	// JudgeDefaultTimeoutSeconds; the cap is JudgeMaxTimeoutSeconds.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`

	// MaxInputBytes caps the diff submitted to the model. Zero means
	// JudgeDefaultMaxInputBytes.
	MaxInputBytes int `json:"maxInputBytes,omitempty"`

	// Temperature is sent only when set; nil omits the parameter, which
	// is required for models that reject sampling controls. Rejected for
	// "decision".
	Temperature *float64 `json:"temperature,omitempty"`

	// MaxTokens bounds the completion, including any reasoning tokens.
	// Zero means JudgeDefaultMaxTokens. Rejected for "decision".
	MaxTokens int `json:"maxTokens,omitempty"`

	// StructuredOutput is "json_schema" or "prompt_only". Empty means
	// "json_schema". Rejected for "decision", whose answers are typed by
	// the protocol.
	StructuredOutput string `json:"structuredOutput,omitempty"`

	// AllowTruncated lets the judge rule on the head of a diff larger
	// than MaxInputBytes. When false, an oversized diff is a judge error
	// rather than a partial review.
	AllowTruncated bool `json:"allowTruncated,omitempty"`
}

// EffectiveProvider returns Provider with the empty value resolved.
func (c JudgeLLMConfig) EffectiveProvider() string {
	if c.Provider == "" {
		return JudgeProviderAnthropic
	}
	return c.Provider
}

// EffectiveTimeoutSeconds returns TimeoutSeconds with the default applied.
func (c JudgeLLMConfig) EffectiveTimeoutSeconds() int {
	if c.TimeoutSeconds == 0 {
		return JudgeDefaultTimeoutSeconds
	}
	return c.TimeoutSeconds
}

// EffectiveMaxInputBytes returns MaxInputBytes with the default applied.
func (c JudgeLLMConfig) EffectiveMaxInputBytes() int {
	if c.MaxInputBytes == 0 {
		return JudgeDefaultMaxInputBytes
	}
	return c.MaxInputBytes
}

// EffectiveMaxTokens returns MaxTokens with the default applied.
func (c JudgeLLMConfig) EffectiveMaxTokens() int {
	if c.MaxTokens == 0 {
		return JudgeDefaultMaxTokens
	}
	return c.MaxTokens
}

// EffectiveStructuredOutput returns StructuredOutput with the default
// applied.
func (c JudgeLLMConfig) EffectiveStructuredOutput() string {
	if c.StructuredOutput == "" {
		return JudgeStructuredJSONSchema
	}
	return c.StructuredOutput
}

// Validate checks the configuration is complete and well-formed. It is
// applied both to an explicit `llm` block at parse time and to the
// fully-resolved configuration (explicit block or invocation defaults
// layered over the built-in default) before a judge call.
func (c JudgeLLMConfig) Validate() error {
	switch c.EffectiveProvider() {
	case JudgeProviderAnthropic, JudgeProviderOpenAICompatible, JudgeProviderDecision:
	default:
		return fmt.Errorf("provider %q must be %q, %q or %q", c.Provider, JudgeProviderAnthropic, JudgeProviderOpenAICompatible, JudgeProviderDecision)
	}
	if c.EffectiveProvider() == JudgeProviderDecision {
		switch {
		case c.Temperature != nil:
			return fmt.Errorf("temperature is not supported by provider %q", JudgeProviderDecision)
		case c.MaxTokens != 0:
			return fmt.Errorf("max_tokens is not supported by provider %q, whose answers are probabilities rather than text", JudgeProviderDecision)
		case c.StructuredOutput != "":
			return fmt.Errorf("structured_output is not supported by provider %q, whose answers are typed by the protocol", JudgeProviderDecision)
		}
	}
	if strings.TrimSpace(c.Model) == "" {
		return fmt.Errorf("model is required")
	}
	if c.BaseURL == "" {
		if c.EffectiveProvider() == JudgeProviderOpenAICompatible {
			return fmt.Errorf("base_url is required for provider %q", JudgeProviderOpenAICompatible)
		}
	} else if err := validateJudgeBaseURL(c.BaseURL, c.APIKeyRef != ""); err != nil {
		return err
	}
	if c.APIKeyRef != "" {
		if err := ValidateJudgeKeyRef(c.APIKeyRef); err != nil {
			return err
		}
	}
	if c.TimeoutSeconds < 0 || c.TimeoutSeconds > JudgeMaxTimeoutSeconds {
		return fmt.Errorf("timeout_seconds %d must be between 1 and %d (0 selects the default of %d)",
			c.TimeoutSeconds, JudgeMaxTimeoutSeconds, JudgeDefaultTimeoutSeconds)
	}
	if c.MaxInputBytes < 0 {
		return fmt.Errorf("max_input_bytes %d must not be negative", c.MaxInputBytes)
	}
	if c.MaxTokens < 0 {
		return fmt.Errorf("max_tokens %d must not be negative", c.MaxTokens)
	}
	if c.Temperature != nil && (*c.Temperature < 0 || *c.Temperature > 2) {
		return fmt.Errorf("temperature %v must be between 0 and 2", *c.Temperature)
	}
	switch c.EffectiveStructuredOutput() {
	case JudgeStructuredJSONSchema, JudgeStructuredPromptOnly:
	default:
		return fmt.Errorf("structured_output %q must be %q or %q", c.StructuredOutput, JudgeStructuredJSONSchema, JudgeStructuredPromptOnly)
	}
	return nil
}

const (
	judgeKeyRefPrefix     = "secret://"
	judgeKeyRefFilePrefix = "secret://file://"
)

var judgeKeyRefEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateJudgeKeyRef checks the syntax of a judge api_key_ref:
// secret://ENV_NAME or secret://file://PATH. Errors never echo the
// reference, which may be a key pasted in the wrong place.
func ValidateJudgeKeyRef(ref string) error {
	switch {
	case !strings.HasPrefix(ref, judgeKeyRefPrefix):
		return errors.New("api_key_ref must be a secret:// reference; raw credentials are not permitted")
	case strings.HasPrefix(ref, judgeKeyRefFilePrefix):
		if ref == judgeKeyRefFilePrefix {
			return errors.New("api_key_ref secret://file:// names no file")
		}
		return nil
	case strings.HasPrefix(ref, "secret://ssm://"):
		return errors.New("api_key_ref: secret://ssm:// references are not supported by eval judges; use secret://ENV_NAME or secret://file://PATH")
	}
	name := strings.TrimPrefix(ref, judgeKeyRefPrefix)
	if name == "" {
		return errors.New("api_key_ref secret:// names no secret")
	}
	if !judgeKeyRefEnvName.MatchString(name) {
		return errors.New("api_key_ref must be secret://ENV_NAME, where ENV_NAME is an environment variable name (letters, digits and underscores, not starting with a digit), or secret://file://PATH")
	}
	return nil
}

// judgeMetadataHost is a cloud metadata service reachable by name.
const judgeMetadataHost = "metadata.google.internal"

// judgeMetadataAddrs are cloud metadata services outside the link-local
// ranges.
var judgeMetadataAddrs = []netip.Addr{netip.MustParseAddr("fd00:ec2::254")}

// validateJudgeBaseURL checks what can be decided without resolving the
// host. Hostnames are resolved and checked again before a run and at
// connect time.
func validateJudgeBaseURL(raw string, keyAttached bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("base_url is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("base_url must use http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("base_url must include a host")
	}
	if u.User != nil {
		return fmt.Errorf("base_url must not embed credentials; use api_key_ref")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == judgeMetadataHost {
		return fmt.Errorf("base_url host %s is a cloud metadata service", host)
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return CheckJudgeEndpointAddr(addr, u.Scheme == "http" && keyAttached)
	}
	return nil
}

// CheckJudgeEndpointAddr reports why a judge request must not be sent to
// addr, or nil. Unspecified, link-local, multicast and cloud metadata
// addresses are always refused. With keyOverHTTP only loopback and private
// addresses are accepted, since the key would otherwise cross a public
// network in cleartext.
func CheckJudgeEndpointAddr(addr netip.Addr, keyOverHTTP bool) error {
	addr = addr.Unmap().WithZone("")
	switch {
	case !addr.IsValid():
		return errors.New("judge endpoint address is not a valid IP address")
	case addr.IsUnspecified():
		return fmt.Errorf("judge endpoint address %s is unspecified", addr)
	case addr.IsLinkLocalUnicast():
		return fmt.Errorf("judge endpoint address %s is link-local, the range that holds cloud metadata services", addr)
	case addr.IsMulticast():
		return fmt.Errorf("judge endpoint address %s is multicast", addr)
	}
	for _, m := range judgeMetadataAddrs {
		if addr == m {
			return fmt.Errorf("judge endpoint address %s is a cloud metadata service", addr)
		}
	}
	if keyOverHTTP && !addr.IsLoopback() && !addr.IsPrivate() {
		return fmt.Errorf("judge endpoint address %s is public and base_url uses http:// with an API key attached; use https", addr)
	}
	return nil
}

// ToolTraceCriteria parameterises the "tool-trace" judge: it asserts on the
// tool-call behaviour recorded in a run's RunTrace (which tools were
// called, in what order, how often, with what success) rather than on the
// resulting workspace state. See docs/eval.md#the-tool-trace-judge.
type ToolTraceCriteria struct {
	// Sequence is an ordered list of internal tool names that must each
	// appear at least once, in this relative order, somewhere in the
	// run's tool calls. Non-adjacent calls between the listed names are
	// permitted — only the relative order of the named tools is checked.
	// Empty means no ordering constraint.
	Sequence []string `json:"sequence,omitempty"`

	// Calls is a set of per-tool count / success expectations evaluated
	// independently of Sequence. Empty means no per-tool constraint.
	Calls []ToolCallExpectation `json:"calls,omitempty"`

	// ForbidUnknown, when true, fails the judge if any tool call recorded a
	// failure never followed by a later successful call to the same tool.
	// Heuristic, not cause-specific: fires on any unrecovered failure, not
	// only unknown-/renamed-tool misses. See docs/eval.md.
	ForbidUnknown bool `json:"forbidUnknown,omitempty"`
}

// ToolCallExpectation is a single per-tool assertion within a
// ToolTraceCriteria. Name is the internal tool ID. The optional bounds
// and success flag let a task assert, for example, "edit_file was called
// at least once and every call succeeded" or "grep_files was called and
// no call errored".
type ToolCallExpectation struct {
	// Name is the internal tool ID to match (e.g. "read_file",
	// "edit_file", "grep_files").
	Name string `json:"name"`

	// MinCalls is the minimum number of matching calls required. Zero
	// means no lower bound.
	MinCalls int `json:"minCalls,omitempty"`

	// MaxCalls is the maximum number of matching calls allowed. A nil
	// pointer means no upper bound; a non-nil zero forbids the tool
	// entirely.
	MaxCalls *int `json:"maxCalls,omitempty"`

	// AllSucceeded, when true, requires every matching call to have
	// succeeded. When false (the default) call success is not asserted —
	// recovery scenarios deliberately expect a failed call followed by a
	// successful one.
	AllSucceeded bool `json:"allSucceeded,omitempty"`
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
