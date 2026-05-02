package permission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/cedar-policy/cedar-go"
	cedartypes "github.com/cedar-policy/cedar-go/types"

	"github.com/rxbynerd/stirrup/types"
)

// CedarSchemaVersion records the Cedar policy schema this implementation
// targets. Bump this constant whenever the entity layout (principal/action/
// resource/context shape) changes — it lets operators detect a stale
// starter-policy bundle at runtime.
//
// Schema v1:
//
//	principal: User::"<runId>"
//	    parents: { User::"any" }
//	    attrs:   {
//	        runId:        String
//	        mode:         String
//	        parentRunId:  String  (only set on sub-agents)
//	        capabilities: Set<String>
//	    }
//	action:    Action::"tool:<toolName>"
//	resource:  Tool::"<toolName>"
//	context:   {
//	    input:          Record  (the tool input, recursively translated)
//	    workspace:      String
//	    dynamicContext: Record  (string keys -> string values)
//	}
const CedarSchemaVersion = "v1"

// rootUserAlias is the parent EntityUID added to every principal so that
// policies may match all runs with `principal in User::"any"`.
const rootUserAlias = "any"

// SecurityEventEmitter is the minimal interface PolicyEnginePolicy needs to
// emit policy-decision audit events. It is intentionally smaller than
// security.SecurityLogger so the permission package does not import the
// security package and risk an import cycle. *security.SecurityLogger
// satisfies this interface implicitly via its Emit method.
type SecurityEventEmitter interface {
	Emit(level, event string, data map[string]any)
}

// PolicyEngineConfig configures a PolicyEnginePolicy. All fields except
// PolicySet and Fallback are optional; defaults are documented per-field.
type PolicyEngineConfig struct {
	// PolicySet is the parsed Cedar policy set evaluated on every Check.
	// Required.
	PolicySet *cedar.PolicySet

	// Fallback is consulted when the policy set returns no decision (no
	// policies matched). Required: pass NewAllowAll, NewDenySideEffects,
	// or NewAskUpstreamPolicy depending on operator preference.
	Fallback PermissionPolicy

	// Security receives policy_decision (allow) and policy_denied (deny)
	// audit events. Optional; nil disables security event emission.
	Security SecurityEventEmitter

	// RunID identifies the current run. Becomes the principal ID
	// (User::"<RunID>"). When empty, "anonymous" is used.
	RunID string

	// Mode is the run mode (execution, planning, review, ...). Exposed
	// to policies as principal.mode.
	Mode string

	// Workspace is the absolute workspace path. Exposed to policies as
	// context.workspace.
	Workspace string

	// ParentRunID, when non-empty, is exposed to policies as
	// principal.parentRunId. Policies use this to detect that the
	// caller is a sub-agent (see subagent-capability-cap.cedar).
	ParentRunID string

	// Capabilities is an optional list of capability names exposed as
	// principal.capabilities (Cedar Set<String>).
	Capabilities []string

	// DynamicContext is exposed as context.dynamicContext (Cedar Record
	// of String -> String). Optional; when empty an empty record is
	// supplied.
	DynamicContext map[string]string
}

// PolicyEnginePolicy is a PermissionPolicy backed by a Cedar policy set.
// On Check it builds a Cedar request from the tool call, evaluates the
// policy set, and:
//
//   - returns Allowed on a Permit decision
//   - returns Denied on a Forbid decision (reason includes matched policy IDs)
//   - delegates to the configured Fallback when no policy matched
//
// Every decision (including the fallback path) is logged via the configured
// SecurityEventEmitter when one is wired.
type PolicyEnginePolicy struct {
	cedar    *cedar.PolicySet
	fallback PermissionPolicy
	security SecurityEventEmitter

	runID        string
	mode         string
	workspace    string
	parentRunID  string
	capabilities []string

	// dynamicContext is stored as a Cedar Record so it does not need to
	// be rebuilt per Check.
	dynamicContext cedar.Record
}

// NewPolicyEnginePolicy constructs a PolicyEnginePolicy from cfg. Returns
// an error when PolicySet or Fallback is nil.
func NewPolicyEnginePolicy(cfg PolicyEngineConfig) (*PolicyEnginePolicy, error) {
	if cfg.PolicySet == nil {
		return nil, errors.New("policy-engine: PolicySet is required")
	}
	if cfg.Fallback == nil {
		return nil, errors.New("policy-engine: Fallback is required")
	}
	runID := cfg.RunID
	if runID == "" {
		runID = "anonymous"
	}
	return &PolicyEnginePolicy{
		cedar:          cfg.PolicySet,
		fallback:       cfg.Fallback,
		security:       cfg.Security,
		runID:          runID,
		mode:           cfg.Mode,
		workspace:      cfg.Workspace,
		parentRunID:    cfg.ParentRunID,
		capabilities:   append([]string(nil), cfg.Capabilities...),
		dynamicContext: stringMapToRecord(cfg.DynamicContext),
	}, nil
}

// LoadPolicySetFromFile reads a Cedar policy file from disk and parses it
// into a *cedar.PolicySet. Returns a wrapped error when the file is
// missing or contains invalid Cedar syntax.
func LoadPolicySetFromFile(path string) (*cedar.PolicySet, error) {
	if path == "" {
		return nil, errors.New("policy-engine: policy file path is empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("policy-engine: read policy file %q: %w", path, err)
	}
	ps, err := cedar.NewPolicySetFromBytes(path, data)
	if err != nil {
		return nil, fmt.Errorf("policy-engine: parse policy file %q: %w", path, err)
	}
	return ps, nil
}

// Check evaluates the Cedar policy set against the tool call. See
// PolicyEnginePolicy for the decision rules.
func (p *PolicyEnginePolicy) Check(ctx context.Context, tool types.ToolDefinition, input json.RawMessage) (*PermissionResult, error) {
	req, entities, err := p.buildRequest(tool, input)
	if err != nil {
		return nil, fmt.Errorf("policy-engine: build cedar request: %w", err)
	}

	decision, diag := cedar.Authorize(p.cedar, entities, req)
	matched := matchedPolicyIDs(diag)

	switch {
	case decision == cedar.Allow:
		p.emit("info", "policy_decision", map[string]any{
			"tool":            tool.Name,
			"decision":        "allow",
			"matchedPolicies": matched,
		})
		return &PermissionResult{Allowed: true}, nil

	case decision == cedar.Deny && len(matched) > 0:
		// Forbid policies matched.
		reason := fmt.Sprintf("denied by Cedar policy: %s", strings.Join(matched, ", "))
		p.emit("warn", "policy_denied", map[string]any{
			"tool":            tool.Name,
			"matchedPolicies": matched,
			"reason":          reason,
		})
		return &PermissionResult{Allowed: false, Reason: reason}, nil

	default:
		// No policy matched (Cedar returns Deny with empty diagnostic).
		// Consult the fallback.
		result, ferr := p.fallback.Check(ctx, tool, input)
		if ferr != nil {
			return nil, fmt.Errorf("policy-engine: fallback check: %w", ferr)
		}
		p.emit("info", "policy_decision", map[string]any{
			"tool":     tool.Name,
			"decision": "no_match",
			"fallback": fallbackOutcome(result),
		})
		return result, nil
	}
}

// ForChildRun returns a shallow clone of the receiver bound to a new
// child run identity. The Cedar policy set, fallback policy, and
// security emitter are reused unchanged; runID is replaced with the
// child's ID and parentRunID is set to the parent's ID. This is the
// only supported way to populate Cedar's principal.parentRunId
// attribute, which the subagent-capability-cap.cedar starter policy
// keys off (M3).
//
// The receiver is not mutated. Capabilities and dynamicContext are
// inherited from the parent — sub-agents do not currently downgrade
// their capability set, but they do gain the parentRunId marker that
// distinguishes them from top-level runs.
func (p *PolicyEnginePolicy) ForChildRun(childRunID string) *PolicyEnginePolicy {
	if p == nil {
		return nil
	}
	clone := *p
	clone.parentRunID = p.runID
	if childRunID != "" {
		clone.runID = childRunID
	}
	return &clone
}

// emit forwards a security event when a SecurityEventEmitter is configured.
// Callers must provide a non-nil data map.
func (p *PolicyEnginePolicy) emit(level, event string, data map[string]any) {
	if p.security == nil {
		return
	}
	// Augment with run identity for downstream correlation.
	if _, ok := data["runId"]; !ok && p.runID != "" {
		data["runId"] = p.runID
	}
	p.security.Emit(level, event, data)
}

// buildRequest constructs the Cedar Request and EntityMap for a single
// tool call. The tool input JSON is converted into Cedar values; any input
// that is not a JSON object is wrapped in a `{ "raw": <value> }` record so
// policies can still pattern-match it.
func (p *PolicyEnginePolicy) buildRequest(tool types.ToolDefinition, input json.RawMessage) (cedar.Request, cedar.EntityMap, error) {
	principalUID := cedar.NewEntityUID("User", cedartypes.String(p.runID))
	parentUID := cedar.NewEntityUID("User", rootUserAlias)
	actionUID := cedar.NewEntityUID("Action", cedartypes.String("tool:"+tool.Name))
	resourceUID := cedar.NewEntityUID("Tool", cedartypes.String(tool.Name))

	principalAttrs := cedartypes.RecordMap{
		"runId": cedartypes.String(p.runID),
		"mode":  cedartypes.String(p.mode),
	}
	if p.parentRunID != "" {
		principalAttrs["parentRunId"] = cedartypes.String(p.parentRunID)
	}
	if len(p.capabilities) > 0 {
		caps := make([]cedartypes.Value, 0, len(p.capabilities))
		for _, c := range p.capabilities {
			caps = append(caps, cedartypes.String(c))
		}
		principalAttrs["capabilities"] = cedartypes.NewSet(caps...)
	}

	principalEntity := cedar.Entity{
		UID:        principalUID,
		Parents:    cedar.NewEntityUIDSet(parentUID),
		Attributes: cedar.NewRecord(principalAttrs),
	}
	parentEntity := cedar.Entity{UID: parentUID}
	actionEntity := cedar.Entity{UID: actionUID}
	resourceEntity := cedar.Entity{UID: resourceUID}

	entities := cedar.EntityMap{
		principalUID: principalEntity,
		parentUID:    parentEntity,
		actionUID:    actionEntity,
		resourceUID:  resourceEntity,
	}

	inputValue, err := jsonToCedarValue(input)
	if err != nil {
		return cedar.Request{}, nil, fmt.Errorf("translate tool input: %w", err)
	}
	var inputRecord cedartypes.Record
	switch v := inputValue.(type) {
	case cedartypes.Record:
		inputRecord = v
	case nil:
		inputRecord = cedar.NewRecord(cedartypes.RecordMap{})
	default:
		// Tool input was a non-object JSON value (string, number, ...).
		// Wrap it so policies can still match on context.input.raw.
		inputRecord = cedar.NewRecord(cedartypes.RecordMap{"raw": v})
	}

	contextRecord := cedar.NewRecord(cedartypes.RecordMap{
		"input":          inputRecord,
		"workspace":      cedartypes.String(p.workspace),
		"dynamicContext": p.dynamicContext,
	})

	return cedar.Request{
		Principal: principalUID,
		Action:    actionUID,
		Resource:  resourceUID,
		Context:   contextRecord,
	}, entities, nil
}

// jsonToCedarValue converts a json.RawMessage into a Cedar Value. Returns
// nil (no value) when the input is empty or JSON null.
func jsonToCedarValue(raw json.RawMessage) (cedartypes.Value, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return goValueToCedar(v)
}

// maxCedarDepth caps recursion through tool input JSON when converting it
// to a Cedar Value. Go's encoding/json decoder allows ~10000 levels of
// nesting, well past what fits on the goroutine stack for downstream
// recursion; an attacker-controlled tool input can therefore reach
// goValueToCedar with a depth that crashes the harness. Cap conservatively
// — 64 levels is far deeper than any legitimate tool schema (M5).
const maxCedarDepth = 64

// goValueToCedar maps a decoded JSON value to a Cedar Value. JSON null
// becomes a Cedar nil (the caller treats this as "absent").
func goValueToCedar(v any) (cedartypes.Value, error) {
	return goValueToCedarDepth(v, 0)
}

func goValueToCedarDepth(v any, depth int) (cedartypes.Value, error) {
	if depth > maxCedarDepth {
		return nil, fmt.Errorf("policy-engine: tool input too deeply nested for Cedar evaluation (limit %d)", maxCedarDepth)
	}
	switch t := v.(type) {
	case nil:
		return nil, nil
	case bool:
		return cedartypes.Boolean(t), nil
	case string:
		return cedartypes.String(t), nil
	case json.Number:
		// Prefer int64 (Cedar Long); fall back to string to preserve
		// precision for floats Cedar cannot represent natively.
		if i, err := t.Int64(); err == nil {
			return cedartypes.Long(i), nil
		}
		// Cedar has no native float; expose as a string so policies can
		// still match on it via `like`.
		return cedartypes.String(t.String()), nil
	case []any:
		items := make([]cedartypes.Value, 0, len(t))
		for _, item := range t {
			cv, err := goValueToCedarDepth(item, depth+1)
			if err != nil {
				return nil, err
			}
			if cv == nil {
				continue
			}
			items = append(items, cv)
		}
		return cedartypes.NewSet(items...), nil
	case map[string]any:
		m := make(cedartypes.RecordMap, len(t))
		for k, val := range t {
			cv, err := goValueToCedarDepth(val, depth+1)
			if err != nil {
				return nil, err
			}
			if cv == nil {
				continue
			}
			m[cedartypes.String(k)] = cv
		}
		return cedar.NewRecord(m), nil
	default:
		return nil, fmt.Errorf("unsupported JSON value type %T", v)
	}
}

// stringMapToRecord converts a Go map[string]string into a Cedar Record of
// String -> String. Returns an empty record when m is nil.
func stringMapToRecord(m map[string]string) cedar.Record {
	rm := make(cedartypes.RecordMap, len(m))
	for k, v := range m {
		rm[cedartypes.String(k)] = cedartypes.String(v)
	}
	return cedar.NewRecord(rm)
}

// matchedPolicyIDs extracts policy IDs from a Cedar Diagnostic. Order
// follows Diagnostic.Reasons; duplicates are not expected from cedar-go.
func matchedPolicyIDs(diag cedar.Diagnostic) []string {
	if len(diag.Reasons) == 0 {
		return nil
	}
	ids := make([]string, 0, len(diag.Reasons))
	for _, r := range diag.Reasons {
		ids = append(ids, string(r.PolicyID))
	}
	return ids
}

// fallbackOutcome stringifies a fallback PermissionResult for inclusion in
// a security event payload.
func fallbackOutcome(result *PermissionResult) string {
	if result == nil {
		return "nil"
	}
	if result.Allowed {
		return "allow"
	}
	if result.Reason != "" {
		return "deny: " + result.Reason
	}
	return "deny"
}
