package transport

import (
	"math"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/gen/runconfig"
	"github.com/rxbynerd/stirrup/types"
)

// harnessEventToProto translates an internal HarnessEvent to its proto
// wire-format representation.
func harnessEventToProto(e types.HarnessEvent) *pb.HarnessEvent {
	pe := &pb.HarnessEvent{
		Type:            e.Type,
		Text:            e.Text,
		Id:              e.ID,
		Name:            e.Name,
		Input:           []byte(e.Input),
		ToolUseId:       e.ToolUseID,
		Content:         e.Content,
		StopReason:      e.StopReason,
		Message:         e.Message,
		RequestId:       e.RequestID,
		ToolName:        e.ToolName,
		HarnessVersion:  e.HarnessVersion,
		Capabilities:    e.Capabilities,
		Audience:        e.Audience,
		Source:          e.Source,
		Model:           e.Model,
		Provider:        e.Provider,
		ToolResultCount: saturateInt32(e.ToolResultCount),
		IsError:         e.IsError,
		DurationMs:      e.DurationMs,
		AbandonReason:   e.AbandonReason,
		Tools:           e.Tools,
		ConfigFields:    e.ConfigFields,
		HarnessSemver:   e.HarnessSemver,
	}
	if e.Turn != nil {
		turn := saturateInt32(*e.Turn)
		pe.Turn = &turn
	}
	if e.Usage != nil {
		pe.Usage = tokenUsageToProto(*e.Usage)
	}

	if e.Trace != nil {
		pe.Trace = runTraceToProto(e.Trace)
	}
	pe.TraceReference = traceReferenceToProto(e.TraceReference)
	pe.TraceContext = traceContextToProto(e.TraceContext)
	if e.SessionPhase != nil {
		pe.SessionPhase = &pb.SessionPhase{Phase: e.SessionPhase.Phase, Step: e.SessionPhase.Step}
	}
	if e.BuildFailure != nil {
		pe.BuildFailure = &pb.BuildFailure{
			Reason: e.BuildFailure.Reason,
			Phase:  &pb.SessionPhase{Phase: e.BuildFailure.Phase.Phase, Step: e.BuildFailure.Phase.Step},
		}
	}

	return pe
}

func traceContextFromProto(c *pb.TraceContext) *types.TraceContext {
	if c == nil {
		return nil
	}
	return &types.TraceContext{
		Traceparent: c.Traceparent,
		Tracestate:  c.Tracestate,
	}
}

func traceContextToProto(c *types.TraceContext) *pb.TraceContext {
	if c == nil {
		return nil
	}
	return &pb.TraceContext{
		Traceparent: c.Traceparent,
		Tracestate:  c.Tracestate,
	}
}

func traceReferenceToProto(r *types.TraceReference) *pb.TraceReference {
	if r == nil {
		return nil
	}
	return &pb.TraceReference{
		TraceId:   r.TraceID,
		RunSpanId: r.RunSpanID,
		Sampled:   r.Sampled,
	}
}

// controlEventFromProto translates a proto ControlEvent to the internal
// types.ControlEvent representation.
func controlEventFromProto(pe *pb.ControlEvent) types.ControlEvent {
	e := types.ControlEvent{
		Type:         pe.Type,
		UserResponse: pe.UserResponse,
		RequestID:    pe.RequestId,
		Reason:       pe.Reason,
		Content:      pe.Content,
		Token:        pe.Token,
	}

	if pe.Allowed != nil {
		v := pe.Allowed.Value
		e.Allowed = &v
	}

	if pe.IsError != nil {
		v := pe.IsError.Value
		e.IsError = &v
	}

	if pe.ExpiresAt != nil {
		v := *pe.ExpiresAt
		e.ExpiresAt = &v
	}

	e.TraceContext = traceContextFromProto(pe.TraceContext)

	if pe.Task != nil {
		e.Task = runconfig.FromProto(pe.Task)
		e.UnknownTaskFields = unknownRunConfigFields(pe.Task)
	}

	return e
}

// runTraceToProto translates the internal RunTrace to a simplified proto
// wire format suitable for streaming back to the control plane.
func runTraceToProto(t *types.RunTrace) *pb.RunTrace {
	// StopReason mirrors Outcome for consumers predating the outcome field.
	pt := &pb.RunTrace{
		RunId:            t.ID,
		Turns:            saturateInt32(t.Turns),
		InputTokens:      saturateInt32(t.TokenUsage.Input),
		OutputTokens:     saturateInt32(t.TokenUsage.Output),
		CacheReadTokens:  saturateInt32(t.TokenUsage.CacheRead),
		CacheWriteTokens: saturateInt32(t.TokenUsage.CacheWrite),
		ReasoningTokens:  saturateInt32(t.TokenUsage.Reasoning),
		DurationMs:       t.CompletedAt.Sub(t.StartedAt).Milliseconds(),
		Outcome:          t.Outcome,
		StopReason:       t.Outcome,
	}
	if d := t.StopDetails; d != nil {
		pt.StopDetails = &pb.StopDetails{
			Type:        d.Type,
			Category:    d.Category,
			Explanation: d.Explanation,
		}
	}
	if g := t.Git; g != nil {
		pt.Git = &pb.GitHandoff{
			Branch:  g.Branch,
			Sha:     g.SHA,
			BaseSha: g.BaseSHA,
			Dirty:   g.Dirty,
			Error:   g.Error,
		}
	}
	if r := t.WorkspaceExport; r != nil {
		pt.WorkspaceExport = &pb.ExportReceipt{
			Uri:      r.URI,
			Status:   r.Status,
			Required: r.Required,
			Error:    r.Error,
			Bytes:    r.Bytes,
			Sha256:   r.SHA256,
			Files:    saturateInt32(r.Files),
		}
	}
	pt.Error = t.Error
	pt.StructuredOutput = string(t.StructuredOutput)
	pt.TraceReference = traceReferenceToProto(t.TraceReference)
	for _, dc := range t.DegradedCapabilities {
		pt.DegradedCapabilities = append(pt.DegradedCapabilities, &pb.DegradedCapability{
			Kind:  dc.Kind,
			Name:  dc.Name,
			Error: dc.Error,
		})
	}
	if n := len(t.VerificationResults); n > 0 {
		last := t.VerificationResults[n-1]
		pt.Verification = &pb.VerificationVerdict{
			Attempt:  saturateInt32(last.Attempt),
			Passed:   last.Passed,
			Feedback: last.Feedback,
			Error:    last.Error,
		}
	}
	pt.PostEditChecks = postEditCheckSummaryToProto(t.PostEditChecks)
	pt.AuxiliaryTokens = tokenUsageToProto(t.AuxiliaryTokens)
	pt.ExternalGuardTokens = tokenUsageToProto(t.ExternalGuardTokens)
	return pt
}

// tokenUsageToProto returns nil for zero usage so the field is absent on
// the wire.
func tokenUsageToProto(u types.TokenUsage) *pb.TokenUsage {
	if u == (types.TokenUsage{}) {
		return nil
	}
	return &pb.TokenUsage{
		InputTokens:      saturateInt32(u.Input),
		OutputTokens:     saturateInt32(u.Output),
		CacheReadTokens:  saturateInt32(u.CacheRead),
		CacheWriteTokens: saturateInt32(u.CacheWrite),
		ReasoningTokens:  saturateInt32(u.Reasoning),
	}
}

// postEditCheckSummaryToProto counts check executions by result; nil when
// none ran.
func postEditCheckSummaryToProto(records []types.PostEditCheckRecord) *pb.PostEditCheckSummary {
	if len(records) == 0 {
		return nil
	}
	var runs, failures, timeouts, errs int
	for _, r := range records {
		runs++
		switch {
		case r.TimedOut:
			timeouts++
		case r.Error != "":
			errs++
		case r.ExitCode != 0:
			failures++
		}
	}
	return &pb.PostEditCheckSummary{
		Runs:     saturateInt32(runs),
		Failures: saturateInt32(failures),
		Timeouts: saturateInt32(timeouts),
		Errors:   saturateInt32(errs),
	}
}

// saturateInt32 narrows n to int32, pinning out-of-range values to the
// nearest bound so a run total past MaxInt32 does not wrap on the wire.
func saturateInt32(n int) int32 {
	switch {
	case n > math.MaxInt32:
		return math.MaxInt32
	case n < math.MinInt32:
		return math.MinInt32
	}
	return int32(n)
}
