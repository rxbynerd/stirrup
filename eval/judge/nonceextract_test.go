package judge

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// testdata/nonce_extraction_vectors.json is a byte-identical copy of
// harness/internal/jsonextract/testdata/nonce_extraction_vectors.json;
// change both copies together.
type extractionVectors struct {
	Comment string `json:"comment"`
	Cases   []struct {
		Name    string          `json:"name"`
		Input   string          `json:"input"`
		Nonce   string          `json:"nonce"`
		Members json.RawMessage `json:"members"`
		Error   string          `json:"error"`
	} `json:"cases"`
}

var vectorErrors = map[string]error{
	"empty_nonce": errEmptyNonce,
	"no_match":    errNoNonceObject,
	"conflict":    errConflictingNonceObjects,
}

func TestFindNonceObject_MatchesSharedVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "nonce_extraction_vectors.json"))
	require.NoError(t, err, "read vectors")
	var v extractionVectors
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&v), "decode vectors")
	require.NotEmpty(t, v.Cases, "vectors file has no cases")

	for _, tc := range v.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			require.NotEqual(t, tc.Error != "", len(tc.Members) > 0, "case must set exactly one of members and error")

			found, err := findNonceObject(tc.Input, tc.Nonce)
			if tc.Error != "" {
				want, ok := vectorErrors[tc.Error]
				require.True(t, ok, "unknown error name %q", tc.Error)
				require.ErrorIs(t, err, want)
				return
			}
			require.NoError(t, err)

			got, err := json.Marshal(found.members)
			require.NoError(t, err, "marshal members")
			require.True(t, sameJSON(string(got), string(tc.Members)), "members = %s, want %s", got, tc.Members)
		})
	}
}

func TestFindNonceObject_ScanBudgetFailsClosed(t *testing.T) {
	const nonce = "0123456789abcdef0123456789abcdef"
	verdict := `{"nonce":"` + nonce + `","verdict":"fail"}`
	for name, in := range map[string]string{
		"braces before match": strings.Repeat("{", 200_000) + verdict,
		"braces after match":  verdict + strings.Repeat("{", 200_000),
	} {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			_, err := findNonceObject(in, nonce)
			require.ErrorIs(t, err, errNonceScanBudget)
			require.LessOrEqual(t, time.Since(start), 5*time.Second, "the scan budget is not bounding work")
		})
	}
}

func TestFindNonceObject_CountsCandidatesAndMatches(t *testing.T) {
	const nonce = "0123456789abcdef0123456789abcdef"
	match := `{"nonce":"` + nonce + `","verdict":"fail"}`
	cases := []struct {
		name                string
		in                  string
		candidates, matches int
	}{
		{name: "unclosed brace is not a candidate", in: "prose { never closed", candidates: 0},
		{name: "candidates without a match", in: `{"a":1} {bad json}`, candidates: 2},
		{name: "single matching object", in: match, candidates: 1, matches: 1},
		{name: "matches among other candidates", in: match + ` {"x":1} ` + match, candidates: 3, matches: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found, _ := findNonceObject(tc.in, nonce)
			require.Equal(t, tc.candidates, found.candidates)
			require.Equal(t, tc.matches, found.matches)
		})
	}
}

func TestMatchBrace_FindsObjectEnd(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{name: "bare object", text: `{"a":1}`, want: `{"a":1}`},
		{name: "trailing text", text: `{"a":1} and more`, want: `{"a":1}`},
		{name: "braces inside strings", text: `{"r":"uses {braces} and a \"quoted } brace\"","f":"fine {"}`, want: `{"r":"uses {braces} and a \"quoted } brace\"","f":"fine {"}`},
		{name: "escaped backslash before quote", text: `{"a":"x\\","b":"}"}`, want: `{"a":"x\\","b":"}"}`},
		{name: "nested object", text: `{"a":{"b":{"c":1}}}x`, want: `{"a":{"b":{"c":1}}}`},
		{name: "unterminated", text: `{"reasoning":"cut off`},
		{name: "unbalanced", text: `{ {"a":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			end, scanned := matchBrace(tc.text, 0, nonceScanBudget)
			if tc.want == "" {
				require.Equal(t, -1, end)
				require.Equal(t, len(tc.text), scanned)
				return
			}
			require.GreaterOrEqual(t, end, 0, "no closing brace found")
			require.Equal(t, tc.want, tc.text[:end+1])
			require.Equal(t, end+1, scanned)
		})
	}
}

func TestMatchBrace_StopsAtBudget(t *testing.T) {
	end, scanned := matchBrace(`{"a":1}`, 0, 3)
	require.Equal(t, -1, end)
	require.Equal(t, 3, scanned)
}
