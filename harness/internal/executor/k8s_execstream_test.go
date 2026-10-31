package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/rxbynerd/stirrup/harness/internal/commandoutput"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

// fakeScriptedPodStream is a remotecommand.Executor that plays a canned exec:
// stdout chunks in order, then stderr, then either block until the request
// context ends or return err. Like client-go's streamers, it drops the rest of
// a stream whose caller writer fails and does not report that failure, so a
// writer that must abort the command has to cancel the request context. Only
// the first request is scripted; later requests (the group-kill follow-up)
// succeed at once.
type fakeScriptedPodStream struct {
	stdout  []string
	stderr  string
	err     error
	block   bool
	blocked chan struct{}
	calls   int
}

func (s *fakeScriptedPodStream) Stream(remotecommand.StreamOptions) error {
	return errors.New("fakeScriptedPodStream: Stream without context")
}

func (s *fakeScriptedPodStream) StreamWithContext(ctx context.Context, opts remotecommand.StreamOptions) error {
	s.calls++
	if s.calls > 1 {
		return nil
	}
	for _, chunk := range s.stdout {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := io.WriteString(opts.Stdout, chunk); err != nil {
			break
		}
	}
	if s.stderr != "" {
		_, _ = io.WriteString(opts.Stderr, s.stderr)
	}
	if s.block {
		close(s.blocked)
		<-ctx.Done()
		return ctx.Err()
	}
	return s.err
}

// execCommands returns the command argv of each recorded pods/exec request.
func execCommands(urls []*url.URL) [][]string {
	calls := make([][]string, len(urls))
	for i, u := range urls {
		calls[i] = u.Query()["command"]
	}
	return calls
}

func TestPodExecCore_ExecStream_StreamsToCallerWriters(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var urls []*url.URL
	stream := &fakeScriptedPodStream{stdout: []string{"alpha ", "beta ", "gamma\n"}, stderr: "warning\n"}
	core := fakePodExecCore(t, stream, &urls)

	var stdout, stderr bytes.Buffer
	result, err := core.ExecStream(context.Background(), "echo hi", 10*time.Second, &stdout, &stderr)
	must.NoError(err, "ExecStream")

	check.Equal("alpha beta gamma\n", stdout.String(), "stdout")
	check.Equal("warning\n", stderr.String(), "stderr")
	check.Equal(0, result.ExitCode, "exit code")
	check.Empty(result.Stdout, "streamed output is not also buffered in the result")
	check.Empty(result.Stderr, "streamed output is not also buffered in the result")

	must.Len(urls, 1, "pods/exec requests")
	query := urls[0].Query()
	check.Equal([]string{k8sAgentContainer}, query["container"], "container")
	check.NotContains(query, "stdin", "no stdin stream is requested")
	check.Equal([]string{"true"}, query["stdout"], "stdout stream")
	check.Equal([]string{"true"}, query["stderr"], "stderr stream")
	command := query["command"]
	must.NotEmpty(command, "exec command")
	check.Equal("echo hi", command[len(command)-1], "user command is the last argv element")
}

func TestPodExecCore_ExecStream_ExitStatus(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantSub  string
	}{
		{name: "clean exit"},
		{
			name:     "non-zero exit",
			err:      utilexec.CodeExitError{Err: errors.New("command terminated with exit code 3"), Code: 3},
			wantCode: 3,
		},
		{name: "transport error", err: errors.New("stream reset"), wantSub: "exec: stream reset"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var urls []*url.URL
			core := fakePodExecCore(t, &fakeScriptedPodStream{stdout: []string{"out\n"}, err: tc.err}, &urls)

			var stdout bytes.Buffer
			result, err := core.ExecStream(context.Background(), "make", 10*time.Second, &stdout, io.Discard)
			if tc.wantSub != "" {
				must.ErrorContains(err, tc.wantSub)
				must.NotNil(result, "a transport failure still returns a result")
				must.Equal(0, result.ExitCode, "exit code of a transport failure")
				must.Len(urls, 1, "a stream that ended on its own is not followed by a kill")
				return
			}
			must.NoError(err, "ExecStream")
			must.Equal(tc.wantCode, result.ExitCode, "exit code")
			must.Equal("out\n", stdout.String(), "output is delivered regardless of the exit status")
			must.Len(urls, 1, "a finished command is not followed by a kill")
		})
	}
}

func TestPodExecCore_ExecStream_OutputBeyondBufferedCapReachesWritersInOrder(t *testing.T) {
	must := require.New(t)
	const chunkSize = 64 << 10
	chunks := make([]string, int(k8sMaxOutput)/chunkSize+2)
	for i := range chunks {
		chunks[i] = strings.Repeat(string(rune('a'+i%26)), chunkSize)
	}
	want := strings.Join(chunks, "")
	must.Greater(int64(len(want)), k8sMaxOutput, "fixture must exceed the buffered Exec cap")

	var urls []*url.URL
	core := fakePodExecCore(t, &fakeScriptedPodStream{stdout: chunks}, &urls)

	var stdout bytes.Buffer
	result, err := core.ExecStream(context.Background(), "yes", 30*time.Second, &stdout, io.Discard)
	must.NoError(err, "ExecStream")
	must.Equal(0, result.ExitCode, "exit code")
	must.Equal(len(want), stdout.Len(), "stdout length")
	must.Equal(sha256Hex(want), sha256Hex(stdout.String()), "stdout bytes arrived out of order or altered")
}

func TestPodExecCore_ExecStream_CaptureLimitCancelsAndKillsGroup(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	cfg := types.CommandOutputConfig{InlineMaxBytes: 1 << 10, PreviewBytesPerStream: 1 << 10, MaxBytesPerStream: 8192, MaxBytesPerRun: 1 << 20}
	store, err := commandoutput.New(commandoutput.Options{RunID: "limit", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "archive.tar.gz")})
	must.NoError(err, "commandoutput.New")
	t.Cleanup(func() { _ = store.Close() })

	execCtx, cancel := context.WithCancelCause(tool.WithCallContext(context.Background(), tool.CallContext{RunID: "limit", ToolUseID: "limit-call"}))
	defer cancel(nil)
	capture, err := store.Begin(execCtx, cancel)
	must.NoError(err, "store.Begin")

	chunk := strings.Repeat("L", 4096)
	var urls []*url.URL
	core := fakePodExecCore(t, &fakeScriptedPodStream{stdout: []string{chunk, chunk, chunk, chunk}}, &urls)

	result, err := core.ExecStream(execCtx, "yes", 10*time.Second, capture.Stdout(), capture.Stderr())
	must.ErrorIs(err, context.Canceled)
	must.NotErrorIs(err, ErrTimeout)
	must.ErrorIs(context.Cause(execCtx), commandoutput.ErrCaptureLimit, "cancellation cause")
	check.Equal(-1, result.ExitCode, "exit code of a cancelled command")
	assertKillFollowedRun(t, execCommands(urls))

	captured, err := capture.Complete(commandoutput.Completion{ExitCode: result.ExitCode})
	must.ErrorIs(err, commandoutput.ErrCaptureLimit, "Complete reports the breach")
	check.False(captured.Record.CaptureComplete, "capture completeness")
	check.Equal(int64(8192), captured.Record.Stdout.RawBytes, "bytes accepted before the breach")
}

func TestPodExecCore_ExecStream_CancelledContextKillsGroup(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var urls []*url.URL
	stream := &fakeScriptedPodStream{stdout: []string{"partial\n"}, block: true, blocked: make(chan struct{})}
	core := fakePodExecCore(t, stream, &urls)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stream.blocked:
			cancel()
		case <-ctx.Done():
		}
	}()

	var stdout bytes.Buffer
	result, err := core.ExecStream(ctx, "echo partial; sleep 600", time.Minute, &stdout, io.Discard)

	must.ErrorIs(err, context.Canceled)
	must.NotErrorIs(err, ErrTimeout, "cancelled well before the one-minute timeout")
	must.NotNil(result, "a cancelled command returns a result")
	check.Equal(-1, result.ExitCode, "exit code of a cancelled command")
	check.Equal("partial\n", stdout.String(), "output written before the cancel")
	assertKillFollowedRun(t, execCommands(urls))
}

func TestPodExecCore_ExecStream_SpillsThroughCommandOutputStore(t *testing.T) {
	must := require.New(t)
	want := spillExpectedStdout()
	var chunks []string
	for rest := want; rest != ""; {
		n := min(len(rest), 4000)
		chunks, rest = append(chunks, rest[:n]), rest[n:]
	}
	var urls []*url.URL
	core := fakePodExecCore(t, &fakeScriptedPodStream{stdout: chunks, stderr: spillExpectedStderr}, &urls)

	assertCommandOutputSpill(t, context.Background(), core)
	must.Len(urls, 1, "the spill command ran as a single exec")
}

const (
	spillCommand        = "seq 1 50000; echo spill-stderr >&2"
	spillExpectedStderr = "spill-stderr\n"
)

// spillExpectedStdout is what spillCommand writes to stdout: far more than
// the 32 KiB inline limit and than one read_command_output page.
func spillExpectedStdout() string {
	var b strings.Builder
	for i := 1; i <= 50000; i++ {
		fmt.Fprintf(&b, "%d\n", i)
	}
	return b.String()
}

// assertCommandOutputSpill runs spillCommand through the run_command capture
// path (ExecStream into a commandoutput.Store) on exec and checks that the
// output spills, pages back byte-for-byte via Store.Read, and is complete in
// the finalized archive. It is shared by the fake-stream unit test and the
// cluster-gated integration suites.
func assertCommandOutputSpill(t *testing.T, ctx context.Context, exec StreamingExecutor) {
	t.Helper()
	must := require.New(t)
	check := assert.New(t)
	want := spillExpectedStdout()
	cfg := types.CommandOutputConfig{
		InlineMaxBytes:        types.DefaultCommandOutputInlineMaxBytes,
		PreviewBytesPerStream: 1 << 10,
		MaxBytesPerStream:     4 << 20,
		MaxBytesPerRun:        8 << 20,
	}
	store, err := commandoutput.New(commandoutput.Options{RunID: "spill", Config: cfg, ArchivePath: filepath.Join(t.TempDir(), "archive.tar.gz")})
	must.NoError(err, "commandoutput.New")
	t.Cleanup(func() { _ = store.Close() })

	callCtx := tool.WithCallContext(ctx, tool.CallContext{RunID: "spill", Turn: 1, ToolUseID: "spill-call"})
	execCtx, cancel := context.WithCancelCause(callCtx)
	defer cancel(nil)
	capture, err := store.Begin(callCtx, cancel)
	must.NoError(err, "store.Begin")

	result, err := exec.ExecStream(execCtx, spillCommand, 60*time.Second, capture.Stdout(), capture.Stderr())
	must.NoError(err, "ExecStream")
	must.Equal(0, result.ExitCode, "exit code")
	captured, err := capture.Complete(commandoutput.Completion{ExitCode: result.ExitCode})
	must.NoError(err, "capture.Complete")

	record := captured.Record
	check.True(record.CaptureComplete, "capture completeness")
	check.Greater(record.Stdout.ScrubbedBytes+record.Stderr.ScrubbedBytes, cfg.InlineMaxBytes, "combined output must exceed the inline limit so run_command spills")
	check.Equal(int64(len(want)), record.Stdout.RawBytes, "stdout raw bytes")
	check.Equal(int64(len(want)), record.Stdout.ScrubbedBytes, "stdout scrubbed bytes")
	check.Equal(int64(len(spillExpectedStderr)), record.Stderr.ScrubbedBytes, "stderr scrubbed bytes")
	check.Equal(sha256Hex(want), record.Stdout.ScrubbedSHA256, "stdout digest")
	check.Len(captured.Stdout, int(cfg.InlineMaxBytes), "model-visible stdout tail")
	check.True(strings.HasSuffix(want, captured.Stdout), "model-visible stdout is the tail of the stream")
	check.Equal(spillExpectedStderr, captured.Stderr, "model-visible stderr")
	check.NotEmpty(record.Stdout.Reference, "stdout reference")

	var paged strings.Builder
	pages := 0
	for offset := int64(0); ; pages++ {
		must.Less(pages, 64, "read_command_output paging did not reach EOF")
		page, err := store.Read(record.Stdout.Reference, offset, 0)
		must.NoError(err, "store.Read at offset %d", offset)
		must.Equal(offset, page.Offset, "page offset")
		must.NotEmpty(page.Bytes, "page at offset %d", offset)
		paged.Write(page.Bytes)
		offset = page.End
		if page.EOF {
			pages++
			break
		}
	}
	check.Greater(pages, 1, "stdout spans several default-size pages")
	check.Equal(sha256Hex(want), sha256Hex(paged.String()), "paged stdout differs from the command's output")

	errPage, err := store.Read(record.Stderr.Reference, 0, 0)
	must.NoError(err, "store.Read stderr")
	check.Equal(spillExpectedStderr, string(errPage.Bytes), "paged stderr")
	check.True(errPage.EOF, "stderr fits one page")

	archivePath, err := store.Finalize(ctx)
	must.NoError(err, "store.Finalize")
	members := readArchiveMembers(t, archivePath)

	var m struct {
		Complete bool                        `json:"complete"`
		Commands []types.CommandOutputRecord `json:"commands"`
	}
	must.NoError(json.Unmarshal(members["manifest.json"], &m), "manifest.json")
	check.True(m.Complete, "manifest completeness")
	must.Len(m.Commands, 1, "manifest commands")
	check.Equal(sha256Hex(want), sha256Hex(string(members[m.Commands[0].Stdout.ArchiveMember])), "archived stdout differs from the command's output")
	check.Equal(spillExpectedStderr, string(members[m.Commands[0].Stderr.ArchiveMember]), "archived stderr")
}

// sha256Hex lets multi-megabyte comparisons fail with digests, not dumps.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// readArchiveMembers returns the contents of every file in a command-output
// archive, keyed by member name.
func readArchiveMembers(t *testing.T, path string) map[string][]byte {
	t.Helper()
	must := require.New(t)
	f, err := os.Open(path)
	must.NoError(err, "open archive")
	t.Cleanup(func() { _ = f.Close() })
	gz, err := gzip.NewReader(f)
	must.NoError(err, "gzip reader")
	members := map[string][]byte{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return members
		}
		must.NoError(err, "tar next")
		data, err := io.ReadAll(tr)
		must.NoError(err, "read member %s", hdr.Name)
		members[hdr.Name] = data
	}
}
