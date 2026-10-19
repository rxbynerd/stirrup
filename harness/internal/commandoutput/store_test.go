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

	"github.com/stretchr/testify/require"

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

func TestStore_ScrubsWholeStreamAndArchivesLedger(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "output.tar.gz")
	store, err := New(Options{RunID: "run-1", Config: testConfig(), ArchivePath: archive})
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	info, err := os.Stat(store.root)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "store root permissions")

	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "run-1", Turn: 2, ToolUseID: "tool-1"}), cancel)
	require.NoError(t, err)
	_, err = capture.Stdout().Write([]byte("before sk-ant-"))
	require.NoError(t, err)
	_, err = capture.Stdout().Write([]byte("abcdefghijklmnop after"))
	require.NoError(t, err)
	_, err = capture.Stderr().Write([]byte("stderr-data"))
	require.NoError(t, err)
	captured, err := capture.Complete(Completion{ExitCode: 7})
	require.NoError(t, err)
	require.NotContains(t, captured.Stdout, "sk-ant-", "stdout must be scrubbed")
	require.Contains(t, captured.Stdout, "[REDACTED]")

	raw := []byte("before sk-ant-abcdefghijklmnop after")
	rawSum := sha256.Sum256(raw)
	require.Equal(t, hex.EncodeToString(rawSum[:]), captured.Record.Stdout.RawSHA256)
	require.Equal(t, 1, captured.Record.Stdout.RedactionCount)

	spooled, err := filepath.Glob(filepath.Join(store.root, "commands", "*", "*"))
	require.NoError(t, err)
	for _, path := range spooled {
		require.Equal(t, ".txt", filepath.Ext(path), "only canonical scrubbed members belong under commands/: %s", path)
		content, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NotContains(t, string(content), "sk-ant-", "secret reached %s", path)
	}

	require.NoError(t, store.RecordInitial(&captured.Record, "model-visible initial"))
	read, err := store.Read(captured.Record.Stdout.Reference, 7, 12)
	require.NoError(t, err)
	require.NoError(t, store.RecordRead(tool.CallContext{RunID: "run-1", ToolUseID: "reader-1"}, captured.Record.Stdout.Reference, read, "model-visible chunk"))
	location, err := store.Finalize(context.Background())
	require.NoError(t, err)
	require.Equal(t, archive, location)

	archiveInfo, err := os.Stat(archive)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), archiveInfo.Mode().Perm(), "archive permissions")

	members := readArchive(t, archive)
	require.Equal(t, captured.Stdout, string(members[captured.Record.Stdout.ArchiveMember]))
	require.Equal(t, "stderr-data", string(members[captured.Record.Stderr.ArchiveMember]))
	require.Equal(t, "model-visible initial", string(members[captured.Record.InitialResultMember]))

	var gotManifest manifest
	require.NoError(t, json.Unmarshal(members["manifest.json"], &gotManifest))
	require.True(t, gotManifest.Complete, "manifest must be complete")
	require.Len(t, gotManifest.Commands, 1)
	require.Len(t, gotManifest.Commands[0].Reads, 1)
	ledgerRead := gotManifest.Commands[0].Reads[0]
	require.Equal(t, int64(7), ledgerRead.Offset)
	require.Equal(t, int64(19), ledgerRead.EndOffset)
	require.NotContains(t, string(members["manifest.json"]), "sk-ant-", "secret leaked into manifest")
}

// The model-visible reference must not trip the encoded_payload tool guard rule,
// which would deny read_command_output calls made with it.
func TestStore_Reference_PassesToolGuard(t *testing.T) {
	store, err := New(Options{RunID: "run-1783849881112083000", Config: testConfig(), ArchivePath: filepath.Join(t.TempDir(), "ref.tar.gz")})
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "run-1783849881112083000", ToolUseID: "toolu_01ErmGRT7vjR545VQFnHNFAx"}), cancel)
	require.NoError(t, err)
	_, err = capture.Stdout().Write([]byte("output"))
	require.NoError(t, err)
	captured, err := capture.Complete(Completion{})
	require.NoError(t, err)
	ref := captured.Record.Stdout.Reference
	input, _ := json.Marshal(map[string]any{"ref": ref, "offset": 0, "limit": 32768})
	findings := security.GuardToolCall("read_command_output", false, input, security.ProtectedWriteTargets{})
	require.Empty(t, findings, "reference %q must pass the tool guard", ref)
}

func TestStore_RecordRead_ScopesLedgerMembersByRunID(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "ledger.tar.gz")
	store, err := New(Options{RunID: "parent", Config: testConfig(), ArchivePath: archive})
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "parent", ToolUseID: "cmd-1"}), cancel)
	require.NoError(t, err)
	_, err = capture.Stdout().Write([]byte("shared output"))
	require.NoError(t, err)
	captured, err := capture.Complete(Completion{})
	require.NoError(t, err)
	require.NoError(t, store.RecordInitial(&captured.Record, "initial"))
	read, err := store.Read(captured.Record.Stdout.Reference, 0, 6)
	require.NoError(t, err)
	ref := captured.Record.Stdout.Reference
	// A parent run and a subagent share the store; providers can reuse short
	// tool-use IDs across the two conversations, so identical tool-use IDs
	// under different run IDs must produce distinct ledger members.
	require.NoError(t, store.RecordRead(tool.CallContext{RunID: "parent", ToolUseID: "call_1"}, ref, read, "parent chunk"))
	require.NoError(t, store.RecordRead(tool.CallContext{RunID: "subagent", ToolUseID: "call_1"}, ref, read, "subagent chunk"))
	_, err = store.Finalize(context.Background())
	require.NoError(t, err)
	members := readArchive(t, archive)
	var got manifest
	require.NoError(t, json.Unmarshal(members["manifest.json"], &got))
	require.Len(t, got.Commands, 1)
	reads := got.Commands[0].Reads
	require.Len(t, reads, 2)
	require.NotEqual(t, reads[0].ArchiveMember, reads[1].ArchiveMember, "ledger members must be distinct")
	require.Equal(t, "parent chunk", string(members[reads[0].ArchiveMember]))
	require.Equal(t, "subagent chunk", string(members[reads[1].ArchiveMember]))
}

func TestStore_LimitFailure_IsStickyAndCancels(t *testing.T) {
	cfg := testConfig()
	cfg.MaxBytesPerStream = 4
	cfg.MaxBytesPerRun = 8
	store, err := New(Options{RunID: "run-limit", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "limit.tar.gz")})
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "run-limit", ToolUseID: "tool-limit"}), cancel)
	require.NoError(t, err)
	_, err = capture.Stdout().Write([]byte("12345"))
	require.Error(t, err, "expected limit error")
	require.ErrorContains(t, context.Cause(ctx), "capture limit")
	require.Error(t, store.FatalError(), "limit failure must be sticky")

	captured, err := capture.Complete(Completion{Cancelled: true, ExitCode: -1})
	require.Error(t, err)
	require.False(t, captured.Record.CaptureComplete)
	_, err = store.Finalize(context.Background())
	require.NoError(t, err, "finalizing the failure manifest archive")
}

func TestStore_BestEffortLimitBreach_IsPerCommand(t *testing.T) {
	cfg := testConfig()
	cfg.FailurePosture = types.CommandOutputPostureBestEffort
	cfg.MaxBytesPerStream = 4
	cfg.MaxBytesPerRun = 16
	archive := filepath.Join(t.TempDir(), "besteffort.tar.gz")
	store, err := New(Options{RunID: "run-be", Config: cfg, ArchivePath: archive})
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "run-be", ToolUseID: "cmd-1"}), cancel)
	require.NoError(t, err)
	_, err = capture.Stdout().Write([]byte("12345"))
	require.Error(t, err, "expected limit error")
	require.Error(t, context.Cause(ctx), "breaching command must still be cancelled")
	captured, err := capture.Complete(Completion{Cancelled: true, ExitCode: -1})
	require.Error(t, err)
	require.False(t, captured.Record.CaptureComplete)
	require.NoError(t, store.FatalError(), "bestEffort limit breach must not poison the store")

	ctx2, cancel2 := context.WithCancelCause(context.Background())
	capture2, err := store.Begin(tool.WithCallContext(ctx2, tool.CallContext{RunID: "run-be", ToolUseID: "cmd-2"}), cancel2)
	require.NoError(t, err, "next command must keep capturing under bestEffort")
	_, err = capture2.Stdout().Write([]byte("ok"))
	require.NoError(t, err)
	captured2, err := capture2.Complete(Completion{})
	require.NoError(t, err)
	require.True(t, captured2.Record.CaptureComplete)
	require.NoError(t, store.RecordInitial(&captured2.Record, "ok"))
	_, err = store.Finalize(context.Background())
	require.NoError(t, err)

	members := readArchive(t, archive)
	var got manifest
	require.NoError(t, json.Unmarshal(members["manifest.json"], &got))
	// The breached capture must stay visible even though the store never
	// went fatal.
	require.False(t, got.Complete, "manifest must not claim completeness with a breached capture")
	require.Len(t, got.Commands, 2)
}

func TestStore_LargeStream_ArchiveReconstructsWithoutModelCopy(t *testing.T) {
	cfg := testConfig()
	cfg.InlineMaxBytes = 32
	cfg.PreviewBytesPerStream = 8
	archive := filepath.Join(t.TempDir(), "large.tar.gz")
	store, err := New(Options{RunID: "large", Config: cfg, ArchivePath: archive})
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "large", ToolUseID: "tool"}), cancel)
	require.NoError(t, err)
	want := strings.Repeat("abcdef", 20_000)
	_, err = io.WriteString(capture.Stdout(), want)
	require.NoError(t, err)
	captured, err := capture.Complete(Completion{})
	require.NoError(t, err)
	require.LessOrEqual(t, len(captured.Stdout), int(cfg.InlineMaxBytes), "model copy must not retain the full stream")
	require.NoError(t, store.RecordInitial(&captured.Record, "preview"))
	_, err = store.Finalize(context.Background())
	require.NoError(t, err)

	members := readArchive(t, archive)
	require.Equal(t, want, string(members[captured.Record.Stdout.ArchiveMember]))
}

func TestStore_DiskAndUploadFailures_FailClosed(t *testing.T) {
	t.Run("spool write fails closed", func(t *testing.T) {
		store, err := New(Options{RunID: "disk", Config: testConfig(), ArchivePath: filepath.Join(t.TempDir(), "disk.tar.gz")})
		require.NoError(t, err)
		defer func() { _ = store.Close() }()
		ctx, cancel := context.WithCancelCause(context.Background())
		capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "disk", ToolUseID: "tool"}), cancel)
		require.NoError(t, err)
		require.NoError(t, capture.stdout.file.Close())
		// Enough output to force a chunk flush inside Write: the scrubbing
		// writer holds a short stream in memory until close, so the disk
		// failure surfaces only once a chunk is due.
		_, err = capture.Stdout().Write([]byte(strings.Repeat("x", 128<<10)))
		require.Error(t, err, "expected closed-file write failure")
		require.Error(t, store.FatalError(), "store must go fatal")
		require.Error(t, context.Cause(ctx), "capture must be cancelled")
	})

	t.Run("upload failure fails closed", func(t *testing.T) {
		archive := filepath.Join(t.TempDir(), "upload.tar.gz")
		store, err := New(Options{RunID: "upload", Config: testConfig(), ArchivePath: archive, Uploader: failingUploader{}})
		require.NoError(t, err)
		defer func() { _ = store.Close() }()
		ctx, cancel := context.WithCancelCause(context.Background())
		capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "upload", ToolUseID: "tool"}), cancel)
		require.NoError(t, err)
		_, _ = capture.Stdout().Write([]byte("ok"))
		captured, err := capture.Complete(Completion{})
		require.NoError(t, err)
		require.NoError(t, store.RecordInitial(&captured.Record, "ok"))
		_, err = store.Finalize(context.Background())
		require.Error(t, err, "expected upload failure")

		members := readArchive(t, archive)
		var got manifest
		require.NoError(t, json.Unmarshal(members["manifest.json"], &got))
		require.False(t, got.Complete)
		require.Contains(t, got.Failure, "upload unavailable")
	})
}

// TestStore_Spool_IsScrubbedBeforeCompletion is the crash-window guarantee: a
// SIGKILL mid-command can only leave behind what is on disk at that instant,
// so the spool must never hold an unscrubbed byte.
func TestStore_Spool_IsScrubbedBeforeCompletion(t *testing.T) {
	store, err := New(Options{RunID: "crash", Config: testConfig(), ArchivePath: filepath.Join(t.TempDir(), "crash.tar.gz")})
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "crash", ToolUseID: "tool"}), cancel)
	require.NoError(t, err)
	secret := "gh" + "p_crashWindowFixture0123456789"
	spool := capture.stdout.file.Name()
	writes := []string{"leading output " + secret + " trailing output\n"}
	for i := 0; i < 16; i++ {
		writes = append(writes, strings.Repeat("filler line\n", 1<<10))
	}
	sawFlush := false
	for _, chunk := range writes {
		_, err := capture.Stdout().Write([]byte(chunk))
		require.NoError(t, err)
		onDisk, err := os.ReadFile(spool)
		require.NoError(t, err)
		require.NotContains(t, string(onDisk), secret, "unscrubbed secret reached the spool file")
		if len(onDisk) > 0 {
			sawFlush = true
			require.Contains(t, string(onDisk), "[REDACTED]", "flushed spool lost the redaction marker")
		}
	}
	require.True(t, sawFlush, "no chunk reached disk mid-command; the crash-window claim was not exercised")

	captured, err := capture.Complete(Completion{})
	require.NoError(t, err)
	raw := sha256.Sum256([]byte(strings.Join(writes, "")))
	require.Equal(t, hex.EncodeToString(raw[:]), captured.Record.Stdout.RawSHA256, "raw digest must describe the stream as produced")

	final, err := os.ReadFile(spool)
	require.NoError(t, err)
	require.Equal(t, security.Scrub(strings.Join(writes, "")), string(final))
	scrubbed := sha256.Sum256(final)
	require.Equal(t, hex.EncodeToString(scrubbed[:]), captured.Record.Stdout.ScrubbedSHA256, "scrubbed metadata does not describe the archived member")
	require.Equal(t, int64(len(final)), captured.Record.Stdout.ScrubbedBytes, "scrubbed metadata does not describe the archived member")
	require.Equal(t, 1, captured.Record.Stdout.RedactionCount)
	require.Equal(t, []string{"github_pat"}, captured.Record.Stdout.RedactionPatterns)
}

// TestStore_BlockedLineAwareCut_ArchivesNoSecret drives the writer's
// blocked-cut case through the capture path, because the spool file is the
// archive member: a secret split across two chunks here is a secret in the
// operator-visible archive, and in GCS when an uploader is configured.
func TestStore_BlockedLineAwareCut_ArchivesNoSecret(t *testing.T) {
	cfg := testConfig()
	cfg.MaxBytesPerStream = 4 << 20
	store, err := New(Options{RunID: "cut", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "cut.tar.gz")})
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "cut", ToolUseID: "tool"}), cancel)
	require.NoError(t, err)
	secret := "AKIA" + "ABCDEFGHIJKLMNOP"
	blocker := "Basic \nQQQQQQQQ."
	stream := blocker + strings.Repeat("x", 64<<10-len(blocker)-len(secret)/2) +
		secret + strings.Repeat("y", 32<<10)
	_, err = io.WriteString(capture.Stdout(), stream)
	require.NoError(t, err)
	_, err = capture.Complete(Completion{})
	require.NoError(t, err)
	member, err := os.ReadFile(capture.stdout.file.Name())
	require.NoError(t, err)
	require.NotContains(t, string(member), secret, "complete access key reached the archive member")
	require.Equal(t, security.Scrub(stream), string(member))
}

// Completion does not re-read the stream, so its allocations stay bounded
// regardless of stream size.
func TestStore_Completion_AllocationIsBoundedByChunk(t *testing.T) {
	cfg := testConfig()
	cfg.MaxBytesPerStream = 32 << 20
	cfg.MaxBytesPerRun = 64 << 20
	store, err := New(Options{RunID: "alloc", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "alloc.tar.gz")})
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "alloc", ToolUseID: "tool"}), cancel)
	require.NoError(t, err)
	block := []byte(strings.Repeat("command output line\n", 1<<10))
	streamed := 0
	for i := 0; i < 400; i++ {
		_, err = capture.Stdout().Write(block)
		require.NoError(t, err)
		streamed += len(block)
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	captured, err := capture.Complete(Completion{})
	require.NoError(t, err)
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	require.LessOrEqual(t, allocated, uint64(1<<20), "completion allocated %d bytes for a %d byte stream", allocated, streamed)
	require.Equal(t, int64(streamed), captured.Record.Stdout.ScrubbedBytes)
}

func TestSweepOrphanedSpools_RemovesOnlyAgedRoots(t *testing.T) {
	tmp := t.TempDir()
	aged := filepath.Join(tmp, spoolPrefix+"aged")
	live := filepath.Join(tmp, spoolPrefix+"live")
	foreign := filepath.Join(tmp, "unrelated-temp-dir")
	for _, dir := range []string{aged, live, foreign} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "commands"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "commands", "stdout.txt"), []byte("x"), 0o600))
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, dir := range []string{aged, foreign} {
		backdate(t, dir, old)
	}
	// New sweeps the OS temp directory, so pointing TMPDIR at the fixture
	// exercises the real entry point rather than the helper alone.
	t.Setenv("TMPDIR", tmp)
	store, err := New(Options{RunID: "sweep", Config: testConfig(), ArchivePath: filepath.Join(t.TempDir(), "sweep.tar.gz")})
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	_, err = os.Stat(aged)
	require.ErrorIs(t, err, os.ErrNotExist, "aged spool root must be removed by the sweep")
	_, err = os.Stat(live)
	require.NoError(t, err, "a spool root younger than the age gate must survive")
	_, err = os.Stat(foreign)
	require.NoError(t, err, "the sweep must not touch unrelated temp directories")
	_, err = os.Stat(store.root)
	require.NoError(t, err, "the new store root must outlive its own sweep")
}

// The age gate reads the whole tree: a directory's mtime does not advance when
// a file beneath it is appended to, so a root with a live capture must survive
// even when its own mtime is old.
func TestSweepOrphanedSpools_SparesRootWithLiveCapture(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	victim, err := New(Options{RunID: "victim", Config: testConfig(), ArchivePath: filepath.Join(t.TempDir(), "victim.tar.gz")})
	require.NoError(t, err)
	defer func() { _ = victim.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := victim.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "victim", ToolUseID: "tool"}), cancel)
	require.NoError(t, err)
	backdate(t, victim.root, time.Now().Add(-48*time.Hour))
	_, err = capture.Stdout().Write([]byte(strings.Repeat("live output\n", 16<<10)))
	require.NoError(t, err)

	second, err := New(Options{RunID: "second", Config: testConfig(), ArchivePath: filepath.Join(t.TempDir(), "second.tar.gz")})
	require.NoError(t, err)
	defer func() { _ = second.Close() }()
	_, err = os.Stat(victim.root)
	require.NoError(t, err, "a concurrent store must not sweep a live run's spool root")
	_, err = capture.Complete(Completion{})
	require.NoError(t, err)
	_, err = os.Stat(capture.stdout.file.Name())
	require.NoError(t, err, "archive member lost")
}

func backdate(t *testing.T, root string, when time.Time) {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, path)
		return nil
	})
	require.NoError(t, err)
	// Depth-first: touching a child refreshes its parent's mtime.
	for i := len(paths) - 1; i >= 0; i-- {
		require.NoError(t, os.Chtimes(paths[i], when, when))
	}
}

func readArchive(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	require.NoError(t, err)
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		data, err := io.ReadAll(tr)
		require.NoError(t, err)
		out[h.Name] = data
	}
	return out
}
