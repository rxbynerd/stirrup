package types

// Judge verdict statuses. "error" means the judge could not render a
// verdict (transport failure, refusal, unparseable output) and is distinct
// from "fail", which means the criteria were evaluated and not met.
const (
	JudgeStatusPass  = "pass"
	JudgeStatusFail  = "fail"
	JudgeStatusError = "error"
)

// JudgeRecordSchemaVersion is the current JudgeRecord.SchemaVersion.
const JudgeRecordSchemaVersion = 1

// JudgeKindDiffReview is the JudgeRecord.Kind of the eval diff-review judge.
const JudgeKindDiffReview = "diff-review"

// JudgeRecord.ParseStatus values.
const (
	// JudgeParseOK means the response was exactly one conforming object.
	JudgeParseOK = "ok"

	// JudgeParseLastMatch means the verdict was extracted as the last JSON
	// object from a response with surrounding text or several objects.
	JudgeParseLastMatch = "last_match"

	// JudgeParseNoJSON means the response held no JSON object.
	JudgeParseNoJSON = "no_json"

	// JudgeParseSchemaViolation means an object was found but did not
	// match the verdict schema.
	JudgeParseSchemaViolation = "schema_violation"

	// JudgeParseRefusal means the model declined to answer.
	JudgeParseRefusal = "refusal"

	// JudgeParseTruncatedOutput means the model hit its output token limit.
	JudgeParseTruncatedOutput = "truncated_output"
)

// JudgeRecord is the provenance of one LLM judge call: which model produced
// the verdict, over what input, at what cost, and how the response was
// interpreted. It is attached to a verdict so a result can be audited and,
// later, replayed or cached without re-deriving the call's identity.
type JudgeRecord struct {
	SchemaVersion int `json:"schemaVersion"`

	// Kind names the judge, e.g. JudgeKindDiffReview.
	Kind string `json:"kind"`

	Provider string `json:"provider"`

	// RequestedModel is the model identifier sent to the provider;
	// ServedModel is the identifier the provider reports having used,
	// which differs when a gateway re-routes or an alias is resolved.
	RequestedModel string `json:"requestedModel"`
	ServedModel    string `json:"servedModel,omitempty"`

	InputTokens  int   `json:"inputTokens,omitempty"`
	OutputTokens int   `json:"outputTokens,omitempty"`
	LatencyMs    int64 `json:"latencyMs,omitempty"`

	// InputSHA256 and InputBytes describe the full judged artefact before
	// any truncation, so the identity is independent of the input cap.
	InputSHA256 string `json:"inputSha256,omitempty"`
	InputBytes  int    `json:"inputBytes,omitempty"`

	// Truncated reports that the artefact exceeded the input cap.
	Truncated bool `json:"truncated,omitempty"`

	// ParseStatus is one of the JudgeParse* values.
	ParseStatus string `json:"parseStatus,omitempty"`

	// StopReason is the provider's stop reason, normalised to
	// "end_turn" / "max_tokens" / "refusal" where the provider's own
	// vocabulary maps onto them.
	StopReason string `json:"stopReason,omitempty"`

	// ConfigHash identifies the judge configuration: two records with the
	// same hash used the same model, prompt template version, criteria and
	// request parameters.
	ConfigHash string `json:"configHash"`

	// CacheStatus is reserved for the judge verdict cache.
	CacheStatus string `json:"cacheStatus,omitempty"`
}
