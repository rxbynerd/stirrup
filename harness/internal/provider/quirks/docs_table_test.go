package quirks

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const providerQuirksDoc = "../../../../docs/provider-quirks.md"

var (
	docRuleCell      = regexp.MustCompile("`([^`]+)`")
	docRuleCountWord = regexp.MustCompile(`The (\w+) tool search rules`)
	docNumberWords   = map[string]int{
		"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12,
	}
)

// toolSearchRuleKeys returns the sorted "provider pattern" key of every
// built-in rule that advertises a native tool search.
func toolSearchRuleKeys() []string {
	var keys []string
	for _, rule := range BuiltinRules() {
		var q ProviderQuirks
		rule.Apply(&q)
		if q.ToolSearch.Supported {
			keys = append(keys, rule.ProviderType+" "+rule.ModelMatch)
		}
	}
	slices.Sort(keys)
	return keys
}

// documentedToolSearchRuleKeys returns the sorted "provider pattern" key of
// every row of the rule table in docs/provider-quirks.md that describes a
// tool search rule, and the rule count the prose states.
func documentedToolSearchRuleKeys(t *testing.T) (keys []string, statedCount int) {
	t.Helper()
	must := require.New(t)
	raw, err := os.ReadFile(filepath.FromSlash(providerQuirksDoc))
	must.NoError(err)
	doc := string(raw)

	for _, line := range strings.Split(doc, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 5 || !strings.HasPrefix(strings.TrimSpace(line), "| `") {
			continue
		}
		description := cells[3]
		if !strings.Contains(description, "tool search") && !strings.Contains(description, "`tool_search`") {
			continue
		}
		provider := strings.Trim(strings.TrimSpace(cells[1]), "`")
		for _, m := range docRuleCell.FindAllStringSubmatch(cells[2], -1) {
			keys = append(keys, provider+" "+m[1])
		}
	}
	slices.Sort(keys)

	if m := docRuleCountWord.FindStringSubmatch(doc); m != nil {
		statedCount = docNumberWords[strings.ToLower(m[1])]
	}
	return keys, statedCount
}

// TestProviderQuirksDoc_ToolSearchRuleTableMatchesBuiltinRules pins the
// documented tool search rules to the registry: a missing, extra or
// duplicated row, or a stale count in the prose, fails the test.
func TestProviderQuirksDoc_ToolSearchRuleTableMatchesBuiltinRules(t *testing.T) {
	must := require.New(t)
	want := toolSearchRuleKeys()
	must.NotEmpty(want)

	got, statedCount := documentedToolSearchRuleKeys(t)

	must.Equal(want, got, "tool search rows in docs/provider-quirks.md must list each BuiltinRules tool search rule exactly once")
	must.Equal(len(want), statedCount, "the prose count of tool search rules must match BuiltinRules")
}
