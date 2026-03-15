package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rubynerd/stirrup/harness/internal/tool"
)

const (
	maxFetchSize    = 100 * 1024 // 100 KB
	fetchTimeout    = 30 * time.Second
	fetchUserAgent  = "stirrup-harness/1.0"
	truncatedNotice = "\n[response truncated at 100KB]"
)

// webFetchSchema is the JSON Schema for the web_fetch tool input.
var webFetchSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"url": {
			"type": "string",
			"description": "The URL to fetch. Must be an HTTP or HTTPS URL."
		}
	},
	"required": ["url"],
	"additionalProperties": false
}`)

// WebFetchTool returns a tool that fetches a URL and returns the response body
// as text, truncated at 100 KB. This tool does not require an executor since
// it performs its own HTTP request.
func WebFetchTool() *tool.Tool {
	client := &http.Client{Timeout: fetchTimeout}

	return &tool.Tool{
		Name:        "web_fetch",
		Description: "Fetch a URL via HTTP GET and return the response body as text. Response is truncated at 100KB.",
		InputSchema: webFetchSchema,
		SideEffects: false,
		Handler: func(ctx context.Context, input json.RawMessage) (string, error) {
			var params struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("parse input: %w", err)
			}
			if params.URL == "" {
				return "", fmt.Errorf("url is required")
			}
			if !strings.HasPrefix(params.URL, "http://") && !strings.HasPrefix(params.URL, "https://") {
				return "", fmt.Errorf("url must start with http:// or https://")
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, params.URL, nil)
			if err != nil {
				return "", fmt.Errorf("create request: %w", err)
			}
			req.Header.Set("User-Agent", fetchUserAgent)

			resp, err := client.Do(req)
			if err != nil {
				return "", fmt.Errorf("fetch URL: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return "", fmt.Errorf("HTTP %d %s", resp.StatusCode, resp.Status)
			}

			// Read up to maxFetchSize + 1 to detect truncation.
			limited := io.LimitReader(resp.Body, maxFetchSize+1)
			body, err := io.ReadAll(limited)
			if err != nil {
				return "", fmt.Errorf("read response body: %w", err)
			}

			if len(body) > maxFetchSize {
				return string(body[:maxFetchSize]) + truncatedNotice, nil
			}
			return string(body), nil
		},
	}
}
