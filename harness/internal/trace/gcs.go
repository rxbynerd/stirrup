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

	// redactionDisabled is the --debug bit; see
	// JSONLTraceEmitter.redactionDisabled for the shared contract. Set only
	// by the factory, and only under a debug build.
	redactionDisabled bool

	mu                   sync.Mutex
	runID                string
	config               *types.RunConfig
	startedAt            time.Time
	turns                []types.TurnTrace
	toolCalls            []types.ToolCallTrace
	permissionDenials    int
	verificationResults  []types.VerificationResult
	finalAssistantText   string
	stopDetails          *types.StopDetails
	commandOutputArchive string
	gitHandoff           *types.GitHandoff
	workspaceExport      *types.ExportReceipt
	runError             string
	structuredOutput     json.RawMessage
	degradedCapabilities []types.DegradedCapability
	repositoryGuidance   []types.RepositoryGuidanceRecord
	auxiliaryTokens      types.TokenUsage
	externalGuardTokens  types.TokenUsage
	postEditChecks       []types.PostEditCheckRecord
}

var _ FinalAssistantTextRecorder = (*GCSTraceEmitter)(nil)
var _ VerificationRecorder = (*GCSTraceEmitter)(nil)
var _ RepositoryGuidanceRecorder = (*GCSTraceEmitter)(nil)
var _ LedgerUsageRecorder = (*GCSTraceEmitter)(nil)
var _ StopDetailsRecorder = (*GCSTraceEmitter)(nil)
var _ DegradedCapabilitiesRecorder = (*GCSTraceEmitter)(nil)
var _ CommandOutputRecorder = (*GCSTraceEmitter)(nil)
var _ CommandOutputArchiveRecorder = (*GCSTraceEmitter)(nil)
var _ GitHandoffRecorder = (*GCSTraceEmitter)(nil)
var _ WorkspaceExportRecorder = (*GCSTraceEmitter)(nil)
var _ RunErrorRecorder = (*GCSTraceEmitter)(nil)
var _ StructuredOutputRecorder = (*GCSTraceEmitter)(nil)
var _ PostEditCheckRecorder = (*GCSTraceEmitter)(nil)

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

	// RedactionDisabled is the --debug bit; see
	// GCSTraceEmitter.redactionDisabled. Production callers pass the
	// factory's already-gated value; every other caller leaves it false.
	RedactionDisabled bool
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
		bucket:            opts.Bucket,
		objectPrefix:      opts.ObjectPrefix,
		bearer:            resolved.BearerToken,
		httpClient:        client,
		endpointBaseURL:   opts.EndpointBaseURL,
		redactionDisabled: opts.RedactionDisabled,
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
	e.permissionDenials = 0
	e.verificationResults = nil
	e.finalAssistantText = ""
	e.stopDetails = nil
	e.degradedCapabilities = nil
	e.commandOutputArchive = ""
	e.gitHandoff = nil
	e.workspaceExport = nil
	e.runError = ""
	e.structuredOutput = nil
	e.repositoryGuidance = nil
	e.auxiliaryTokens = types.TokenUsage{}
	e.externalGuardTokens = types.TokenUsage{}
	e.postEditChecks = nil
}

// RecordTurn appends a turn trace.
func (e *GCSTraceEmitter) RecordTurn(turn types.TurnTrace) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.turns = append(e.turns, turn)
}

// RecordTurnRecord is a no-op for GCS. The single-blob upload path
// is summary-only by design — full transcript recording lives on the
// JSONLTraceEmitter which streams events to a local file.
func (e *GCSTraceEmitter) RecordTurnRecord(_ types.TurnRecord) {}

// RecordToolCall appends a tool call trace.
func (e *GCSTraceEmitter) RecordToolCall(call types.ToolCallTrace) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.toolCalls = append(e.toolCalls, call)
}

// RecordPermissionDenial increments the run-level permission denial count.
func (e *GCSTraceEmitter) RecordPermissionDenial() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.permissionDenials++
}

// RecordVerification appends a verifier attempt for the uploaded trace
// object.
func (e *GCSTraceEmitter) RecordVerification(result types.VerificationResult) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.verificationResults = append(e.verificationResults, result)
}

// RecordFinalAssistantText stores the run's final assistant text so the
// uploaded trace object carries it. The loop forwards a value already
// scrubbed and gated by the PhasePostTurn guard.
func (e *GCSTraceEmitter) RecordFinalAssistantText(text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.finalAssistantText = text
}

// RecordStopDetails stores the run's final stop details so the uploaded
// trace object carries them.
func (e *GCSTraceEmitter) RecordStopDetails(details *types.StopDetails) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopDetails = details
}

// RecordDegradedCapabilities stores the capabilities the run proceeded
// without so the uploaded trace object carries them.
func (e *GCSTraceEmitter) RecordDegradedCapabilities(degraded []types.DegradedCapability) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.degradedCapabilities = append([]types.DegradedCapability(nil), degraded...)
}

// RecordLedgerUsage stores the run's auxiliary and external guard usage
// for the uploaded trace object.
func (e *GCSTraceEmitter) RecordLedgerUsage(auxiliary, externalGuard types.TokenUsage) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.auxiliaryTokens = auxiliary
	e.externalGuardTokens = externalGuard
}

func (e *GCSTraceEmitter) RecordCommandOutput(_ types.CommandOutputRecord) {}

// RecordRepositoryGuidance stores the guidance records so the uploaded
// trace object carries them.
func (e *GCSTraceEmitter) RecordRepositoryGuidance(records []types.RepositoryGuidanceRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.repositoryGuidance = append([]types.RepositoryGuidanceRecord(nil), records...)
}

// RecordPostEditCheck accumulates a post-edit check record for the
// uploaded trace object.
func (e *GCSTraceEmitter) RecordPostEditCheck(record types.PostEditCheckRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.postEditChecks = append(e.postEditChecks, scrubPostEditCheck(record, e.redactionDisabled))
}

func (e *GCSTraceEmitter) RecordCommandOutputArchive(location string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.commandOutputArchive = location
}

// RecordGitHandoff stores the run's git handoff for the RunTrace Finish
// builds.
func (e *GCSTraceEmitter) RecordGitHandoff(handoff types.GitHandoff) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.gitHandoff = &handoff
}

// RecordWorkspaceExport stores the run's workspace export receipt for the
// RunTrace Finish builds.
func (e *GCSTraceEmitter) RecordWorkspaceExport(receipt types.ExportReceipt) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.workspaceExport = &receipt
}

// RecordRunError stores the run's error text for the RunTrace Finish
// builds.
func (e *GCSTraceEmitter) RecordRunError(text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.runError = text
}

// RecordStructuredOutput stores the run's validated structured answer for
// the RunTrace Finish builds.
func (e *GCSTraceEmitter) RecordStructuredOutput(doc json.RawMessage) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.structuredOutput = append(json.RawMessage(nil), doc...)
}

// Finish builds the final RunTrace, marshals it as a single JSONL
// line, and PUTs the bytes to gs://{bucket}/{objectPrefix}/{runID}.jsonl.
// A run with zero turns still produces a single valid JSON line —
// keeping the contract identical to JSONLTraceEmitter so downstream
// consumers can ingest both emitters' output with one parser.
//
// Concurrency: the mutex is held only long enough to snapshot the
// mutable fields; the HTTP upload runs lock-free so a concurrent
// RecordTurn/RecordToolCall or a second Finish call cannot deadlock
// against it.
func (e *GCSTraceEmitter) Finish(ctx context.Context, outcome string) (*types.RunTrace, error) {
	e.mu.Lock()
	runID := e.runID
	cfg := e.config
	startedAt := e.startedAt
	turns := append([]types.TurnTrace(nil), e.turns...)
	toolCalls := append([]types.ToolCallTrace(nil), e.toolCalls...)
	permissionDenials := e.permissionDenials
	verificationResults := append([]types.VerificationResult(nil), e.verificationResults...)
	finalAssistantText := e.finalAssistantText
	stopDetails := e.stopDetails
	commandOutputArchive := e.commandOutputArchive
	gitHandoff := e.gitHandoff
	workspaceExport := e.workspaceExport
	runError := e.runError
	structuredOutput := e.structuredOutput
	degradedCapabilities := e.degradedCapabilities
	repositoryGuidance := e.repositoryGuidance
	auxiliaryTokens := e.auxiliaryTokens
	externalGuardTokens := e.externalGuardTokens
	postEditChecks := append([]types.PostEditCheckRecord(nil), e.postEditChecks...)
	e.mu.Unlock()

	now := time.Now()

	var totalTokens types.TokenUsage
	for _, turn := range turns {
		totalTokens.Add(turn.Tokens)
	}
	totalTokens.Add(auxiliaryTokens)

	summaries := make([]types.ToolCallSummary, len(toolCalls))
	for i, tc := range toolCalls {
		summaries[i] = types.ToolCallSummary(tc)
	}

	var redactedConfig types.RunConfig
	if cfg != nil {
		if e.redactionDisabled {
			redactedConfig = *cfg
		} else {
			redactedConfig = redactRunConfig(cfg)
		}
	}

	trace := &types.RunTrace{
		ID:                   runID,
		Config:               redactedConfig,
		StartedAt:            startedAt,
		CompletedAt:          now,
		Turns:                len(turns),
		TokenUsage:           totalTokens,
		ToolCalls:            summaries,
		PermissionDenials:    permissionDenials,
		VerificationResults:  verificationResults,
		Outcome:              outcome,
		StopDetails:          stopDetails,
		FinalAssistantText:   finalAssistantText,
		CommandOutputArchive: commandOutputArchive,
		Git:                  gitHandoff,
		WorkspaceExport:      workspaceExport,
		Error:                runError,
		StructuredOutput:     structuredOutput,
		DegradedCapabilities: degradedCapabilities,
		RepositoryGuidance:   repositoryGuidance,
		AuxiliaryTokens:      auxiliaryTokens,
		ExternalGuardTokens:  externalGuardTokens,
		PostEditChecks:       postEditChecks,
	}

	data, err := json.Marshal(trace)
	if err != nil {
		return trace, fmt.Errorf("marshal trace: %w", err)
	}
	data = append(data, '\n')

	object := gcsObjectName(e.objectPrefix, runID)
	if err := gcs.UploadObject(ctx, e.httpClient, gcs.UploadOptions{
		Bucket:          e.bucket,
		Object:          object,
		Body:            data,
		ContentType:     "application/x-ndjson",
		Bearer:          e.bearer,
		EndpointBaseURL: e.endpointBaseURL,
	}); err != nil {
		return trace, fmt.Errorf("gcs trace emitter: upload trace: %w", err)
	}

	return trace, nil
}

// Probe verifies the credential can access the configured bucket for a
// dry-run preflight via a read-only bucket-metadata GET. It uploads
// nothing, so a dry-run leaves no trace object behind. A 403/404/401
// surfaces with the GCS diagnostic so a missing bucket or an
// insufficiently-scoped credential is caught before the run ends and
// silently drops its trace.
func (e *GCSTraceEmitter) Probe(ctx context.Context) error {
	return gcs.BucketAccessible(ctx, e.httpClient, gcs.BucketProbeOptions{
		Bucket:          e.bucket,
		Bearer:          e.bearer,
		EndpointBaseURL: e.endpointBaseURL,
	})
}

// Close is a no-op — the in-memory buffer is released when the emitter
// is GC'd, and there is no file handle to release. Implementing Close
// satisfies the io.Closer protocol that the factory uses to register
// owned closers.
func (e *GCSTraceEmitter) Close() error { return nil }
