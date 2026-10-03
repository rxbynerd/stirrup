package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"

	"github.com/rxbynerd/stirrup/types"
)

// LoadJudgeLLMConfig reads a standalone judge llm configuration. A ".json"
// file holds a types.JudgeLLMConfig object; any other file is HCL holding
// either the attributes of a suite's `llm` block or a single `llm` block.
// The result is not validated; callers resolve it with
// judge.ResolveLLMConfig.
func LoadJudgeLLMConfig(path string) (types.JudgeLLMConfig, error) {
	if info, err := os.Stat(path); err == nil && info.Size() > MaxSuiteBytes {
		return types.JudgeLLMConfig{}, fmt.Errorf("judge config %s is %d bytes, exceeds limit of %d", path, info.Size(), MaxSuiteBytes)
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return types.JudgeLLMConfig{}, fmt.Errorf("reading judge config: %w", err)
	}
	if strings.EqualFold(filepath.Ext(path), ".json") {
		return parseJudgeLLMConfigJSON(src)
	}
	return parseJudgeLLMConfigHCL(src, path)
}

func parseJudgeLLMConfigJSON(src []byte) (types.JudgeLLMConfig, error) {
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.DisallowUnknownFields()
	var cfg types.JudgeLLMConfig
	if err := dec.Decode(&cfg); err != nil {
		return types.JudgeLLMConfig{}, fmt.Errorf("decoding judge config: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return types.JudgeLLMConfig{}, errors.New("decoding judge config: data after the top-level object")
	}
	return cfg, nil
}

func parseJudgeLLMConfigHCL(src []byte, path string) (types.JudgeLLMConfig, error) {
	parser := hclparse.NewParser()
	file, diags := parser.ParseHCL(src, path)
	if diags.HasErrors() {
		return types.JudgeLLMConfig{}, judgeConfigDiagnostics(diags)
	}
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return types.JudgeLLMConfig{}, errors.New("judge config is not native HCL syntax")
	}
	var spec llmSpec
	switch {
	case len(body.Blocks) == 0:
		if d := gohcl.DecodeBody(body, nil, &spec); d.HasErrors() {
			return types.JudgeLLMConfig{}, judgeConfigDiagnostics(d)
		}
	case len(body.Blocks) == 1 && len(body.Attributes) == 0 && body.Blocks[0].Type == "llm" && len(body.Blocks[0].Labels) == 0:
		if d := gohcl.DecodeBody(body.Blocks[0].Body, nil, &spec); d.HasErrors() {
			return types.JudgeLLMConfig{}, judgeConfigDiagnostics(d)
		}
	default:
		return types.JudgeLLMConfig{}, errors.New("judge config must hold llm attributes or a single unlabelled llm block, not both or other blocks")
	}
	return llmSpecToType(&spec), nil
}

// judgeConfigDiagnostics renders diags by position and message only. Unlike
// formatDiagnostics it never quotes the source, where a line may hold a key
// pasted in place of its secret:// reference.
func judgeConfigDiagnostics(diags hcl.Diagnostics) error {
	msgs := make([]string, 0, len(diags))
	for _, d := range diags {
		msgs = append(msgs, d.Error())
	}
	return fmt.Errorf("hcl: %s", strings.Join(msgs, "\n"))
}
