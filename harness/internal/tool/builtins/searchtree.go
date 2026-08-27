package builtins

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"
	"sync"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

const (
	// treeSearchMaxFiles bounds how many files a single grep_files call
	// fetches from a tree-backed executor. Each file costs one API request,
	// so an unbounded scan of a large repository would exhaust the caller's
	// rate limit within one tool call.
	treeSearchMaxFiles = 200
	// treeSearchMaxFileSize skips files too large to be worth transferring
	// for a line-oriented search, mirroring the native walker's silent skip
	// of binary content.
	treeSearchMaxFileSize = 1 << 20
	// treeSearchConcurrency bounds in-flight fetches so a full scan lands
	// well inside searchTimeout without flooding the backing API.
	treeSearchConcurrency = 8
)

// treeSearchIncompleteNotice is appended to a tree-backed search rendering
// when the scan could not cover every candidate file, so a partial result is
// not read as exhaustive.
const treeSearchIncompleteNotice = "[search incomplete: not every candidate file was scanned - narrow the search with path or include for full coverage]"

// grepTree searches file contents on an executor that serves its workspace as
// a remote tree rather than a host directory. It enumerates candidates in a
// single ListTree call, then fetches only the files that survive the
// include/exclude filters. The bool reports whether the scan is known to be
// incomplete.
func grepTree(ctx context.Context, lister executor.TreeLister, reader executor.Executor, root string, re *regexp.Regexp, include, exclude []string, maxResults int) ([]searchMatch, bool, error) {
	candidates, incomplete, err := treeCandidates(ctx, lister, root, include, exclude)
	if err != nil {
		return nil, false, fmt.Errorf("search failed: %w", err)
	}

	scannable := make([]executor.TreeEntry, 0, len(candidates))
	for _, entry := range candidates {
		if entry.Size > treeSearchMaxFileSize {
			continue
		}
		scannable = append(scannable, entry)
	}
	if len(scannable) > treeSearchMaxFiles {
		scannable = scannable[:treeSearchMaxFiles]
		incomplete = true
	}

	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	contents, failures, err := readTreeFiles(ctx, reader, root, scannable)
	if err != nil {
		return nil, false, fmt.Errorf("search failed: %w", err)
	}
	if failures > 0 {
		incomplete = true
	}

	var results []searchMatch
	for i, entry := range scannable {
		data := contents[i]
		if data == "" || looksBinary([]byte(data)) {
			continue
		}
		for lineNum, line := range strings.Split(data, "\n") {
			if !re.MatchString(line) {
				continue
			}
			results = append(results, searchMatch{Path: entry.Path, Line: lineNum + 1, Text: line})
			if len(results) >= maxResults {
				return results, incomplete, nil
			}
		}
	}
	return results, incomplete, nil
}

// findTree matches file names on an executor that serves its workspace as a
// remote tree. The whole enumeration is one ListTree call, so no file content
// is fetched. The bool reports whether the enumeration is known to be
// incomplete.
func findTree(ctx context.Context, lister executor.TreeLister, root, name string, include, exclude []string, maxResults int) ([]string, bool, error) {
	candidates, incomplete, err := treeCandidates(ctx, lister, root, include, exclude)
	if err != nil {
		return nil, false, fmt.Errorf("find failed: %w", err)
	}

	var results []string
	for _, entry := range candidates {
		matched, matchErr := path.Match(name, path.Base(entry.Path))
		if matchErr != nil {
			return nil, false, fmt.Errorf("match name pattern: %w", matchErr)
		}
		if !matched {
			continue
		}
		results = append(results, entry.Path)
		if len(results) >= maxResults {
			break
		}
	}
	return results, incomplete, nil
}

// treeCandidates enumerates the files under root that pass the include/exclude
// filters, preserving the executor's tree order.
func treeCandidates(ctx context.Context, lister executor.TreeLister, root string, include, exclude []string) ([]executor.TreeEntry, bool, error) {
	listing, err := lister.ListTree(ctx, root)
	if err != nil {
		return nil, false, err
	}
	candidates := make([]executor.TreeEntry, 0, len(listing.Entries))
	for _, entry := range listing.Entries {
		if !matchesFilters(path.Base(entry.Path), entry.Path, include, exclude) {
			continue
		}
		candidates = append(candidates, entry)
	}
	return candidates, listing.Truncated, nil
}

// readTreeFiles fetches each entry's content through the executor, bounded by
// treeSearchConcurrency in-flight requests. Contents are returned positionally;
// an entry that could not be read yields an empty string and is counted as a
// failure so one unreadable file does not blank the whole result. A context
// that ends while reads are still outstanding is fatal.
func readTreeFiles(ctx context.Context, reader executor.Executor, root string, entries []executor.TreeEntry) ([]string, int, error) {
	contents := make([]string, len(entries))
	failed := make([]bool, len(entries))
	sem := make(chan struct{}, treeSearchConcurrency)

	var wg sync.WaitGroup
	for i, entry := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				failed[i] = true
				return
			}
			defer func() { <-sem }()

			data, err := reader.ReadFile(ctx, joinTreePath(root, entry.Path))
			if err != nil {
				failed[i] = true
				return
			}
			contents[i] = data
		}()
	}
	wg.Wait()

	failures := 0
	for _, f := range failed {
		if f {
			failures++
		}
	}
	if failures > 0 && ctx.Err() != nil {
		return nil, 0, ctx.Err()
	}
	return contents, failures, nil
}

// joinTreePath rebuilds the workspace-relative path of a tree entry, whose
// Path is relative to the search root.
func joinTreePath(root, rel string) string {
	trimmed := strings.Trim(root, "/")
	if trimmed == "" || trimmed == "." {
		return rel
	}
	return trimmed + "/" + rel
}
