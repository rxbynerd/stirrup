// Package workspaceexport uploads the executor's workspace as a
// gzipped tarball to a remote destination URI. The package targets
// the Shape B (artifact production) flow from issue #164: Cloud Run
// jobs and other serverless runtimes have no native output channel
// for the workspace, so the harness exfiltrates it before the
// container exits.
//
// Only "gs://" destinations are supported in v1. S3 and Azure Blob
// are reserved for future issues — types.ValidateRunConfig only
// accepts the gs:// scheme so the dispatch here is exhaustive.
package workspaceexport

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/harness/internal/gcs"
)

// MaxArchiveBytes caps the uploaded tarball size at 1 GiB. The cap
// exists to fail loudly when a runaway workspace (e.g. a build that
// dropped a multi-gigabyte cache into the working dir) would
// otherwise saturate the upload bandwidth and burn the run's
// remaining wall-clock. Configurable later; hard-coded for v1.
const MaxArchiveBytes = 1 << 30

// uploadTimeout bounds the GCS PUT round-trip. 5 minutes accommodates
// a near-maximum (1 GiB) tarball at typical Cloud Run egress
// throughput while still failing fast on a wedged network path.
const uploadTimeout = 5 * time.Minute

// Exporter uploads a workspace directory to a remote destination
// URI. The interface is small by design — single Export call per
// run, no state held between calls — so a future S3 / Azure
// implementation can sit alongside the GCS one without ceremony.
type Exporter interface {
	// Export tars + gzips workspaceDir and PUTs it to destURI. An
	// empty or non-existent workspaceDir is a silent skip (returns
	// nil); the caller decides whether that's an error via the
	// Required flag at the wiring site.
	Export(ctx context.Context, workspaceDir, destURI string) error
}

// GCSExporter implements Exporter against a gs:// destination URI.
// Authentication uses the same gcp-workload-identity default as the
// gcs trace emitter (the Cloud Run / GKE shape this targets); an
// explicit credential.Source can be injected for non-default runtimes.
type GCSExporter struct {
	credentialSource credential.Source
	httpClient       *http.Client
	endpointBaseURL  string // override for tests
}

// GCSExporterOptions configures a GCSExporter. CredentialSource is
// required; the default factory in cmd/ wires in
// credential.NewGoogleWorkloadIdentitySource() when no override is
// configured.
type GCSExporterOptions struct {
	CredentialSource credential.Source

	// HTTPClient is optional; a sensibly-bounded client is
	// constructed when nil. Exposed primarily so tests can inject an
	// httptest server.
	HTTPClient *http.Client

	// EndpointBaseURL overrides the GCS upload host. Defaults to
	// gcs.DefaultEndpointBaseURL. Tests set this to point at an
	// httptest.NewServer; production callers leave it empty.
	EndpointBaseURL string
}

// NewGCSExporter validates options and constructs an exporter.
// Credential resolution is deferred until Export so a transient
// metadata-server hiccup at boot doesn't block the entire run before
// the agentic loop gets a chance to run.
func NewGCSExporter(opts GCSExporterOptions) (*GCSExporter, error) {
	if opts.CredentialSource == nil {
		return nil, fmt.Errorf("workspace exporter: credential source is required")
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: uploadTimeout,
			Transport: &http.Transport{
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		}
	}
	return &GCSExporter{
		credentialSource: opts.CredentialSource,
		httpClient:       client,
		endpointBaseURL:  opts.EndpointBaseURL,
	}, nil
}

// Export tars + gzips workspaceDir into an in-memory buffer (subject
// to MaxArchiveBytes) and uploads it to destURI. destURI must be a
// gs:// URI; any other scheme is rejected as unsupported. An empty
// or non-existent workspaceDir is a silent no-op (returns nil) so
// the caller's wiring can opt in to "fail when missing" via the
// Required flag at the call site rather than this package judging
// the run's intent.
func (e *GCSExporter) Export(ctx context.Context, workspaceDir, destURI string) error {
	bucket, object, err := parseGCSURI(destURI)
	if err != nil {
		return fmt.Errorf("workspace exporter: %w", err)
	}

	// Skip when the workspace dir is missing entirely. Empty-dir
	// case is handled below after we've started walking.
	info, err := os.Stat(workspaceDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("workspace exporter: stat workspace %q: %w", workspaceDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace exporter: workspace %q is not a directory", workspaceDir)
	}

	buf := &bytes.Buffer{}
	wrote, err := writeTarGz(workspaceDir, buf, MaxArchiveBytes)
	if err != nil {
		return fmt.Errorf("workspace exporter: build tarball: %w", err)
	}
	if wrote == 0 {
		// Empty workspace — nothing to upload. Silent skip mirrors
		// the missing-dir branch.
		return nil
	}

	resolved, err := e.credentialSource.Resolve(ctx)
	if err != nil {
		return fmt.Errorf("workspace exporter: resolve credential: %w", err)
	}
	if resolved == nil || resolved.BearerToken == nil {
		return fmt.Errorf("workspace exporter: credential source produced no bearer token")
	}

	if err := gcs.UploadObject(ctx, e.httpClient, gcs.UploadOptions{
		Bucket:          bucket,
		Object:          object,
		Body:            buf.Bytes(),
		ContentType:     "application/gzip",
		Bearer:          resolved.BearerToken,
		EndpointBaseURL: e.endpointBaseURL,
	}); err != nil {
		return fmt.Errorf("workspace exporter: upload tarball: %w", err)
	}
	return nil
}

// Probe verifies the export destination is reachable for a dry-run
// preflight: it parses destURI, resolves the credential, and performs a
// read-only bucket-metadata GET. It tars nothing and uploads nothing, so
// a dry-run leaves no artifact behind. A malformed URI, an unresolvable
// credential, or a bucket the credential cannot see is surfaced so the
// operator catches it before a real run spends wall-clock building a
// tarball it cannot upload.
func (e *GCSExporter) Probe(ctx context.Context, destURI string) error {
	bucket, _, err := parseGCSURI(destURI)
	if err != nil {
		return fmt.Errorf("workspace exporter: %w", err)
	}
	resolved, err := e.credentialSource.Resolve(ctx)
	if err != nil {
		return fmt.Errorf("workspace exporter: resolve credential: %w", err)
	}
	if resolved == nil || resolved.BearerToken == nil {
		return fmt.Errorf("workspace exporter: credential source produced no bearer token")
	}
	if err := gcs.BucketAccessible(ctx, e.httpClient, gcs.BucketProbeOptions{
		Bucket:          bucket,
		Bearer:          resolved.BearerToken,
		EndpointBaseURL: e.endpointBaseURL,
	}); err != nil {
		return fmt.Errorf("workspace exporter: %w", err)
	}
	return nil
}

// parseGCSURI splits a gs://bucket/object URI into its parts. The
// object name must be non-empty so callers cannot accidentally
// overwrite the bucket root or trigger surprising GCS API behaviour.
func parseGCSURI(uri string) (bucket, object string, err error) {
	const prefix = "gs://"
	if !strings.HasPrefix(uri, prefix) {
		return "", "", fmt.Errorf("unsupported destination URI %q: only gs:// is supported in v1", uri)
	}
	rest := strings.TrimPrefix(uri, prefix)
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return "", "", fmt.Errorf("destination URI %q: missing object path", uri)
	}
	bucket = rest[:slash]
	object = rest[slash+1:]
	if bucket == "" {
		return "", "", fmt.Errorf("destination URI %q: empty bucket name", uri)
	}
	if object == "" {
		return "", "", fmt.Errorf("destination URI %q: empty object path", uri)
	}
	return bucket, object, nil
}

// writeTarGz walks rootDir and writes a gzipped tar of every regular
// file, symlink (recorded as a symlink entry, not followed), and
// directory underneath it. Returns the number of regular files
// included so the caller can distinguish "wrote an empty archive
// header" (silent skip) from "wrote real content".
//
// Path-traversal safety: symlinks are recorded as symlink entries
// rather than dereferenced. Any path that resolves outside rootDir
// (via a symlinked parent directory or a relative .. segment in the
// computed archive name) is refused. The executor's workspace is the
// trust boundary; refusing escapes here mirrors the hostile-input
// posture the rest of the harness maintains.
//
// Size cap: writeTarGz tracks the cumulative compressed bytes
// written and aborts as soon as the cap would be exceeded. The cap
// is a defence against runaway workspaces (build caches, accidental
// committed binaries) saturating the upload before the run can
// complete.
func writeTarGz(rootDir string, w io.Writer, maxBytes int64) (int, error) {
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return 0, fmt.Errorf("abs root %q: %w", rootDir, err)
	}
	// Evaluate symlinks once at the root so a workspace passed as
	// /tmp/work where /tmp/work is a symlink to /var/realwork still
	// has a stable trust boundary against escapes by intermediate
	// symlinks.
	absRootResolved, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		// EvalSymlinks fails on a missing path; the caller's earlier
		// os.Stat already established existence, so a failure here
		// is a structural problem worth surfacing.
		return 0, fmt.Errorf("resolve root %q: %w", rootDir, err)
	}

	// limitWriter caps the bytes flowing into the gzip stream. We
	// cap on compressed output because that's what the GCS upload
	// actually transmits — a 100 GiB sparse file compresses cheaply
	// and would otherwise blow the local memory budget.
	limited := &limitWriter{w: w, max: maxBytes}
	gz := gzip.NewWriter(limited)
	tw := tar.NewWriter(gz)

	regularFiles := 0
	walkErr := filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Compute the in-archive name relative to the user-supplied
		// rootDir (not the eval-symlinks-resolved path) so the
		// archive structure mirrors what the operator sees on disk.
		archiveName, err := filepath.Rel(absRoot, path)
		if err != nil {
			return fmt.Errorf("relative path %q: %w", path, err)
		}
		if archiveName == "." {
			// Root directory itself — recorded implicitly via each
			// child's path.
			return nil
		}

		// Refuse any computed name that contains a ".." segment.
		// filepath.Rel can return such a name when the source path
		// is outside rootDir, which happens if a symlink in the
		// walked tree points outside.
		if archiveName == ".." || strings.HasPrefix(archiveName, ".."+string(filepath.Separator)) {
			return fmt.Errorf("refusing path %q: escapes workspace root", path)
		}

		// Refuse paths that, after resolving any symlinks, leave the
		// resolved root. This catches the case where a symlinked
		// parent directory exists inside the workspace but points at
		// a sibling outside it.
		//
		// A dangling symlink (target does not exist) trips
		// EvalSymlinks with an error. The previous behaviour skipped
		// the containment check on error, so a dangling symlink
		// pointing to an absolute path outside the workspace was
		// silently included in the tarball; downstream `tar -xzf`
		// without --no-dereference on Linux can be exploited via a
		// symlink-then-overwrite pattern. Refuse such paths
		// explicitly. The smoke workflow's tar invocation does not
		// pass --no-dereference, so defending here is the only layer.
		resolvedPath, evErr := filepath.EvalSymlinks(path)
		if evErr != nil {
			return fmt.Errorf("refusing path %q: cannot resolve symlink target: %w", path, evErr)
		}
		rel2, relErr := filepath.Rel(absRootResolved, resolvedPath)
		if relErr == nil && (rel2 == ".." || strings.HasPrefix(rel2, ".."+string(filepath.Separator))) {
			return fmt.Errorf("refusing path %q: resolves outside workspace root", path)
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %q: %w", path, err)
		}

		switch {
		case info.Mode()&os.ModeSymlink != 0:
			// Record the symlink itself, not its target. tar treats
			// symlinks as a distinct entry type so a downstream
			// untar produces an identical link rather than a
			// dereferenced file.
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink %q: %w", path, err)
			}
			hdr := &tar.Header{
				Name:     filepath.ToSlash(archiveName),
				Linkname: target,
				Mode:     int64(info.Mode().Perm()),
				ModTime:  info.ModTime(),
				Typeflag: tar.TypeSymlink,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return fmt.Errorf("write symlink header %q: %w", path, err)
			}

		case info.IsDir():
			hdr := &tar.Header{
				Name:     filepath.ToSlash(archiveName) + "/",
				Mode:     int64(info.Mode().Perm()),
				ModTime:  info.ModTime(),
				Typeflag: tar.TypeDir,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return fmt.Errorf("write dir header %q: %w", path, err)
			}

		case info.Mode().IsRegular():
			hdr := &tar.Header{
				Name:     filepath.ToSlash(archiveName),
				Mode:     int64(info.Mode().Perm()),
				Size:     info.Size(),
				ModTime:  info.ModTime(),
				Typeflag: tar.TypeReg,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return fmt.Errorf("write file header %q: %w", path, err)
			}
			f, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("open %q: %w", path, err)
			}
			_, copyErr := io.Copy(tw, f)
			_ = f.Close()
			if copyErr != nil {
				return fmt.Errorf("copy %q: %w", path, copyErr)
			}
			regularFiles++

		default:
			// Skip block / char / fifo / socket entries — they make
			// no sense in a portable workspace tarball and might
			// surprise a downstream untar that treats unknown types
			// as a security event.
		}
		return nil
	})

	if walkErr != nil {
		_ = tw.Close()
		_ = gz.Close()
		return 0, walkErr
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return 0, fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return 0, fmt.Errorf("close gzip: %w", err)
	}
	if limited.exceeded {
		return 0, fmt.Errorf("workspace tarball exceeds %d-byte cap", maxBytes)
	}
	return regularFiles, nil
}

// limitWriter wraps an io.Writer and refuses writes that would push
// the cumulative byte count over max. Reports the cap breach via the
// exceeded flag so the caller can produce a clear error message
// (io.ErrShortWrite alone would be ambiguous).
type limitWriter struct {
	w        io.Writer
	max      int64
	written  int64
	exceeded bool
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.exceeded {
		return 0, io.ErrShortWrite
	}
	if l.written+int64(len(p)) > l.max {
		l.exceeded = true
		return 0, io.ErrShortWrite
	}
	n, err := l.w.Write(p)
	l.written += int64(n)
	return n, err
}
