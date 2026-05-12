package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/harness/internal/gcs"
	"github.com/rxbynerd/stirrup/types"
)

// gcsObjectName joins the configured object prefix with the run ID,
// appending ".jsonl". The convention is "prefix joined with '/' when
// prefix is non-empty and does not already end with '/'", so:
//
//	prefix=""           runID="abc" -> "abc.jsonl"
//	prefix="traces"     runID="abc" -> "traces/abc.jsonl"
//	prefix="traces/"    runID="abc" -> "traces/abc.jsonl"
//	prefix="a/b"        runID="abc" -> "a/b/abc.jsonl"
//
// Trailing slash in the prefix is treated as implicit. This is
// documented on TraceEmitterConfig.ObjectPrefix and validated by
// types.ValidateRunConfig.
func gcsObjectName(prefix, runID string) string {
	if prefix == "" {
		return runID + ".jsonl"
	}
	if strings.HasSuffix(prefix, "/") {
		return prefix + runID + ".jsonl"
	}
	return prefix + "/" + runID + ".jsonl"
}

// gcsUploadTimeout bounds the HTTP round-trip for the JSONL trace
// upload. Traces are typically small (well under 1 MiB even for
// verbose runs) so 60s is generous; the cap exists so a wedged
// metadata-server or silently-blackholed GCS endpoint cannot
// indefinitely delay run-end.
const gcsUploadTimeout = 60 * time.Second

// GCSTraceEmitter buffers the final JSONL trace line in memory and
// uploads it to a GCS object at Finish. Unlike JSONLTraceEmitter, no
// file handle is held — the buffer is materialised once at Finish and
// PUT to gs://{bucket}/{objectPrefix}/{runID}.jsonl via the GCS JSON
// API (media upload, single-shot). Streaming/resumable uploads are
// not needed: a JSONL run trace is one line on the wire.
//
// Auth: bearer token from the configured CredentialConfig (defaulting
// to gcp-workload-identity against the metadata server when Credential
// is nil). The credential source is a stirrup credential.Source, not
// the cloud.google.com/go/storage SDK — per project policy in
// CLAUDE.md, vendor cloud SDKs are avoided unless justified.
type GCSTraceEmitter struct {
	bucket          string
	objectPrefix    string
	bearer          credential.BearerTokenFunc
	httpClient      *http.Client
	endpointBaseURL string // override for tests

	mu        sync.Mutex
	runID     string
	config    *types.RunConfig
	startedAt time.Time
	turns     []types.TurnTrace
	toolCalls []types.ToolCallTrace
}

// GCSTraceEmitterOptions configures a GCSTraceEmitter. Bucket is
// required; ObjectPrefix is optional. CredentialSource is the resolved
// credential.Source — wiring decides whether to default to
// GoogleWorkloadIdentitySource or honour an explicit override.
type GCSTraceEmitterOptions struct {
	Bucket           string
	ObjectPrefix     string
	CredentialSource credential.Source

	// HTTPClient is optional; a sensibly-bounded client is constructed
	// when nil. Exposed primarily so tests can inject an httptest
	// server.
	HTTPClient *http.Client

	// EndpointBaseURL overrides the GCS upload host. Defaults to
	// gcs.DefaultEndpointBaseURL. Tests set this to point at an
	// httptest.NewServer; production callers leave it empty.
	EndpointBaseURL string
}

// NewGCSTraceEmitter validates the options and constructs an emitter.
// Returns a config-shaped error when Bucket is empty or the credential
// source is nil — these are unrecoverable at construction time and the
// factory rejects the run rather than waiting until Finish to fail.
func NewGCSTraceEmitter(ctx context.Context, opts GCSTraceEmitterOptions) (*GCSTraceEmitter, error) {
	if opts.Bucket == "" {
		return nil, fmt.Errorf("gcs trace emitter: bucket is required")
	}
	if opts.CredentialSource == nil {
		return nil, fmt.Errorf("gcs trace emitter: credential source is required")
	}

	resolved, err := opts.CredentialSource.Resolve(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcs trace emitter: resolve credential: %w", err)
	}
	if resolved == nil || resolved.BearerToken == nil {
		return nil, fmt.Errorf("gcs trace emitter: credential source produced no bearer token")
	}

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: gcsUploadTimeout,
			Transport: &http.Transport{
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		}
	}

	return &GCSTraceEmitter{
		bucket:          opts.Bucket,
		objectPrefix:    opts.ObjectPrefix,
		bearer:          resolved.BearerToken,
		httpClient:      client,
		endpointBaseURL: opts.EndpointBaseURL,
	}, nil
}

// Start initialises the trace with run metadata. Matches the
// JSONLTraceEmitter contract exactly so the two emitters are
// substitutable in tests and in the factory.
func (e *GCSTraceEmitter) Start(runID string, config *types.RunConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.runID = runID
	e.config = config
	e.startedAt = time.Now()
	e.turns = nil
	e.toolCalls = nil
}

// RecordTurn appends a turn trace.
func (e *GCSTraceEmitter) RecordTurn(turn types.TurnTrace) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.turns = append(e.turns, turn)
}

// RecordToolCall appends a tool call trace.
func (e *GCSTraceEmitter) RecordToolCall(call types.ToolCallTrace) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.toolCalls = append(e.toolCalls, call)
}

// Finish builds the final RunTrace, marshals it as a single JSONL
// line, and PUTs the bytes to gs://{bucket}/{objectPrefix}/{runID}.jsonl.
// A run with zero turns still produces a single valid JSON line —
// keeping the contract identical to JSONLTraceEmitter so downstream
// consumers can ingest both emitters' output with one parser.
func (e *GCSTraceEmitter) Finish(ctx context.Context, outcome string) (*types.RunTrace, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()

	var totalTokens types.TokenUsage
	for _, turn := range e.turns {
		totalTokens.Input += turn.Tokens.Input
		totalTokens.Output += turn.Tokens.Output
	}

	summaries := make([]types.ToolCallSummary, len(e.toolCalls))
	for i, tc := range e.toolCalls {
		summaries[i] = types.ToolCallSummary(tc)
	}

	var redactedConfig types.RunConfig
	if e.config != nil {
		redactedConfig = e.config.Redact()
	}

	trace := &types.RunTrace{
		ID:          e.runID,
		Config:      redactedConfig,
		StartedAt:   e.startedAt,
		CompletedAt: now,
		Turns:       len(e.turns),
		TokenUsage:  totalTokens,
		ToolCalls:   summaries,
		Outcome:     outcome,
	}

	data, err := json.Marshal(trace)
	if err != nil {
		return nil, fmt.Errorf("marshal trace: %w", err)
	}
	data = append(data, '\n')

	object := gcsObjectName(e.objectPrefix, e.runID)
	if err := gcs.UploadObject(ctx, e.httpClient, gcs.UploadOptions{
		Bucket:          e.bucket,
		Object:          object,
		Body:            data,
		ContentType:     "application/x-ndjson",
		Bearer:          e.bearer,
		EndpointBaseURL: e.endpointBaseURL,
	}); err != nil {
		return nil, fmt.Errorf("gcs trace emitter: upload trace: %w", err)
	}

	return trace, nil
}

// Close is a no-op — the in-memory buffer is released when the emitter
// is GC'd, and there is no file handle to release. Implementing Close
// satisfies the io.Closer protocol that the factory uses to register
// owned closers.
func (e *GCSTraceEmitter) Close() error { return nil }
