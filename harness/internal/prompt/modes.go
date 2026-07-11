package prompt

import (
	"embed"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"text/template"
)

//go:embed systemprompts/*.md
var systemPromptsFS embed.FS

var (
	modePromptsOnce sync.Once
	modePromptsMap  map[string]string
)

// ModePrompts returns a map of mode name to system prompt text, loaded from
// the embedded systemprompts/*.md files. The map key is the filename without
// the .md extension (e.g. "execution", "planning"). Content is trimmed of
// leading/trailing whitespace.
//
// This function is safe for concurrent use. It panics if any embedded file
// cannot be read, since that indicates a build-time packaging error that
// should never reach production.
func ModePrompts() map[string]string {
	modePromptsOnce.Do(func() {
		entries, err := systemPromptsFS.ReadDir("systemprompts")
		if err != nil {
			panic("prompt: failed to read embedded systemprompts directory: " + err.Error())
		}

		modePromptsMap = make(map[string]string, len(entries))
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if filepath.Ext(name) != ".md" {
				continue
			}

			data, err := systemPromptsFS.ReadFile("systemprompts/" + name)
			if err != nil {
				panic("prompt: failed to read embedded file " + name + ": " + err.Error())
			}

			mode := strings.TrimSuffix(name, ".md")
			modePromptsMap[mode] = strings.TrimSpace(string(data))
		}
	})
	return modePromptsMap
}

var (
	modeTemplatesOnce sync.Once
	modeTemplatesMap  map[string]*template.Template
)

// modeTemplates parses each embedded mode prompt as a Go text/template.
// Like ModePrompts, it panics on failure: an embedded prompt that does
// not parse is a build-time packaging error (typically a stray "{{" from
// a prompt edit) that the parse-all test in modes_test.go catches before
// it can ship.
func modeTemplates() map[string]*template.Template {
	modeTemplatesOnce.Do(func() {
		prompts := ModePrompts()
		modeTemplatesMap = make(map[string]*template.Template, len(prompts))
		for mode, text := range prompts {
			tmpl, err := template.New(mode).Parse(text)
			if err != nil {
				panic("prompt: embedded system prompt " + mode + ".md does not parse as a text/template: " + err.Error())
			}
			modeTemplatesMap[mode] = tmpl
		}
	})
	return modeTemplatesMap
}

// RenderModePrompt renders the embedded system prompt template for the
// given mode against the prompt model carried in pc (see TemplateData for
// the surface templates can use). A model matching neither tier table
// renders the base prompt text only, so unrecognised models always get a
// functional prompt.
func RenderModePrompt(mode string, pc PromptContext) (string, error) {
	tmpl, ok := modeTemplates()[mode]
	if !ok {
		return "", fmt.Errorf("unknown mode: %q", mode)
	}
	var sb strings.Builder
	if err := tmpl.Execute(&sb, TemplateData{Model: pc.Model, Mode: mode}); err != nil {
		return "", fmt.Errorf("render mode prompt %q: %w", mode, err)
	}
	return strings.TrimSpace(sb.String()), nil
}
