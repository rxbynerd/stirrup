package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
)

// fakePodStream stands in for the pods/exec streamer: it writes stdout and
// stderr to the request's streams and returns err.
type fakePodStream struct {
	stdout, stderr string
	err            error
	hadDeadline    bool
}

func (f *fakePodStream) Stream(remotecommand.StreamOptions) error {
	return errors.New("fakePodStream: Stream without context")
}

func (f *fakePodStream) StreamWithContext(ctx context.Context, opts remotecommand.StreamOptions) error {
	_, f.hadDeadline = ctx.Deadline()
	if opts.Stdout != nil {
		if _, err := io.WriteString(opts.Stdout, f.stdout); err != nil {
			return err
		}
	}
	if opts.Stderr != nil {
		_, _ = io.WriteString(opts.Stderr, f.stderr)
	}
	return f.err
}

// fakePodExecCore returns a podExecCore whose pods/exec requests reach
// stream, recording each request URL in urls.
func fakePodExecCore(t *testing.T, stream remotecommand.Executor, urls *[]*url.URL) *podExecCore {
	t.Helper()
	must := require.New(t)
	cfg := &rest.Config{Host: "http://127.0.0.1:1"}
	clientset, err := kubernetes.NewForConfig(cfg)
	must.NoError(err, "kubernetes.NewForConfig")
	prev := newRemoteExecutor
	t.Cleanup(func() { newRemoteExecutor = prev })
	newRemoteExecutor = func(_ *rest.Config, u *url.URL) (remotecommand.Executor, error) {
		*urls = append(*urls, u)
		return stream, nil
	}
	return &podExecCore{clientset: clientset, restConfig: cfg, namespace: "ns", podName: "run-pod"}
}

func TestPodExecCore_ArchiveWorkspace(t *testing.T) {
	cases := []struct {
		name    string
		stream  fakePodStream
		wantIs  error
		wantSub string
	}{
		{name: "tar stream", stream: fakePodStream{stdout: "tarbytes"}},
		{
			name:   "missing root",
			stream: fakePodStream{err: utilexec.CodeExitError{Err: errors.New("command terminated with exit code 66"), Code: workspaceRootMissingExit}},
			wantIs: ErrWorkspaceRootMissing,
		},
		{
			name:    "tar failure",
			stream:  fakePodStream{stderr: "tar: ./x: Permission denied\n", err: utilexec.CodeExitError{Err: errors.New("command terminated with exit code 2"), Code: 2}},
			wantSub: "tar exited with status 2: tar: ./x: Permission denied",
		},
		{
			name:    "transport error",
			stream:  fakePodStream{err: errors.New("stream reset")},
			wantSub: "stream reset",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var urls []*url.URL
			stream := tc.stream
			core := fakePodExecCore(t, &stream, &urls)

			var out bytes.Buffer
			stats, err := core.ArchiveWorkspace(context.Background(), &out, 1<<20)
			switch {
			case tc.wantIs != nil:
				must.ErrorIs(err, tc.wantIs)
			case tc.wantSub != "":
				must.ErrorContains(err, tc.wantSub)
			default:
				must.NoError(err)
				must.Equal("tarbytes", out.String(), "forwarded tar bytes")
				must.Equal(int64(len("tarbytes")), stats.Bytes, "reported byte count")
			}

			must.True(stream.hadDeadline, "pods/exec ran without a deadline")
			must.Len(urls, 1, "pods/exec requests")
			must.True(strings.HasSuffix(urls[0].Path, "/namespaces/ns/pods/run-pod/exec"),
				"request path %q should end with the run Pod's exec subresource", urls[0].Path)
			must.Equal(workspaceArchiveArgv("/bin/sh", k8sWorkspace), urls[0].Query()["command"])
		})
	}
}
