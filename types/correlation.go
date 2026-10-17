package types

const (
	// MaxCorrelationSessionIDLength is one below the session ID length
	// at which Langfuse silently drops the session.
	MaxCorrelationSessionIDLength = 199

	// MaxCorrelationIDLength bounds TaskID, AttemptID, UserID and each
	// label value, in bytes.
	MaxCorrelationIDLength = 255

	// MaxCorrelationURLLength is the longest URL the Langfuse metadata
	// view renders, in bytes.
	MaxCorrelationURLLength = 1500

	// MaxCorrelationLabels bounds CorrelationConfig.Labels.
	MaxCorrelationLabels = 16
)

// CorrelationConfig carries the identifiers a control plane uses for the
// run in its own UI. The harness stamps them on every span and never
// places them in the model's context. Every value is opaque to the
// harness.
type CorrelationConfig struct {
	// SessionID groups runs into one session. It takes precedence over
	// RunConfig.SessionName for session.id and gen_ai.conversation.id.
	SessionID string `json:"sessionId,omitempty"`
	TaskID    string `json:"taskId,omitempty"`
	AttemptID string `json:"attemptId,omitempty"`
	// UserID identifies the end user. The control plane chooses a
	// pseudonymous value before setting it.
	UserID string `json:"userId,omitempty"`
	// URL is the control plane's canonical page for this run.
	URL string `json:"url,omitempty"`
	// Labels carries the control plane's own slicing dimensions, such as
	// an agent profile, stamped as stirrup.label.<key>.
	Labels map[string]string `json:"labels,omitempty"`
}
