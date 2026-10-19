package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v\n%s", args, out)
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
		require.NoError(t, os.WriteFile(filepath.Join(dir, "version.txt"), []byte(content), 0o644))
		runGit(t, dir, "add", "version.txt")
		runGit(t, dir, "commit", "--quiet", "-m", content)
		shas = append(shas, runGit(t, dir, "rev-parse", "HEAD"))
	}
	return "file://" + dir, shas
}

func readVersion(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "version.txt"))
	require.NoError(t, err)
	return string(data)
}

func TestCloneRepo_EmptyRefTakesTheDefaultBranchTip(t *testing.T) {
	url, _ := newThreeCommitRepo(t)
	dest := t.TempDir()
	err := cloneRepo(context.Background(), url, "", dest)
	require.NoError(t, err)
	require.Equal(t, "three", readVersion(t, dest))
}

func TestCloneRepo_ChecksOutNonTipCommitSHA(t *testing.T) {
	url, shas := newThreeCommitRepo(t)
	dest := t.TempDir()
	err := cloneRepo(context.Background(), url, shas[1], dest)
	require.NoError(t, err)
	require.Equal(t, "two", readVersion(t, dest))
	require.Equal(t, shas[1], runGit(t, dest, "rev-parse", "HEAD"))
}

func TestCloneRepo_AcceptsBranchAndTagRefs(t *testing.T) {
	url, shas := newThreeCommitRepo(t)
	src := strings.TrimPrefix(url, "file://")
	runGit(t, src, "tag", "v1", shas[0])
	runGit(t, src, "branch", "side", shas[1])

	cases := []struct {
		name string
		ref  string
		want string
	}{
		{name: "tag resolves to its commit", ref: "v1", want: "one"},
		{name: "branch resolves to its commit", ref: "side", want: "two"},
		{name: "main resolves to the branch tip", ref: "main", want: "three"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			err := cloneRepo(context.Background(), url, tc.ref, dest)
			require.NoError(t, err)
			require.Equal(t, tc.want, readVersion(t, dest))
		})
	}
}

func TestCloneRepo_RejectsUnknownRef(t *testing.T) {
	url, _ := newThreeCommitRepo(t)
	missing := strings.Repeat("0", 40)
	err := cloneRepo(context.Background(), url, missing, t.TempDir())
	require.Error(t, err)
	require.ErrorContains(t, err, "git fetch", "error should name the failing git step")
}

func TestCloneRepo_RejectsOptionLikeArguments(t *testing.T) {
	t.Run("ref starting with a dash", func(t *testing.T) {
		url, _ := newThreeCommitRepo(t)
		err := cloneRepo(context.Background(), url, "--upload-pack=touch pwned", t.TempDir())
		require.Error(t, err)
	})
	t.Run("repo starting with a dash", func(t *testing.T) {
		err := cloneRepo(context.Background(), "--upload-pack=touch pwned", "", t.TempDir())
		require.Error(t, err)
	})
}
