package commandoutput

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
)

const (
	spoolKillChildEnv  = "STIRRUP_SPOOL_KILL_CHILD"
	spoolKillSecretEnv = "STIRRUP_SPOOL_KILL_SECRET"
)

// TestSpoolSurvivesSIGKILLWithoutSecrets drives the crash window itself: a
// child process with a live capture is killed outright, leaving whatever the
// spool held at that instant. Nothing else in the suite can prove the
// guarantee, because every in-process path runs the completion cleanup, which
// makes a scrubbed spool indistinguishable from a deleted one.
func TestSpoolSurvivesSIGKILLWithoutSecrets(t *testing.T) {
	if os.Getenv(spoolKillChildEnv) == "1" {
		spoolKillChild()
		return
	}
	tmp := t.TempDir()
	secret := "gh" + "p_sigkillFixture0123456789abcdef"
	child := exec.Command(os.Args[0], "-test.run=TestSpoolSurvivesSIGKILLWithoutSecrets")
	child.Env = append(os.Environ(), spoolKillChildEnv+"=1", spoolKillSecretEnv+"="+secret, "TMPDIR="+tmp)
	child.Stderr = os.Stderr
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Wait() }()
	ready := bufio.NewScanner(stdout)
	for ready.Scan() && ready.Text() != "ready" {
	}
	if err := ready.Err(); err != nil {
		t.Fatalf("child never reported a live spool: %v", err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()

	spooled, redacted := 0, false
	err = filepath.WalkDir(tmp, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if len(content) == 0 {
			return nil
		}
		spooled++
		if strings.Contains(string(content), secret) {
			t.Errorf("killed run left an unscrubbed secret in %s", path)
		}
		redacted = redacted || strings.Contains(string(content), "[REDACTED]")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if spooled == 0 || !redacted {
		t.Fatalf("the crash window was not exercised: %d spool files survived, redaction seen=%v", spooled, redacted)
	}
}

// spoolKillChild starts a capture, drives enough output through it to force a
// chunk to disk, then blocks so the parent can kill it mid-command.
func spoolKillChild() {
	secret := os.Getenv(spoolKillSecretEnv)
	store, err := New(Options{RunID: "kill", Config: testConfig(), ArchivePath: filepath.Join(os.TempDir(), "kill.tar.gz")})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "kill", ToolUseID: "tool"}), cancel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if _, err := capture.Stdout().Write([]byte("leading output " + secret + " trailing\n")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if _, err := capture.Stdout().Write([]byte(strings.Repeat("filler line\n", 32<<10))); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	info, err := os.Stat(capture.stdout.file.Name())
	if err != nil || info.Size() == 0 {
		fmt.Fprintf(os.Stderr, "spool not on disk: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("ready")
	// Sleep rather than block: a parked goroutine trips the runtime's
	// deadlock detector, which would end the process before the parent's
	// signal lands.
	time.Sleep(time.Minute)
}
