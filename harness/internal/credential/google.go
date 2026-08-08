package credential

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// cloudPlatformScope is the OAuth2 scope Vertex AI Gemini's
// streamGenerateContent endpoint authenticates against.
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// maxServiceAccountKeyBytes bounds the size of a service-account JSON key
// file so a misconfigured path (e.g. pointing at a multi-megabyte log)
// fails fast instead of streaming arbitrary content into the JSON parser.
const maxServiceAccountKeyBytes = 64 * 1024

// GoogleADCSource resolves credentials via Google Application Default
// Credentials, rejecting user-mode gcloud credentials
// (`"type":"authorized_user"`): the harness must not be tethered to a
// single human's account and its expiring, personally-scoped IAM
// grants. Service accounts and metadata-server credentials are accepted.
type GoogleADCSource struct{}

func (g *GoogleADCSource) Resolve(ctx context.Context) (*Resolved, error) {
	creds, err := google.FindDefaultCredentials(ctx, cloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("find default Google credentials: %w", err)
	}

	// creds.JSON is non-empty only when ADC was sourced from a JSON file;
	// metadata-server credentials leave it nil and are unambiguously
	// non-user.
	if len(creds.JSON) > 0 {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(creds.JSON, &probe); err != nil {
			return nil, fmt.Errorf("parse Google credentials JSON: %w", err)
		}
		if probe.Type == "authorized_user" {
			return nil, fmt.Errorf(
				"refusing user-mode Google Application Default Credentials (authorized_user): " +
					"the harness must not run with personal gcloud credentials. " +
					"Set GOOGLE_APPLICATION_CREDENTIALS to a service account JSON key, " +
					"deploy on GKE/GCE with Workload Identity, " +
					"or set provider.credential.type to \"gcp-service-account\" or \"gcp-workload-identity\"",
			)
		}
	}

	return &Resolved{BearerToken: bearerFromTokenSource(creds.TokenSource)}, nil
}

// bearerFromTokenSource adapts an oauth2.TokenSource to the BearerTokenFunc
// closure contract. The closure ignores the request context because
// oauth2.TokenSource.Token() does not accept one — token acquisition and
// refresh are bound to whatever context the caller used when constructing
// the underlying TokenSource (typically context.Background(), so a
// cancelled Resolve context cannot poison subsequent refreshes).
func bearerFromTokenSource(ts oauth2.TokenSource) BearerTokenFunc {
	return func(_ context.Context) (string, error) {
		tok, err := ts.Token()
		if err != nil {
			return "", fmt.Errorf("acquire google token: %w", err)
		}
		return tok.AccessToken, nil
	}
}

// ServiceAccountKeySource resolves credentials from an explicit Google
// service account JSON key file, for runtimes with no metadata server.
// The path itself is not treated as a secret; the file's contents are.
type ServiceAccountKeySource struct {
	path string
}

// NewServiceAccountKeySource creates a credential source that loads a
// service account JSON key file from path.
func NewServiceAccountKeySource(path string) *ServiceAccountKeySource {
	return &ServiceAccountKeySource{path: path}
}

func (s *ServiceAccountKeySource) Resolve(ctx context.Context) (*Resolved, error) {
	if s.path == "" {
		return nil, fmt.Errorf("service account key path is empty")
	}

	// Open + LimitReader rather than Stat + ReadFile to avoid a TOCTOU
	// gap: a symlink swap between the two syscalls could repoint s.path
	// at /dev/zero and stream an unbounded buffer into the JSON parser.
	f, err := os.Open(s.path)
	if err != nil {
		return nil, fmt.Errorf("open service account key %q: %w", s.path, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat service account key %q: %w", s.path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("service account key %q is a directory, not a file", s.path)
	}

	// +1 distinguishes "exactly at the cap" (allowed) from "over the
	// cap" (rejected) without a separate Stat-based size check.
	data, err := io.ReadAll(io.LimitReader(f, maxServiceAccountKeyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read service account key %q: %w", s.path, err)
	}
	if int64(len(data)) > maxServiceAccountKeyBytes {
		return nil, fmt.Errorf(
			"service account key %q exceeds %d bytes; refusing to read files larger than that",
			s.path, maxServiceAccountKeyBytes,
		)
	}

	// Validate the JSON shape before handing to JWTConfigFromJSON to
	// produce a clear error when an operator points at the wrong file.
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("parse service account key %q: %w", s.path, err)
	}
	switch probe.Type {
	case "service_account":
		// expected
	case "authorized_user":
		return nil, fmt.Errorf(
			"service account key %q is a user-mode credential (authorized_user); "+
				"GCPCredentialsFile must be a service_account JSON key",
			s.path,
		)
	case "":
		return nil, fmt.Errorf(
			"service account key %q has no \"type\" field; expected \"service_account\"",
			s.path,
		)
	default:
		return nil, fmt.Errorf(
			"service account key %q has type %q; expected \"service_account\"",
			s.path, probe.Type,
		)
	}

	cfg, err := google.JWTConfigFromJSON(data, cloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("build JWT config from %q: %w", s.path, err)
	}

	// jwt.Config.TokenSource(ctx) binds ctx to every future Token() call,
	// including refreshes; bind to context.Background() rather than the
	// caller's Resolve context so a cancelled pre-run context cannot
	// poison later refreshes. ReuseTokenSource caches the access token
	// between calls, mirroring GoogleWorkloadIdentitySource.
	ts := oauth2.ReuseTokenSource(nil, cfg.TokenSource(context.Background()))
	return &Resolved{BearerToken: bearerFromTokenSource(ts)}, nil
}

// GoogleWorkloadIdentitySource resolves credentials via the GCE/GKE
// metadata server (GKE Workload Identity or a plain GCE VM). Preferred
// over GoogleADCSource on GCP: it fails fast if no metadata server is
// reachable rather than walking the ADC search order.
type GoogleWorkloadIdentitySource struct{}

// NewGoogleWorkloadIdentitySource creates a credential source backed by
// the GCE/GKE metadata server.
func NewGoogleWorkloadIdentitySource() *GoogleWorkloadIdentitySource {
	return &GoogleWorkloadIdentitySource{}
}

func (g *GoogleWorkloadIdentitySource) Resolve(_ context.Context) (*Resolved, error) {
	// Empty service-account name = "default" service account for the
	// running compute instance, which is what Workload Identity binds.
	ts := google.ComputeTokenSource("", cloudPlatformScope)

	cached := oauth2.ReuseTokenSource(nil, ts)
	return &Resolved{BearerToken: bearerFromTokenSource(cached)}, nil
}
