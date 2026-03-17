package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const githubAPIBaseURL = "https://api.github.com"

// APIExecutor implements the Executor interface for read-only modes backed by
// the GitHub REST API. It supports reading files and listing directories from
// a specific repository ref. Write and exec operations return errors since
// the API executor is designed for review/research modes that do not modify
// the workspace.
type APIExecutor struct {
	client  *http.Client
	token   string
	owner   string
	repo    string
	ref     string
	baseURL string // overridable for testing
}

// NewAPIExecutor creates an executor that reads from a GitHub repository
// via the REST API. The token is used for Bearer authentication.
func NewAPIExecutor(token, owner, repo, ref string) *APIExecutor {
	return &APIExecutor{
		client:  &http.Client{Timeout: 30 * time.Second},
		token:   token,
		owner:   owner,
		repo:    repo,
		ref:     ref,
		baseURL: githubAPIBaseURL,
	}
}

// ReadFile fetches the raw content of a file from the repository.
func (a *APIExecutor) ReadFile(ctx context.Context, path string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/contents/%s", a.baseURL, a.owner, a.repo, path)
	if a.ref != "" {
		url += "?ref=" + a.ref
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("api executor: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Accept", "application/vnd.github.v3.raw")

	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("api executor: read file %q: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("api executor: read file %q: HTTP %d", path, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("api executor: read file %q body: %w", path, err)
	}

	return string(body), nil
}

// WriteFile is not supported by the API executor.
func (a *APIExecutor) WriteFile(_ context.Context, _ string, _ string) error {
	return fmt.Errorf("api executor: write operations not supported")
}

// githubContentEntry represents a single entry in a GitHub directory listing.
type githubContentEntry struct {
	Name string `json:"name"`
}

// ListDirectory fetches the contents of a directory from the repository.
func (a *APIExecutor) ListDirectory(ctx context.Context, path string) ([]string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/contents/%s", a.baseURL, a.owner, a.repo, path)
	if a.ref != "" {
		url += "?ref=" + a.ref
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("api executor: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("api executor: list directory %q: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("api executor: list directory %q: HTTP %d", path, resp.StatusCode)
	}

	var entries []githubContentEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("api executor: list directory %q: decode response: %w", path, err)
	}

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	return names, nil
}

// Exec is not supported by the API executor.
func (a *APIExecutor) Exec(_ context.Context, _ string, _ time.Duration) (*ExecResult, error) {
	return nil, fmt.Errorf("api executor: command execution not supported")
}

// ResolvePath returns the path as-is since there is no local filesystem.
func (a *APIExecutor) ResolvePath(path string) (string, error) {
	return path, nil
}

// Capabilities returns the read-only capabilities of the API executor.
func (a *APIExecutor) Capabilities() ExecutorCapabilities {
	return ExecutorCapabilities{
		CanRead:    true,
		CanWrite:   false,
		CanExec:    false,
		CanNetwork: true,
	}
}
