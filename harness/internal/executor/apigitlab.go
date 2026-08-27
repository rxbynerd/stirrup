package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const gitlabAPIBaseURL = "https://gitlab.com/api/v4"

// gitlabTreePageSize is the largest per_page the repository tree endpoint
// honours; without it GitLab returns only the first 20 entries of a
// directory. gitlabTreeMaxPages bounds a single listing at the same
// 1000-entry ceiling the GitHub contents API imposes.
const (
	gitlabTreePageSize = 100
	gitlabTreeMaxPages = 10
)

// GitLabExecutor implements the Executor interface for read-only modes backed
// by the GitLab REST API. It supports reading files and listing directories
// from a specific project ref. Write and exec operations return errors since
// the API executor is designed for review/research modes that do not modify
// the workspace.
type GitLabExecutor struct {
	client *http.Client
	token  string
	// project is the full namespace path ("group/subgroup/project"), sent
	// URL-encoded as the ":id" path parameter of every projects endpoint.
	project string
	ref     string
	baseURL string // overridable for testing
}

// NewGitLabExecutor creates an executor that reads from a GitLab project via
// the REST API. The token authenticates as a personal, project, or group
// access token; an empty token selects unauthenticated access, which GitLab
// allows for public projects.
func NewGitLabExecutor(token, project, ref string) *GitLabExecutor {
	return &GitLabExecutor{
		client:  &http.Client{Timeout: 30 * time.Second},
		token:   token,
		project: project,
		ref:     ref,
		baseURL: gitlabAPIBaseURL,
	}
}

// ReadFile fetches the raw content of a file from the project.
func (g *GitLabExecutor) ReadFile(ctx context.Context, path string) (string, error) {
	filePath := gitlabRepoPath(path)
	if filePath == "" {
		return "", fmt.Errorf("gitlab api executor: read file: path is empty")
	}

	endpoint, err := g.projectURL("repository/files/"+url.PathEscape(filePath)+"/raw", nil)
	if err != nil {
		return "", fmt.Errorf("gitlab api executor: build request URL: %w", err)
	}

	resp, err := g.get(ctx, endpoint, "")
	if err != nil {
		return "", fmt.Errorf("gitlab api executor: read file %q: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gitlab api executor: read file %q: HTTP %d", path, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("gitlab api executor: read file %q body: %w", path, err)
	}
	return string(body), nil
}

// WriteFile is not supported by the API executor.
func (g *GitLabExecutor) WriteFile(_ context.Context, _ string, _ string) error {
	return fmt.Errorf("gitlab api executor: write operations not supported")
}

// gitlabTreeEntry represents a single entry in a GitLab repository tree.
type gitlabTreeEntry struct {
	Name string `json:"name"`
	Type string `json:"type"` // "tree" | "blob" | "commit" (submodule)
}

// ListDirectory fetches the entries of a directory from the project.
// Directory entries carry a trailing slash, matching every other
// read-capable executor.
func (g *GitLabExecutor) ListDirectory(ctx context.Context, path string) ([]string, error) {
	dirPath := gitlabRepoPath(path)
	names := make([]string, 0, gitlabTreePageSize)

	for page := 1; page <= gitlabTreeMaxPages; page++ {
		query := url.Values{}
		if dirPath != "" {
			query.Set("path", dirPath)
		}
		query.Set("per_page", strconv.Itoa(gitlabTreePageSize))
		query.Set("page", strconv.Itoa(page))

		endpoint, err := g.projectURL("repository/tree", query)
		if err != nil {
			return nil, fmt.Errorf("gitlab api executor: build request URL: %w", err)
		}

		entries, err := g.treePage(ctx, endpoint, path)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			name := e.Name
			if e.Type == "tree" {
				name += "/"
			}
			names = append(names, name)
		}
		if len(entries) < gitlabTreePageSize {
			break
		}
	}
	return names, nil
}

func (g *GitLabExecutor) treePage(ctx context.Context, endpoint, path string) ([]gitlabTreeEntry, error) {
	resp, err := g.get(ctx, endpoint, "application/json")
	if err != nil {
		return nil, fmt.Errorf("gitlab api executor: list directory %q: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gitlab api executor: list directory %q: HTTP %d", path, resp.StatusCode)
	}

	var entries []gitlabTreeEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("gitlab api executor: list directory %q: decode response: %w", path, err)
	}
	return entries, nil
}

// Exec is not supported by the API executor.
func (g *GitLabExecutor) Exec(_ context.Context, _ string, _ time.Duration) (*ExecResult, error) {
	return nil, fmt.Errorf("gitlab api executor: command execution not supported")
}

// ResolvePath returns the path as-is since there is no local filesystem.
func (g *GitLabExecutor) ResolvePath(path string) (string, error) {
	return path, nil
}

// Capabilities returns the read-only capabilities of the API executor.
func (g *GitLabExecutor) Capabilities() ExecutorCapabilities {
	return ExecutorCapabilities{
		CanRead:    true,
		CanWrite:   false,
		CanExec:    false,
		CanNetwork: true,
	}
}

func (g *GitLabExecutor) get(ctx context.Context, endpoint, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if g.token != "" {
		req.Header.Set("PRIVATE-TOKEN", g.token)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	return g.client.Do(req)
}

// projectURL builds a URL for an endpoint under /projects/:id. The project
// path and any repository path are single path segments, so their slashes
// stay percent-encoded.
func (g *GitLabExecutor) projectURL(suffix string, query url.Values) (string, error) {
	endpoint, err := url.Parse(fmt.Sprintf("%s/projects/%s/%s",
		strings.TrimRight(g.baseURL, "/"),
		url.PathEscape(g.project),
		suffix,
	))
	if err != nil {
		return "", err
	}
	if query == nil {
		query = url.Values{}
	}
	if g.ref != "" {
		query.Set("ref", g.ref)
	}
	if len(query) > 0 {
		endpoint.RawQuery = query.Encode()
	}
	return endpoint.String(), nil
}

func gitlabRepoPath(repoPath string) string {
	trimmed := strings.Trim(repoPath, "/")
	if trimmed == "." {
		return ""
	}
	return trimmed
}
