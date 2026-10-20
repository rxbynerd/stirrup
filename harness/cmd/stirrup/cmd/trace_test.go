package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/types"
	tracereader "github.com/rxbynerd/stirrup/types/trace"
)

func writeTraceFile(t *testing.T, traces []types.RunTrace) string {
	t.Helper()
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	must.NoError(os.WriteFile(path, traceJSONL(t, traces), 0o644))
	return path
}

// traceJSONL encodes each trace as one line of JSON.
func traceJSONL(t *testing.T, traces []types.RunTrace) []byte {
	t.Helper()
	must := require.New(t)
	var buf bytes.Buffer
	for _, tr := range traces {
		data, err := json.Marshal(tr)
		must.NoError(err)
		buf.Write(data)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

func sampleTraces() []types.RunTrace {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Second)
	return []types.RunTrace{{
		ID:          "run-1",
		StartedAt:   start,
		CompletedAt: end,
		Turns:       3,
		TokenUsage:  types.TokenUsage{Input: 100, Output: 200},
		ToolCalls: []types.ToolCallSummary{
			{Name: "edit_file", DurationMs: 50, Success: true, InputSize: 64, OutputSize: 12},
			{Name: "run_command", DurationMs: 500, Success: false, ErrorReason: "exit 1"},
			{Name: "edit_file", DurationMs: 25, Success: true},
		},
		PermissionDenials:   2,
		VerificationResults: []types.VerificationResult{{Passed: true, Feedback: "all green"}},
		Outcome:             "success",
	}}
}

// writeEmitterTrace writes a trace file through the production JSONL
// emitter: a run_started event followed by run_finished, with the
// verifier attempts recorded the way the loop records them.
func writeEmitterTrace(t *testing.T, outcome string, results []types.VerificationResult) string {
	t.Helper()
	must := require.New(t)
	var buf bytes.Buffer
	emitter := trace.NewJSONLTraceEmitter(&buf, false)
	emitter.Start("run-verify", &types.RunConfig{RunID: "run-verify"})
	for _, r := range results {
		emitter.RecordVerification(r)
	}
	_, err := emitter.Finish(context.Background(), outcome)
	must.NoError(err)
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	must.NoError(os.WriteFile(path, buf.Bytes(), 0o644))
	return path
}

func emitterVerificationAttempts() []types.VerificationResult {
	return []types.VerificationResult{
		{Attempt: 1, Feedback: "Test command \"go test ./...\" failed (exit code 1):\n--- FAIL: TestAdd"},
		{Attempt: 2, Error: "test-runner verifier: exec failed: connection refused"},
	}
}

func TestTraceShow_RendersEmitterVerificationAttempts(t *testing.T) {
	must := require.New(t)
	path := writeEmitterTrace(t, "verification_error", emitterVerificationAttempts())

	var out bytes.Buffer
	must.NoError(runTraceShowWith(path, &out, colorNever))

	for _, tc := range []struct {
		name string
		want string
	}{
		{"verification header", "verification:"},
		{"failed attempt with feedback", "  1. FAIL  Test command \"go test ./...\" failed (exit code 1):\n          --- FAIL: TestAdd"},
		{"attempt with verifier error", "  2. FAIL  err=test-runner verifier: exec failed: connection refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Contains(out.String(), tc.want)
		})
	}
}

func TestTraceStats_CountsEmitterVerificationAttempts(t *testing.T) {
	must := require.New(t)
	path := writeEmitterTrace(t, "verification_error", emitterVerificationAttempts())

	var text bytes.Buffer
	must.NoError(runTraceStatsWith(path, &text, "text", 10))
	must.Contains(text.String(), "verifications:    2 run (passed: 0, failed: 2, verifier errors: 1)")

	var js bytes.Buffer
	must.NoError(runTraceStatsWith(path, &js, "json", 10))
	var stats TraceStats
	must.NoError(json.Unmarshal(js.Bytes(), &stats))
	must.Equal(2, stats.VerificationsRun)
	must.Equal(2, stats.VerificationsFailed)
	must.Equal(1, stats.VerificationErrors)
}

func TestTraceShow_PrintsRecords(t *testing.T) {
	must := require.New(t)
	path := writeTraceFile(t, sampleTraces())

	var out bytes.Buffer
	must.NoError(runTraceShowWith(path, &out, colorNever))
	for _, want := range []string{"run-1", "edit_file", "run_command", "success", "tokens", "3", "ok", "fail"} {
		must.Contains(out.String(), want)
	}
	must.NotContains(out.String(), "\x1b[", "colorNever output must not contain ANSI escapes")
}

func TestTraceShow_TokenBreakdown(t *testing.T) {
	traces := sampleTraces()

	t.Run("bare counts without breakdown", func(t *testing.T) {
		must := require.New(t)
		var out bytes.Buffer
		must.NoError(renderRunTrace(&out, &traces[0], false))
		must.Contains(out.String(), "tokens:   in=100 out=200\n")
	})

	t.Run("breakdown with cache and reasoning", func(t *testing.T) {
		must := require.New(t)
		traces[0].TokenUsage = types.TokenUsage{Input: 1234, Output: 56, CacheRead: 1000, Reasoning: 12}
		var out bytes.Buffer
		must.NoError(renderRunTrace(&out, &traces[0], false))
		must.Contains(out.String(), "tokens:   in=1234 (cache read 1000) out=56 (reasoning 12)\n")
	})
}

// TestToolStatus_RendersErrorCategory pins that a failed tool call
// carrying an ErrorCategory renders it inline (e.g. "fail
// (unknown_tool)"), while a failure without a category and a success
// keep their bare renderings.
func TestToolStatus_RendersErrorCategory(t *testing.T) {
	cases := []struct {
		name string
		tc   types.ToolCallSummary
		want string
	}{
		{"failure with category", types.ToolCallSummary{Success: false, ErrorCategory: "unknown_tool"}, "fail (unknown_tool)"},
		{"failure without category", types.ToolCallSummary{Success: false}, "fail"},
		{"success ignores category", types.ToolCallSummary{Success: true, ErrorCategory: "unknown_tool"}, "ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, toolStatus(tc.tc, false))
		})
	}
}

// TestTraceShow_RendersErrorCategoryEndToEnd drives the full trace show
// path on a trace whose failed call carries a category, confirming the
// category reaches the operator-facing output (not just the unit-tested
// helper).
func TestTraceShow_RendersErrorCategoryEndToEnd(t *testing.T) {
	must := require.New(t)
	traces := sampleTraces()
	traces[0].ToolCalls[1].ErrorCategory = "permission_denied"
	path := writeTraceFile(t, traces)

	var out bytes.Buffer
	must.NoError(runTraceShowWith(path, &out, colorNever))
	must.Contains(out.String(), "fail (permission_denied)")
}

func TestTraceShow_AlwaysEmitsAnsi(t *testing.T) {
	must := require.New(t)
	path := writeTraceFile(t, sampleTraces())
	var out bytes.Buffer
	must.NoError(runTraceShowWith(path, &out, colorAlways))
	must.Contains(out.String(), "\x1b[", "colorAlways output must contain ANSI escapes")
}

func TestTraceShow_EmptyFileReturnsError(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "empty.jsonl")
	must.NoError(os.WriteFile(path, nil, 0o644))
	var out bytes.Buffer
	must.Error(runTraceShowWith(path, &out, colorNever))
}

func TestTraceStats_JSONOutput(t *testing.T) {
	must := require.New(t)
	path := writeTraceFile(t, sampleTraces())
	var out bytes.Buffer
	must.NoError(runTraceStatsWith(path, &out, "json", 5))

	var stats TraceStats
	must.NoError(json.Unmarshal(bytes.TrimSpace(out.Bytes()), &stats))
	must.Contains(out.String(), `"permissionDenials":2`)

	must.Equal(3, stats.TotalTurns)
	must.Equal(100, stats.TokensInput)
	must.Equal(200, stats.TokensOutput)
	must.Equal(3, stats.ToolCalls)
	must.Equal(1, stats.ToolErrors)
	must.Equal(2, stats.PermissionDenials)
	must.Equal(int64(500), stats.LongestToolCallMs)
	must.Equal(int64(5000), stats.TotalWallClockMs)
	must.Equal(2, stats.ToolCallsByName["edit_file"])
	must.Equal(1, stats.Outcomes["success"])
	must.Equal("run-1", stats.RunID)
	must.Len(stats.SlowestToolCalls, 3)
	must.Equal(int64(500), stats.SlowestToolCalls[0].DurationMs)
}

func TestTraceStats_TextOutput(t *testing.T) {
	must := require.New(t)
	path := writeTraceFile(t, sampleTraces())
	var out bytes.Buffer
	must.NoError(runTraceStatsWith(path, &out, "text", 5))
	for _, want := range []string{"trace stats", "run-1", "total turns:", "tokens in / out:", "permission denials:", "2", "edit_file", "outcomes:", "success"} {
		must.Contains(out.String(), want)
	}
}

func TestTraceStats_TokenBreakdown(t *testing.T) {
	must := require.New(t)
	traces := sampleTraces()
	traces[0].TokenUsage = types.TokenUsage{Input: 1234, Output: 56, CacheRead: 1000, CacheWrite: 20, Reasoning: 12}
	path := writeTraceFile(t, traces)

	var jsonOut bytes.Buffer
	must.NoError(runTraceStatsWith(path, &jsonOut, "json", 5))
	var stats TraceStats
	must.NoError(json.Unmarshal(bytes.TrimSpace(jsonOut.Bytes()), &stats))
	must.Equal(1000, stats.TokensCacheRead)
	must.Equal(20, stats.TokensCacheWrite)
	must.Equal(12, stats.TokensReasoning)

	var textOut bytes.Buffer
	must.NoError(runTraceStatsWith(path, &textOut, "text", 5))
	must.Contains(textOut.String(), "tokens in / out:  1234 (cache read 1000, cache write 20) / 56 (reasoning 12)\n")
}

func TestTraceStats_OmitsZeroTokenBreakdown(t *testing.T) {
	must := require.New(t)
	path := writeTraceFile(t, sampleTraces())
	var jsonOut bytes.Buffer
	must.NoError(runTraceStatsWith(path, &jsonOut, "json", 5))
	for _, key := range []string{"tokensCacheRead", "tokensCacheWrite", "tokensReasoning"} {
		must.NotContains(jsonOut.String(), key, "a run without breakdown must omit %s", key)
	}

	var textOut bytes.Buffer
	must.NoError(runTraceStatsWith(path, &textOut, "text", 5))
	must.Contains(textOut.String(), "tokens in / out:  100 / 200\n")
}

func TestTraceStats_RejectsUnknownFormat(t *testing.T) {
	must := require.New(t)
	path := writeTraceFile(t, sampleTraces())
	var out bytes.Buffer
	must.Error(runTraceStatsWith(path, &out, "yaml", 5))
}

func TestTraceGrep_SubstringMatch(t *testing.T) {
	must := require.New(t)
	path := writeTraceFile(t, sampleTraces())
	pred, err := compileJQ("")
	must.NoError(err)

	var out bytes.Buffer
	must.NoError(runTraceGrepWith(context.Background(), path, &out, "edit_file", pred, false))
	must.Contains(out.String(), "edit_file")

	out.Reset()
	must.NoError(runTraceGrepWith(context.Background(), path, &out, "no_such_string", pred, false))
	must.Empty(out.String(), "a substring miss must produce no output")
}

func TestTraceGrep_JQEqual(t *testing.T) {
	path := writeTraceFile(t, sampleTraces())

	t.Run("matching predicate keeps record", func(t *testing.T) {
		must := require.New(t)
		pred, err := compileJQ(`.outcome == "success"`)
		must.NoError(err)
		var out bytes.Buffer
		must.NoError(runTraceGrepWith(context.Background(), path, &out, "", pred, false))
		must.Contains(out.String(), `"id":"run-1"`)
	})

	t.Run("non-matching predicate drops record", func(t *testing.T) {
		must := require.New(t)
		pred, err := compileJQ(`.outcome == "max_turns"`)
		must.NoError(err)
		var out bytes.Buffer
		must.NoError(runTraceGrepWith(context.Background(), path, &out, "", pred, false))
		must.Empty(out.String())
	})
}

func TestTraceGrep_JQContains(t *testing.T) {
	must := require.New(t)
	path := writeTraceFile(t, sampleTraces())
	pred, err := compileJQ(`.outcome contains "uc"`)
	must.NoError(err)

	var out bytes.Buffer
	must.NoError(runTraceGrepWith(context.Background(), path, &out, "", pred, false))
	must.Contains(out.String(), `"outcome":"success"`)
}

func TestTraceGrep_JQPathIntoArray(t *testing.T) {
	must := require.New(t)
	path := writeTraceFile(t, sampleTraces())
	pred, err := compileJQ(`.toolCalls.0.name == "edit_file"`)
	must.NoError(err)

	var out bytes.Buffer
	must.NoError(runTraceGrepWith(context.Background(), path, &out, "", pred, false))
	must.Contains(out.String(), `"id":"run-1"`)
}

func TestTraceGrep_JQNotEqual(t *testing.T) {
	must := require.New(t)
	traces := []types.RunTrace{
		{ID: "run-1", Outcome: "success"},
		{ID: "run-2", Outcome: "error"},
		{ID: "run-3", Outcome: "max_turns"},
	}
	path := writeTraceFile(t, traces)

	pred, err := compileJQ(`.outcome != "success"`)
	must.NoError(err)
	var out bytes.Buffer
	must.NoError(runTraceGrepWith(context.Background(), path, &out, "", pred, false))
	got := out.String()
	must.NotContains(got, `"id":"run-1"`, "!= predicate must drop success record")
	must.Contains(got, `"id":"run-2"`, "!= predicate must keep non-success records")
	must.Contains(got, `"id":"run-3"`, "!= predicate must keep non-success records")
}

func TestTraceGrep_Invert(t *testing.T) {
	must := require.New(t)
	path := writeTraceFile(t, sampleTraces())
	pred, err := compileJQ(`.outcome == "success"`)
	must.NoError(err)

	var out bytes.Buffer
	must.NoError(runTraceGrepWith(context.Background(), path, &out, "", pred, true))
	must.Empty(out.String(), "inverted match of the only record must be empty")
}

func TestCompileJQ_RejectsMalformedExpressions(t *testing.T) {
	must := require.New(t)
	for _, expr := range []string{
		"no_dot == 1",
		`. notanop "x"`,
		`. == `,
		`. == unclosed"`,
	} {
		_, err := compileJQ(expr)
		must.Error(err, "expression %q", expr)
	}
}

// withStdinFromTraces swaps os.Stdin for a temp file containing the
// JSONL-encoded traces and restores the original on test cleanup.
// The subcommand stdin paths all go through tracereader.Open("-"),
// which reads from os.Stdin directly.
func withStdinFromTraces(t *testing.T, traces []types.RunTrace) {
	t.Helper()
	must := require.New(t)
	tmp := filepath.Join(t.TempDir(), "stdin.jsonl")
	must.NoError(os.WriteFile(tmp, traceJSONL(t, traces), 0o644))
	f, err := os.Open(tmp)
	must.NoError(err)
	origStdin := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = origStdin
		_ = f.Close()
	})
}

func TestTraceGrep_StdinPath(t *testing.T) {
	must := require.New(t)
	withStdinFromTraces(t, sampleTraces())

	pred, err := compileJQ(`.id == "run-1"`)
	must.NoError(err)
	var out bytes.Buffer
	must.NoError(runTraceGrepWith(context.Background(), "-", &out, "", pred, false))
	must.Contains(out.String(), `"id":"run-1"`)
}

func TestTraceShow_ReadsStdinPath(t *testing.T) {
	must := require.New(t)
	withStdinFromTraces(t, sampleTraces())

	var out bytes.Buffer
	must.NoError(runTraceShowWith("-", &out, colorNever))
	must.Contains(out.String(), "run-1")
}

func TestTraceStats_ReadsStdinPath(t *testing.T) {
	must := require.New(t)
	withStdinFromTraces(t, sampleTraces())

	var out bytes.Buffer
	must.NoError(runTraceStatsWith("-", &out, "json", 5))
	var stats TraceStats
	must.NoError(json.Unmarshal(bytes.TrimSpace(out.Bytes()), &stats))
	must.Equal("run-1", stats.RunID)
	must.Equal(3, stats.TotalTurns)
}

func TestTraceTail_ReadsStdinPath(t *testing.T) {
	must := require.New(t)
	withStdinFromTraces(t, sampleTraces())

	var out bytes.Buffer
	must.NoError(runTraceTailWith(context.Background(), "-", &out, colorNever, false, 10*time.Millisecond))
	must.Contains(out.String(), "run-1")
}

func TestParseColorMode_AcceptsAliasesAndRejectsUnknown(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantMode  colorMode
		wantError bool
	}{
		{"empty input defaults to auto", "", colorAuto, false},
		{"auto", "auto", colorAuto, false},
		{"auto in upper case", "AUTO", colorAuto, false},
		{"always", "always", colorAlways, false},
		{"force is an always alias", "force", colorAlways, false},
		{"yes is an always alias", "yes", colorAlways, false},
		{"never", "never", colorNever, false},
		{"no is a never alias", "no", colorNever, false},
		{"off is a never alias", "off", colorNever, false},
		{"unknown word", "rainbow", colorAuto, true},
		{"whitespace only", "  ", colorAuto, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got, err := parseColorMode(tc.in)
			if tc.wantError {
				must.Error(err)
				return
			}
			must.NoError(err)
			must.Equal(tc.wantMode, got)
		})
	}
}

// NO_COLOR suppresses ANSI output in auto mode (https://no-color.org/),
// but an explicit colorAlways still produces it.
func TestShouldColor_NoColorEnvVar(t *testing.T) {
	must := require.New(t)
	t.Setenv("NO_COLOR", "1")
	r, w, err := os.Pipe()
	must.NoError(err)
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	must.False(shouldColor(colorAuto, w), "shouldColor(colorAuto) with NO_COLOR=1 must be false")
	must.True(shouldColor(colorAlways, w), "shouldColor(colorAlways) must remain true even with NO_COLOR set")
}

func TestTraceGrep_SkipsOversizedLine(t *testing.T) {
	must := require.New(t)
	// Write a valid record, then a line larger than MaxLineBytes, then
	// a second valid record. The oversized line must be silently
	// skipped and BOTH valid records must appear in the output.
	first, err := json.Marshal(sampleTraces()[0])
	must.NoError(err)
	secondBytes, err := json.Marshal(types.RunTrace{ID: "run-2", Outcome: "success"})
	must.NoError(err)

	oversized := bytes.Repeat([]byte("x"), tracereader.MaxLineBytes+128)

	path := filepath.Join(t.TempDir(), "oversized.jsonl")
	var buf bytes.Buffer
	buf.Write(first)
	buf.WriteByte('\n')
	buf.Write(oversized)
	buf.WriteByte('\n')
	buf.Write(secondBytes)
	buf.WriteByte('\n')
	must.NoError(os.WriteFile(path, buf.Bytes(), 0o644))

	pred, err := compileJQ("")
	must.NoError(err)
	var out bytes.Buffer
	must.NoError(runTraceGrepWith(context.Background(), path, &out, "", pred, false))
	must.Contains(out.String(), `"id":"run-1"`, "first record must precede the oversized line")
	must.Contains(out.String(), `"id":"run-2"`, "record after the oversized line must still be emitted")
}

func TestTraceTail_OneShot(t *testing.T) {
	must := require.New(t)
	path := writeTraceFile(t, sampleTraces())
	var out bytes.Buffer
	must.NoError(runTraceTailWith(context.Background(), path, &out, colorNever, false, 10*time.Millisecond))
	must.Contains(out.String(), "run-1")
}

// safeBuf is a tiny synchronised buffer so the tail goroutine can write
// to a sink the test goroutine reads from without racing on bytes.Buffer.
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestOutcomeColor_MapsOutcomeToColor(t *testing.T) {
	cases := []struct {
		name    string
		outcome string
		want    string
	}{
		{"success", "success", ansiGreen},
		{"error", "error", ansiRed},
		{"verification failed", "verification_failed", ansiRed},
		{"verification error", "verification_error", ansiRed},
		{"tool failures", "tool_failures", ansiRed},
		{"max turns", "max_turns", ansiYellow},
		{"max tokens", "max_tokens", ansiYellow},
		{"budget exceeded", "budget_exceeded", ansiYellow},
		{"stalled", "stalled", ansiYellow},
		{"timeout", "timeout", ansiYellow},
		{"context exhausted", "context_exhausted", ansiYellow},
		{"cancelled", "cancelled", ansiGrey},
		{"empty outcome", "", ansiBlue},
		{"unknown outcome", "some_unknown_outcome", ansiBlue},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(c.want, outcomeColor(c.outcome))
		})
	}
}

func TestParseJQValue_ParsesLiteralsAndRejectsInvalid(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		want      any
		wantError bool
	}{
		{"double-quoted string", `"hello"`, "hello", false},
		{"escaped quotes", `"with \"quotes\""`, `with "quotes"`, false},
		{"true", "true", true, false},
		{"false", "false", false, false},
		{"null", "null", nil, false},
		{"integer", "42", float64(42), false},
		{"decimal", "3.14", float64(3.14), false},
		{"negative integer", "-7", float64(-7), false},
		{"empty input", "", nil, true},
		{"unclosed string", `"unclosed`, nil, true},
		{"bare word", "not_a_value", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			must := require.New(t)
			got, err := parseJQValue(c.in)
			if c.wantError {
				must.Error(err)
				return
			}
			must.NoError(err)
			must.Equal(c.want, got)
		})
	}
}

func TestWalkPath_ResolvesObjectAndArrayPaths(t *testing.T) {
	doc := map[string]any{
		"name": "stirrup",
		"toolCalls": []any{
			map[string]any{"name": "edit_file"},
			map[string]any{"name": "run_command"},
		},
		"count": float64(3),
	}

	cases := []struct {
		name string
		path []pathSeg
		want any
		ok   bool
	}{
		{
			name: "object key",
			path: []pathSeg{{key: "name"}},
			want: "stirrup", ok: true,
		},
		{
			name: "array indexed element",
			path: []pathSeg{{key: "toolCalls"}, {key: "0", idx: 0, num: true}, {key: "name"}},
			want: "edit_file", ok: true,
		},
		{
			name: "non-numeric segment on array",
			path: []pathSeg{{key: "toolCalls"}, {key: "first"}},
			ok:   false,
		},
		{
			name: "out-of-bounds index",
			path: []pathSeg{{key: "toolCalls"}, {key: "9", idx: 9, num: true}},
			ok:   false,
		},
		{
			name: "path into scalar",
			path: []pathSeg{{key: "name"}, {key: "any"}},
			ok:   false,
		},
		{
			name: "missing object key",
			path: []pathSeg{{key: "missing"}},
			ok:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			must := require.New(t)
			got, ok := walkPath(doc, c.path)
			must.Equal(c.ok, ok)
			if !c.ok {
				return
			}
			must.Equal(c.want, got)
		})
	}
}

func TestRunTraceGrepWith_JQNumericAndBool(t *testing.T) {
	// Numeric and boolean jq comparisons match the JSON values of the same type.
	traces := []types.RunTrace{
		{ID: "run-1", Turns: 3, Outcome: "success", ToolCalls: []types.ToolCallSummary{{Name: "edit_file", Success: true}}},
		{ID: "run-2", Turns: 0, Outcome: "max_turns", ToolCalls: []types.ToolCallSummary{{Name: "run_command", Success: false}}},
	}
	path := writeTraceFile(t, traces)

	t.Run("numeric equality", func(t *testing.T) {
		must := require.New(t)
		pred, err := compileJQ(".turns == 3")
		must.NoError(err)
		var out bytes.Buffer
		must.NoError(runTraceGrepWith(context.Background(), path, &out, "", pred, false))
		must.Contains(out.String(), `"id":"run-1"`)
		must.NotContains(out.String(), `"id":"run-2"`)
	})

	t.Run("boolean equality", func(t *testing.T) {
		must := require.New(t)
		pred, err := compileJQ(".toolCalls.0.success == true")
		must.NoError(err)
		var out bytes.Buffer
		must.NoError(runTraceGrepWith(context.Background(), path, &out, "", pred, false))
		must.Contains(out.String(), `"id":"run-1"`)
		must.NotContains(out.String(), `"id":"run-2"`)
	})
}

func TestRunTraceStatsWith_PrefersConfigRunID(t *testing.T) {
	must := require.New(t)
	// The stats run ID comes from RunConfig.RunID when it is set, not from RunTrace.ID.
	traces := []types.RunTrace{{
		ID:        "event-1",
		Config:    types.RunConfig{RunID: "operator-supplied-id"},
		Outcome:   "success",
		StartedAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
	}}
	path := writeTraceFile(t, traces)
	var out bytes.Buffer
	must.NoError(runTraceStatsWith(path, &out, "json", 5))

	var stats TraceStats
	must.NoError(json.Unmarshal(bytes.TrimSpace(out.Bytes()), &stats))
	must.Equal("operator-supplied-id", stats.RunID)
}

func TestTraceTail_FollowReadsAppended(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "live.jsonl")
	initial := sampleTraces()
	data, _ := json.Marshal(initial[0])
	must.NoError(os.WriteFile(path, append(data, '\n'), 0o644))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out := &safeBuf{}
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- runTraceTailWith(ctx, path, out, colorNever, true, 10*time.Millisecond)
	}()

	must.Eventually(func() bool {
		return strings.Contains(out.String(), "run-1")
	}, 2*time.Second, 20*time.Millisecond, "tail did not surface the initial record")

	// Append a second record while tail is following.
	second := types.RunTrace{ID: "run-2", Turns: 1}
	data2, _ := json.Marshal(second)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	must.NoError(err)
	_, err = f.Write(append(data2, '\n'))
	must.NoError(err)
	_ = f.Close()

	must.Eventually(func() bool {
		return strings.Contains(out.String(), "run-2")
	}, 2*time.Second, 20*time.Millisecond, "tail did not surface the appended record")

	cancel()
	select {
	case err := <-doneCh:
		if err != nil {
			must.ErrorIs(err, context.Canceled)
		}
	case <-time.After(2 * time.Second):
		must.FailNow("tail did not exit after cancel")
	}
}
