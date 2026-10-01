package judge

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestFindNonceObject_Vectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "nonce_extraction_vectors.json"))
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var v extractionVectors
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode vectors: %v", err)
	}
	if len(v.Cases) == 0 {
		t.Fatal("vectors file has no cases")
	}
	for _, tc := range v.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			if (tc.Error == "") == (len(tc.Members) == 0) {
				t.Fatalf("case must set exactly one of members and error")
			}
			found, err := findNonceObject(tc.Input, tc.Nonce)
			if tc.Error != "" {
				want, ok := vectorErrors[tc.Error]
				if !ok {
					t.Fatalf("unknown error name %q", tc.Error)
				}
				if !errors.Is(err, want) {
					t.Fatalf("findNonceObject = %v, %v; want error %v", found.members, err, want)
				}
				return
			}
			if err != nil {
				t.Fatalf("findNonceObject: %v", err)
			}
			got, err := json.Marshal(found.members)
			if err != nil {
				t.Fatalf("marshal members: %v", err)
			}
			if !sameJSON(string(got), string(tc.Members)) {
				t.Fatalf("members = %s, want %s", got, tc.Members)
			}
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
			found, err := findNonceObject(in, nonce)
			if !errors.Is(err, errNonceScanBudget) {
				t.Fatalf("findNonceObject = %v, %v; want the scan budget error", found.members, err)
			}
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("extraction took %s; the scan budget is not bounding work", elapsed)
			}
		})
	}
}

func TestFindNonceObject_CountsCandidatesAndMatches(t *testing.T) {
	const nonce = "0123456789abcdef0123456789abcdef"
	match := `{"nonce":"` + nonce + `","verdict":"fail"}`
	cases := []struct {
		in                  string
		candidates, matches int
	}{
		{in: "prose { never closed", candidates: 0},
		{in: `{"a":1} {bad json}`, candidates: 2},
		{in: match, candidates: 1, matches: 1},
		{in: match + ` {"x":1} ` + match, candidates: 3, matches: 2},
	}
	for _, tc := range cases {
		found, _ := findNonceObject(tc.in, nonce)
		if found.candidates != tc.candidates || found.matches != tc.matches {
			t.Errorf("findNonceObject(%q): %d candidates, %d matches; want %d, %d", tc.in, found.candidates, found.matches, tc.candidates, tc.matches)
		}
	}
}

func TestMatchBrace(t *testing.T) {
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
				if end != -1 || scanned != len(tc.text) {
					t.Fatalf("matchBrace = %d, %d; want -1, %d", end, scanned, len(tc.text))
				}
				return
			}
			if end < 0 || tc.text[:end+1] != tc.want || scanned != end+1 {
				t.Fatalf("matchBrace = %d, %d; want the object %q", end, scanned, tc.want)
			}
		})
	}
	if end, scanned := matchBrace(`{"a":1}`, 0, 3); end != -1 || scanned != 3 {
		t.Errorf("matchBrace past its budget = %d, %d; want -1, 3", end, scanned)
	}
}
