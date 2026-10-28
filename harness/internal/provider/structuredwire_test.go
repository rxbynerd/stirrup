package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/tool/builtins"
	"github.com/rxbynerd/stirrup/types"
)

// structuredWireWorkspace copies this package's non-test Go sources into a
// temporary workspace, and mirrors the file names (not contents) of the
// harness/internal tree under tree/, giving the real read, search and find
// tools realistic input.
func structuredWireWorkspace(t *testing.T) executor.Executor {
	t.Helper()
	must := require.New(t)
	dir := t.TempDir()
	err := filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		rel, err := filepath.Rel("..", path)
		if err != nil {
			return err
		}
		target := filepath.Join(dir, "tree", rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return os.WriteFile(target, nil, 0o600)
	})
	must.NoError(err, "mirror source tree")

	sources, err := filepath.Glob("*.go")
	must.NoError(err, "glob sources")
	for _, src := range sources {
		if strings.HasSuffix(src, "_test.go") {
			continue
		}
		data, err := os.ReadFile(src)
		must.NoError(err, "read %s", src)
		must.NoError(os.WriteFile(filepath.Join(dir, src), data, 0o600), "write %s", src)
	}

	exec, err := executor.NewLocalExecutor(dir)
	must.NoError(err, "new local executor")
	return exec
}

type structuredWireCase struct {
	name  string
	tool  func(executor.Executor) *tool.Tool
	input string
}

var structuredWireCases = []structuredWireCase{
	{name: "read_file", tool: builtins.ReadFileTool, input: `{"path":"anthropic.go"}`},
	{name: "grep_files", tool: builtins.GrepFilesTool, input: `{"pattern":"func ","include":["*.go"],"exclude":["tree/*"]}`},
	{name: "find_files", tool: builtins.FindFilesTool, input: `{"name":"*.go","path":"tree"}`},
}

func runStructuredTool(t *testing.T, exec executor.Executor, c structuredWireCase) types.ContentBlock {
	t.Helper()
	must := require.New(t)
	res, err := c.tool(exec).StructuredHandler(context.Background(), json.RawMessage(c.input))
	must.NoError(err, "%s", c.name)
	must.NotEmpty(res.Structured, "%s returned no structured envelope", c.name)
	must.NotEmpty(res.Kind, "%s returned no structured envelope kind", c.name)
	return types.ContentBlock{
		Type:       "tool_result",
		ToolUseID:  "call_1",
		Content:    res.Text,
		Structured: res.Structured,
		Kind:       res.Kind,
	}
}

func toolResultHistory(block types.ContentBlock) []types.Message {
	return []types.Message{
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "look"}}},
		{Role: "assistant", Content: []types.ContentBlock{
			{Type: "tool_use", ID: block.ToolUseID, Name: "read_file", Input: json.RawMessage(`{}`)},
		}},
		{Role: "user", Content: []types.ContentBlock{block}},
	}
}

// anthropicEnvelopePart returns the fields of the JSON envelope that the
// Anthropic tool_result content carries as its second part.
func anthropicEnvelopePart(t *testing.T, content json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	must := require.New(t)
	var parts []anthropicToolResultPart
	must.NoError(json.Unmarshal(content, &parts), "tool_result content is not the array form")
	must.Len(parts, 2, "expected text and envelope parts")
	var fields map[string]json.RawMessage
	must.NoError(json.Unmarshal([]byte(parts[1].Text), &fields), "envelope part is not a JSON object")
	return fields
}

// TestAnthropicStructuredToolResult_WireSizeBound pins that a structured
// read, search or find result costs at most 1.15 times its text rendering
// on the Anthropic wire, measured against both the raw text and the text's
// own JSON-string encoding.
func TestAnthropicStructuredToolResult_WireSizeBound(t *testing.T) {
	exec := structuredWireWorkspace(t)
	capability := quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "anthropic", Model: "claude-sonnet-4-5"}).StructuredToolResults
	for _, c := range structuredWireCases {
		t.Run(c.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			block := runStructuredTool(t, exec, c)
			wire := anthropicToolResultContent(block, capability)
			anthropicEnvelopePart(t, wire)
			textOnly, err := json.Marshal(block.Content)
			must.NoError(err, "marshal text")

			rawRatio := float64(len(wire)) / float64(len(block.Content))
			encodedRatio := float64(len(wire)) / float64(len(textOnly))
			t.Logf("text=%d structured=%d wire=%d raw ratio=%.3f encoded ratio=%.3f",
				len(block.Content), len(block.Structured), len(wire), rawRatio, encodedRatio)
			check.LessOrEqual(rawRatio, 1.15, "wire size relative to the text rendering")
			check.LessOrEqual(encodedRatio, 1.15, "wire size relative to the text-only encoding")
		})
	}
}

// TestStructuredToolResult_MetadataReachesAnthropicAndGemini pins that the
// metadata the text rendering does not show still reaches the model on both
// providers, and that the duplicated bodies do not.
func TestStructuredToolResult_MetadataReachesAnthropicAndGemini(t *testing.T) {
	exec := structuredWireWorkspace(t)
	cases := []struct {
		structuredWireCase
		want    map[string]string
		absent  []string
		present []string
	}{
		{
			structuredWireCase: structuredWireCases[0],
			want:               map[string]string{"path": `"anthropic.go"`, "start_line": "1", "truncated": "false"},
			present:            []string{"end_line"},
			absent:             []string{"lines"},
		},
		{
			structuredWireCase: structuredWireCase{name: "read_file past EOF", tool: builtins.ReadFileTool, input: `{"path":"anthropic.go","start_line":999999}`},
			want:               map[string]string{"path": `"anthropic.go"`, "start_line": "999999", "past_eof": "true"},
			present:            []string{"end_line", "truncated"},
			absent:             []string{"lines"},
		},
		{
			structuredWireCase: structuredWireCase{name: "read_file window", tool: builtins.ReadFileTool, input: `{"path":"anthropic.go","start_line":10,"limit":5}`},
			want:               map[string]string{"start_line": "10", "end_line": "14", "truncated": "true"},
			absent:             []string{"lines"},
		},
		{
			structuredWireCase: structuredWireCase{name: "grep_files capped", tool: builtins.GrepFilesTool, input: `{"pattern":"func ","max_results":3}`},
			want:               map[string]string{"truncated": "true"},
			absent:             []string{"matches"},
		},
		{
			structuredWireCase: structuredWireCase{name: "find_files capped", tool: builtins.FindFilesTool, input: `{"name":"*.go","max_results":2}`},
			want:               map[string]string{"truncated": "true"},
			absent:             []string{"paths"},
		},
	}
	anthropicCap := quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "anthropic", Model: "claude-sonnet-4-5"})
	geminiCap := quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "gemini", Model: "gemini-2.5-pro"})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			check := assert.New(t)
			must := require.New(t)
			block := runStructuredTool(t, exec, c.structuredWireCase)
			assertEnvelope := func(provider string, fields map[string]json.RawMessage) {
				t.Helper()
				for k, v := range c.want {
					check.Equal(v, string(fields[k]), "%s: %s", provider, k)
				}
				for _, k := range c.present {
					check.Contains(fields, k, "%s: %s missing from the envelope", provider, k)
				}
				for _, k := range c.absent {
					check.NotContains(fields, k, "%s: duplicated body field %s sent", provider, k)
				}
			}

			raw := mustMarshal(t, buildAnthropicRequest(types.StreamParams{
				Model: "claude-sonnet-4-5", MaxTokens: 1024, Tools: anthropicTools(),
				Messages: toolResultHistory(block),
			}, true, anthropicCap))
			var decoded struct {
				Messages []struct {
					Content []struct {
						Content json.RawMessage `json:"content"`
					} `json:"content"`
				} `json:"messages"`
			}
			must.NoError(json.Unmarshal(raw, &decoded), "decode anthropic body")
			assertEnvelope("anthropic", anthropicEnvelopePart(t, decoded.Messages[2].Content[0].Content))

			body, _, err := BuildGenerateContentRequest(types.StreamParams{
				Model: "gemini-2.5-pro", Tools: anthropicTools(), Messages: toolResultHistory(block),
			}, nil, geminiCap)
			must.NoError(err, "build gemini request")
			var resp struct {
				Content    string                     `json:"content"`
				Structured map[string]json.RawMessage `json:"structured"`
				Kind       string                     `json:"kind"`
			}
			must.NoError(json.Unmarshal(geminiFunctionResponseFromBody(t, body), &resp), "decode gemini response")
			must.Equal(block.Content, resp.Content, "gemini: content is not the canonical text")
			must.Equal(block.Kind, resp.Kind, "gemini: kind")
			assertEnvelope("gemini", resp.Structured)
		})
	}
}

// TestStructuredToolResult_EncodingLeavesHistoryEnvelopeIntact pins that
// building the wire view never rewrites the full envelope held in history,
// which is what the trace and later turns read.
func TestStructuredToolResult_EncodingLeavesHistoryEnvelopeIntact(t *testing.T) {
	must := require.New(t)
	exec := structuredWireWorkspace(t)
	block := runStructuredTool(t, exec, structuredWireCases[0])
	original := append(json.RawMessage(nil), block.Structured...)
	history := toolResultHistory(block)

	_ = mustMarshal(t, buildAnthropicRequest(types.StreamParams{
		Model: "claude-sonnet-4-5", MaxTokens: 1024, Messages: history,
	}, true, quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "anthropic", Model: "claude-sonnet-4-5"})))
	_, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro", Messages: history,
	}, nil, quirks.DefaultRegistry().Resolve(quirks.Target{ProviderType: "gemini", Model: "gemini-2.5-pro"}))
	must.NoError(err, "build gemini request")

	got := history[2].Content[0].Structured
	must.Equal(string(original), string(got), "history envelope changed by encoding")

	var excerpt struct {
		Lines []string `json:"lines"`
	}
	must.NoError(json.Unmarshal(got, &excerpt), "history envelope is not a JSON object")
	must.NotEmpty(excerpt.Lines, "history envelope lost its lines")
}

// TestGeminiToolResultResponse_IdenticalStructuredCollapses pins that an
// envelope byte-identical to the text is sent once, as content only.
func TestGeminiToolResultResponse_IdenticalStructuredCollapses(t *testing.T) {
	must := require.New(t)
	payload := `{"ref":"stirrup://command-output/abc/stdout","content":"chunk bytes"}`
	raw, err := geminiToolResultResponse(types.ContentBlock{
		Type: "tool_result", ToolUseID: "call_1", Content: payload,
		Structured: json.RawMessage(payload), Kind: types.ToolResultKindCommandOutputChunk,
	}, quirks.StructuredToolResultCapability{Supported: true, ObjectResponse: true})
	must.NoError(err, "geminiToolResultResponse")

	want := mustMarshal(t, geminiFunctionResponseBody{Content: payload})
	must.Equal(string(want), string(raw), "identical envelope must collapse to content only")
}
