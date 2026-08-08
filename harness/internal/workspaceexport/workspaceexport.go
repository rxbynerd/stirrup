// Package workspaceexport uploads the executor's workspace as a
// gzipped tarball to a remote destination URI. Only "gs://"
// destinations are supported in v1; see docs/cloud-run-jobs.md.
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

// MaxArchiveBytes caps the uploaded tarball size, guarding against a
// runaway workspace saturating upload bandwidth. Hard-coded for v1.
const MaxArchiveBytes = 1 << 30

// uploadTimeout bounds the GCS PUT round-trip.
const uploadTimeout = 5 * time.Minute

// Exporter uploads a workspace directory to a remote destination URI.
type Exporter interface {
	// Export tars + gzips workspaceDir and PUTs it to destURI. An
	// empty or non-existent workspaceDir is a silent skip (returns
	// nil); the caller decides whether that's an error.
	Export(ctx context.Context, workspaceDir, destURI string) error
}

// GCSExporter implements Exporter against a gs:// destination URI.
type GCSExporter struct {
	credentialSource credential.Source
	httpClient       *http.Client
	endpointBaseURL  string // override for tests
}

// GCSExporterOptions configures a GCSExporter. CredentialSource is required.
type GCSExporterOptions struct {
	CredentialSource credential.Source

	// HTTPClient is optional; a sensibly-bounded client is
	// constructed when nil.
	HTTPClient *http.Client

	// EndpointBaseURL overrides the GCS upload host. Tests point it
	// at an httptest.NewServer; production callers leave it empty.
	EndpointBaseURL string
}

// NewGCSExporter validates options and constructs an exporter.
// Credential resolution is deferred until Export so a transient
// metadata-server hiccup at boot doesn't block the run.
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
// to MaxArchiveBytes) and uploads it to destURI, which must be a
// gs:// URI. An empty or non-existent workspaceDir is a silent no-op.
func (e *GCSExporter) Export(ctx context.Context, workspaceDir, destURI string) error {
	bucket, object, err := parseGCSURI(destURI)
	if err != nil {
		return fmt.Errorf("workspace exporter: %w", err)
	}

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
// preflight: it parses destURI, resolves the credential, and performs
// a read-only bucket-metadata GET. It tars and uploads nothing.
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
// included so the caller can distinguish an empty archive from real
// content. Refuses any path that resolves outside rootDir; see
// docs/security.md#path-traversal-prevention.
func writeTarGz(rootDir string, w io.Writer, maxBytes int64) (int, error) {
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return 0, fmt.Errorf("abs root %q: %w", rootDir, err)
	}
	// Resolved once at the root so a workspace passed via a symlink
	// still has a stable trust boundary against intermediate escapes.
	absRootResolved, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return 0, fmt.Errorf("resolve root %q: %w", rootDir, err)
	}

	// Cap on compressed output, since that's what the GCS upload
	// transmits.
	limited := &limitWriter{w: w, max: maxBytes}
	gz := gzip.NewWriter(limited)
	tw := tar.NewWriter(gz)

	regularFiles := 0
	walkErr := filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Relative to the user-supplied rootDir, not the
		// symlinks-resolved path, so the archive mirrors what the
		// operator sees on disk.
		archiveName, err := filepath.Rel(absRoot, path)
		if err != nil {
			return fmt.Errorf("relative path %q: %w", path, err)
		}
		if archiveName == "." {
			return nil
		}

		if archiveName == ".." || strings.HasPrefix(archiveName, ".."+string(filepath.Separator)) {
			return fmt.Errorf("refusing path %q: escapes workspace root", path)
		}

		// A dangling symlink also trips EvalSymlinks with an error;
		// treat that as a refusal too rather than skip the
		// containment check, since it could otherwise smuggle a
		// symlink-then-overwrite target past a downstream untar that
		// dereferences links.
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
			// Skip block / char / fifo / socket entries.
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
// the cumulative byte count over max, reporting the breach via
// exceeded so the caller can produce a clear error message.
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
