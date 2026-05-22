package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/stirrup/types"
)

var configExplainCmd = &cobra.Command{
	Use:   "explain [field-path]",
	Short: "Print documentation for a RunConfig field path",
	Long: `Print the documentation, type, defaults, and accepted values
for a RunConfig field path. The information is the same doc comments
that live above each field in types/runconfig.go, surfaced via a
generated lookup table.

Examples:
  stirrup config explain mode
  stirrup config explain provider.batch.maxWaitSeconds
  stirrup config explain provider.batch
  stirrup config explain "provider.*"
  stirrup config explain --list
  stirrup config explain --root
  stirrup config explain provider.type --output=json

Paths use the JSON field names (camelCase), dot-separated. Map-valued
fields contribute a synthetic <key> segment (e.g. providers.<name>);
slice elements use [].`,
	Args: cobra.MaximumNArgs(1),
	RunE: runConfigExplain,
}

func init() {
	configCmd.AddCommand(configExplainCmd)

	f := configExplainCmd.Flags()
	f.Bool("list", false, "Print every leaf field path on RunConfig, alphabetised, one per line.")
	f.Bool("root", false, "Print the top-level RunConfig overview.")
	f.String("output", "text", "Output format: text|json. With a field path: FieldDoc object; with --list: []string; with a wildcard suffix: []FieldDoc array.")

	// Closed-set flag completion mirrors runconfigflags.go's
	// addRunConfigFlagCompletions: registering the value list lets
	// `stirrup config explain --output <TAB>` complete to text|json.
	// The positional <field-path> completion is wired via the
	// ValidArgsFunction below — operators get `--list` parity from
	// the completion surface itself.
	_ = configExplainCmd.RegisterFlagCompletionFunc("output", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return []string{"text", "json"}, cobra.ShellCompDirectiveNoFileComp
	})

	configExplainCmd.ValidArgsFunction = func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			// Args is MaximumNArgs(1); no completions past the first.
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return leafPaths(), cobra.ShellCompDirectiveNoFileComp
	}
}

func runConfigExplain(cmd *cobra.Command, args []string) error {
	return runConfigExplainWithIO(cmd, args, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

// runConfigExplainWithIO is the testable seam for runConfigExplain: it
// accepts explicit writers so tests can drive the cobra flag-reading
// path through the real entry point without redirecting global os.Stdout.
func runConfigExplainWithIO(cmd *cobra.Command, args []string, stdout, stderr io.Writer) error {
	f := cmd.Flags()
	list, _ := f.GetBool("list")
	root, _ := f.GetBool("root")
	output, _ := f.GetString("output")

	switch output {
	case "text", "json":
	default:
		return fmt.Errorf("--output must be one of: text, json (got %q)", output)
	}

	switch {
	case list && root:
		return fmt.Errorf("--list and --root are mutually exclusive")
	case list && len(args) > 0:
		return fmt.Errorf("--list does not take a field-path argument")
	case root && len(args) > 0:
		return fmt.Errorf("--root does not take a field-path argument")
	}

	if list {
		return emitLeafList(stdout, output)
	}
	if root || len(args) == 0 {
		return emitFieldDoc(stdout, "", output)
	}

	path := args[0]
	if path == "" {
		// An explicit empty-string positional is a user mistake, not a
		// request for the root overview. `--root` is the documented
		// way to ask for that.
		return fmt.Errorf("no field at path %q (use --root for the top-level overview)", path)
	}
	if strings.HasSuffix(path, ".*") {
		return emitWildcard(stdout, strings.TrimSuffix(path, ".*"), output)
	}
	return emitFieldDoc(stdout, path, output)
}

// emitLeafList prints every leaf path in alphabetical order. Useful
// for shell completion and discovery. JSON output emits a string
// array.
func emitLeafList(w io.Writer, output string) error {
	paths := leafPaths()
	if output == "json" {
		return writeJSON(w, paths)
	}
	var buf bytes.Buffer
	for _, p := range paths {
		buf.WriteString(p)
		buf.WriteByte('\n')
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// leafPaths returns the sorted list of every leaf field path in the
// generated lookup. Leaves are entries with Kind == "leaf"; struct,
// map, and slice entries are excluded because they are parents, not
// directly-settable fields.
func leafPaths() []string {
	out := make([]string, 0, len(types.FieldDocs))
	for p, fd := range types.FieldDocs {
		if fd.Kind != "leaf" {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// emitFieldDoc looks up `path` and renders it. Missing paths trigger
// a fuzzy-match suggestion before returning a non-nil error so the
// process exits non-zero (per spec).
func emitFieldDoc(w io.Writer, path, output string) error {
	fd, ok := types.FieldDocs[path]
	if !ok {
		suggestion := nearestPath(path)
		if suggestion != "" {
			return fmt.Errorf("no field at path %q — did you mean %q?", path, suggestion)
		}
		return fmt.Errorf("no field at path %q", path)
	}
	if output == "json" {
		return writeJSON(w, fd)
	}
	return writeText(w, fd)
}

// emitWildcard expands `<prefix>.*` to the documentation entries of
// every direct child of the prefix. The list is sorted for stable
// output.
func emitWildcard(w io.Writer, prefix, output string) error {
	parent, ok := types.FieldDocs[prefix]
	if !ok {
		suggestion := nearestPath(prefix)
		if suggestion != "" {
			return fmt.Errorf("no field at path %q — did you mean %q?", prefix, suggestion)
		}
		return fmt.Errorf("no field at path %q", prefix)
	}
	if len(parent.Children) == 0 {
		return fmt.Errorf("field %q has no children to expand (kind=%s)", prefix, parent.Kind)
	}

	children := make([]types.FieldDoc, 0, len(parent.Children))
	for _, name := range parent.Children {
		childPath := name
		if prefix != "" {
			childPath = prefix + "." + name
		}
		if cd, ok := types.FieldDocs[childPath]; ok {
			children = append(children, cd)
		}
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Path < children[j].Path })

	if output == "json" {
		return writeJSON(w, children)
	}
	var buf bytes.Buffer
	for i, c := range children {
		if i > 0 {
			buf.WriteByte('\n')
		}
		renderText(&buf, c)
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// writeText renders a FieldDoc to w. The body is built into a buffer
// first so the rendering helpers can use fmt.Fprintf into a
// non-failing writer (bytes.Buffer always succeeds), and the single
// w.Write at the end has the only error that matters.
func writeText(w io.Writer, fd types.FieldDoc) error {
	var buf bytes.Buffer
	renderText(&buf, fd)
	_, err := w.Write(buf.Bytes())
	return err
}

// renderText writes the kubectl-explain-style block for a single
// FieldDoc into buf:
//
//	KIND:    RunConfig
//	FIELD:   <path>  (<type>)
//
//	DESCRIPTION:
//	  <wrapped doc>
//
//	DEFAULT: <value>
//	VALID VALUES:
//	  <enum line per value>
//
//	CHILDREN:
//	  <child>  <type>  <one-line summary>
func renderText(buf *bytes.Buffer, fd types.FieldDoc) {
	kind := "RunConfig"
	if fd.OwnerStruct != "" {
		kind = fd.OwnerStruct
	}
	pathLabel := fd.Path
	if pathLabel == "" {
		pathLabel = "(root)"
	}
	// The generator emits FieldDoc.Type with a leading `*` for every
	// pointer (Optional) field, so the type label already carries
	// the nilability marker — no extra prefixing required here. The
	// types/docsgen TestGenerator_PointerFieldOptional fixture and
	// TestConfigExplain_OptionalFieldRendersPointerType below pin
	// that contract.
	typeLabel := fd.Type
	fmt.Fprintf(buf, "KIND:    %s\n", kind)
	fmt.Fprintf(buf, "FIELD:   %s  (%s)\n", pathLabel, typeLabel)
	buf.WriteByte('\n')
	buf.WriteString("DESCRIPTION:\n")
	if fd.Doc == "" {
		buf.WriteString("  (no inline documentation - see docs/configuration.md)\n")
	} else {
		for _, line := range strings.Split(fd.Doc, "\n") {
			buf.WriteString("  ")
			buf.WriteString(strings.TrimRight(line, " \t"))
			buf.WriteByte('\n')
		}
	}
	if fd.Default != "" {
		buf.WriteByte('\n')
		fmt.Fprintf(buf, "DEFAULT: %s\n", fd.Default)
	}
	if len(fd.Enum) > 0 {
		buf.WriteByte('\n')
		buf.WriteString("VALID VALUES:\n")
		for _, v := range fd.Enum {
			fmt.Fprintf(buf, "  %s\n", v)
		}
	}
	if len(fd.Children) > 0 {
		buf.WriteByte('\n')
		buf.WriteString("CHILDREN:\n")
		renderChildren(buf, fd)
	}
	if fd.ParentPath != "" || fd.Kind == "leaf" {
		related := relatedPaths(fd)
		if len(related) > 0 {
			buf.WriteByte('\n')
			buf.WriteString("RELATED FIELDS:\n")
			for _, p := range related {
				fmt.Fprintf(buf, "  %s\n", p)
			}
		}
	}
}

// renderChildren emits the CHILDREN block: one line per direct child
// with `<name>  <type>  <first sentence of doc>`. Columns are padded
// to the longest name + longest type for readability.
func renderChildren(buf *bytes.Buffer, fd types.FieldDoc) {
	type row struct{ name, ty, summary string }
	rows := make([]row, 0, len(fd.Children))
	maxName, maxType := 0, 0
	for _, name := range fd.Children {
		childPath := name
		if fd.Path != "" {
			childPath = fd.Path + "." + name
		}
		cd, ok := types.FieldDocs[childPath]
		if !ok {
			continue
		}
		r := row{name: name, ty: cd.Type, summary: firstSentence(cd.Doc)}
		if len(r.name) > maxName {
			maxName = len(r.name)
		}
		if len(r.ty) > maxType {
			maxType = len(r.ty)
		}
		rows = append(rows, r)
	}
	for _, r := range rows {
		fmt.Fprintf(buf, "  %-*s  %-*s  %s\n", maxName, r.name, maxType, r.ty, r.summary)
	}
}

// relatedPaths returns sibling field paths (other children of the
// same parent) for a leaf or struct entry. Mirrors kubectl-explain's
// "RELATED" affordance — useful when the doc cross-references a
// neighbour ("Mutually exclusive with transport.type == grpc").
//
// Capped at six entries so a leaf under a wide parent (RunConfig has
// 30+ direct children) does not flood the output.
func relatedPaths(fd types.FieldDoc) []string {
	if fd.ParentPath == "" && fd.Path == "" {
		return nil
	}
	parent, ok := types.FieldDocs[fd.ParentPath]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(parent.Children))
	for _, name := range parent.Children {
		if name == fd.JSONTag {
			continue
		}
		p := name
		if fd.ParentPath != "" {
			p = fd.ParentPath + "." + name
		}
		out = append(out, p)
	}
	const maxRelated = 6
	if len(out) > maxRelated {
		out = out[:maxRelated]
	}
	return out
}

func firstSentence(doc string) string {
	if doc == "" {
		return ""
	}
	// Doc comments are often multi-paragraph; collapse to a single
	// line and stop at the first period followed by space or EOL.
	doc = strings.ReplaceAll(doc, "\n", " ")
	doc = strings.Join(strings.Fields(doc), " ")
	for i := 0; i < len(doc); i++ {
		if doc[i] != '.' {
			continue
		}
		if i == len(doc)-1 || doc[i+1] == ' ' {
			return doc[:i+1]
		}
	}
	const cap = 100
	if len(doc) > cap {
		return doc[:cap] + "…"
	}
	return doc
}

// nearestPath returns the closest leaf path to `query` using
// Levenshtein distance, or "" if no path is within a sensible
// threshold. The threshold is min(8, len(query)/2 + 2) so a typo on
// a short path doesn't suggest something wildly different.
func nearestPath(query string) string {
	if query == "" {
		return ""
	}
	threshold := len(query)/2 + 2
	if threshold > 8 {
		threshold = 8
	}
	best := ""
	bestDist := threshold + 1
	for p := range types.FieldDocs {
		if p == "" {
			continue
		}
		d := levenshtein(query, p)
		if d < bestDist {
			bestDist = d
			best = p
		}
	}
	if best == "" || bestDist > threshold {
		return ""
	}
	return best
}

// levenshtein computes the edit distance between a and b. A standard
// dynamic-programming implementation, kept inline so the explain
// command does not pull in an external dependency for a one-shot
// suggestion ("did you mean ...?") routine.
func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			curr[j] = min(del, ins, sub)
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

func writeJSON(w io.Writer, v interface{}) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
