package prompt

import (
	"embed"
	"path/filepath"
	"strings"
	"sync"
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
