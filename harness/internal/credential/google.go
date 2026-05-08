package credential

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// cloudPlatformScope is the OAuth2 scope required for Vertex AI Gemini
// access. The Vertex API documents this as the canonical scope; narrower
// scopes (e.g. .../auth/aiplatform) exist but are not used by Vertex's
// streamGenerateContent endpoint, which authenticates against the broad
// Cloud Platform scope.
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// maxServiceAccountKeyBytes bounds the size of a service-account JSON key
// file. Real keys are well under 4 KiB; the cap exists to fail fast on
// "wrong file" misconfigurations (e.g. someone points GCPCredentialsFile
// at a multi-megabyte log) rather than streaming arbitrary content into
// the JSON parser.
const maxServiceAccountKeyBytes = 64 * 1024

// GoogleADCSource resolves credentials via Google Application Default
// Credentials. The autonomy invariant of the harness — a coding agent
// must not be tethered to a single human's account — means user-mode
// gcloud credentials are explicitly rejected: these expire whenever the
// human re-runs `gcloud auth login` and bind blast radius to one
// person's IAM grants. Rejection is by inspecting the credentials JSON
// for `"type":"authorized_user"`. Service accounts and metadata-server
// credentials (no JSON, sourced from GCE/GKE) are accepted.
type GoogleADCSource struct{}

func (g *GoogleADCSource) Resolve(ctx context.Context) (*Resolved, error) {
	creds, err := google.FindDefaultCredentials(ctx, cloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("find default Google credentials: %w", err)
	}

	// creds.JSON is non-empty only when ADC was sourced from a JSON file
	// (either GOOGLE_APPLICATION_CREDENTIALS or `gcloud auth
	// application-default login`). Metadata-server credentials leave
	// JSON nil — those are unambiguously non-user credentials.
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

	return &Resolved{GoogleTokenSource: creds.TokenSource}, nil
}

// ServiceAccountKeySource resolves credentials from an explicit Google
// service account JSON key file. Use this when the runtime is not on
// GCP (so there is no metadata server) and operators must mount a key
// file via secret storage. The path itself is not treated as a secret;
// the file's contents are.
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

	info, err := os.Stat(s.path)
	if err != nil {
		return nil, fmt.Errorf("stat service account key %q: %w", s.path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("service account key %q is a directory, not a file", s.path)
	}
	if info.Size() > maxServiceAccountKeyBytes {
		return nil, fmt.Errorf(
			"service account key %q is %d bytes; refusing to read files larger than %d bytes",
			s.path, info.Size(), maxServiceAccountKeyBytes,
		)
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("read service account key %q: %w", s.path, err)
	}

	// Validate the JSON shape before handing to JWTConfigFromJSON so we
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

	// jwt.Config.TokenSource returns an oauth2.TokenSource that handles
	// signing the JWT and exchanging it for an access token, with caching
	// and refresh built in.
	return &Resolved{GoogleTokenSource: cfg.TokenSource(ctx)}, nil
}

// GoogleWorkloadIdentitySource resolves credentials via the GCE/GKE
// metadata server. On GKE this surfaces as Workload Identity: the pod's
// Kubernetes service account is mapped to a Google service account by
// the cluster, and the metadata server vends short-lived OAuth2 access
// tokens for that GSA. The same code path also works on plain GCE VMs.
//
// This source is preferred over GoogleADCSource on GCP because it is
// explicit — it fails fast if no metadata server is reachable rather
// than walking the ADC search order.
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
	// ReuseTokenSource caches the access token in memory and refreshes
	// it lazily before expiry. ComputeTokenSource itself does not cache.
	return &Resolved{GoogleTokenSource: oauth2.ReuseTokenSource(nil, ts)}, nil
}
