package commandoutput

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

type failingUploader struct{}

func (failingUploader) UploadCommandOutputArchive(context.Context, string, string) (string, error) {
	return "", errors.New("upload unavailable")
}

func testConfig() types.CommandOutputConfig {
	return types.CommandOutputConfig{InlineMaxBytes: 32 << 10, PreviewBytesPerStream: 4 << 10, MaxBytesPerStream: 1 << 20, MaxBytesPerRun: 2 << 20}
}

func TestStoreWholeStreamScrubArchiveAndLedger(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "output.tar.gz")
	store, err := New(Options{RunID: "run-1", Config: testConfig(), ArchivePath: archive})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	info, err := os.Stat(store.root)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("store mode=%v err=%v", info.Mode().Perm(), err)
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "run-1", Turn: 2, ToolUseID: "tool-1"}), cancel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capture.Stdout().Write([]byte("before sk-ant-")); err != nil {
		t.Fatal(err)
	}
	if _, err := capture.Stdout().Write([]byte("abcdefghijklmnop after")); err != nil {
		t.Fatal(err)
	}
	if _, err := capture.Stderr().Write([]byte("stderr-data")); err != nil {
		t.Fatal(err)
	}
	captured, err := capture.Complete(Completion{ExitCode: 7})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(captured.Stdout, "sk-ant-") || !strings.Contains(captured.Stdout, "[REDACTED]") {
		t.Fatalf("stdout not scrubbed: %q", captured.Stdout)
	}
	raw := []byte("before sk-ant-abcdefghijklmnop after")
	rawSum := sha256.Sum256(raw)
	if captured.Record.Stdout.RawSHA256 != hex.EncodeToString(rawSum[:]) {
		t.Fatalf("raw hash=%s", captured.Record.Stdout.RawSHA256)
	}
	if captured.Record.Stdout.RedactionCount != 1 {
		t.Fatalf("redactions=%d", captured.Record.Stdout.RedactionCount)
	}
	spooled, err := filepath.Glob(filepath.Join(store.root, "commands", "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range spooled {
		if filepath.Ext(path) != ".txt" {
			t.Fatalf("only canonical scrubbed members belong under commands/: %s", path)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), "sk-ant-") {
			t.Fatalf("secret reached %s", path)
		}
	}

	if err := store.RecordInitial(&captured.Record, "model-visible initial"); err != nil {
		t.Fatal(err)
	}
	read, err := store.Read(captured.Record.Stdout.Reference, 7, 12)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRead(tool.CallContext{RunID: "run-1", ToolUseID: "reader-1"}, captured.Record.Stdout.Reference, read, "model-visible chunk"); err != nil {
		t.Fatal(err)
	}
	location, err := store.Finalize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if location != archive {
		t.Fatalf("location=%q", location)
	}
	archiveInfo, err := os.Stat(archive)
	if err != nil || archiveInfo.Mode().Perm() != 0o600 {
		t.Fatalf("archive mode=%v err=%v", archiveInfo.Mode().Perm(), err)
	}

	members := readArchive(t, archive)
	if got := string(members[captured.Record.Stdout.ArchiveMember]); got != captured.Stdout {
		t.Fatalf("archived stdout=%q", got)
	}
	if got := string(members[captured.Record.Stderr.ArchiveMember]); got != "stderr-data" {
		t.Fatalf("archived stderr=%q", got)
	}
	if got := string(members[captured.Record.InitialResultMember]); got != "model-visible initial" {
		t.Fatalf("initial=%q", got)
	}
	var gotManifest manifest
	if err := json.Unmarshal(members["manifest.json"], &gotManifest); err != nil {
		t.Fatal(err)
	}
	if !gotManifest.Complete || len(gotManifest.Commands) != 1 || len(gotManifest.Commands[0].Reads) != 1 {
		t.Fatalf("manifest=%+v", gotManifest)
	}
	if gotManifest.Commands[0].Reads[0].Offset != 7 || gotManifest.Commands[0].Reads[0].EndOffset != 19 {
		t.Fatalf("ledger=%+v", gotManifest.Commands[0].Reads[0])
	}
	if strings.Contains(string(members["manifest.json"]), "sk-ant-") {
		t.Fatal("secret leaked into manifest")
	}
}

// TestStoreReferencePassesToolGuard pins the model-visible reference format
// against the loop's tool guard: a reference embedding the base64url member
// ID formed a >100-character base64-like run and tripped the encoded_payload
// rule, so the model's first read_command_output call was denied.
func TestStoreReferencePassesToolGuard(t *testing.T) {
	store, err := New(Options{RunID: "run-1783849881112083000", Config: testConfig(), ArchivePath: filepath.Join(t.TempDir(), "ref.tar.gz")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "run-1783849881112083000", ToolUseID: "toolu_01ErmGRT7vjR545VQFnHNFAx"}), cancel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capture.Stdout().Write([]byte("output")); err != nil {
		t.Fatal(err)
	}
	captured, err := capture.Complete(Completion{})
	if err != nil {
		t.Fatal(err)
	}
	ref := captured.Record.Stdout.Reference
	input, _ := json.Marshal(map[string]any{"ref": ref, "offset": 0, "limit": 32768})
	if findings := security.GuardToolCall("read_command_output", false, input); len(findings) != 0 {
		t.Fatalf("reference %q must pass the tool guard, got findings %+v", ref, findings)
	}
}

func TestStoreRecordReadScopesLedgerMembersByRunID(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "ledger.tar.gz")
	store, err := New(Options{RunID: "parent", Config: testConfig(), ArchivePath: archive})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "parent", ToolUseID: "cmd-1"}), cancel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capture.Stdout().Write([]byte("shared output")); err != nil {
		t.Fatal(err)
	}
	captured, err := capture.Complete(Completion{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordInitial(&captured.Record, "initial"); err != nil {
		t.Fatal(err)
	}
	read, err := store.Read(captured.Record.Stdout.Reference, 0, 6)
	if err != nil {
		t.Fatal(err)
	}
	ref := captured.Record.Stdout.Reference
	// A parent run and a subagent share the store; providers can reuse short
	// tool-use IDs across the two conversations, so identical tool-use IDs
	// under different run IDs must produce distinct ledger members.
	if err := store.RecordRead(tool.CallContext{RunID: "parent", ToolUseID: "call_1"}, ref, read, "parent chunk"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRead(tool.CallContext{RunID: "subagent", ToolUseID: "call_1"}, ref, read, "subagent chunk"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	members := readArchive(t, archive)
	var got manifest
	if err := json.Unmarshal(members["manifest.json"], &got); err != nil {
		t.Fatal(err)
	}
	reads := got.Commands[0].Reads
	if len(reads) != 2 || reads[0].ArchiveMember == reads[1].ArchiveMember {
		t.Fatalf("reads=%+v", reads)
	}
	if string(members[reads[0].ArchiveMember]) != "parent chunk" || string(members[reads[1].ArchiveMember]) != "subagent chunk" {
		t.Fatalf("ledger contents: %q / %q", members[reads[0].ArchiveMember], members[reads[1].ArchiveMember])
	}
}

func TestStoreLimitFailureIsStickyAndCancels(t *testing.T) {
	cfg := testConfig()
	cfg.MaxBytesPerStream = 4
	cfg.MaxBytesPerRun = 8
	store, err := New(Options{RunID: "run-limit", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "limit.tar.gz")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "run-limit", ToolUseID: "tool-limit"}), cancel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capture.Stdout().Write([]byte("12345")); err == nil {
		t.Fatal("expected limit error")
	}
	if context.Cause(ctx) == nil || !strings.Contains(context.Cause(ctx).Error(), "capture limit") {
		t.Fatalf("cause=%v", context.Cause(ctx))
	}
	if store.FatalError() == nil {
		t.Fatal("failure not sticky")
	}
	captured, err := capture.Complete(Completion{Cancelled: true, ExitCode: -1})
	if err == nil || captured.Record.CaptureComplete {
		t.Fatalf("err=%v record=%+v", err, captured.Record)
	}
	if _, err := store.Finalize(context.Background()); err != nil {
		t.Fatalf("failure manifest archive: %v", err)
	}
}

func TestStoreBestEffortLimitBreachIsPerCommand(t *testing.T) {
	cfg := testConfig()
	cfg.FailurePosture = types.CommandOutputPostureBestEffort
	cfg.MaxBytesPerStream = 4
	cfg.MaxBytesPerRun = 16
	archive := filepath.Join(t.TempDir(), "besteffort.tar.gz")
	store, err := New(Options{RunID: "run-be", Config: cfg, ArchivePath: archive})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "run-be", ToolUseID: "cmd-1"}), cancel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capture.Stdout().Write([]byte("12345")); err == nil {
		t.Fatal("expected limit error")
	}
	if context.Cause(ctx) == nil {
		t.Fatal("breaching command must still be cancelled")
	}
	if captured, err := capture.Complete(Completion{Cancelled: true, ExitCode: -1}); err == nil || captured.Record.CaptureComplete {
		t.Fatalf("err=%v record=%+v", err, captured.Record)
	}
	if store.FatalError() != nil {
		t.Fatalf("bestEffort limit breach must not poison the store: %v", store.FatalError())
	}

	ctx2, cancel2 := context.WithCancelCause(context.Background())
	capture2, err := store.Begin(tool.WithCallContext(ctx2, tool.CallContext{RunID: "run-be", ToolUseID: "cmd-2"}), cancel2)
	if err != nil {
		t.Fatalf("next command must keep capturing under bestEffort: %v", err)
	}
	if _, err := capture2.Stdout().Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	captured2, err := capture2.Complete(Completion{})
	if err != nil || !captured2.Record.CaptureComplete {
		t.Fatalf("err=%v record=%+v", err, captured2.Record)
	}
	if err := store.RecordInitial(&captured2.Record, "ok"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	members := readArchive(t, archive)
	var got manifest
	if err := json.Unmarshal(members["manifest.json"], &got); err != nil {
		t.Fatal(err)
	}
	// The breached capture must stay visible even though the store never
	// went fatal.
	if got.Complete {
		t.Fatalf("manifest must not claim completeness with a breached capture: %+v", got)
	}
	if len(got.Commands) != 2 {
		t.Fatalf("commands=%d", len(got.Commands))
	}
}

func TestStoreLargeStreamArchiveReconstructsWithoutRetainingFullModelCopy(t *testing.T) {
	cfg := testConfig()
	cfg.InlineMaxBytes = 32
	cfg.PreviewBytesPerStream = 8
	archive := filepath.Join(t.TempDir(), "large.tar.gz")
	store, err := New(Options{RunID: "large", Config: cfg, ArchivePath: archive})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "large", ToolUseID: "tool"}), cancel)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat("abcdef", 20_000)
	if _, err := io.WriteString(capture.Stdout(), want); err != nil {
		t.Fatal(err)
	}
	captured, err := capture.Complete(Completion{})
	if err != nil {
		t.Fatal(err)
	}
	if len(captured.Stdout) > int(cfg.InlineMaxBytes) {
		t.Fatalf("model copy retained %d bytes", len(captured.Stdout))
	}
	if err := store.RecordInitial(&captured.Record, "preview"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	members := readArchive(t, archive)
	if got := string(members[captured.Record.Stdout.ArchiveMember]); got != want {
		t.Fatalf("archive reconstruction mismatch: got %d bytes want %d", len(got), len(want))
	}
}

func TestStoreDiskAndUploadFailuresFailClosed(t *testing.T) {
	t.Run("spool write", func(t *testing.T) {
		store, err := New(Options{RunID: "disk", Config: testConfig(), ArchivePath: filepath.Join(t.TempDir(), "disk.tar.gz")})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = store.Close() }()
		ctx, cancel := context.WithCancelCause(context.Background())
		capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "disk", ToolUseID: "tool"}), cancel)
		if err != nil {
			t.Fatal(err)
		}
		if err := capture.stdout.file.Close(); err != nil {
			t.Fatal(err)
		}
		// Enough output to force a chunk flush inside Write: the scrubbing
		// writer holds a short stream in memory until close, so the disk
		// failure surfaces only once a chunk is due.
		if _, err := capture.Stdout().Write([]byte(strings.Repeat("x", 128<<10))); err == nil {
			t.Fatal("expected closed-file write failure")
		}
		if store.FatalError() == nil || context.Cause(ctx) == nil {
			t.Fatalf("fatal=%v cause=%v", store.FatalError(), context.Cause(ctx))
		}
	})

	t.Run("upload", func(t *testing.T) {
		archive := filepath.Join(t.TempDir(), "upload.tar.gz")
		store, err := New(Options{RunID: "upload", Config: testConfig(), ArchivePath: archive, Uploader: failingUploader{}})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = store.Close() }()
		ctx, cancel := context.WithCancelCause(context.Background())
		capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "upload", ToolUseID: "tool"}), cancel)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = capture.Stdout().Write([]byte("ok"))
		captured, err := capture.Complete(Completion{})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordInitial(&captured.Record, "ok"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Finalize(context.Background()); err == nil {
			t.Fatal("expected upload failure")
		}
		members := readArchive(t, archive)
		var got manifest
		if err := json.Unmarshal(members["manifest.json"], &got); err != nil {
			t.Fatal(err)
		}
		if got.Complete || !strings.Contains(got.Failure, "upload unavailable") {
			t.Fatalf("manifest=%+v", got)
		}
	})
}

// TestStoreSpoolIsScrubbedBeforeCompletion is the crash-window guarantee: a
// SIGKILL mid-command can only leave behind what is on disk at that instant,
// so the spool must never hold an unscrubbed byte.
func TestStoreSpoolIsScrubbedBeforeCompletion(t *testing.T) {
	store, err := New(Options{RunID: "crash", Config: testConfig(), ArchivePath: filepath.Join(t.TempDir(), "crash.tar.gz")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "crash", ToolUseID: "tool"}), cancel)
	if err != nil {
		t.Fatal(err)
	}
	secret := "gh" + "p_crashWindowFixture0123456789"
	spool := capture.stdout.file.Name()
	writes := []string{"leading output " + secret + " trailing output\n"}
	for i := 0; i < 16; i++ {
		writes = append(writes, strings.Repeat("filler line\n", 1<<10))
	}
	sawFlush := false
	for _, chunk := range writes {
		if _, err := capture.Stdout().Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
		onDisk, err := os.ReadFile(spool)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(onDisk), secret) {
			t.Fatal("unscrubbed secret reached the spool file")
		}
		if len(onDisk) > 0 {
			sawFlush = true
			if !strings.Contains(string(onDisk), "[REDACTED]") {
				t.Fatalf("flushed spool lost the redaction marker: %.64q", onDisk)
			}
		}
	}
	if !sawFlush {
		t.Fatal("no chunk reached disk mid-command; the crash-window claim was not exercised")
	}
	captured, err := capture.Complete(Completion{})
	if err != nil {
		t.Fatal(err)
	}
	raw := sha256.Sum256([]byte(strings.Join(writes, "")))
	if captured.Record.Stdout.RawSHA256 != hex.EncodeToString(raw[:]) {
		t.Fatal("raw digest must still describe the stream as produced")
	}
	final, err := os.ReadFile(spool)
	if err != nil {
		t.Fatal(err)
	}
	if want := security.Scrub(strings.Join(writes, "")); string(final) != want {
		t.Fatalf("scrubbed spool diverges from a whole-stream scrub: %d bytes want %d", len(final), len(want))
	}
	scrubbed := sha256.Sum256(final)
	if captured.Record.Stdout.ScrubbedSHA256 != hex.EncodeToString(scrubbed[:]) ||
		captured.Record.Stdout.ScrubbedBytes != int64(len(final)) {
		t.Fatalf("scrubbed metadata does not describe the archived member: %+v", captured.Record.Stdout)
	}
	if captured.Record.Stdout.RedactionCount != 1 ||
		len(captured.Record.Stdout.RedactionPatterns) != 1 || captured.Record.Stdout.RedactionPatterns[0] != "github_pat" {
		t.Fatalf("redaction statistics=%+v", captured.Record.Stdout)
	}
}

// TestStoreArchivesNoSecretWhenLineAwareCutIsBlocked drives the writer's
// blocked-cut case through the capture path, because the spool file is the
// archive member: a secret split across two chunks here is a secret in the
// operator-visible archive, and in GCS when an uploader is configured.
func TestStoreArchivesNoSecretWhenLineAwareCutIsBlocked(t *testing.T) {
	cfg := testConfig()
	cfg.MaxBytesPerStream = 4 << 20
	store, err := New(Options{RunID: "cut", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "cut.tar.gz")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "cut", ToolUseID: "tool"}), cancel)
	if err != nil {
		t.Fatal(err)
	}
	secret := "AKIA" + "ABCDEFGHIJKLMNOP"
	blocker := "Basic \nQQQQQQQQ."
	stream := blocker + strings.Repeat("x", 64<<10-len(blocker)-len(secret)/2) +
		secret + strings.Repeat("y", 32<<10)
	if _, err := io.WriteString(capture.Stdout(), stream); err != nil {
		t.Fatal(err)
	}
	if _, err := capture.Complete(Completion{}); err != nil {
		t.Fatal(err)
	}
	member, err := os.ReadFile(capture.stdout.file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(member), secret) {
		t.Fatal("complete access key reached the archive member")
	}
	if string(member) != security.Scrub(stream) {
		t.Fatal("archive member diverges from a whole-stream scrub")
	}
}

// TestStoreCompletionAllocationIsBoundedByChunk pins the second half of the
// scrub-on-write change: completion no longer reads the stream back, so its
// cost is independent of stream size.
func TestStoreCompletionAllocationIsBoundedByChunk(t *testing.T) {
	cfg := testConfig()
	cfg.MaxBytesPerStream = 32 << 20
	cfg.MaxBytesPerRun = 64 << 20
	store, err := New(Options{RunID: "alloc", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "alloc.tar.gz")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "alloc", ToolUseID: "tool"}), cancel)
	if err != nil {
		t.Fatal(err)
	}
	block := []byte(strings.Repeat("command output line\n", 1<<10))
	streamed := 0
	for i := 0; i < 400; i++ {
		if _, err := capture.Stdout().Write(block); err != nil {
			t.Fatal(err)
		}
		streamed += len(block)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	captured, err := capture.Complete(Completion{})
	if err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("completion allocated %d bytes for a %d byte stream", allocated, streamed)
	}
	if captured.Record.Stdout.ScrubbedBytes != int64(streamed) {
		t.Fatalf("scrubbed bytes=%d want %d", captured.Record.Stdout.ScrubbedBytes, streamed)
	}
}

func TestSweepOrphanedSpoolsRemovesOnlyAgedRoots(t *testing.T) {
	tmp := t.TempDir()
	aged := filepath.Join(tmp, spoolPrefix+"aged")
	live := filepath.Join(tmp, spoolPrefix+"live")
	foreign := filepath.Join(tmp, "unrelated-temp-dir")
	for _, dir := range []string{aged, live, foreign} {
		if err := os.MkdirAll(filepath.Join(dir, "commands"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "commands", "stdout.txt"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, dir := range []string{aged, foreign} {
		backdate(t, dir, old)
	}
	// New sweeps the OS temp directory, so pointing TMPDIR at the fixture
	// exercises the real entry point rather than the helper alone.
	t.Setenv("TMPDIR", tmp)
	store, err := New(Options{RunID: "sweep", Config: testConfig(), ArchivePath: filepath.Join(t.TempDir(), "sweep.tar.gz")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if _, err := os.Stat(aged); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("aged spool root survived the sweep: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("a spool root younger than the age gate must survive: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("the sweep must not touch unrelated temp directories: %v", err)
	}
	if _, err := os.Stat(store.root); err != nil {
		t.Fatalf("the new store root must outlive its own sweep: %v", err)
	}
}

// TestSweepSparesRootWithLiveCaptureBeneathIt pins the reason the gate reads
// the whole tree: a directory's mtime does not advance when a capture
// appends to a file two levels down, so a root created over a day ago but
// still being written would otherwise be removed out from under its run.
func TestSweepSparesRootWithLiveCaptureBeneathIt(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	victim, err := New(Options{RunID: "victim", Config: testConfig(), ArchivePath: filepath.Join(t.TempDir(), "victim.tar.gz")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = victim.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := victim.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "victim", ToolUseID: "tool"}), cancel)
	if err != nil {
		t.Fatal(err)
	}
	backdate(t, victim.root, time.Now().Add(-48*time.Hour))
	if _, err := capture.Stdout().Write([]byte(strings.Repeat("live output\n", 16<<10))); err != nil {
		t.Fatal(err)
	}

	second, err := New(Options{RunID: "second", Config: testConfig(), ArchivePath: filepath.Join(t.TempDir(), "second.tar.gz")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if _, err := os.Stat(victim.root); err != nil {
		t.Fatalf("a concurrent store swept a live run's spool root: %v", err)
	}
	if _, err := capture.Complete(Completion{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(capture.stdout.file.Name()); err != nil {
		t.Fatalf("archive member lost: %v", err)
	}
}

func backdate(t *testing.T, root string, when time.Time) {
	t.Helper()
	var paths []string
	if err := filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Depth-first: touching a child refreshes its parent's mtime.
	for i := len(paths) - 1; i >= 0; i-- {
		if err := os.Chtimes(paths[i], when, when); err != nil {
			t.Fatal(err)
		}
	}
}

func readArchive(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = data
	}
	return out
}
