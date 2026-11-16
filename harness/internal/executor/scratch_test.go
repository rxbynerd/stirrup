package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes/fake"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/rxbynerd/stirrup/types"
)

func TestBuildSandboxPodSpec_MountsSizedScratchAndRunsAsOneUserAndGroup(t *testing.T) {
	cases := []struct {
		name      string
		resources *types.ResourceLimits
		wantSize  string
	}{
		{name: "default size", resources: nil, wantSize: "1Gi"},
		{name: "explicit size", resources: &types.ResourceLimits{ScratchMB: 3072}, wantSize: "3Gi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			cfg := baseSandboxCfg()
			cfg.Resources = tc.resources

			spec := buildSandboxPodSpec(cfg, nil, nil)

			var scratch *corev1.Volume
			for i := range spec.Volumes {
				if spec.Volumes[i].Name == k8sScratchVolume {
					scratch = &spec.Volumes[i]
				}
			}
			must.NotNil(scratch, "volume %q missing from %+v", k8sScratchVolume, spec.Volumes)
			must.NotNil(scratch.EmptyDir, "scratch must be an emptyDir")
			must.Empty(scratch.EmptyDir.Medium, "scratch is disk-backed")
			must.NotNil(scratch.EmptyDir.SizeLimit)
			want := resource.MustParse(tc.wantSize)
			must.Zero(want.Cmp(*scratch.EmptyDir.SizeLimit), "size limit %s", scratch.EmptyDir.SizeLimit)

			c := spec.Containers[0]
			must.Contains(c.VolumeMounts, corev1.VolumeMount{Name: k8sScratchVolume, MountPath: sandboxScratchDir})
			must.Equal([]corev1.EnvVar{{Name: "HOME", Value: sandboxHomeDir}, {Name: "TMPDIR", Value: sandboxTmpDir}}, c.Env)
			must.Equal(sandboxUID, *c.SecurityContext.RunAsUser)
			must.Equal(sandboxGID, *c.SecurityContext.RunAsGroup)
			must.Equal(sandboxGID, *spec.SecurityContext.FSGroup)
		})
	}
}

func TestPrepareK8sExecutor_RejectsScratchAboveDiskLimit(t *testing.T) {
	must := require.New(t)
	cfg := constructorCfg("")
	cfg.Resources = &types.ResourceLimits{DiskMB: 512, ScratchMB: 1024}

	_, _, err := prepareK8sExecutor(cfg)

	must.ErrorContains(err, "executor.resources.scratchMb (1024) must not exceed executor.resources.diskMb (512)")
}

func TestNewK8sExecutor_ScratchSetupRunsLastAndFailureTearsDown(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		must := require.New(t)
		cs := fake.NewClientset()
		admitPods(cs, nil)
		var commands [][]string
		execFn := func(_ context.Context, command []string, _ io.Reader, _, _ io.Writer) error {
			commands = append(commands, command)
			return nil
		}

		exec, err := newFakeK8sExecutorWithExec(context.Background(), cs, constructorCfg(""), execFn)

		must.NoError(err)
		must.Equal([][]string{execProbeCommand, scratchSetupCommand()}, commands)
		must.NoError(exec.Close())
	})

	t.Run("failure", func(t *testing.T) {
		must := require.New(t)
		cs := fake.NewClientset()
		admitPods(cs, nil)
		execFn := func(_ context.Context, command []string, _ io.Reader, _, stderr io.Writer) error {
			if !isScratchSetup(command) {
				return nil
			}
			_, _ = io.WriteString(stderr, "mkdir: can't create directory '/scratch/home': Permission denied\n")
			return utilexec.CodeExitError{Err: errors.New("command terminated with exit code 1"), Code: 1}
		}

		exec, err := newFakeK8sExecutorWithExec(context.Background(), cs, constructorCfg(""), execFn)

		must.Nil(exec)
		must.ErrorContains(err, "pod "+testK8sNamespace+"/")
		must.ErrorContains(err, "prepare sandbox scratch: exit 1: mkdir: can't create directory '/scratch/home': Permission denied")
		requireTornDown(t, cs)
	})
}

// scratchEngine is a mock engine that records the create request, each
// exec's command and the container removals.
type scratchEngine struct {
	mu       sync.Mutex
	create   containerCreateRequest
	cmds     [][]string
	removals int
}

// handlers serves the engine; with failSetup every exec exits 1.
func (s *scratchEngine) handlers(failSetup bool) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"POST /containers/create": func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&s.create)
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(containerCreateResponse{ID: "test-id"})
		},
		"POST /containers/*/start": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
		"POST /containers/*/stop":  func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
		"DELETE /containers/*": func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			s.removals++
			s.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		},
		"POST /containers/*/exec": func(w http.ResponseWriter, r *http.Request) {
			var req execCreateRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			s.mu.Lock()
			s.cmds = append(s.cmds, req.Cmd)
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execCreateResponse{ID: "setup"})
		},
		"POST /exec/*/start": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
			if failSetup {
				writeDockerFrame(w, 2, []byte("mkdir: can't create directory '/scratch/home': File exists\n"))
			}
		},
		"GET /exec/*/json": func(w http.ResponseWriter, r *http.Request) {
			code := 0
			if failSetup {
				code = 1
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execInspectResponse{ExitCode: code})
		},
	}
}

func TestNewContainerExecutor_PreparesSizedExecutableScratch(t *testing.T) {
	must := require.New(t)
	engine := &scratchEngine{}
	sock, cleanup := mockEngineServer(t, engine.handlers(false))
	defer cleanup()

	exec, err := NewContainerExecutorWithContext(context.Background(), ContainerExecutorConfig{
		Image:      "busybox",
		HostDir:    "/tmp/workspace",
		SocketPath: sock,
		Resources:  &types.ResourceLimits{MemoryMB: 4096, ScratchMB: 2048},
	})
	must.NoError(err)
	defer func() { _ = exec.Close() }()

	must.Equal("rw,nosuid,nodev,exec,mode=1777,size=2147483648", engine.create.HostConfig.Tmpfs[sandboxScratchDir])
	must.Equal([]string{"HOME=/scratch/home", "TMPDIR=/scratch/tmp"}, engine.create.Env)
	must.Equal("65532:65532", engine.create.User)
	must.Equal([][]string{scratchSetupCommand()}, engine.cmds, "the constructor runs only the scratch setup")
}

func TestNewContainerExecutor_ScratchSetupFailureRemovesContainer(t *testing.T) {
	must := require.New(t)
	engine := &scratchEngine{}
	sock, cleanup := mockEngineServer(t, engine.handlers(true))
	defer cleanup()

	exec, err := NewContainerExecutorWithContext(context.Background(), ContainerExecutorConfig{
		Image:      "busybox",
		HostDir:    "/tmp/workspace",
		SocketPath: sock,
	})

	must.Nil(exec)
	must.ErrorContains(err, "prepare sandbox scratch: exit 1: mkdir: can't create directory '/scratch/home': File exists")
	must.Equal(1, engine.removals, "the container is removed")
}

func TestNewContainerExecutor_RejectsScratchAboveMemoryLimitBeforeCreate(t *testing.T) {
	must := require.New(t)
	engine := &scratchEngine{}
	sock, cleanup := mockEngineServer(t, engine.handlers(false))
	defer cleanup()

	_, err := NewContainerExecutorWithContext(context.Background(), ContainerExecutorConfig{
		Image:      "busybox",
		HostDir:    "/tmp/workspace",
		SocketPath: sock,
		Resources:  &types.ResourceLimits{MemoryMB: 512, ScratchMB: 1024},
	})

	must.ErrorContains(err, "executor.resources.scratchMb (1024) must not exceed executor.resources.memoryMb (512)")
	must.Empty(engine.create.Image, "no container is created")
}

// sandboxScratchProbe checks the scratch contract from inside a live
// sandbox: the identity, HOME and TMPDIR, that both are owned by the sandbox
// user with owner-only access, that a script written to TMPDIR executes,
// that HOME is writable, and that scratch holds nothing else.
const sandboxScratchProbe = `set -e
id -u
id -g
printf '%s %s\n' "$HOME" "$TMPDIR"
stat -c '%u %g %a' "$HOME" "$TMPDIR"
printf '#!/bin/sh\necho ran\n' > "$TMPDIR/probe.sh"
chmod +x "$TMPDIR/probe.sh"
"$TMPDIR/probe.sh"
: > "$HOME/.probe"
ls -A /scratch
ls -A "$HOME"`

// assertSandboxScratchUsable runs sandboxScratchProbe in exec, a sandbox
// no command has run in yet, and checks its output line by line.
func assertSandboxScratchUsable(t *testing.T, ctx context.Context, exec Executor) {
	t.Helper()
	must := require.New(t)
	res, err := exec.Exec(ctx, sandboxScratchProbe, 30*time.Second)
	must.NoError(err, "Exec")
	must.Equal(0, res.ExitCode, "stdout %q stderr %q", res.Stdout, res.Stderr)
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	must.Len(lines, 9, "probe output %q", res.Stdout)
	must.Equal([]string{"65532", "65532", "/scratch/home /scratch/tmp"}, lines[:3])
	for _, line := range lines[3:5] {
		must.True(strings.HasPrefix(line, "65532 65532 "), "owner of a scratch directory: %q", line)
		must.True(strings.HasSuffix(line, "700"), "mode of a scratch directory: %q", line)
	}
	must.Equal([]string{"ran", "home", "tmp", ".probe"}, lines[5:])
}
