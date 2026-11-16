package trace

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/rxbynerd/stirrup/harness/internal/httpsput"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// HTTPSPutTraceEmitterOptions configures NewHTTPSPutTraceEmitter. Target
// and Secrets are required.
type HTTPSPutTraceEmitterOptions struct {
	Target  *types.HTTPSPutConfig
	Secrets security.SecretStore

	// HTTPClient is optional; httpsput.NewClient(objectUploadTimeout) is
	// used when nil.
	HTTPClient *http.Client

	// RedactionDisabled is the --debug bit; see
	// ObjectTraceEmitter.redactionDisabled.
	RedactionDisabled bool
}

// NewHTTPSPutTraceEmitter constructs an emitter that uploads the run's
// JSONL trace line with one HTTP PUT to the URL opts.Target resolves to.
// The destination names a single object, so every run the emitter
// records uploads to what the reference holds when that run finishes;
// the run ID is in the document, not the object's name. No error from
// the upload carries the URL.
func NewHTTPSPutTraceEmitter(opts HTTPSPutTraceEmitterOptions) (*ObjectTraceEmitter, error) {
	if opts.Target == nil {
		return nil, errors.New("https-put trace emitter: httpsPut is required")
	}
	if opts.Secrets == nil {
		return nil, errors.New("https-put trace emitter: a secret store is required")
	}
	client := opts.HTTPClient
	if client == nil {
		client = httpsput.NewClient(objectUploadTimeout)
	}
	return &ObjectTraceEmitter{
		upload: func(ctx context.Context, _ string, data []byte) error {
			target, err := httpsput.Resolve(ctx, opts.Secrets, opts.Target)
			if err != nil {
				return fmt.Errorf("https-put trace emitter: %w", err)
			}
			if err := httpsput.Put(ctx, client, target, data, traceContentType); err != nil {
				return fmt.Errorf("https-put trace emitter: upload trace: %w", err)
			}
			return nil
		},
		// A PUT is the only request an upload URL answers, and it would
		// write the object, so the probe stops at resolving the reference.
		probe: func(ctx context.Context) error {
			if _, err := httpsput.Resolve(ctx, opts.Secrets, opts.Target); err != nil {
				return fmt.Errorf("https-put trace emitter: %w", err)
			}
			return nil
		},
		redactionDisabled: opts.RedactionDisabled,
	}, nil
}
