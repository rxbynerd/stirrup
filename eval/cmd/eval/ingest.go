package main

// stirrup-eval ingest reads JSONL trace files produced by the
// harness and persists them into a FileStore lakehouse. See
// docs/eval.md for the on-wire shapes and scrubbing posture.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rxbynerd/stirrup/eval/lakehouse"
	"github.com/rxbynerd/stirrup/types"
	tracereader "github.com/rxbynerd/stirrup/types/trace"
)

// cmdIngest is the `stirrup-eval ingest` entry point.
func cmdIngest(args []string) {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	lakehousePath := fs.String("lakehouse", "", "Path to lakehouse directory (required)")
	skipPartial := fs.Bool("skip-partial", false, "Skip streamed traces that end without a run_finished event. By default, partial captures are ingested as recordings with FinalOutcome.Outcome=\"interrupted\".")
	traceArgs := newStringSliceFlag(fs, "trace", "Trace JSONL file to ingest (repeatable; '-' reads stdin)")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}
	if *lakehousePath == "" {
		log.Fatal("-lakehouse is required")
	}
	if len(*traceArgs) == 0 {
		log.Fatal("at least one -trace is required")
	}

	store, err := lakehouse.NewFileStore(*lakehousePath)
	if err != nil {
		log.Fatalf("opening lakehouse: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var totalTraces, totalRecordings, totalSkipped int
	for _, path := range *traceArgs {
		t, r, s, err := ingestFile(ctx, store, path, *skipPartial)
		if err != nil {
			log.Fatalf("ingesting %q: %v", path, err)
		}
		totalTraces += t
		totalRecordings += r
		totalSkipped += s
	}

	fmt.Fprintf(os.Stderr,
		"ingested %d traces and %d recordings from %d file(s)",
		totalTraces, totalRecordings, len(*traceArgs))
	if totalSkipped > 0 {
		fmt.Fprintf(os.Stderr, "; skipped %d partial recording(s)", totalSkipped)
	}
	fmt.Fprintln(os.Stderr)
}

// ingestFile dispatches on the per-file wire shape and returns
// (#traces written, #recordings written, #partials skipped, error).
// The error is non-nil only on unrecoverable I/O failures; per-line
// malformed records are skipped with a stderr warning.
func ingestFile(ctx context.Context, store *lakehouse.FileStore, path string, skipPartial bool) (int, int, int, error) {
	reader, err := tracereader.Open(path)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("opening trace file: %w", err)
	}
	defer func() { _ = reader.Close() }()

	first, format, err := detectFormat(path)
	if err != nil {
		return 0, 0, 0, err
	}
	switch format {
	case formatLegacy:
		// Re-open the underlying file because detectFormat consumed
		// its first scanner line. tracereader.Reader does not expose
		// a "rewind" hook; the cheap alternative is a fresh Open.
		reader2, err := tracereader.Open(path)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("re-opening trace file: %w", err)
		}
		defer func() { _ = reader2.Close() }()
		return ingestLegacyTraces(ctx, store, reader2)
	case formatStreaming:
		// ReadRecording consumes the whole stream and yields one
		// recording; a single .jsonl from the streaming emitter
		// represents exactly one run.
		reader2, err := tracereader.Open(path)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("re-opening trace file: %w", err)
		}
		defer func() { _ = reader2.Close() }()
		return ingestStreamingTrace(ctx, store, reader2, skipPartial)
	case formatEmpty:
		fmt.Fprintf(os.Stderr, "ingest: %s is empty; skipping\n", path)
		return 0, 0, 0, nil
	default:
		// Should be unreachable.
		return 0, 0, 0, fmt.Errorf("internal: unknown format %v for %q (first line: %.80s)", format, path, first)
	}
}

type traceFormat int

const (
	formatEmpty traceFormat = iota
	formatLegacy
	formatStreaming
)

// detectFormat peeks at the first non-blank line of path to decide
// between the legacy single-blob shape and the streaming event
// shape (present `kind` discriminator => streaming). The caller
// re-opens the file for the actual ingest.
func detectFormat(path string) (string, traceFormat, error) {
	r, err := tracereader.Open(path)
	if err != nil {
		return "", formatEmpty, fmt.Errorf("peek open: %w", err)
	}
	defer func() { _ = r.Close() }()

	// tracereader does not expose its scanner and Reader.Next
	// silently skips non-run_finished streaming lines, so the raw
	// first line is read manually here instead.
	f, err := os.Open(path)
	if err != nil {
		if path == "-" {
			// Cannot peek stdin without consuming it; the streaming
			// reader handles both wire shapes, so default to it.
			return "", formatStreaming, nil
		}
		return "", formatEmpty, fmt.Errorf("peek open file: %w", err)
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, 4096)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", formatEmpty, fmt.Errorf("peek read: %w", err)
	}
	chunk := string(buf[:n])
	// Blank leading lines are tolerated.
	chunk = strings.TrimLeft(chunk, "\r\n\t ")
	if chunk == "" {
		return "", formatEmpty, nil
	}
	// Examine only the first line.
	firstLine := chunk
	if nl := strings.IndexByte(firstLine, '\n'); nl >= 0 {
		firstLine = firstLine[:nl]
	}
	// A streaming event starts with a `kind` key, modulo field order.
	if hasKindKey(firstLine) {
		return firstLine, formatStreaming, nil
	}
	return firstLine, formatLegacy, nil
}

// hasKindKey reports whether a JSON object string contains a
// top-level `"kind":` token. Intentionally lexical rather than a
// full JSON parse; false positives are possible but harmless since
// the streaming reader still yields a correct shape either way.
func hasKindKey(line string) bool {
	return strings.Contains(line, `"kind":`)
}

// ingestLegacyTraces walks a legacy single-blob JSONL file, writing
// one trace per line. No recordings are produced — the legacy shape
// has no transcript content. Per-line malformed records are skipped
// (tracereader.Reader already logs them at debug; this layer
// surfaces a count via the caller's stderr summary).
func ingestLegacyTraces(ctx context.Context, store *lakehouse.FileStore, r *tracereader.Reader) (int, int, int, error) {
	traces, err := r.All()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("reading legacy traces: %w", err)
	}
	written := 0
	for _, trace := range traces {
		if err := ctx.Err(); err != nil {
			return written, 0, 0, err
		}
		if trace.ID == "" {
			fmt.Fprintln(os.Stderr, "ingest: legacy trace missing id; skipping")
			continue
		}
		if err := store.StoreTrace(ctx, trace); err != nil {
			return written, 0, 0, fmt.Errorf("storing trace %q: %w", trace.ID, err)
		}
		written++
	}
	return written, 0, 0, nil
}

// ingestStreamingTrace reassembles a single recording from the
// stream's events and writes both the recording and its embedded
// RunTrace summary. A stream that ends without run_finished is a
// partial capture: by default it is written with
// FinalOutcome.Outcome=="interrupted" so it remains discoverable to
// mine-failures and replay; pass skipPartial to drop it.
func ingestStreamingTrace(ctx context.Context, store *lakehouse.FileStore, r *tracereader.Reader, skipPartial bool) (int, int, int, error) {
	rec, err := r.ReadRecording()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("reading streaming trace: %w", err)
	}
	if rec.RunID == "" {
		fmt.Fprintln(os.Stderr, "ingest: streaming trace missing runId; skipping")
		return 0, 0, 0, nil
	}
	completed := rec.FinalOutcome.ID != ""
	if !completed {
		if skipPartial {
			fmt.Fprintf(os.Stderr, "ingest: %s ended without run_finished; --skip-partial set, dropping\n", rec.RunID)
			return 0, 0, 1, nil
		}
		// Synthesise a minimal FinalOutcome so downstream
		// consumers (mine-failures, baseline) can still tag the run.
		// ID and Config come from run_started; outcome is the
		// distinguished sentinel "interrupted".
		rec.FinalOutcome = types.RunTrace{
			ID:        rec.RunID,
			Config:    rec.Config,
			Outcome:   "interrupted",
			Turns:     len(rec.Turns),
			StartedAt: time.Now(),
		}
	}
	if err := store.StoreTrace(ctx, rec.FinalOutcome); err != nil {
		return 0, 0, 0, fmt.Errorf("storing trace %q: %w", rec.RunID, err)
	}
	if err := store.StoreRecording(ctx, *rec); err != nil {
		return 1, 0, 0, fmt.Errorf("storing recording %q: %w", rec.RunID, err)
	}
	return 1, 1, 0, nil
}

// stringSliceFlag is a flag.Value for repeated string flags. Used so
// `--trace a.jsonl --trace b.jsonl --trace -` parses as
// []string{"a.jsonl","b.jsonl","-"} rather than overwriting on each
// occurrence.
type stringSliceFlag struct {
	values *[]string
}

func newStringSliceFlag(fs *flag.FlagSet, name, usage string) *[]string {
	sl := &stringSliceFlag{values: &[]string{}}
	fs.Var(sl, name, usage)
	return sl.values
}

func (s *stringSliceFlag) String() string {
	if s == nil || s.values == nil {
		return ""
	}
	return filepath.Join(*s.values...)
}

func (s *stringSliceFlag) Set(v string) error {
	*s.values = append(*s.values, v)
	return nil
}
