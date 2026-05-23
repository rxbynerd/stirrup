package main

// stirrup-eval replay re-evaluates one or more recorded runs against a
// fresh judge specification, without invoking the harness or any
// provider. This is the fast loop for iterating on judge criteria —
// change the regex or composite logic, replay the recording set, see
// whether outcomes match expectations — without re-burning provider
// tokens.
//
// The v0.1 scope is the judge-only flavour: load recordings from the
// lakehouse, apply each suite task's judge to a workspace dir, write
// a SuiteResult. The harness-replay flavour (spawning a harness
// configured with ReplayProvider+ReplayExecutor) is a follow-up; the
// building blocks already exist in harness/internal/provider/replay.go
// and harness/internal/executor/replay.go but the orchestration is
// non-trivial. See #272 for the scope decision.
//
// Workspace caveat: judge-only replay against a recording with no
// preserved workspace dir works for content-only judges (composite
// over text predicates, future LLM-as-judge) but fails for judges
// that require file state (file-exists, file-contains, test-command).
// Operators must preserve the workspace alongside the recording —
// the `eval run --output` path retains per-task artifacts that suit
// this. For arbitrary production recordings the workspace is opt-in
// per #272.

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/eval/lakehouse"
	"github.com/rxbynerd/stirrup/eval/runner"
	"github.com/rxbynerd/stirrup/types"
)

// cmdReplay is the `stirrup-eval replay` entry point.
func cmdReplay(args []string) {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	lakehousePath := fs.String("lakehouse", "", "Path to lakehouse directory (required)")
	suitePath := fs.String("suite", "", "Path to eval suite HCL file (required)")
	workspaceDir := fs.String("workspace", "", "Workspace directory to evaluate judges against. May be empty for judges that do not need file state (some composite judges).")
	output := fs.String("output", "", "Write SuiteResult JSON to this path (default: print summary only)")
	recordingIDs := newStringSliceFlag(fs, "recording", "RunID of a recording to replay (repeatable). If omitted, all recordings in the lakehouse are replayed.")
	outcomeFilter := fs.String("outcome", "", "Filter recordings to replay by outcome (e.g. failed, error). Ignored if --recording is set.")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}
	if *lakehousePath == "" {
		log.Fatal("-lakehouse is required")
	}
	if *suitePath == "" {
		log.Fatal("-suite is required")
	}

	suite, err := loadSuite(*suitePath)
	if err != nil {
		log.Fatalf("loading suite: %v", err)
	}
	if len(suite.Tasks) == 0 {
		log.Fatal("suite has no tasks; nothing to replay against")
	}

	store, err := lakehouse.NewFileStore(*lakehousePath)
	if err != nil {
		log.Fatalf("opening lakehouse: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	recordings, err := selectRecordings(ctx, store, *recordingIDs, *outcomeFilter)
	if err != nil {
		log.Fatalf("selecting recordings: %v", err)
	}
	if len(recordings) == 0 {
		log.Fatal("no matching recordings found")
	}

	runID := fmt.Sprintf("replay-%d", time.Now().UnixMilli())
	startedAt := time.Now()

	tasks := make([]eval.TaskResult, 0, len(recordings))
	pass := 0
	for i, rec := range recordings {
		// Pair recording with suite task by position; if the suite
		// has fewer tasks than recordings, the i-th recording uses
		// task i%len(tasks). Sole-task suites are a common authoring
		// pattern (one judge applied across all mined failures).
		task := suite.Tasks[i%len(suite.Tasks)]
		result, err := runner.ReplayRecording(ctx, rec, task, *workspaceDir)
		if err != nil {
			result = eval.TaskResult{
				TaskID:  task.ID,
				Outcome: "error",
				Error:   err.Error(),
				JudgeVerdict: eval.JudgeVerdict{
					Passed: false,
					Reason: err.Error(),
				},
			}
		}
		// Tag the result with the source recording's runId so a
		// downstream `compare` knows which production run each
		// verdict came from. result.TaskID is otherwise the suite
		// task ID, which collapses when one task replays N
		// recordings.
		result.TaskID = fmt.Sprintf("%s/%s", task.ID, rec.RunID)
		if result.Outcome == "pass" {
			pass++
		}
		tasks = append(tasks, result)
	}

	passRate := float64(0)
	if len(tasks) > 0 {
		passRate = float64(pass) / float64(len(tasks))
	}
	result := eval.SuiteResult{
		SuiteID:     suite.ID,
		RunID:       runID,
		StartedAt:   startedAt,
		CompletedAt: time.Now(),
		Tasks:       tasks,
		PassRate:    passRate,
	}

	if *output != "" {
		// Ensure parent dir exists for callers that pass a fresh path.
		if dir := filepath.Dir(*output); dir != "." && dir != "/" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				log.Fatalf("creating output dir: %v", err)
			}
		}
		if err := writeJSON(*output, result); err != nil {
			log.Fatalf("writing replay result: %v", err)
		}
		fmt.Fprintf(os.Stderr, "Replay result written to %s\n", *output)
	}

	fmt.Printf("Replay: %d recordings, %d passed, %d failed/errored (pass rate %.1f%%)\n",
		len(tasks), pass, len(tasks)-pass, passRate*100)
}

// selectRecordings resolves the --recording / --outcome flags into a
// concrete slice. Explicit IDs short-circuit the lakehouse filter:
// each ID is fetched individually and a missing ID is fatal so the
// operator notices the typo before the replay runs.
func selectRecordings(ctx context.Context, store *lakehouse.FileStore, ids []string, outcome string) ([]types.RunRecording, error) {
	if len(ids) > 0 {
		all, err := store.QueryRecordings(ctx, types.TraceFilter{})
		if err != nil {
			return nil, fmt.Errorf("query recordings: %w", err)
		}
		byID := make(map[string]types.RunRecording, len(all))
		for _, rec := range all {
			byID[rec.RunID] = rec
		}
		out := make([]types.RunRecording, 0, len(ids))
		for _, id := range ids {
			rec, ok := byID[id]
			if !ok {
				return nil, fmt.Errorf("recording %q not found in lakehouse", id)
			}
			out = append(out, rec)
		}
		return out, nil
	}
	filter := types.TraceFilter{Outcome: outcome}
	return store.QueryRecordings(ctx, filter)
}
