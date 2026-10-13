package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newThreeCommitRepo returns a file:// URL for a repository whose file
// version.txt reads "one", "two" and "three" across three commits, along with
// the SHA of each commit.
func newThreeCommitRepo(t *testing.T) (url string, shas []string) {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet", "--initial-branch=main")
	for _, content := range []string{"one", "two", "three"} {
		if err := os.WriteFile(filepath.Join(dir, "version.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, dir, "add", "version.txt")
		runGit(t, dir, "commit", "--quiet", "-m", content)
		shas = append(shas, runGit(t, dir, "rev-parse", "HEAD"))
	}
	return "file://" + dir, shas
}

func readVersion(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "version.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCloneRepo_EmptyRefTakesTheDefaultBranchTip(t *testing.T) {
	url, _ := newThreeCommitRepo(t)
	dest := t.TempDir()
	if err := cloneRepo(context.Background(), url, "", dest); err != nil {
		t.Fatalf("cloneRepo: %v", err)
	}
	if got := readVersion(t, dest); got != "three" {
		t.Errorf("version.txt = %q, want %q", got, "three")
	}
}

func TestCloneRepo_ChecksOutANonTipCommitSHA(t *testing.T) {
	url, shas := newThreeCommitRepo(t)
	dest := t.TempDir()
	if err := cloneRepo(context.Background(), url, shas[1], dest); err != nil {
		t.Fatalf("cloneRepo: %v", err)
	}
	if got := readVersion(t, dest); got != "two" {
		t.Errorf("version.txt = %q, want %q", got, "two")
	}
	if head := runGit(t, dest, "rev-parse", "HEAD"); head != shas[1] {
		t.Errorf("HEAD = %s, want %s", head, shas[1])
	}
}

func TestCloneRepo_AcceptsBranchAndTagRefs(t *testing.T) {
	url, shas := newThreeCommitRepo(t)
	src := strings.TrimPrefix(url, "file://")
	runGit(t, src, "tag", "v1", shas[0])
	runGit(t, src, "branch", "side", shas[1])

	for ref, want := range map[string]string{"v1": "one", "side": "two", "main": "three"} {
		dest := t.TempDir()
		if err := cloneRepo(context.Background(), url, ref, dest); err != nil {
			t.Fatalf("cloneRepo(%q): %v", ref, err)
		}
		if got := readVersion(t, dest); got != want {
			t.Errorf("ref %q: version.txt = %q, want %q", ref, got, want)
		}
	}
}

func TestCloneRepo_RejectsAnUnknownRef(t *testing.T) {
	url, _ := newThreeCommitRepo(t)
	missing := strings.Repeat("0", 40)
	err := cloneRepo(context.Background(), url, missing, t.TempDir())
	if err == nil {
		t.Fatal("cloneRepo with an unknown SHA succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "git fetch") {
		t.Errorf("error %q does not name the failing git step", err)
	}
}

func TestCloneRepo_RejectsOptionLikeArguments(t *testing.T) {
	url, _ := newThreeCommitRepo(t)
	if err := cloneRepo(context.Background(), url, "--upload-pack=touch pwned", t.TempDir()); err == nil {
		t.Error("cloneRepo accepted a ref starting with '-'")
	}
	if err := cloneRepo(context.Background(), "--upload-pack=touch pwned", "", t.TempDir()); err == nil {
		t.Error("cloneRepo accepted a repo starting with '-'")
	}
}
