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
	must := require.New(t)
	archive := filepath.Join(t.TempDir(), "output.tar.gz")
	store, err := New(Options{RunID: "run-1", Config: testConfig(), ArchivePath: archive})
	must.NoError(err)
	defer func() { _ = store.Close() }()

	info, err := os.Stat(store.root)
	must.NoError(err)
	must.Equal(os.FileMode(0o700), info.Mode().Perm(), "store root permissions")

	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "run-1", Turn: 2, ToolUseID: "tool-1"}), cancel)
	must.NoError(err)
	_, err = capture.Stdout().Write([]byte("before sk-ant-"))
	must.NoError(err)
	_, err = capture.Stdout().Write([]byte("abcdefghijklmnop after"))
	must.NoError(err)
	_, err = capture.Stderr().Write([]byte("stderr-data"))
	must.NoError(err)
	captured, err := capture.Complete(Completion{ExitCode: 7})
	must.NoError(err)
	must.NotContains(captured.Stdout, "sk-ant-", "stdout must be scrubbed")
	must.Contains(captured.Stdout, "[REDACTED]")

	raw := []byte("before sk-ant-abcdefghijklmnop after")
	rawSum := sha256.Sum256(raw)
	must.Equal(hex.EncodeToString(rawSum[:]), captured.Record.Stdout.RawSHA256)
	must.Equal(1, captured.Record.Stdout.RedactionCount)

	spooled, err := filepath.Glob(filepath.Join(store.root, "commands", "*", "*"))
	must.NoError(err)
	for _, path := range spooled {
		must.Equal(".txt", filepath.Ext(path), "only canonical scrubbed members belong under commands/: %s", path)
		content, err := os.ReadFile(path)
		must.NoError(err)
		must.NotContains(string(content), "sk-ant-", "secret reached %s", path)
	}

	must.NoError(store.RecordInitial(&captured.Record, "model-visible initial"))
	read, err := store.Read(captured.Record.Stdout.Reference, 7, 12)
	must.NoError(err)
	must.NoError(store.RecordRead(tool.CallContext{RunID: "run-1", ToolUseID: "reader-1"}, captured.Record.Stdout.Reference, read, "model-visible chunk"))
	location, err := store.Finalize(context.Background())
	must.NoError(err)
	must.Equal(archive, location)

	archiveInfo, err := os.Stat(archive)
	must.NoError(err)
	must.Equal(os.FileMode(0o600), archiveInfo.Mode().Perm(), "archive permissions")

	members := readArchive(t, archive)
	must.Equal(captured.Stdout, string(members[captured.Record.Stdout.ArchiveMember]))
	must.Equal("stderr-data", string(members[captured.Record.Stderr.ArchiveMember]))
	must.Equal("model-visible initial", string(members[captured.Record.InitialResultMember]))

	var gotManifest manifest
	must.NoError(json.Unmarshal(members["manifest.json"], &gotManifest))
	must.True(gotManifest.Complete, "manifest must be complete")
	must.Len(gotManifest.Commands, 1)
	must.Len(gotManifest.Commands[0].Reads, 1)
	ledgerRead := gotManifest.Commands[0].Reads[0]
	must.Equal(int64(7), ledgerRead.Offset)
	must.Equal(int64(19), ledgerRead.EndOffset)
	must.NotContains(string(members["manifest.json"]), "sk-ant-", "secret leaked into manifest")
}

// The model-visible reference must not trip the encoded_payload tool guard rule,
// which would deny read_command_output calls made with it.
func TestStore_Reference_PassesToolGuard(t *testing.T) {
	must := require.New(t)
	store, err := New(Options{RunID: "run-1783849881112083000", Config: testConfig(), ArchivePath: filepath.Join(t.TempDir(), "ref.tar.gz")})
	must.NoError(err)
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "run-1783849881112083000", ToolUseID: "toolu_01ErmGRT7vjR545VQFnHNFAx"}), cancel)
	must.NoError(err)
	_, err = capture.Stdout().Write([]byte("output"))
	must.NoError(err)
	captured, err := capture.Complete(Completion{})
	must.NoError(err)
	ref := captured.Record.Stdout.Reference
	input, _ := json.Marshal(map[string]any{"ref": ref, "offset": 0, "limit": 32768})
	findings := security.GuardToolCall("read_command_output", false, input, security.ProtectedWriteTargets{})
	must.Empty(findings, "reference %q must pass the tool guard", ref)
}

func TestStore_RecordRead_ScopesLedgerMembersByRunID(t *testing.T) {
	must := require.New(t)
	archive := filepath.Join(t.TempDir(), "ledger.tar.gz")
	store, err := New(Options{RunID: "parent", Config: testConfig(), ArchivePath: archive})
	must.NoError(err)
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "parent", ToolUseID: "cmd-1"}), cancel)
	must.NoError(err)
	_, err = capture.Stdout().Write([]byte("shared output"))
	must.NoError(err)
	captured, err := capture.Complete(Completion{})
	must.NoError(err)
	must.NoError(store.RecordInitial(&captured.Record, "initial"))
	read, err := store.Read(captured.Record.Stdout.Reference, 0, 6)
	must.NoError(err)
	ref := captured.Record.Stdout.Reference
	// A parent run and a subagent share the store; providers can reuse short
	// tool-use IDs across the two conversations, so identical tool-use IDs
	// under different run IDs must produce distinct ledger members.
	must.NoError(store.RecordRead(tool.CallContext{RunID: "parent", ToolUseID: "call_1"}, ref, read, "parent chunk"))
	must.NoError(store.RecordRead(tool.CallContext{RunID: "subagent", ToolUseID: "call_1"}, ref, read, "subagent chunk"))
	_, err = store.Finalize(context.Background())
	must.NoError(err)
	members := readArchive(t, archive)
	var got manifest
	must.NoError(json.Unmarshal(members["manifest.json"], &got))
	must.Len(got.Commands, 1)
	reads := got.Commands[0].Reads
	must.Len(reads, 2)
	must.NotEqual(reads[0].ArchiveMember, reads[1].ArchiveMember, "ledger members must be distinct")
	must.Equal("parent chunk", string(members[reads[0].ArchiveMember]))
	must.Equal("subagent chunk", string(members[reads[1].ArchiveMember]))
}

func TestStore_LimitFailure_IsStickyAndCancels(t *testing.T) {
	must := require.New(t)
	cfg := testConfig()
	cfg.MaxBytesPerStream = 4
	cfg.MaxBytesPerRun = 8
	store, err := New(Options{RunID: "run-limit", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "limit.tar.gz")})
	must.NoError(err)
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "run-limit", ToolUseID: "tool-limit"}), cancel)
	must.NoError(err)
	_, err = capture.Stdout().Write([]byte("12345"))
	must.Error(err, "expected limit error")
	must.ErrorContains(context.Cause(ctx), "capture limit")
	must.Error(store.FatalError(), "limit failure must be sticky")

	captured, err := capture.Complete(Completion{Cancelled: true, ExitCode: -1})
	must.Error(err)
	must.False(captured.Record.CaptureComplete)
	_, err = store.Finalize(context.Background())
	must.NoError(err, "finalizing the failure manifest archive")
}

func TestStore_BestEffortLimitBreach_IsPerCommand(t *testing.T) {
	must := require.New(t)
	cfg := testConfig()
	cfg.FailurePosture = types.CommandOutputPostureBestEffort
	cfg.MaxBytesPerStream = 4
	cfg.MaxBytesPerRun = 16
	archive := filepath.Join(t.TempDir(), "besteffort.tar.gz")
	store, err := New(Options{RunID: "run-be", Config: cfg, ArchivePath: archive})
	must.NoError(err)
	defer func() { _ = store.Close() }()

	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "run-be", ToolUseID: "cmd-1"}), cancel)
	must.NoError(err)
	_, err = capture.Stdout().Write([]byte("12345"))
	must.Error(err, "expected limit error")
	must.Error(context.Cause(ctx), "breaching command must still be cancelled")
	captured, err := capture.Complete(Completion{Cancelled: true, ExitCode: -1})
	must.Error(err)
	must.False(captured.Record.CaptureComplete)
	must.NoError(store.FatalError(), "bestEffort limit breach must not poison the store")

	ctx2, cancel2 := context.WithCancelCause(context.Background())
	capture2, err := store.Begin(tool.WithCallContext(ctx2, tool.CallContext{RunID: "run-be", ToolUseID: "cmd-2"}), cancel2)
	must.NoError(err, "next command must keep capturing under bestEffort")
	_, err = capture2.Stdout().Write([]byte("ok"))
	must.NoError(err)
	captured2, err := capture2.Complete(Completion{})
	must.NoError(err)
	must.True(captured2.Record.CaptureComplete)
	must.NoError(store.RecordInitial(&captured2.Record, "ok"))
	_, err = store.Finalize(context.Background())
	must.NoError(err)

	members := readArchive(t, archive)
	var got manifest
	must.NoError(json.Unmarshal(members["manifest.json"], &got))
	// The breached capture must stay visible even though the store never
	// went fatal.
	must.False(got.Complete, "manifest must not claim completeness with a breached capture")
	must.Len(got.Commands, 2)
}

func TestStore_LargeStream_ArchiveReconstructsWithoutModelCopy(t *testing.T) {
	must := require.New(t)
	cfg := testConfig()
	cfg.InlineMaxBytes = 32
	cfg.PreviewBytesPerStream = 8
	archive := filepath.Join(t.TempDir(), "large.tar.gz")
	store, err := New(Options{RunID: "large", Config: cfg, ArchivePath: archive})
	must.NoError(err)
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "large", ToolUseID: "tool"}), cancel)
	must.NoError(err)
	want := strings.Repeat("abcdef", 20_000)
	_, err = io.WriteString(capture.Stdout(), want)
	must.NoError(err)
	captured, err := capture.Complete(Completion{})
	must.NoError(err)
	must.LessOrEqual(len(captured.Stdout), int(cfg.InlineMaxBytes), "model copy must not retain the full stream")
	must.NoError(store.RecordInitial(&captured.Record, "preview"))
	_, err = store.Finalize(context.Background())
	must.NoError(err)

	members := readArchive(t, archive)
	must.Equal(want, string(members[captured.Record.Stdout.ArchiveMember]))
}

func TestStore_DiskAndUploadFailures_FailClosed(t *testing.T) {
	t.Run("spool write fails closed", func(t *testing.T) {
		must := require.New(t)
		store, err := New(Options{RunID: "disk", Config: testConfig(), ArchivePath: filepath.Join(t.TempDir(), "disk.tar.gz")})
		must.NoError(err)
		defer func() { _ = store.Close() }()
		ctx, cancel := context.WithCancelCause(context.Background())
		capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "disk", ToolUseID: "tool"}), cancel)
		must.NoError(err)
		must.NoError(capture.stdout.file.Close())
		// Enough output to force a chunk flush inside Write: the scrubbing
		// writer holds a short stream in memory until close, so the disk
		// failure surfaces only once a chunk is due.
		_, err = capture.Stdout().Write([]byte(strings.Repeat("x", 128<<10)))
		must.Error(err, "expected closed-file write failure")
		must.Error(store.FatalError(), "store must go fatal")
		must.Error(context.Cause(ctx), "capture must be cancelled")
	})

	t.Run("upload failure fails closed", func(t *testing.T) {
		must := require.New(t)
		archive := filepath.Join(t.TempDir(), "upload.tar.gz")
		store, err := New(Options{RunID: "upload", Config: testConfig(), ArchivePath: archive, Uploader: failingUploader{}})
		must.NoError(err)
		defer func() { _ = store.Close() }()
		ctx, cancel := context.WithCancelCause(context.Background())
		capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "upload", ToolUseID: "tool"}), cancel)
		must.NoError(err)
		_, _ = capture.Stdout().Write([]byte("ok"))
		captured, err := capture.Complete(Completion{})
		must.NoError(err)
		must.NoError(store.RecordInitial(&captured.Record, "ok"))
		_, err = store.Finalize(context.Background())
		must.Error(err, "expected upload failure")

		members := readArchive(t, archive)
		var got manifest
		must.NoError(json.Unmarshal(members["manifest.json"], &got))
		must.False(got.Complete)
		must.Contains(got.Failure, "upload unavailable")
	})
}

// TestStore_Spool_IsScrubbedBeforeCompletion is the crash-window guarantee: a
// SIGKILL mid-command can only leave behind what is on disk at that instant,
// so the spool must never hold an unscrubbed byte.
func TestStore_Spool_IsScrubbedBeforeCompletion(t *testing.T) {
	must := require.New(t)
	store, err := New(Options{RunID: "crash", Config: testConfig(), ArchivePath: filepath.Join(t.TempDir(), "crash.tar.gz")})
	must.NoError(err)
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "crash", ToolUseID: "tool"}), cancel)
	must.NoError(err)
	secret := "gh" + "p_crashWindowFixture0123456789"
	spool := capture.stdout.file.Name()
	writes := []string{"leading output " + secret + " trailing output\n"}
	for i := 0; i < 16; i++ {
		writes = append(writes, strings.Repeat("filler line\n", 1<<10))
	}
	sawFlush := false
	for _, chunk := range writes {
		_, err := capture.Stdout().Write([]byte(chunk))
		must.NoError(err)
		onDisk, err := os.ReadFile(spool)
		must.NoError(err)
		must.NotContains(string(onDisk), secret, "unscrubbed secret reached the spool file")
		if len(onDisk) > 0 {
			sawFlush = true
			must.Contains(string(onDisk), "[REDACTED]", "flushed spool lost the redaction marker")
		}
	}
	must.True(sawFlush, "no chunk reached disk mid-command; the crash-window claim was not exercised")

	captured, err := capture.Complete(Completion{})
	must.NoError(err)
	raw := sha256.Sum256([]byte(strings.Join(writes, "")))
	must.Equal(hex.EncodeToString(raw[:]), captured.Record.Stdout.RawSHA256, "raw digest must describe the stream as produced")

	final, err := os.ReadFile(spool)
	must.NoError(err)
	must.Equal(security.Scrub(strings.Join(writes, "")), string(final))
	scrubbed := sha256.Sum256(final)
	must.Equal(hex.EncodeToString(scrubbed[:]), captured.Record.Stdout.ScrubbedSHA256, "scrubbed metadata does not describe the archived member")
	must.Equal(int64(len(final)), captured.Record.Stdout.ScrubbedBytes, "scrubbed metadata does not describe the archived member")
	must.Equal(1, captured.Record.Stdout.RedactionCount)
	must.Equal([]string{"github_pat"}, captured.Record.Stdout.RedactionPatterns)
}

// TestStore_BlockedLineAwareCut_ArchivesNoSecret drives the writer's
// blocked-cut case through the capture path, because the spool file is the
// archive member: a secret split across two chunks here is a secret in the
// operator-visible archive, and in GCS when an uploader is configured.
func TestStore_BlockedLineAwareCut_ArchivesNoSecret(t *testing.T) {
	must := require.New(t)
	cfg := testConfig()
	cfg.MaxBytesPerStream = 4 << 20
	store, err := New(Options{RunID: "cut", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "cut.tar.gz")})
	must.NoError(err)
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "cut", ToolUseID: "tool"}), cancel)
	must.NoError(err)
	secret := "AKIA" + "ABCDEFGHIJKLMNOP"
	blocker := "Basic \nQQQQQQQQ."
	stream := blocker + strings.Repeat("x", 64<<10-len(blocker)-len(secret)/2) +
		secret + strings.Repeat("y", 32<<10)
	_, err = io.WriteString(capture.Stdout(), stream)
	must.NoError(err)
	_, err = capture.Complete(Completion{})
	must.NoError(err)
	member, err := os.ReadFile(capture.stdout.file.Name())
	must.NoError(err)
	must.NotContains(string(member), secret, "complete access key reached the archive member")
	must.Equal(security.Scrub(stream), string(member))
}

// Completion does not re-read the stream, so its allocations stay bounded
// regardless of stream size.
func TestStore_Completion_AllocationIsBoundedByChunk(t *testing.T) {
	must := require.New(t)
	cfg := testConfig()
	cfg.MaxBytesPerStream = 32 << 20
	cfg.MaxBytesPerRun = 64 << 20
	store, err := New(Options{RunID: "alloc", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "alloc.tar.gz")})
	must.NoError(err)
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "alloc", ToolUseID: "tool"}), cancel)
	must.NoError(err)
	block := []byte(strings.Repeat("command output line\n", 1<<10))
	streamed := 0
	for i := 0; i < 400; i++ {
		_, err = capture.Stdout().Write(block)
		must.NoError(err)
		streamed += len(block)
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	captured, err := capture.Complete(Completion{})
	must.NoError(err)
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	must.LessOrEqual(allocated, uint64(1<<20), "completion allocated %d bytes for a %d byte stream", allocated, streamed)
	must.Equal(int64(streamed), captured.Record.Stdout.ScrubbedBytes)
}

func TestSweepOrphanedSpools_RemovesOnlyAgedRoots(t *testing.T) {
	must := require.New(t)
	tmp := t.TempDir()
	aged := filepath.Join(tmp, spoolPrefix+"aged")
	live := filepath.Join(tmp, spoolPrefix+"live")
	foreign := filepath.Join(tmp, "unrelated-temp-dir")
	for _, dir := range []string{aged, live, foreign} {
		must.NoError(os.MkdirAll(filepath.Join(dir, "commands"), 0o700))
		must.NoError(os.WriteFile(filepath.Join(dir, "commands", "stdout.txt"), []byte("x"), 0o600))
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, dir := range []string{aged, foreign} {
		backdate(t, dir, old)
	}
	// New sweeps the OS temp directory, so pointing TMPDIR at the fixture
	// exercises the real entry point rather than the helper alone.
	t.Setenv("TMPDIR", tmp)
	store, err := New(Options{RunID: "sweep", Config: testConfig(), ArchivePath: filepath.Join(t.TempDir(), "sweep.tar.gz")})
	must.NoError(err)
	defer func() { _ = store.Close() }()

	_, err = os.Stat(aged)
	must.ErrorIs(err, os.ErrNotExist, "aged spool root must be removed by the sweep")
	_, err = os.Stat(live)
	must.NoError(err, "a spool root younger than the age gate must survive")
	_, err = os.Stat(foreign)
	must.NoError(err, "the sweep must not touch unrelated temp directories")
	_, err = os.Stat(store.root)
	must.NoError(err, "the new store root must outlive its own sweep")
}

// The age gate reads the whole tree: a directory's mtime does not advance when
// a file beneath it is appended to, so a root with a live capture must survive
// even when its own mtime is old.
func TestSweepOrphanedSpools_SparesRootWithLiveCapture(t *testing.T) {
	must := require.New(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	victim, err := New(Options{RunID: "victim", Config: testConfig(), ArchivePath: filepath.Join(t.TempDir(), "victim.tar.gz")})
	must.NoError(err)
	defer func() { _ = victim.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := victim.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "victim", ToolUseID: "tool"}), cancel)
	must.NoError(err)
	backdate(t, victim.root, time.Now().Add(-48*time.Hour))
	_, err = capture.Stdout().Write([]byte(strings.Repeat("live output\n", 16<<10)))
	must.NoError(err)

	second, err := New(Options{RunID: "second", Config: testConfig(), ArchivePath: filepath.Join(t.TempDir(), "second.tar.gz")})
	must.NoError(err)
	defer func() { _ = second.Close() }()
	_, err = os.Stat(victim.root)
	must.NoError(err, "a concurrent store must not sweep a live run's spool root")
	_, err = capture.Complete(Completion{})
	must.NoError(err)
	_, err = os.Stat(capture.stdout.file.Name())
	must.NoError(err, "archive member lost")
}

func backdate(t *testing.T, root string, when time.Time) {
	t.Helper()
	must := require.New(t)
	var paths []string
	err := filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, path)
		return nil
	})
	must.NoError(err)
	// Depth-first: touching a child refreshes its parent's mtime.
	for i := len(paths) - 1; i >= 0; i-- {
		must.NoError(os.Chtimes(paths[i], when, when))
	}
}

func readArchive(t *testing.T, path string) map[string][]byte {
	t.Helper()
	must := require.New(t)
	f, err := os.Open(path)
	must.NoError(err)
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	must.NoError(err)
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		must.NoError(err)
		data, err := io.ReadAll(tr)
		must.NoError(err)
		out[h.Name] = data
	}
	return out
}
