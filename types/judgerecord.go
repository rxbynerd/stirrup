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

// JudgeRecord.BaselineSource values: where the commit a diff-review judge
// diffed against came from.
const (
	// JudgeBaselineRunner is a baseline the eval runner committed to a
	// judge-owned repository before the agent ran.
	JudgeBaselineRunner = "runner"

	// JudgeBaselineSidecar is a runner baseline restored from the
	// judge-baseline.json sidecar retained with the task's artifacts.
	JudgeBaselineSidecar = "sidecar"

	// JudgeBaselineWorkspaceHead is the HEAD commit of a replayed
	// workspace's own repository, used when no runner baseline is
	// available.
	JudgeBaselineWorkspaceHead = "workspace-head"
)

// JudgeRecord.ParseStatus values.
const (
	// JudgeParseOK means exactly one JSON object in the response carried
	// the call's nonce and it conformed to the verdict schema. Other
	// objects and surrounding text are ignored.
	JudgeParseOK = "ok"

	// JudgeParseLastMatch means several identical conforming objects
	// carried the call's nonce.
	JudgeParseLastMatch = "last_match"

	// JudgeParseNoJSON means the response held no balanced JSON object.
	JudgeParseNoJSON = "no_json"

	// JudgeParseSchemaViolation means no object carried the call's nonce,
	// objects carrying it differed, the object carrying it did not match
	// the verdict schema, or the reply exhausted the scan budget.
	JudgeParseSchemaViolation = "schema_violation"

	// JudgeParseRefusal means the model declined to answer.
	JudgeParseRefusal = "refusal"

	// JudgeParseTruncatedOutput means the model stopped before finishing
	// its turn: it hit its output token limit or reported another stop
	// reason than end of turn or a stop sequence.
	JudgeParseTruncatedOutput = "truncated_output"
)

// JudgeRecord.CacheStatus values: how a verdict relates to the judge
// verdict cache.
const (
	// JudgeCacheBypass means the cache was not consulted: the invocation
	// runs live, or the judge failed before the verdict's cache key was
	// known.
	JudgeCacheBypass = "bypass"

	// JudgeCacheMiss means the cache held no usable verdict, or was not
	// read, and the verdict was not stored.
	JudgeCacheMiss = "miss"

	// JudgeCacheStored means the model was called and its verdict written
	// to the cache.
	JudgeCacheStored = "stored"

	// JudgeCacheHit means the verdict was served from the cache without a
	// model call.
	JudgeCacheHit = "hit"
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

	// BaselineSource is one of the JudgeBaseline* values for judges that
	// diff the workspace against a baseline commit.
	BaselineSource string `json:"baselineSource,omitempty"`

	// ParseStatus is one of the JudgeParse* values.
	ParseStatus string `json:"parseStatus,omitempty"`

	// StopReason is the provider's stop reason, normalised to
	// "end_turn" / "stop_sequence" / "max_tokens" / "refusal" where the
	// provider's own vocabulary maps onto them.
	StopReason string `json:"stopReason,omitempty"`

	// ConfigHash identifies the judge configuration: two records with the
	// same hash used the same model, prompt template version, criteria and
	// request parameters.
	ConfigHash string `json:"configHash"`

	// CacheStatus is one of the JudgeCache* values. On a hit, LatencyMs is
	// the cache lookup time and the remaining call fields describe the call
	// that produced the cached verdict.
	CacheStatus string `json:"cacheStatus,omitempty"`

	// CacheKey is the verdict's content address in the judge cache, set
	// whenever CacheStatus is not JudgeCacheBypass.
	CacheKey string `json:"cacheKey,omitempty"`
}
