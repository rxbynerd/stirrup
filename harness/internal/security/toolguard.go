package security

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"
)

// ToolGuardFinding describes a prompt-injection tripwire match in tool input.
type ToolGuardFinding struct {
	Rule   string `json:"rule"`
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

var (
	commandToolPattern  = regexp.MustCompile(`(?i)(^|[\s;&|()])(?:curl|wget|nc|netcat|ncat|socat)(?:[\s;&|()]|$)`)
	base64TokenPattern  = regexp.MustCompile(`[A-Za-z0-9+/_-]{101,}={0,2}`)
	shellEscapePatterns = []*regexp.Regexp{
		regexp.MustCompile("`"),
		regexp.MustCompile(`\$\(`),
	}
)

var protectedWriteTargets = []string{
	"AGENTS.md",
	"CLAUDE.md",
	".codex",
	".agents",
	"harness/internal/prompt/systemprompts",
}

var credentialPathMatchers = []func(string) bool{
	func(p string) bool { return path.Base(p) == ".env" },
	func(p string) bool { return hasPathSegment(p, ".ssh") },
	func(p string) bool { return strings.Contains(p, ".aws/credentials") },
	func(p string) bool { return strings.Contains(p, ".docker/config.json") },
	func(p string) bool { return strings.Contains(p, ".kube/config") },
	func(p string) bool { return path.Base(p) == ".netrc" },
	func(p string) bool { return path.Base(p) == ".npmrc" },
	func(p string) bool { return path.Base(p) == ".pypirc" },
	func(p string) bool { return path.Base(p) == "id_rsa" },
	func(p string) bool { return path.Base(p) == "id_ed25519" },
	func(p string) bool { return strings.HasSuffix(path.Base(p), ".pem") },
	func(p string) bool { return strings.HasSuffix(path.Base(p), ".key") },
	func(p string) bool { return hasPathSegment(p, "credentials") || path.Base(p) == "credentials" },
}

// GuardToolCall inspects tool input for prompt-injection tripwires before
// dispatch. This is defense-in-depth and not a sandbox or permission boundary.
func GuardToolCall(toolName string, sideEffects bool, input json.RawMessage) []ToolGuardFinding {
	var decoded any
	if err := json.Unmarshal(input, &decoded); err != nil {
		return nil
	}

	var findings []ToolGuardFinding
	walkToolInput(decoded, "", func(field string, value string) {
		lowerField := strings.ToLower(field)
		if isCommandField(lowerField) {
			findings = append(findings, guardCommandField(field, value)...)
			if containsCredentialPath(value) {
				findings = append(findings, ToolGuardFinding{
					Rule:   "credential_path",
					Field:  field,
					Reason: "command references a credential path",
				})
			}
		}
		if looksPathLike(lowerField) && containsCredentialPath(value) {
			findings = append(findings, ToolGuardFinding{
				Rule:   "credential_path",
				Field:  field,
				Reason: "input references a credential path",
			})
		}
		if isWriteTool(toolName, sideEffects) && looksPathLike(lowerField) && targetsHarnessConfig(value) {
			findings = append(findings, ToolGuardFinding{
				Rule:   "protected_write_target",
				Field:  field,
				Reason: "write targets harness configuration",
			})
		}
		if base64TokenPattern.MatchString(value) {
			findings = append(findings, ToolGuardFinding{
				Rule:   "encoded_payload",
				Field:  field,
				Reason: "input contains a base64-like payload longer than 100 characters",
			})
		}
	})

	return findings
}

func guardCommandField(field, value string) []ToolGuardFinding {
	var findings []ToolGuardFinding
	if commandToolPattern.MatchString(value) {
		findings = append(findings, ToolGuardFinding{
			Rule:   "exfiltration_command",
			Field:  field,
			Reason: "command invokes a network exfiltration utility",
		})
	}
	for _, p := range shellEscapePatterns {
		if p.MatchString(value) {
			findings = append(findings, ToolGuardFinding{
				Rule:   "shell_escape",
				Field:  field,
				Reason: "command contains shell escape syntax",
			})
			break
		}
	}
	return findings
}

func walkToolInput(value any, field string, visit func(string, string)) {
	switch v := value.(type) {
	case map[string]any:
		for k, child := range v {
			next := k
			if field != "" {
				next = field + "." + k
			}
			walkToolInput(child, next, visit)
		}
	case []any:
		for _, child := range v {
			walkToolInput(child, field, visit)
		}
	case string:
		visit(field, v)
	}
}

func isCommandField(field string) bool {
	base := path.Base(strings.ReplaceAll(field, ".", "/"))
	switch base {
	case "command", "cmd", "script", "shell":
		return true
	default:
		return strings.HasSuffix(base, "_command") || strings.HasSuffix(base, "_cmd")
	}
}

func looksPathLike(field string) bool {
	base := path.Base(strings.ReplaceAll(field, ".", "/"))
	return base == "path" ||
		base == "file" ||
		base == "filename" ||
		strings.HasSuffix(base, "_path") ||
		strings.HasSuffix(base, "_file") ||
		strings.HasSuffix(base, "filename")
}

func isWriteTool(toolName string, sideEffects bool) bool {
	switch toolName {
	case "write_file", "search_replace", "apply_diff":
		return true
	default:
		return sideEffects && strings.Contains(toolName, "write")
	}
}

func containsCredentialPath(value string) bool {
	normalized := normalizeGuardPath(value)
	for _, match := range credentialPathMatchers {
		if match(normalized) {
			return true
		}
	}
	for _, token := range splitPathTokens(value) {
		normalizedToken := normalizeGuardPath(token)
		for _, match := range credentialPathMatchers {
			if match(normalizedToken) {
				return true
			}
		}
	}
	return false
}

func targetsHarnessConfig(value string) bool {
	normalized := normalizeGuardPath(value)
	for _, target := range protectedWriteTargets {
		if normalized == target || strings.HasPrefix(normalized, target+"/") {
			return true
		}
	}
	return false
}

func normalizeGuardPath(value string) string {
	p := strings.ReplaceAll(strings.TrimSpace(value), `\`, "/")
	p = strings.Trim(p, `"'`)
	p = strings.TrimPrefix(p, "./")
	p = path.Clean(p)
	if p == "." {
		return ""
	}
	return p
}

func hasPathSegment(p, segment string) bool {
	for _, part := range strings.Split(p, "/") {
		if part == segment {
			return true
		}
	}
	return false
}

func splitPathTokens(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', ';', '&', '|', '(', ')', '<', '>', ',':
			return true
		default:
			return false
		}
	})
}
