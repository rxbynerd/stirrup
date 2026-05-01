package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

const (
	maxFileSize     = 10 * 1024 * 1024 // 10 MB
	maxOutputSize   = 1 * 1024 * 1024  // 1 MB
	defaultTimeout  = 30 * time.Second
	maxTimeout      = 5 * time.Minute
	truncatedSuffix = "\n[output truncated at 1MB]"
)

// SecurityEventEmitter is an optional interface for emitting structured security events.
type SecurityEventEmitter interface {
	PathTraversalBlocked(path, workspace string)
	FileSizeLimitExceeded(path string, size, limit int64)
	OutputTruncated(command string, originalSize, limit int)
}

// LocalExecutor implements Executor by performing operations directly on the
// local filesystem, sandboxed to a workspace directory.
type LocalExecutor struct {
	workspace string // absolute, symlink-resolved workspace root
	Security  SecurityEventEmitter
}

// LocalExecutorConfig configures NewLocalExecutorWithConfig. NewLocalExecutor
// is preserved for callers that do not need the additional fields.
type LocalExecutorConfig struct {
	// Workspace is the directory the executor is rooted at.
	Workspace string
	// Network describes the network policy the harness intends. The local
	// executor cannot enforce an egress allowlist (no sandbox boundary) so
	// constructing one with Mode == "allowlist" returns an error here.
	Network *types.NetworkConfig
}

// NewLocalExecutor creates an executor rooted at the given workspace directory.
// The workspace path is resolved to an absolute, symlink-free canonical form.
func NewLocalExecutor(workspace string) (*LocalExecutor, error) {
	return NewLocalExecutorWithConfig(LocalExecutorConfig{Workspace: workspace})
}

// NewLocalExecutorWithConfig is the configurable constructor. It currently
// adds the Network policy check; future fields will continue to be added
// here rather than as positional arguments to NewLocalExecutor.
func NewLocalExecutorWithConfig(cfg LocalExecutorConfig) (*LocalExecutor, error) {
	if cfg.Network != nil && cfg.Network.Mode == "allowlist" {
		return nil, fmt.Errorf("local executor does not support allowlist networking; use a container executor")
	}

	abs, err := filepath.Abs(cfg.Workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("eval symlinks for workspace: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("stat workspace: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace is not a directory: %s", resolved)
	}
	return &LocalExecutor{workspace: resolved}, nil
}

// ResolvePath resolves a relative path against the workspace root and verifies
// the result is contained within the workspace. It rejects path traversal
// attempts and symlinks that escape the workspace.
func (e *LocalExecutor) ResolvePath(relativePath string) (string, error) {
	if filepath.IsAbs(relativePath) {
		// Allow absolute paths only if they fall within the workspace.
		resolved, err := filepath.EvalSymlinks(relativePath)
		if err != nil {
			// The path may not exist yet (e.g., for writes). Evaluate the
			// parent and append the base name.
			resolved, err = resolveNewPath(relativePath)
			if err != nil {
				return "", fmt.Errorf("resolve absolute path: %w", err)
			}
		}
		if !isWithin(resolved, e.workspace) {
			if e.Security != nil {
				e.Security.PathTraversalBlocked(relativePath, e.workspace)
			}
			return "", fmt.Errorf("path escapes workspace: %s", relativePath)
		}
		return resolved, nil
	}

	joined := filepath.Join(e.workspace, relativePath)
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		// Path may not exist yet; resolve the parent instead.
		resolved, err = resolveNewPath(joined)
		if err != nil {
			return "", fmt.Errorf("resolve path: %w", err)
		}
	}
	if !isWithin(resolved, e.workspace) {
		if e.Security != nil {
			e.Security.PathTraversalBlocked(relativePath, e.workspace)
		}
		return "", fmt.Errorf("path escapes workspace: %s", relativePath)
	}
	return resolved, nil
}

// ReadFile reads a file from the workspace. The file must be within the
// workspace and no larger than 10 MB.
func (e *LocalExecutor) ReadFile(ctx context.Context, path string) (string, error) {
	resolved, err := e.ResolvePath(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat file: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("path is a directory, not a file: %s", path)
	}
	if info.Size() > maxFileSize {
		if e.Security != nil {
			e.Security.FileSizeLimitExceeded(path, info.Size(), maxFileSize)
		}
		return "", fmt.Errorf("file too large: %d bytes (max %d)", info.Size(), maxFileSize)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	return string(data), nil
}

// WriteFile writes content to a file in the workspace. Parent directories are
// created as needed. Content must not exceed 10 MB.
func (e *LocalExecutor) WriteFile(ctx context.Context, path string, content string) error {
	if len(content) > maxFileSize {
		if e.Security != nil {
			e.Security.FileSizeLimitExceeded(path, int64(len(content)), maxFileSize)
		}
		return fmt.Errorf("content too large: %d bytes (max %d)", len(content), maxFileSize)
	}
	resolved, err := e.ResolvePath(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return fmt.Errorf("create parent directories: %w", err)
	}
	if err := os.WriteFile(resolved, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	return nil
}

// ListDirectory lists the entries in a directory within the workspace.
func (e *LocalExecutor) ListDirectory(ctx context.Context, path string) ([]string, error) {
	resolved, err := e.ResolvePath(path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return nil, fmt.Errorf("read directory: %w", err)
	}
	names := make([]string, len(entries))
	for i, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		names[i] = name
	}
	return names, nil
}

// Exec runs a shell command in the workspace directory with the given timeout.
// A zero timeout uses the default (30s). Output is truncated at 1 MB.
// The process is killed if the context or timeout expires.
func (e *LocalExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (*ExecResult, error) {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = e.workspace
	cmd.Env = filteredCommandEnv()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	result := &ExecResult{
		Stdout: truncate(stdout.String(), maxOutputSize),
		Stderr: truncate(stderr.String(), maxOutputSize),
	}

	if e.Security != nil {
		combinedSize := stdout.Len() + stderr.Len()
		if combinedSize > maxOutputSize {
			e.Security.OutputTruncated(command, combinedSize, maxOutputSize)
		}
	}

	if err != nil {
		// Check context cancellation first — a killed process also produces
		// an ExitError, but the root cause is the timeout.
		if ctx.Err() != nil {
			return result, fmt.Errorf("command timed out after %s", timeout)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		} else {
			return result, fmt.Errorf("exec command: %w", err)
		}
	}

	return result, nil
}

var allowedCommandEnvKeys = map[string]bool{
	"CC":              true,
	"CGO_ENABLED":     true,
	"CI":              true,
	"COLORTERM":       true,
	"CXX":             true,
	"GOENV":           true,
	"GOCACHE":         true,
	"GOMAXPROCS":      true,
	"GOMODCACHE":      true,
	"GOPATH":          true,
	"GOROOT":          true,
	"GOTOOLCHAIN":     true,
	"HOME":            true,
	"LANG":            true,
	"LOGNAME":         true,
	"MAKEFLAGS":       true,
	"NO_COLOR":        true,
	"PATH":            true,
	"PKG_CONFIG":      true,
	"SHELL":           true,
	"TEMP":            true,
	"TERM":            true,
	"TMP":             true,
	"TMPDIR":          true,
	"TZ":              true,
	"USER":            true,
	"XDG_CACHE_HOME":  true,
	"XDG_CONFIG_HOME": true,
	"XDG_DATA_HOME":   true,
}

func filteredCommandEnv() []string {
	raw := os.Environ()
	filtered := make([]string, 0, len(raw))
	for _, entry := range raw {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if allowedCommandEnvKeys[key] || strings.HasPrefix(key, "LC_") {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

// Capabilities returns the capabilities of the local executor.
func (e *LocalExecutor) Capabilities() ExecutorCapabilities {
	return ExecutorCapabilities{
		CanRead:    true,
		CanWrite:   true,
		CanExec:    true,
		CanNetwork: true,
		MaxTimeout: maxTimeout,
	}
}

// isWithin reports whether child is contained within parent. Both paths must
// be absolute and cleaned.
func isWithin(child, parent string) bool {
	// Ensure parent ends with separator so "/workspace-evil" doesn't match "/workspace".
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..") && rel != ".."
}

// resolveNewPath resolves a path that may not exist yet by walking up the
// directory tree until an existing ancestor is found, resolving symlinks on
// that ancestor, and re-appending the non-existent tail segments.
func resolveNewPath(path string) (string, error) {
	path = filepath.Clean(path)
	// Collect path segments that don't exist yet.
	var tail []string
	current := path
	for {
		_, err := os.Lstat(current)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		tail = append([]string{filepath.Base(current)}, tail...)
		parent := filepath.Dir(current)
		if parent == current {
			// Reached filesystem root without finding existing path.
			return "", fmt.Errorf("no existing ancestor for path: %s", path)
		}
		current = parent
	}
	resolved, err := filepath.EvalSymlinks(current)
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{resolved}, tail...)...), nil
}

// truncate shortens s to maxLen bytes, appending a truncation notice if cut.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + truncatedSuffix
}
