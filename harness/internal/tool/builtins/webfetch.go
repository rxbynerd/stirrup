package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
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

type webFetchOptions struct {
	client            *http.Client
	allowPrivateHosts bool
}

// WebFetchTool returns a tool that fetches a URL and returns the response body
// as text, truncated at 100 KB.
func WebFetchTool() *tool.Tool {
	return newWebFetchTool(webFetchOptions{})
}

func newWebFetchTool(opts webFetchOptions) *tool.Tool {
	client := opts.client
	if client == nil {
		client = &http.Client{
			Timeout: fetchTimeout,
			Transport: &http.Transport{
				DialContext: safeFetchDialContext(opts.allowPrivateHosts),
			},
		}
	}

	return &tool.Tool{
		Name:        "web_fetch",
		Description: "Fetch a URL via HTTP GET and return the response body as text. Response is truncated at 100KB.",
		InputSchema: webFetchSchema,
		SideEffects: true,
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
			parsedURL, err := validateFetchURL(params.URL, opts.allowPrivateHosts)
			if err != nil {
				return "", err
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
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

func validateFetchURL(rawURL string, allowPrivateHosts bool) (*url.URL, error) {
	parsedURL, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return nil, fmt.Errorf("url must start with http:// or https://")
	}
	host := parsedURL.Hostname()
	if host == "" {
		return nil, fmt.Errorf("url must include a host")
	}
	if !allowPrivateHosts {
		if err := validatePublicHost(host); err != nil {
			return nil, err
		}
	}
	return parsedURL, nil
}

func safeFetchDialContext(allowPrivateHosts bool) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: fetchTimeout}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if !allowPrivateHosts {
			if err := validatePublicHost(host); err != nil {
				return nil, err
			}
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(host, port))
	}
}

func validatePublicHost(host string) error {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return fmt.Errorf("url must include a host")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return fmt.Errorf("refusing to fetch private host %q", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !isPublicIP(ip) {
			return fmt.Errorf("refusing to fetch private host %q", host)
		}
		return nil
	}

	addrs, err := net.DefaultResolver.LookupIPAddr(context.Background(), host)
	if err != nil {
		return fmt.Errorf("resolve host %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("resolve host %q: no addresses found", host)
	}
	for _, addr := range addrs {
		if !isPublicIP(addr.IP) {
			return fmt.Errorf("refusing to fetch private host %q", host)
		}
	}
	return nil
}

func isPublicIP(ip net.IP) bool {
	return !(ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsMulticast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsUnspecified())
}
