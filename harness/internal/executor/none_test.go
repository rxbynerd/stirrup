package executor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

var _ Executor = (*NoneExecutor)(nil)

func TestNoneExecutor_ReadFile_ReturnsNotSupported(t *testing.T) {
	n := NewNoneExecutor()
	_, err := n.ReadFile(context.Background(), "main.go")
	require.ErrorContains(t, err, "not supported")
}

func TestNoneExecutor_WriteFile_ReturnsNotSupported(t *testing.T) {
	n := NewNoneExecutor()
	err := n.WriteFile(context.Background(), "main.go", "content")
	require.ErrorContains(t, err, "not supported")
}

func TestNoneExecutor_ListDirectory_ReturnsNotSupported(t *testing.T) {
	n := NewNoneExecutor()
	_, err := n.ListDirectory(context.Background(), ".")
	require.ErrorContains(t, err, "not supported")
}

func TestNoneExecutor_Exec_ReturnsNotSupported(t *testing.T) {
	n := NewNoneExecutor()
	_, err := n.Exec(context.Background(), "echo hi", 0)
	require.ErrorContains(t, err, "not supported")
}

func TestNoneExecutor_ResolvePath_LeavesPathUnchanged(t *testing.T) {
	n := NewNoneExecutor()
	got, err := n.ResolvePath("foo/bar.go")
	require.NoError(t, err)
	require.Equal(t, "foo/bar.go", got)
}

func TestNoneExecutor_Capabilities_AllDisabled(t *testing.T) {
	n := NewNoneExecutor()
	caps := n.Capabilities()
	require.False(t, caps.CanRead)
	require.False(t, caps.CanWrite)
	require.False(t, caps.CanExec)
	require.False(t, caps.CanNetwork)
}
