package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"slices"
	"strings"
	"testing"

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
func fakePodExecCore(t *testing.T, stream *fakePodStream, urls *[]*url.URL) *podExecCore {
	t.Helper()
	cfg := &rest.Config{Host: "http://127.0.0.1:1"}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("NewForConfig: %v", err)
	}
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
			var urls []*url.URL
			stream := tc.stream
			core := fakePodExecCore(t, &stream, &urls)

			var out bytes.Buffer
			stats, err := core.ArchiveWorkspace(context.Background(), &out, 1<<20)
			switch {
			case tc.wantIs != nil:
				if !errors.Is(err, tc.wantIs) {
					t.Fatalf("error = %v, want %v", err, tc.wantIs)
				}
			case tc.wantSub != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantSub)
				}
			default:
				if err != nil {
					t.Fatalf("error = %v, want nil", err)
				}
				if out.String() != "tarbytes" || stats.Bytes != int64(len("tarbytes")) {
					t.Errorf("forwarded %q (stats %d), want the tar bytes", out.String(), stats.Bytes)
				}
			}
			if !stream.hadDeadline {
				t.Error("pods/exec ran without a deadline")
			}
			if len(urls) != 1 {
				t.Fatalf("pods/exec requests = %d, want 1", len(urls))
			}
			if !strings.HasSuffix(urls[0].Path, "/namespaces/ns/pods/run-pod/exec") {
				t.Errorf("request path = %q, want the run Pod's exec subresource", urls[0].Path)
			}
			if got, want := urls[0].Query()["command"], workspaceArchiveArgv("/bin/sh", k8sWorkspace); !slices.Equal(got, want) {
				t.Errorf("command = %q, want %q", got, want)
			}
		})
	}
}
