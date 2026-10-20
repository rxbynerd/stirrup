package executor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

var _ Executor = (*NoneExecutor)(nil)

func TestNoneExecutor_ReadFile_ReturnsNotSupported(t *testing.T) {
	must := require.New(t)
	n := NewNoneExecutor()
	_, err := n.ReadFile(context.Background(), "main.go")
	must.ErrorContains(err, "not supported")
}

func TestNoneExecutor_WriteFile_ReturnsNotSupported(t *testing.T) {
	must := require.New(t)
	n := NewNoneExecutor()
	err := n.WriteFile(context.Background(), "main.go", "content")
	must.ErrorContains(err, "not supported")
}

func TestNoneExecutor_ListDirectory_ReturnsNotSupported(t *testing.T) {
	must := require.New(t)
	n := NewNoneExecutor()
	_, err := n.ListDirectory(context.Background(), ".")
	must.ErrorContains(err, "not supported")
}

func TestNoneExecutor_Exec_ReturnsNotSupported(t *testing.T) {
	must := require.New(t)
	n := NewNoneExecutor()
	_, err := n.Exec(context.Background(), "echo hi", 0)
	must.ErrorContains(err, "not supported")
}

func TestNoneExecutor_ResolvePath_LeavesPathUnchanged(t *testing.T) {
	must := require.New(t)
	n := NewNoneExecutor()
	got, err := n.ResolvePath("foo/bar.go")
	must.NoError(err)
	must.Equal("foo/bar.go", got)
}

func TestNoneExecutor_Capabilities_AllDisabled(t *testing.T) {
	must := require.New(t)
	n := NewNoneExecutor()
	caps := n.Capabilities()
	must.False(caps.CanRead)
	must.False(caps.CanWrite)
	must.False(caps.CanExec)
	must.False(caps.CanNetwork)
}
