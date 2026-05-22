package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/types"
	tracereader "github.com/rxbynerd/stirrup/types/trace"
)

func writeTraceFile(t *testing.T, traces []types.RunTrace) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.jsonl")
	var buf bytes.Buffer
	for _, tr := range traces {
		data, err := json.Marshal(tr)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
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
		VerificationResults: []types.VerificationResult{{Passed: true, Feedback: "all green"}},
		Outcome:             "success",
	}}
}

func TestTraceShow_PrintsRecords(t *testing.T) {
	path := writeTraceFile(t, sampleTraces())

	var out bytes.Buffer
	if err := runTraceShowWith(path, &out, colorNever); err != nil {
		t.Fatalf("show: %v", err)
	}
	s := out.String()
	wants := []string{"run-1", "edit_file", "run_command", "success", "tokens", "3", "ok", "fail"}
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("show output missing %q\n--- output ---\n%s", w, s)
		}
	}
	if strings.Contains(s, "\x1b[") {
		t.Error("colorNever output must not contain ANSI escapes")
	}
}

func TestTraceShow_AlwaysEmitsAnsi(t *testing.T) {
	path := writeTraceFile(t, sampleTraces())
	var out bytes.Buffer
	if err := runTraceShowWith(path, &out, colorAlways); err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(out.String(), "\x1b[") {
		t.Error("colorAlways output must contain ANSI escapes")
	}
}

func TestTraceShow_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runTraceShowWith(path, &out, colorNever)
	if err == nil {
		t.Fatal("show on empty file: expected error")
	}
}

func TestTraceStats_JSONOutput(t *testing.T) {
	path := writeTraceFile(t, sampleTraces())
	var out bytes.Buffer
	if err := runTraceStatsWith(path, &out, "json", 5); err != nil {
		t.Fatalf("stats: %v", err)
	}

	var stats TraceStats
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}

	if stats.TotalTurns != 3 {
		t.Errorf("TotalTurns = %d, want 3", stats.TotalTurns)
	}
	if stats.TokensInput != 100 || stats.TokensOutput != 200 {
		t.Errorf("tokens = %d/%d, want 100/200", stats.TokensInput, stats.TokensOutput)
	}
	if stats.ToolCalls != 3 || stats.ToolErrors != 1 {
		t.Errorf("ToolCalls/Errors = %d/%d, want 3/1", stats.ToolCalls, stats.ToolErrors)
	}
	if stats.LongestToolCallMs != 500 {
		t.Errorf("LongestToolCallMs = %d, want 500", stats.LongestToolCallMs)
	}
	if stats.TotalWallClockMs != 5000 {
		t.Errorf("TotalWallClockMs = %d, want 5000", stats.TotalWallClockMs)
	}
	if stats.ToolCallsByName["edit_file"] != 2 {
		t.Errorf("edit_file count = %d, want 2", stats.ToolCallsByName["edit_file"])
	}
	if stats.Outcomes["success"] != 1 {
		t.Errorf("success count = %d, want 1", stats.Outcomes["success"])
	}
	if stats.RunID != "run-1" {
		t.Errorf("RunID = %q, want run-1", stats.RunID)
	}
	if len(stats.SlowestToolCalls) != 3 {
		t.Fatalf("SlowestToolCalls len = %d, want 3", len(stats.SlowestToolCalls))
	}
	if stats.SlowestToolCalls[0].DurationMs != 500 {
		t.Errorf("slowest top = %dms, want 500ms", stats.SlowestToolCalls[0].DurationMs)
	}
}

func TestTraceStats_TextOutput(t *testing.T) {
	path := writeTraceFile(t, sampleTraces())
	var out bytes.Buffer
	if err := runTraceStatsWith(path, &out, "text", 5); err != nil {
		t.Fatalf("stats text: %v", err)
	}
	s := out.String()
	for _, want := range []string{"trace stats", "run-1", "total turns:", "tokens in / out:", "edit_file", "outcomes:", "success"} {
		if !strings.Contains(s, want) {
			t.Errorf("text stats missing %q\n%s", want, s)
		}
	}
}

func TestTraceStats_InvalidFormat(t *testing.T) {
	path := writeTraceFile(t, sampleTraces())
	var out bytes.Buffer
	err := runTraceStatsWith(path, &out, "yaml", 5)
	if err == nil {
		t.Fatal("expected error for invalid format")
	}
}

func TestTraceGrep_SubstringMatch(t *testing.T) {
	path := writeTraceFile(t, sampleTraces())
	var out bytes.Buffer
	pred, _ := compileJQ("")
	if err := runTraceGrepWith(context.Background(), path, &out, "edit_file", pred, false); err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.Contains(out.String(), "edit_file") {
		t.Errorf("grep output missing match: %q", out.String())
	}

	out.Reset()
	if err := runTraceGrepWith(context.Background(), path, &out, "no_such_string", pred, false); err != nil {
		t.Fatalf("grep: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("grep miss should be empty, got %q", out.String())
	}
}

func TestTraceGrep_JQEqual(t *testing.T) {
	path := writeTraceFile(t, sampleTraces())
	pred, err := compileJQ(`.outcome == "success"`)
	if err != nil {
		t.Fatalf("compileJQ: %v", err)
	}
	var out bytes.Buffer
	if err := runTraceGrepWith(context.Background(), path, &out, "", pred, false); err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.Contains(out.String(), `"id":"run-1"`) {
		t.Errorf("grep output missing record: %q", out.String())
	}

	pred2, err := compileJQ(`.outcome == "max_turns"`)
	if err != nil {
		t.Fatalf("compileJQ: %v", err)
	}
	out.Reset()
	if err := runTraceGrepWith(context.Background(), path, &out, "", pred2, false); err != nil {
		t.Fatalf("grep: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("non-matching predicate should produce no output, got %q", out.String())
	}
}

func TestTraceGrep_JQContains(t *testing.T) {
	path := writeTraceFile(t, sampleTraces())
	pred, err := compileJQ(`.outcome contains "uc"`)
	if err != nil {
		t.Fatalf("compileJQ: %v", err)
	}
	var out bytes.Buffer
	if err := runTraceGrepWith(context.Background(), path, &out, "", pred, false); err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.Contains(out.String(), `"outcome":"success"`) {
		t.Errorf("contains should match: %q", out.String())
	}
}

func TestTraceGrep_JQPathIntoArray(t *testing.T) {
	path := writeTraceFile(t, sampleTraces())
	pred, err := compileJQ(`.toolCalls.0.name == "edit_file"`)
	if err != nil {
		t.Fatalf("compileJQ: %v", err)
	}
	var out bytes.Buffer
	if err := runTraceGrepWith(context.Background(), path, &out, "", pred, false); err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.Contains(out.String(), `"id":"run-1"`) {
		t.Errorf("array path should match: %q", out.String())
	}
}

func TestTraceGrep_Invert(t *testing.T) {
	path := writeTraceFile(t, sampleTraces())
	pred, err := compileJQ(`.outcome == "success"`)
	if err != nil {
		t.Fatalf("compileJQ: %v", err)
	}
	var out bytes.Buffer
	if err := runTraceGrepWith(context.Background(), path, &out, "", pred, true); err != nil {
		t.Fatalf("grep: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("inverted match of the only record should be empty, got %q", out.String())
	}
}

func TestCompileJQ_Errors(t *testing.T) {
	cases := []string{
		"no_dot == 1",
		`. notanop "x"`,
		`. == `,
		`. == unclosed"`,
	}
	for _, c := range cases {
		if _, err := compileJQ(c); err == nil {
			t.Errorf("expected error for %q", c)
		}
	}
}

func TestTraceGrep_StdinPath(t *testing.T) {
	traces := sampleTraces()
	data, _ := json.Marshal(traces[0])

	// Redirect os.Stdin to a temp file containing one record.
	dir := t.TempDir()
	tmp := filepath.Join(dir, "stdin.jsonl")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	origStdin := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = origStdin }()

	pred, _ := compileJQ(`.id == "run-1"`)
	var out bytes.Buffer
	if err := runTraceGrepWith(context.Background(), "-", &out, "", pred, false); err != nil {
		t.Fatalf("grep -: %v", err)
	}
	if !strings.Contains(out.String(), `"id":"run-1"`) {
		t.Errorf("stdin grep missing match: %q", out.String())
	}
}

func TestParseColorMode(t *testing.T) {
	cases := []struct {
		in        string
		wantMode  colorMode
		wantError bool
	}{
		{"", colorAuto, false},
		{"auto", colorAuto, false},
		{"AUTO", colorAuto, false},
		{"always", colorAlways, false},
		{"force", colorAlways, false},
		{"yes", colorAlways, false},
		{"never", colorNever, false},
		{"no", colorNever, false},
		{"off", colorNever, false},
		{"rainbow", colorAuto, true},
		{"  ", colorAuto, true},
	}
	for _, c := range cases {
		got, err := parseColorMode(c.in)
		if c.wantError {
			if err == nil {
				t.Errorf("parseColorMode(%q): expected error, got nil", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseColorMode(%q): unexpected error %v", c.in, err)
		}
		if got != c.wantMode {
			t.Errorf("parseColorMode(%q) = %v, want %v", c.in, got, c.wantMode)
		}
	}
}

func TestShouldColor_NoColorEnvVar(t *testing.T) {
	// Per https://no-color.org/, any non-empty NO_COLOR must suppress
	// ANSI output under auto-mode. Use a pipe (one end is an *os.File
	// that IsTerminal returns true for is impossible to fabricate in
	// a test without a real TTY) — but the NO_COLOR check sits before
	// the TTY check, so even an actual TTY-looking file goes false.
	t.Setenv("NO_COLOR", "1")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	if shouldColor(colorAuto, w) {
		t.Error("shouldColor(colorAuto) with NO_COLOR=1 must be false")
	}
	// colorAlways must still win over NO_COLOR — an explicit opt-in
	// from the operator is not overridden by ambient env.
	if !shouldColor(colorAlways, w) {
		t.Error("shouldColor(colorAlways) must remain true even with NO_COLOR set")
	}
}

func TestRunTraceGrepWith_OversizedLineSkipped(t *testing.T) {
	// Write a valid record, then a line larger than MaxLineBytes, then
	// a second valid record. The oversized line must be silently
	// skipped and BOTH valid records must appear in the output.
	traces := sampleTraces()
	first, _ := json.Marshal(traces[0])
	second := types.RunTrace{ID: "run-2", Outcome: "success"}
	secondBytes, _ := json.Marshal(second)

	oversized := bytes.Repeat([]byte("x"), tracereader.MaxLineBytes+128)

	dir := t.TempDir()
	path := filepath.Join(dir, "oversized.jsonl")
	var buf bytes.Buffer
	buf.Write(first)
	buf.WriteByte('\n')
	buf.Write(oversized)
	buf.WriteByte('\n')
	buf.Write(secondBytes)
	buf.WriteByte('\n')
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	pred, _ := compileJQ("")
	var out bytes.Buffer
	if err := runTraceGrepWith(context.Background(), path, &out, "", pred, false); err != nil {
		t.Fatalf("grep oversized: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, `"id":"run-1"`) {
		t.Errorf("first record missing from output: %q", got)
	}
	if !strings.Contains(got, `"id":"run-2"`) {
		t.Errorf("second record (after oversized line) missing — grep dropped records after the cap: %q", got)
	}
}

func TestTraceTail_OneShot(t *testing.T) {
	path := writeTraceFile(t, sampleTraces())
	var out bytes.Buffer
	if err := runTraceTailWith(context.Background(), path, &out, colorNever, false, 10*time.Millisecond); err != nil {
		t.Fatalf("tail: %v", err)
	}
	if !strings.Contains(out.String(), "run-1") {
		t.Errorf("tail output missing record: %q", out.String())
	}
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

func TestTraceTail_FollowReadsAppended(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.jsonl")
	initial := sampleTraces()
	data, _ := json.Marshal(initial[0])
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out := &safeBuf{}
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- runTraceTailWith(ctx, path, out, colorNever, true, 10*time.Millisecond)
	}()

	waitFor := func(needle string, deadline time.Duration) bool {
		t.Helper()
		end := time.Now().Add(deadline)
		for time.Now().Before(end) {
			if strings.Contains(out.String(), needle) {
				return true
			}
			time.Sleep(20 * time.Millisecond)
		}
		return false
	}

	if !waitFor("run-1", 2*time.Second) {
		cancel()
		t.Fatalf("tail did not surface initial record; got %q", out.String())
	}

	// Append a second record while tail is following.
	second := types.RunTrace{ID: "run-2", Turns: 1}
	data2, _ := json.Marshal(second)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(data2, '\n')); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	if !waitFor("run-2", 2*time.Second) {
		cancel()
		t.Fatalf("tail did not surface appended record; got %q", out.String())
	}

	cancel()
	select {
	case err := <-doneCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("tail returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tail did not exit after cancel")
	}
}
