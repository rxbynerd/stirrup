package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestReader_AllAndLast(t *testing.T) {
	traces := []types.RunTrace{
		{ID: "first", Turns: 1},
		{ID: "second", Turns: 5},
	}

	var buf bytes.Buffer
	for _, tr := range traces {
		buf.Write(mustMarshal(t, tr))
		buf.WriteByte('\n')
	}
	body := buf.Bytes()

	r := NewReader(bytes.NewReader(body), WithLogger(discardLogger()))
	got, err := r.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(All) = %d, want 2", len(got))
	}
	if got[0].ID != "first" || got[1].ID != "second" {
		t.Errorf("All IDs = %q,%q want first,second", got[0].ID, got[1].ID)
	}

	r2 := NewReader(bytes.NewReader(body), WithLogger(discardLogger()))
	last, err := r2.Last()
	if err != nil {
		t.Fatalf("Last: %v", err)
	}
	if last.ID != "second" {
		t.Errorf("Last.ID = %q, want second", last.ID)
	}
}

func TestReader_SkipMalformed(t *testing.T) {
	good := mustMarshal(t, types.RunTrace{ID: "ok", Turns: 2})

	var buf bytes.Buffer
	buf.WriteString("not json at all\n")
	buf.WriteString("\n") // blank line tolerated, not malformed
	buf.Write(good)
	buf.WriteByte('\n')
	buf.WriteString("{still bad\n")

	r := NewReader(&buf, WithLogger(discardLogger()))
	got, err := r.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 || got[0].ID != "ok" {
		t.Fatalf("All = %+v, want one record with ID=ok", got)
	}
}

func TestReader_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := Open(path, WithLogger(discardLogger()))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = r.Close() }()

	if _, err := r.Last(); err == nil {
		t.Fatal("Last on empty file: expected error")
	}
}

func TestReader_OversizedLineSkipped(t *testing.T) {
	good := mustMarshal(t, types.RunTrace{ID: "ok"})
	oversized := bytes.Repeat([]byte("x"), MaxLineBytes+8)

	var buf bytes.Buffer
	buf.Write(oversized)
	buf.WriteByte('\n')
	buf.Write(good)
	buf.WriteByte('\n')

	r := NewReader(&buf, WithLogger(discardLogger()))
	got, err := r.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 || got[0].ID != "ok" {
		t.Fatalf("got %+v, want one record with ID=ok", got)
	}
}

func TestReader_NextReturnsEOF(t *testing.T) {
	r := NewReader(strings.NewReader(""), WithLogger(discardLogger()))
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next on empty: err = %v, want io.EOF", err)
	}
}

func TestOpen_StdinSentinel(t *testing.T) {
	r, err := Open("-", WithLogger(discardLogger()))
	if err != nil {
		t.Fatalf("Open '-': %v", err)
	}
	defer func() { _ = r.Close() }()
	if r.closer != nil {
		t.Error("Open('-') must not own stdin")
	}
}

func TestTail_OneShotConsumesAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.jsonl")
	var buf bytes.Buffer
	for i, id := range []string{"a", "b", "c"} {
		buf.Write(mustMarshal(t, types.RunTrace{ID: id, Turns: i + 1}))
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	var seen []string
	err := Tail(context.Background(), path, TailOptions{Logger: discardLogger()}, func(tr *types.RunTrace) error {
		seen = append(seen, tr.ID)
		return nil
	})
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if got, want := strings.Join(seen, ","), "a,b,c"; got != want {
		t.Errorf("Tail order = %q, want %q", got, want)
	}
}

func TestTail_FollowStreamsAppends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.jsonl")
	initial := mustMarshal(t, types.RunTrace{ID: "first"})
	if err := os.WriteFile(path, append(initial, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	seenCh := make(chan string, 8)
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- Tail(ctx, path, TailOptions{
			Follow:       true,
			PollInterval: 10 * time.Millisecond,
			Logger:       discardLogger(),
		}, func(tr *types.RunTrace) error {
			seenCh <- tr.ID
			return nil
		})
	}()

	expectID := func(want string) {
		t.Helper()
		select {
		case got := <-seenCh:
			if got != want {
				t.Fatalf("Tail saw %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for record %q", want)
		}
	}

	expectID("first")

	// Append a second record while Tail is following.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(mustMarshal(t, types.RunTrace{ID: "second"}), '\n')); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	expectID("second")

	cancel()
	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("Tail returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Tail did not exit after cancel")
	}
}

func TestTail_HandlerErrorAborts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.jsonl")
	if err := os.WriteFile(path, append(mustMarshal(t, types.RunTrace{ID: "x"}), '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	stop := errors.New("stop")
	err := Tail(context.Background(), path, TailOptions{Logger: discardLogger()}, func(*types.RunTrace) error {
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("Tail err = %v, want %v", err, stop)
	}
}
