package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/stirrup/types"
	tracereader "github.com/rxbynerd/stirrup/types/trace"
	"github.com/rxbynerd/stirrup/types/version"
)

var traceStatsCmd = &cobra.Command{
	Use:   "stats <file>",
	Short: "Aggregate token, tool-call, and outcome counters from a trace",
	Long: `Aggregate the records in a JSONL trace file into a compact summary:
total turns, total token usage, tool-call counts by tool, the top-N
slowest tool calls, the longest tool call duration, the overall
wall-clock duration, and per-outcome counts.

Pass ` + "`-`" + ` to read from stdin. Use --output=json for a
machine-readable line that downstream tooling can pipe through jq.

Stats output includes the trace's runId and the version of the
stirrup binary computing the stats, so an operator collecting
aggregates across multiple traces can correlate the schema across
harness versions.`,
	Args: cobra.ExactArgs(1),
	RunE: runTraceStats,
}

func init() {
	traceCmd.AddCommand(traceStatsCmd)
	f := traceStatsCmd.Flags()
	f.String("output", "text", "Output format: text|json. JSON emits a single line; text is human-readable.")
	f.Int("top", 10, "Number of slowest tool calls to list in the text output. Has no effect on JSON output, which carries the full list.")
}

func runTraceStats(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	format, _ := cmd.Flags().GetString("output")
	top, _ := cmd.Flags().GetInt("top")
	return runTraceStatsWith(args[0], out, format, top)
}

func runTraceStatsWith(path string, out io.Writer, format string, top int) error {
	r, err := tracereader.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()

	stats := newTraceStats()
	stats.HarnessVersion = version.Version()

	for {
		t, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		stats.absorb(t)
	}
	if stats.Records == 0 {
		return fmt.Errorf("trace file %q contained no well-formed records", path)
	}
	stats.finalize()

	switch strings.ToLower(format) {
	case "", "text":
		return writeStatsText(out, stats, top)
	case "json":
		enc := json.NewEncoder(out)
		return enc.Encode(stats)
	default:
		return fmt.Errorf("invalid --output value %q (want text|json)", format)
	}
}

// TraceStats is the aggregate produced by `stirrup trace stats`. The
// metric names mirror the OTel counters in
// harness/internal/observability/metrics.go so an operator switching
// between online dashboards and offline trace inspection uses one
// vocabulary.
type TraceStats struct {
	RunID                string             `json:"runId,omitempty"`
	HarnessVersion       string             `json:"harnessVersion,omitempty"`
	Records              int                `json:"records"`
	TotalTurns           int                `json:"totalTurns"`
	TokensInput          int                `json:"tokensInput"`
	TokensOutput         int                `json:"tokensOutput"`
	ToolCalls            int                `json:"toolCalls"`
	ToolErrors           int                `json:"toolErrors"`
	ToolCallsByName      map[string]int     `json:"toolCallsByName,omitempty"`
	ToolErrorsByName     map[string]int     `json:"toolErrorsByName,omitempty"`
	SubAgentToolCalls    int                `json:"subAgentToolCalls,omitempty"`
	VerificationsRun     int                `json:"verificationsRun,omitempty"`
	VerificationsPassed  int                `json:"verificationsPassed,omitempty"`
	VerificationsFailed  int                `json:"verificationsFailed,omitempty"`
	Outcomes             map[string]int     `json:"outcomes,omitempty"`
	LongestToolCallMs    int64              `json:"longestToolCallMs"`
	TotalWallClockMs     int64              `json:"totalWallClockMs"`
	SlowestToolCalls     []SlowToolCallStat `json:"slowestToolCalls,omitempty"`
	EarliestStart        time.Time          `json:"earliestStart,omitempty"`
	LatestCompletion     time.Time          `json:"latestCompletion,omitempty"`

	// slowest is an unsorted scratch slice used during aggregation;
	// finalize() promotes it onto SlowestToolCalls.
	slowest []SlowToolCallStat
}

// SlowToolCallStat captures one tool-call entry in the
// SlowestToolCalls list.
type SlowToolCallStat struct {
	Name       string `json:"name"`
	DurationMs int64  `json:"durationMs"`
	Success    bool   `json:"success"`
}

func newTraceStats() *TraceStats {
	return &TraceStats{
		ToolCallsByName:  map[string]int{},
		ToolErrorsByName: map[string]int{},
		Outcomes:         map[string]int{},
	}
}

func (s *TraceStats) absorb(t *types.RunTrace) {
	s.Records++
	if s.RunID == "" {
		s.RunID = t.ID
	}

	s.TotalTurns += t.Turns
	s.TokensInput += t.TokenUsage.Input
	s.TokensOutput += t.TokenUsage.Output

	for _, tc := range t.ToolCalls {
		s.ToolCalls++
		s.ToolCallsByName[tc.Name]++
		if !tc.Success {
			s.ToolErrors++
			s.ToolErrorsByName[tc.Name]++
		}
		if tc.RunID != "" {
			s.SubAgentToolCalls++
		}
		if tc.DurationMs > s.LongestToolCallMs {
			s.LongestToolCallMs = tc.DurationMs
		}
		s.slowest = append(s.slowest, SlowToolCallStat{
			Name:       tc.Name,
			DurationMs: tc.DurationMs,
			Success:    tc.Success,
		})
	}

	for _, v := range t.VerificationResults {
		s.VerificationsRun++
		if v.Passed {
			s.VerificationsPassed++
		} else {
			s.VerificationsFailed++
		}
	}

	if t.Outcome != "" {
		s.Outcomes[t.Outcome]++
	}

	if !t.StartedAt.IsZero() && (s.EarliestStart.IsZero() || t.StartedAt.Before(s.EarliestStart)) {
		s.EarliestStart = t.StartedAt
	}
	if !t.CompletedAt.IsZero() && (s.LatestCompletion.IsZero() || t.CompletedAt.After(s.LatestCompletion)) {
		s.LatestCompletion = t.CompletedAt
	}
}

// finalize promotes scratch state to its final form: sorted slowest list,
// wall-clock duration, and pruning of empty maps so JSON output is tidy.
func (s *TraceStats) finalize() {
	sort.SliceStable(s.slowest, func(i, j int) bool {
		return s.slowest[i].DurationMs > s.slowest[j].DurationMs
	})
	s.SlowestToolCalls = s.slowest
	s.slowest = nil

	if !s.EarliestStart.IsZero() && !s.LatestCompletion.IsZero() {
		s.TotalWallClockMs = s.LatestCompletion.Sub(s.EarliestStart).Milliseconds()
	}

	if len(s.ToolCallsByName) == 0 {
		s.ToolCallsByName = nil
	}
	if len(s.ToolErrorsByName) == 0 {
		s.ToolErrorsByName = nil
	}
	if len(s.Outcomes) == 0 {
		s.Outcomes = nil
	}
}

func writeStatsText(out io.Writer, s *TraceStats, top int) error {
	if top <= 0 {
		top = 10
	}
	fmt.Fprintf(out, "trace stats\n")
	if s.RunID != "" {
		fmt.Fprintf(out, "  runId:            %s\n", s.RunID)
	}
	if s.HarnessVersion != "" {
		fmt.Fprintf(out, "  harness version:  %s\n", s.HarnessVersion)
	}
	fmt.Fprintf(out, "  records:          %d\n", s.Records)
	fmt.Fprintf(out, "  total turns:      %d\n", s.TotalTurns)
	fmt.Fprintf(out, "  tokens in / out:  %d / %d\n", s.TokensInput, s.TokensOutput)
	fmt.Fprintf(out, "  tool calls:       %d (errors: %d)\n", s.ToolCalls, s.ToolErrors)
	if s.SubAgentToolCalls > 0 {
		fmt.Fprintf(out, "  sub-agent calls:  %d (of total tool calls)\n", s.SubAgentToolCalls)
	}
	if s.VerificationsRun > 0 {
		fmt.Fprintf(out, "  verifications:    %d run (passed: %d, failed: %d)\n",
			s.VerificationsRun, s.VerificationsPassed, s.VerificationsFailed)
	}
	fmt.Fprintf(out, "  longest call ms:  %d\n", s.LongestToolCallMs)
	if s.TotalWallClockMs > 0 {
		fmt.Fprintf(out, "  wall clock:       %s\n", time.Duration(s.TotalWallClockMs)*time.Millisecond)
	}

	if len(s.ToolCallsByName) > 0 {
		names := sortedMapKeys(s.ToolCallsByName)
		fmt.Fprintln(out, "  tool counts:")
		for _, n := range names {
			fmt.Fprintf(out, "    %-32s %d", n, s.ToolCallsByName[n])
			if e := s.ToolErrorsByName[n]; e > 0 {
				fmt.Fprintf(out, "  (errors: %d)", e)
			}
			fmt.Fprintln(out)
		}
	}

	if len(s.Outcomes) > 0 {
		names := sortedMapKeys(s.Outcomes)
		fmt.Fprintln(out, "  outcomes:")
		for _, n := range names {
			fmt.Fprintf(out, "    %-32s %d\n", n, s.Outcomes[n])
		}
	}

	if len(s.SlowestToolCalls) > 0 {
		fmt.Fprintln(out, "  slowest tool calls:")
		limit := top
		if limit > len(s.SlowestToolCalls) {
			limit = len(s.SlowestToolCalls)
		}
		for i := 0; i < limit; i++ {
			tc := s.SlowestToolCalls[i]
			status := "ok"
			if !tc.Success {
				status = "fail"
			}
			fmt.Fprintf(out, "    %2d. %-32s %6dms  %s\n", i+1, tc.Name, tc.DurationMs, status)
		}
	}

	return nil
}

func sortedMapKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
