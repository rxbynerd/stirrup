package transport

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

// TestRunTraceToProto_OutcomePopulated pins that the canonical Outcome
// lands on the proto outcome field verbatim, and stop_reason mirrors it
// for consumers predating the outcome field. The proto.Marshal/Unmarshal
// round-trip pins the field's wire tag, which a struct-copy assertion
// alone would not catch.
func TestRunTraceToProto_OutcomePopulated(t *testing.T) {
	cases := []struct {
		name    string
		outcome string
	}{
		{name: "success", outcome: "success"},
		{name: "error", outcome: "error"},
		{name: "max_turns", outcome: "max_turns"},
		{name: "verification_failed", outcome: "verification_failed"},
		{name: "verification_error", outcome: "verification_error"},
		{name: "budget_exceeded", outcome: "budget_exceeded"},
		{name: "stalled", outcome: "stalled"},
		{name: "tool_failures", outcome: "tool_failures"},
		{name: "cancelled", outcome: "cancelled"},
		{name: "timeout", outcome: "timeout"},
		{name: "max_tokens", outcome: "max_tokens"},
		{name: "empty", outcome: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			start := time.Unix(1700000000, 0)
			tr := &types.RunTrace{
				ID:          "run-141",
				Turns:       3,
				TokenUsage:  types.TokenUsage{Input: 120, Output: 45, CacheRead: 80, CacheWrite: 30, Reasoning: 12},
				StartedAt:   start,
				CompletedAt: start.Add(2500 * time.Millisecond),
				Outcome:     tc.outcome,
			}
			pt := runTraceToProto(tr)
			must.Equal(tc.outcome, pt.Outcome, "Outcome")
			must.Equal(tc.outcome, pt.StopReason, "StopReason should mirror Outcome")
			must.Equal("run-141", pt.RunId, "RunId")
			must.EqualValues(3, pt.Turns, "Turns")
			must.EqualValues(120, pt.InputTokens, "InputTokens")
			must.EqualValues(45, pt.OutputTokens, "OutputTokens")
			must.EqualValues(80, pt.CacheReadTokens, "CacheReadTokens")
			must.EqualValues(30, pt.CacheWriteTokens, "CacheWriteTokens")
			must.EqualValues(12, pt.ReasoningTokens, "ReasoningTokens")
			must.EqualValues(2500, pt.DurationMs, "DurationMs")

			raw, err := proto.Marshal(pt)
			must.NoError(err, "marshal")
			var decoded pb.RunTrace
			must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")
			must.Equal(tc.outcome, decoded.Outcome, "Outcome after wire round-trip")
			must.Equal(tc.outcome, decoded.StopReason, "StopReason after wire round-trip")
			must.EqualValues(80, decoded.CacheReadTokens, "CacheReadTokens after wire round-trip")
			must.EqualValues(30, decoded.CacheWriteTokens, "CacheWriteTokens after wire round-trip")
			must.EqualValues(12, decoded.ReasoningTokens, "ReasoningTokens after wire round-trip")
		})
	}
}

func TestRunTraceToProto_ZeroBreakdownRoundTrip(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	pt := runTraceToProto(&types.RunTrace{ID: "run-zero", TokenUsage: types.TokenUsage{Input: 120, Output: 45}})
	raw, err := proto.Marshal(pt)
	must.NoError(err, "marshal")
	var decoded pb.RunTrace
	must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")
	check.EqualValues(120, decoded.InputTokens, "InputTokens")
	check.EqualValues(45, decoded.OutputTokens, "OutputTokens")
	check.Zero(decoded.CacheReadTokens, "CacheReadTokens")
	check.Zero(decoded.CacheWriteTokens, "CacheWriteTokens")
	check.Zero(decoded.ReasoningTokens, "ReasoningTokens")
}

func TestRunTraceToProto_SaturatesTokenCounts(t *testing.T) {
	check := assert.New(t)
	pt := runTraceToProto(&types.RunTrace{TokenUsage: types.TokenUsage{
		Input:     math.MaxInt,
		Output:    math.MaxInt32 + 1,
		CacheRead: math.MaxInt32,
		Reasoning: math.MinInt,
	}})
	check.EqualValues(math.MaxInt32, pt.InputTokens, "InputTokens")
	check.EqualValues(math.MaxInt32, pt.OutputTokens, "OutputTokens")
	check.EqualValues(math.MaxInt32, pt.CacheReadTokens, "CacheReadTokens")
	check.EqualValues(math.MinInt32, pt.ReasoningTokens, "ReasoningTokens")
}

func TestRunTraceToProto_LedgerUsage(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	pt := runTraceToProto(&types.RunTrace{
		ID:                  "run-aux",
		TokenUsage:          types.TokenUsage{Input: 500, Output: 60},
		AuxiliaryTokens:     types.TokenUsage{Input: 200, Output: 10, CacheRead: 50, CacheWrite: 7, Reasoning: 3},
		ExternalGuardTokens: types.TokenUsage{Input: math.MaxInt, Output: 1},
	})
	raw, err := proto.Marshal(pt)
	must.NoError(err, "marshal")
	var decoded pb.RunTrace
	must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")
	aux := decoded.GetAuxiliaryTokens()
	check.EqualValues(200, aux.GetInputTokens(), "AuxiliaryTokens.InputTokens")
	check.EqualValues(10, aux.GetOutputTokens(), "AuxiliaryTokens.OutputTokens")
	check.EqualValues(50, aux.GetCacheReadTokens(), "AuxiliaryTokens.CacheReadTokens")
	check.EqualValues(7, aux.GetCacheWriteTokens(), "AuxiliaryTokens.CacheWriteTokens")
	check.EqualValues(3, aux.GetReasoningTokens(), "AuxiliaryTokens.ReasoningTokens")
	ext := decoded.GetExternalGuardTokens()
	check.EqualValues(math.MaxInt32, ext.GetInputTokens(), "ExternalGuardTokens.InputTokens saturates")
	check.EqualValues(1, ext.GetOutputTokens(), "ExternalGuardTokens.OutputTokens")
}

func TestRunTraceToProto_ZeroLedgerUsageIsAbsent(t *testing.T) {
	check := assert.New(t)
	pt := runTraceToProto(&types.RunTrace{ID: "run-plain", TokenUsage: types.TokenUsage{Input: 5}})
	check.Nil(pt.AuxiliaryTokens, "AuxiliaryTokens")
	check.Nil(pt.ExternalGuardTokens, "ExternalGuardTokens")
}

// TestRunTraceToProto_StopDetails pins the stop_details mirror through a
// wire round-trip, and that a nil StopDetails stays absent on the wire.
func TestRunTraceToProto_StopDetails(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	tr := &types.RunTrace{
		ID:      "run-refused",
		Outcome: "refusal",
		StopDetails: &types.StopDetails{
			Type:        "refusal",
			Category:    "reasoning_extraction",
			Explanation: "declined",
		},
	}
	raw, err := proto.Marshal(runTraceToProto(tr))
	must.NoError(err, "marshal")
	var decoded pb.RunTrace
	must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")
	got := decoded.GetStopDetails()
	check.Equal("refusal", got.GetType(), "StopDetails.Type")
	check.Equal("reasoning_extraction", got.GetCategory(), "StopDetails.Category")
	check.Equal("declined", got.GetExplanation(), "StopDetails.Explanation")

	check.Nil(runTraceToProto(&types.RunTrace{Outcome: "success"}).StopDetails, "StopDetails absent for a nil StopDetails")
}

// TestRunTraceToProto_PartialStopDetails pins that a refusal with no
// category or explanation still reaches the wire as a present message.
func TestRunTraceToProto_PartialStopDetails(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	raw, err := proto.Marshal(runTraceToProto(&types.RunTrace{
		Outcome:     "refusal",
		StopDetails: &types.StopDetails{Type: "refusal"},
	}))
	must.NoError(err, "marshal")
	var decoded pb.RunTrace
	must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")
	got := decoded.GetStopDetails()
	must.NotNil(got, "StopDetails should be present with type refusal")
	check.Equal("refusal", got.GetType(), "StopDetails.Type")
	check.Empty(got.GetCategory(), "StopDetails.Category")
	check.Empty(got.GetExplanation(), "StopDetails.Explanation")
}

// TestRunTraceToProto_Verification pins that the last verifier attempt
// reaches the wire through a round trip, and that a run with no recorded
// attempt leaves the field absent.
func TestRunTraceToProto_Verification(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	tr := &types.RunTrace{
		Outcome: "verification_error",
		VerificationResults: []types.VerificationResult{
			{Attempt: 1, Passed: false, Feedback: "tests failed"},
			{Attempt: 2, Passed: false, Error: "exec failed: [REDACTED]"},
		},
	}
	raw, err := proto.Marshal(runTraceToProto(tr))
	must.NoError(err, "marshal")
	var decoded pb.RunTrace
	must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")
	got := decoded.GetVerification()
	check.EqualValues(2, got.GetAttempt(), "Verification.Attempt")
	check.False(got.GetPassed(), "Verification.Passed")
	check.Empty(got.GetFeedback(), "Verification.Feedback")
	check.Equal("exec failed: [REDACTED]", got.GetError(), "Verification.Error")

	passed := runTraceToProto(&types.RunTrace{
		Outcome:             "success",
		VerificationResults: []types.VerificationResult{{Attempt: 1, Passed: true}},
	})
	check.True(passed.GetVerification().GetPassed(), "passing Verification.Passed")
	check.EqualValues(1, passed.GetVerification().GetAttempt(), "passing Verification.Attempt")

	check.Nil(runTraceToProto(&types.RunTrace{Outcome: "success"}).Verification, "Verification absent with no recorded attempt")
}

// wireRoundTrip encodes in and decodes the bytes into out, so a test reads
// values that have passed through the wire format.
func wireRoundTrip(t *testing.T, in, out proto.Message) {
	t.Helper()
	must := require.New(t)
	raw, err := proto.Marshal(in)
	must.NoError(err, "marshal")
	must.NoError(proto.Unmarshal(raw, out), "unmarshal")
}

// TestHarnessEventToProto_AudiencePreserved checks that a HarnessEvent's
// Audience survives the internal-to-proto hop unchanged. The control plane
// has no other source for the intended JWT aud claim.
func TestHarnessEventToProto_AudiencePreserved(t *testing.T) {
	check := assert.New(t)
	e := types.HarnessEvent{
		Type:      "sandbox_token_request",
		RequestID: "req-1",
		Audience:  "https://haybale.internal",
	}

	pe := harnessEventToProto(e)

	check.Equal("https://haybale.internal", pe.Audience)
	check.Equal("req-1", pe.RequestId)
}

// TestControlEventFromProto_SandboxTokenFieldsPreserved checks that the
// sandbox token response fields survive translation. The token case goes
// through the wire format so an unset optional ExpiresAt stays nil instead
// of reading back as an explicit zero.
func TestControlEventFromProto_SandboxTokenFieldsPreserved(t *testing.T) {
	t.Run("token and expires_at round-trip", func(t *testing.T) {
		must := require.New(t)
		var exp int64 = 1700003600
		original := &pb.ControlEvent{
			Type:      "sandbox_token_response",
			RequestId: "req-1",
			Token:     "eyJhbGciOiJFUzI1NiJ9.eyJzdWIiOiJydW4tMSJ9.sig",
			ExpiresAt: &exp,
		}

		var decoded pb.ControlEvent
		wireRoundTrip(t, original, &decoded)

		e := controlEventFromProto(&decoded)

		must.Equal(original.Token, e.Token)
		must.NotNil(e.ExpiresAt, "ExpiresAt: got nil, want non-nil pointer")
		must.Equal(exp, *e.ExpiresAt)
	})

	t.Run("expires_at unset stays nil", func(t *testing.T) {
		must := require.New(t)
		original := &pb.ControlEvent{
			Type:      "sandbox_token_response",
			RequestId: "req-2",
			Token:     "some-token",
		}

		var decoded pb.ControlEvent
		wireRoundTrip(t, original, &decoded)

		e := controlEventFromProto(&decoded)

		must.Nil(e.ExpiresAt, "ExpiresAt should be nil when proto field is unset")
	})

	t.Run("is_error and reason round-trip for a decline", func(t *testing.T) {
		must := require.New(t)
		original := &pb.ControlEvent{
			Type:      "sandbox_token_response",
			RequestId: "req-3",
			IsError:   &pb.OptionalBool{Value: true},
			Reason:    "no issuer configured",
		}

		e := controlEventFromProto(original)

		must.NotNil(e.IsError)
		must.True(*e.IsError)
		must.Equal("no issuer configured", e.Reason)
		must.Empty(e.Token, "Token should be empty on an error response")
	})
}

// TestRunTraceToProto_HandoffSummaryRoundTrip pins that the git handoff,
// the export receipt and the run error reach the "done" trace summary.
func TestRunTraceToProto_HandoffSummaryRoundTrip(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	tr := &types.RunTrace{
		ID:      "run-handoff",
		Outcome: "handoff_failed",
		Git: &types.GitHandoff{
			Branch: "stirrup/run-handoff", SHA: "abc123", BaseSHA: "def456", Dirty: true, Error: "dirty tree",
		},
		WorkspaceExport: &types.ExportReceipt{
			URI: "gs://bucket/ws.tar.gz", Status: types.ExportStatusFailed, Required: true, Error: "upload refused",
			Bytes: 2048, SHA256: "0123abcd", Files: 7,
		},
		Error: "workspace export: upload refused",
	}
	raw, err := proto.Marshal(runTraceToProto(tr))
	must.NoError(err, "marshal")
	var decoded pb.RunTrace
	must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")

	g := decoded.GetGit()
	check.Equal("stirrup/run-handoff", g.GetBranch(), "git branch")
	check.Equal("abc123", g.GetSha(), "git sha")
	check.Equal("def456", g.GetBaseSha(), "git base sha")
	check.True(g.GetDirty(), "git dirty")
	check.Equal("dirty tree", g.GetError(), "git error")

	r := decoded.GetWorkspaceExport()
	check.Equal("gs://bucket/ws.tar.gz", r.GetUri(), "workspace export uri")
	check.Equal("failed", r.GetStatus(), "workspace export status")
	check.True(r.GetRequired(), "workspace export required")
	check.Equal("upload refused", r.GetError(), "workspace export error")
	check.EqualValues(2048, r.GetBytes(), "workspace export bytes")
	check.Equal("0123abcd", r.GetSha256(), "workspace export sha256")
	check.EqualValues(7, r.GetFiles(), "workspace export files")

	check.Equal("workspace export: upload refused", decoded.GetError(), "run error")

	bare := runTraceToProto(&types.RunTrace{Outcome: "success"})
	check.Nil(bare.Git, "bare summary has no git handoff")
	check.Nil(bare.WorkspaceExport, "bare summary has no workspace export")
	check.Empty(bare.Error, "bare summary has no error")
}

// TestRunTraceToProto_SummaryStaysSmall bounds the "done" trace summary
// with every string field at its cap: it carries no config or tool
// bodies, so it stays far below the gRPC message limit.
func TestRunTraceToProto_SummaryStaysSmall(t *testing.T) {
	must := require.New(t)
	capped := strings.Repeat("x", 1024+64)
	tr := &types.RunTrace{
		ID:          strings.Repeat("r", 128),
		Outcome:     "handoff_failed",
		StopDetails: &types.StopDetails{Type: "refusal", Category: "other", Explanation: capped},
		Git: &types.GitHandoff{
			Branch: strings.Repeat("b", 256), SHA: strings.Repeat("a", 64), BaseSHA: strings.Repeat("c", 64), Error: capped,
		},
		WorkspaceExport: &types.ExportReceipt{URI: strings.Repeat("u", 1024), Status: "failed", Required: true, Error: capped},
		Error:           capped,
	}
	size := proto.Size(runTraceToProto(tr))
	must.LessOrEqual(size, 16<<10, "summary size in bytes")
}

func TestRunTraceToProto_StructuredOutputRoundTrip(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	doc := `{"title":"Fix the flaky test","labels":["bug"],"risk":0.25}`
	raw, err := proto.Marshal(runTraceToProto(&types.RunTrace{
		Outcome:          "success",
		StructuredOutput: json.RawMessage(doc),
	}))
	must.NoError(err, "marshal")
	var decoded pb.RunTrace
	must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")
	check.Equal(doc, decoded.GetStructuredOutput(), "structured_output")

	bare := runTraceToProto(&types.RunTrace{Outcome: "success"})
	check.Empty(bare.StructuredOutput, "structured_output without a structured answer")
}

func TestRunTraceToProto_DegradedCapabilities(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	pt := runTraceToProto(&types.RunTrace{
		ID: "run-degraded",
		DegradedCapabilities: []types.DegradedCapability{
			{Kind: types.DegradedCapabilityMCP, Name: "docs", Error: "connection refused"},
		},
	})

	raw, err := proto.Marshal(pt)
	must.NoError(err, "marshal")
	var decoded pb.RunTrace
	must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")
	must.Len(decoded.DegradedCapabilities, 1)
	got := decoded.DegradedCapabilities[0]
	check.Equal("mcp", got.Kind, "degraded kind")
	check.Equal("docs", got.Name, "degraded name")
	check.Equal("connection refused", got.Error, "degraded error")

	empty := runTraceToProto(&types.RunTrace{ID: "run-clean"})
	check.Empty(empty.DegradedCapabilities, "a run with nothing degraded carries no entries")
}

// harnessEventWireRoundTrip translates e and decodes it from the wire, so a
// field the translation drops or the proto lacks reads back as zero.
func harnessEventWireRoundTrip(t *testing.T, e types.HarnessEvent) *pb.HarnessEvent {
	t.Helper()
	must := require.New(t)
	raw, err := proto.Marshal(harnessEventToProto(e))
	must.NoError(err, "marshal")
	var decoded pb.HarnessEvent
	must.NoError(proto.Unmarshal(raw, &decoded), "unmarshal")
	return &decoded
}

func TestHarnessEventToProto_LiveTranscriptFields(t *testing.T) {
	turn := func(n int) *int { return &n }

	t.Run("user_message", func(t *testing.T) {
		must := require.New(t)
		got := harnessEventWireRoundTrip(t, types.HarnessEvent{
			Type: "user_message", Text: "fix it", Source: types.UserMessageSourceFollowUpPrompt, RequestID: "req-9",
		})
		must.Equal("fix it", got.GetText(), "text")
		must.Equal("followup_prompt", got.GetSource(), "source")
		must.Equal("req-9", got.GetRequestId(), "request_id")
	})

	t.Run("turn_started keeps turn 0 present", func(t *testing.T) {
		must := require.New(t)
		got := harnessEventWireRoundTrip(t, types.HarnessEvent{
			Type: "turn_started", Turn: turn(0), Model: "claude-x", Provider: "anthropic",
		})
		must.NotNil(got.Turn, "turn 0 must stay present")
		must.Zero(got.GetTurn(), "turn")
		must.Equal("claude-x", got.GetModel(), "model")
		must.Equal("anthropic", got.GetProvider(), "provider")
	})

	t.Run("turn_committed", func(t *testing.T) {
		must := require.New(t)
		got := harnessEventWireRoundTrip(t, types.HarnessEvent{
			Type: "turn_committed", Turn: turn(3), StopReason: "tool_use", ToolResultCount: 2,
			Usage: &types.TokenUsage{Input: 100, Output: 20, CacheRead: 5, CacheWrite: 6, Reasoning: 7},
		})
		must.EqualValues(3, got.GetTurn(), "turn")
		must.Equal("tool_use", got.GetStopReason(), "stop_reason")
		must.EqualValues(2, got.GetToolResultCount(), "tool_result_count")

		u := got.GetUsage()
		must.EqualValues(100, u.GetInputTokens(), "input_tokens")
		must.EqualValues(20, u.GetOutputTokens(), "output_tokens")
		must.EqualValues(5, u.GetCacheReadTokens(), "cache_read_tokens")
		must.EqualValues(6, u.GetCacheWriteTokens(), "cache_write_tokens")
		must.EqualValues(7, u.GetReasoningTokens(), "reasoning_tokens")
	})

	t.Run("turn_abandoned", func(t *testing.T) {
		must := require.New(t)
		got := harnessEventWireRoundTrip(t, types.HarnessEvent{
			Type: "turn_abandoned", Turn: turn(0), AbandonReason: types.TurnAbandonedUnansweredToolCalls,
		})
		must.NotNil(got.Turn, "turn 0 must stay present")
		must.Zero(got.GetTurn(), "turn")
		must.Equal(types.TurnAbandonedUnansweredToolCalls, got.GetAbandonReason(), "abandon_reason")
	})

	t.Run("tools_loaded", func(t *testing.T) {
		must := require.New(t)
		got := harnessEventWireRoundTrip(t, types.HarnessEvent{
			Type: "tools_loaded", Turn: turn(0), Source: types.ToolsLoadedSourceNative, Name: "tool_search_tool_bm25",
			Input: json.RawMessage(`{"query":"github"}`), Tools: []string{"mcp_github_create_issue", "mcp_github_add_comment"},
		})
		must.Equal("tools_loaded", got.GetType(), "type")
		must.NotNil(got.Turn, "turn 0 must stay present")
		must.Zero(got.GetTurn(), "turn")
		must.Equal("native", got.GetSource(), "source")
		must.Equal("tool_search_tool_bm25", got.GetName(), "name")
		must.JSONEq(`{"query":"github"}`, string(got.GetInput()), "input")
		must.Equal([]string{"mcp_github_create_issue", "mcp_github_add_comment"}, got.GetTools(), "tools")
	})

	t.Run("tool_result", func(t *testing.T) {
		must := require.New(t)
		got := harnessEventWireRoundTrip(t, types.HarnessEvent{
			Type: "tool_result", ToolUseID: "tu-1", Name: "read_file", Content: "boom", IsError: true, DurationMs: 42,
		})
		must.Equal("tu-1", got.GetToolUseId(), "tool_use_id")
		must.Equal("read_file", got.GetName(), "name")
		must.True(got.GetIsError(), "is_error")
		must.EqualValues(42, got.GetDurationMs(), "duration_ms")
	})

	t.Run("unset turn and usage stay absent", func(t *testing.T) {
		must := require.New(t)
		got := harnessEventWireRoundTrip(t, types.HarnessEvent{Type: "text_delta", Text: "x"})
		must.Nil(got.Turn, "turn")
		must.Nil(got.Usage, "usage")
	})
}

func TestTraceReferenceToProto_RoundTrip(t *testing.T) {
	ref := &types.TraceReference{
		TraceID:   "4bf92f3577b34da6a3ce929d0e0e4736",
		RunSpanID: "00f067aa0ba902b7",
		Sampled:   true,
	}
	requireRef := func(t *testing.T, got *pb.TraceReference) {
		t.Helper()
		must := require.New(t)
		must.NotNil(got, "trace_reference")
		must.Equal(ref.TraceID, got.GetTraceId(), "trace_id")
		must.Equal(ref.RunSpanID, got.GetRunSpanId(), "run_span_id")
		must.Equal(ref.Sampled, got.GetSampled(), "sampled")
	}

	t.Run("run_started event", func(t *testing.T) {
		must := require.New(t)
		var decoded pb.HarnessEvent
		wireRoundTrip(t, harnessEventToProto(types.HarnessEvent{
			Type: "run_started", ID: "run-ref", TraceReference: ref,
		}), &decoded)
		must.Equal("run_started", decoded.GetType())
		must.Equal("run-ref", decoded.GetId())
		requireRef(t, decoded.GetTraceReference())
	})

	t.Run("done summary", func(t *testing.T) {
		var decoded pb.RunTrace
		wireRoundTrip(t, runTraceToProto(&types.RunTrace{Outcome: "success", TraceReference: ref}), &decoded)
		requireRef(t, decoded.GetTraceReference())
	})

	t.Run("unsampled reference keeps sampled false", func(t *testing.T) {
		must := require.New(t)
		got := traceReferenceToProto(&types.TraceReference{TraceID: ref.TraceID, RunSpanID: ref.RunSpanID})
		must.NotNil(got, "trace_reference")
		must.False(got.GetSampled(), "sampled")
	})

	t.Run("absent stays absent", func(t *testing.T) {
		must := require.New(t)
		pe := harnessEventToProto(types.HarnessEvent{Type: "text_delta"})
		must.Nil(pe.TraceReference, "event trace_reference")
		pt := runTraceToProto(&types.RunTrace{Outcome: "success"})
		must.Nil(pt.TraceReference, "summary trace_reference")
	})
}

func TestTraceContextToProto_ToolResultRequestCarriesTraceContext(t *testing.T) {
	must := require.New(t)
	carrier := &types.TraceContext{
		Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		Tracestate:  "vendor=opaque",
	}
	var decoded pb.HarnessEvent
	wireRoundTrip(t, harnessEventToProto(types.HarnessEvent{
		Type: "tool_result_request", RequestID: "req-1", ToolUseID: "tc_1", TraceContext: carrier,
	}), &decoded)
	got := decoded.GetTraceContext()
	must.Equal(carrier.Traceparent, got.GetTraceparent(), "traceparent")
	must.Equal(carrier.Tracestate, got.GetTracestate(), "tracestate")
	must.Equal("req-1", decoded.GetRequestId())
	must.Equal("tc_1", decoded.GetToolUseId())

	pe := harnessEventToProto(types.HarnessEvent{Type: "tool_result_request"})
	must.Nil(pe.TraceContext, "absent trace context")
}

func TestTraceContextFromProto_TaskAssignmentCarriesTraceContext(t *testing.T) {
	must := require.New(t)
	var decoded pb.ControlEvent
	wireRoundTrip(t, &pb.ControlEvent{
		Type: "task_assignment",
		Task: &pb.RunConfig{RunId: "run-parented"},
		TraceContext: &pb.TraceContext{
			Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			Tracestate:  "vendor=opaque",
		},
	}, &decoded)
	e := controlEventFromProto(&decoded)
	want := types.TraceContext{
		Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		Tracestate:  "vendor=opaque",
	}
	must.NotNil(e.TraceContext, "trace context")
	must.Equal(want, *e.TraceContext)
	must.NotNil(e.Task, "task")
	must.Equal("run-parented", e.Task.RunID)

	absent := controlEventFromProto(&pb.ControlEvent{Type: "task_assignment"})
	must.Nil(absent.TraceContext, "absent trace context")
}

func TestHarnessEventToProto_ReadyCapabilities(t *testing.T) {
	must := require.New(t)
	decoded := harnessEventWireRoundTrip(t, types.HarnessEvent{
		Type: "ready", HarnessVersion: "dev", Capabilities: types.HarnessCapabilities(),
	})
	want := types.HarnessCapabilities()
	must.Equal(want, decoded.GetCapabilities(), "capabilities")

	bare := harnessEventWireRoundTrip(t, types.HarnessEvent{Type: "ready"})
	must.Empty(bare.GetCapabilities(), "ready without capabilities")
}

func TestHarnessEventToProto_HeartbeatSessionPhase(t *testing.T) {
	must := require.New(t)
	decoded := harnessEventWireRoundTrip(t, types.HarnessEvent{
		Type:         "heartbeat",
		SessionPhase: &types.SessionPhase{Phase: types.SessionPhaseConstructing, Step: types.SessionStepSandboxReady},
	})
	must.Equal(types.SessionPhaseConstructing, decoded.GetSessionPhase().GetPhase())
	must.Equal(types.SessionStepSandboxReady, decoded.GetSessionPhase().GetStep())

	bare := harnessEventWireRoundTrip(t, types.HarnessEvent{Type: "heartbeat"})
	must.Nil(bare.GetSessionPhase(), "a run heartbeat carries no session phase")
}

func TestHarnessEventToProto_ErrorBuildFailure(t *testing.T) {
	must := require.New(t)
	decoded := harnessEventWireRoundTrip(t, types.HarnessEvent{
		Type:    "error",
		Message: "building harness: pod not ready",
		BuildFailure: &types.BuildFailure{
			Reason: types.BuildFailureSandboxImage,
			Phase:  types.SessionPhase{Phase: types.SessionPhaseConstructing, Step: types.SessionStepSandboxReady},
		},
	})
	must.Equal(types.BuildFailureSandboxImage, decoded.GetBuildFailure().GetReason())
	must.Equal(types.SessionPhaseConstructing, decoded.GetBuildFailure().GetPhase().GetPhase())
	must.Equal(types.SessionStepSandboxReady, decoded.GetBuildFailure().GetPhase().GetStep())

	bare := harnessEventWireRoundTrip(t, types.HarnessEvent{Type: "error", Message: "x"})
	must.Nil(bare.GetBuildFailure(), "an error from a run carries no build failure")
}

func TestHarnessEventToProto_ReadyConfigFieldsAndSemver(t *testing.T) {
	must := require.New(t)
	decoded := harnessEventWireRoundTrip(t, types.HarnessEvent{
		Type:          "ready",
		HarnessSemver: "0.1.0-rc.1",
		ConfigFields:  TaskAssignmentFieldPaths(),
	})
	must.Equal(TaskAssignmentFieldPaths(), decoded.GetConfigFields(), "config fields")
	must.Equal("0.1.0-rc.1", decoded.GetHarnessSemver(), "harness semver")

	bare := harnessEventWireRoundTrip(t, types.HarnessEvent{Type: "ready"})
	must.Empty(bare.GetConfigFields(), "ready without config fields")
	must.Empty(bare.GetHarnessSemver(), "ready without a semantic version")
}
