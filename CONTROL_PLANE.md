# Control Plane Design Document

## Overview

The control plane is the orchestration layer that sits between external clients (API consumers, CI systems, scheduled triggers) and the Stirrup harness workers. It receives session requests, provisions sandboxed environments, manages credentials, brokers interactive communication during sessions, and reclaims resources on completion.

The harness is a short-lived job. The control plane is a long-lived service. This separation is deliberate: the harness is stateless and disposable; the control plane owns state, scheduling, and lifecycle management.

### Design principles

1. **The harness is a worker, not an orchestrator.** The control plane makes all provisioning, scheduling, and teardown decisions. The harness receives a task, executes it, and exits.
2. **Secrets never transit the harness boundary unnecessarily.** The control plane resolves and injects credentials at provision time. The harness receives secret *references*, not raw values.
3. **Sessions are the unit of isolation.** Each session gets its own sandbox, credential scope, and resource allocation. No shared mutable state between sessions.
4. **The control plane is provider-agnostic.** It dispatches work via a `WorkerDispatcher` interface — gRPC to our harness, A2A to third-party agents. The control plane doesn't know what's behind the adapter.
5. **One API, every environment.** Local development, CI, and production all exercise the same control plane API (`CreateSession`, `SendMessage`, etc.). The API surface is constant; only the backing implementations change per environment. This ensures that local and CI runs test the same code paths that production uses. There is no separate "local runner" binary with its own provisioning logic.

## System architecture

```
                                    ┌──────────────────────────────┐
                                    │        Control Plane         │
                                    │                              │
 ┌──────────┐   gRPC (external)     │  ┌────────────────────────┐  │
 │  Client  │ ────────────────────► │  │   Session Manager      │  │
 │  (API)   │ ◄──────────────────── │  │   - Create/query/cancel│  │
 └──────────┘                       │  │   - Lifecycle FSM       │  │
                                    │  └──────────┬─────────────┘  │
 ┌──────────┐   webhooks / cron     │             │                │
 │Scheduler │ ────────────────────► │  ┌──────────▼─────────────┐  │
 │(internal)│                       │  │   Sandbox Provisioner   │  │
 └──────────┘                       │  │   - K8s Job / container │  │
                                    │  │   - Workspace setup     │  │
                                    │  │   - Credential injection│  │
                                    │  └──────────┬─────────────┘  │
                                    │             │                │
                                    │  ┌──────────▼─────────────┐  │
                                    │  │   Worker Dispatcher     │  │
                                    │  │   - gRPC adapter        │  │
                                    │  │   - A2A adapter         │  │
                                    │  └──────────┬─────────────┘  │
                                    │             │                │
                                    │  ┌──────────▼─────────────┐  │
                                    │  │   Event Broker          │  │
                                    │  │   - Stream to client    │  │
                                    │  │   - Approval routing    │  │
                                    │  │   - Trace persistence   │  │
                                    │  └────────────────────────┘  │
                                    │                              │
                                    │  ┌────────────────────────┐  │
                                    │  │   Credential Vault      │  │
                                    │  │   - Short-lived tokens  │  │
                                    │  │   - Scope-limited       │  │
                                    │  └────────────────────────┘  │
                                    │                              │
                                    │  ┌────────────────────────┐  │
                                    │  │   Session Store         │  │
                                    │  │   - State persistence   │  │
                                    │  │   - Trace lakehouse     │  │
                                    │  └────────────────────────┘  │
                                    └──────────────────────────────┘
                                                  │
                                    gRPC bidi streaming (internal)
                                                  │
                                    ┌─────────────▼────────────────┐
                                    │      Stirrup Harness         │
                                    │      (K8s Job / container)   │
                                    │                              │
                                    │  Receives TaskAssignment     │
                                    │  Streams HarnessEvents       │
                                    │  Exits on completion         │
                                    └──────────────────────────────┘
```

## Deployment profiles

The control plane is a single binary with swappable backend implementations, following the same interface-based composition pattern as the harness. Every environment — local development, CI, production — runs the same control plane code and exercises the same API paths. The difference is which concrete implementations back each internal component.

### Profile overview

| Component | `local` | `ci` | `production` |
|---|---|---|---|
| **Session store** | In-memory | SQLite (file-backed) | PostgreSQL |
| **Sandbox provisioner** | In-process (direct `core.BuildLoop()` call) | Docker container | K8s Job + PV |
| **Credential vault** | Environment variables (`os.Getenv`) | Environment variables (CI secrets) | AWS STS, GitHub App, HashiCorp Vault |
| **Worker dispatcher** | In-process function call (no network) | gRPC to harness in Docker container | gRPC to harness in K8s Pod |
| **Event broker** | Direct channel (no buffering) | Direct channel | Buffered with replay support |
| **Trace persistence** | JSONL to local file | JSONL to local file | S3 + PostgreSQL metadata |
| **Scheduler** | Disabled | Disabled | Cron + webhooks + event triggers |
| **Auth** | None (localhost trusted) | None or static token | mTLS / OAuth2 / OIDC |
| **TLS** | Disabled | Disabled | Required |

### How profiles are selected

```
controlplane serve                          # production defaults
controlplane serve -profile local           # in-memory store, in-process harness, env creds
controlplane serve -profile ci              # SQLite store, Docker provisioner, env creds
```

Profiles are not magic — they are named presets for the `ControlPlaneConfig`, analogous to how `ModePreset` works for `RunConfig`. Any field can be overridden via flags or environment variables regardless of profile.

```go
// ControlPlaneConfig selects implementations for each control plane component.
// This is the control plane's equivalent of RunConfig.
type ControlPlaneConfig struct {
    Profile           string              `json:"profile"`           // "local" | "ci" | "production"
    ListenAddr        string              `json:"listenAddr"`        // gRPC listen address
    SessionStore      SessionStoreConfig  `json:"sessionStore"`      // Type: "memory" | "sqlite" | "postgres"
    Provisioner       ProvisionerConfig   `json:"provisioner"`       // Type: "in-process" | "docker" | "kubernetes"
    CredentialVault   VaultConfig         `json:"credentialVault"`   // Type: "env" | "aws-sts" | "vault"
    Dispatcher        DispatcherConfig    `json:"dispatcher"`        // Type: "in-process" | "grpc"
    EventBroker       EventBrokerConfig   `json:"eventBroker"`       // Type: "direct" | "buffered"
    TracePersistence  TracePersistConfig  `json:"tracePersistence"`  // Type: "jsonl" | "s3"
    TLS               *TLSConfig          `json:"tls,omitempty"`
}
```

### The `local` profile: in-process composition

The `local` profile is the most interesting case. Instead of launching a separate harness process and connecting over gRPC, the control plane constructs the `AgenticLoop` in-process and calls it directly. This eliminates all network overhead while still exercising the full session lifecycle:

```
┌─────────────────────────────────────────────────┐
│  controlplane serve -profile local               │
│                                                  │
│  ┌──────────────┐     ┌───────────────────────┐  │
│  │  gRPC Server  │     │  Session Manager      │  │
│  │  (localhost)  │────►│  (in-memory store)    │  │
│  └──────────────┘     └───────────┬───────────┘  │
│                                   │              │
│                       ┌───────────▼───────────┐  │
│                       │  Sandbox Provisioner   │  │
│                       │  (in-process)          │  │
│                       │                        │  │
│                       │  Calls core.BuildLoop()│  │
│                       │  directly — no Docker, │  │
│                       │  no K8s, no gRPC to    │  │
│                       │  harness.              │  │
│                       └───────────┬───────────┘  │
│                                   │              │
│                       ┌───────────▼───────────┐  │
│                       │  AgenticLoop           │  │
│                       │  (runs in goroutine)   │  │
│                       │  Uses local executor   │  │
│                       └───────────────────────┘  │
└─────────────────────────────────────────────────┘
```

The in-process provisioner implements the same `SandboxProvisioner` interface as the Docker and K8s provisioners. It:

1. Resolves the workspace to the current directory (or a specified path).
2. Resolves credentials via `os.Getenv()`.
3. Builds a `RunConfig` with `executor.type: "local"`.
4. Calls `core.BuildLoop()` and `loop.Run()` in a goroutine.
5. Bridges the harness's `Transport` interface to the control plane's event broker via Go channels (no serialisation, no network).
6. On completion, returns the `RunTrace` to the session manager.

This means `CreateSession` on the local profile goes through the same state machine (PENDING → PROVISIONING → RUNNING → COMPLETING → COMPLETED), the same credential resolution interface, and the same event routing — just with lighter-weight implementations behind each interface.

### The `ci` profile: Docker with real isolation

CI needs real sandbox isolation (the harness runs untrusted code from the repo under test) but doesn't need K8s, a database, or cloud credential providers. The `ci` profile:

1. Uses SQLite for session state (survives process restarts during long CI jobs, no external database).
2. Uses the Docker provisioner (same code path as production's K8s provisioner, but targeting the Docker API instead of the K8s API).
3. Resolves credentials from CI environment variables (GitHub Actions secrets, GitLab CI variables, etc.).
4. Connects to the harness via gRPC — same transport as production.

This is the closest to production without requiring cloud infrastructure. It exercises the full gRPC transport path, real container isolation, and real credential injection.

### The CLI client: `cmd/stirrup`

The CLI is a thin gRPC client. It does not embed control plane logic.

```
cmd/
  harness/main.go            # the harness worker (unchanged)
  controlplane/main.go       # the control plane server (all profiles)
  stirrup/main.go             # CLI client — thin gRPC wrapper
```

For local development, `stirrup` starts an in-process control plane (local profile), calls `CreateSession`, streams events to the terminal, and shuts down on completion. The user never sees the control plane — it looks like a single command:

```
stirrup -prompt "Fix the race condition in pkg/cache/lru.go"
```

Under the hood:

1. `stirrup` starts the control plane in-process (local profile, ephemeral gRPC listener on a random port or Unix socket).
2. `stirrup` calls `CreateSession` with the prompt, mode, and current directory as workspace.
3. The control plane provisions the session (in-process: just builds the loop and runs it).
4. Events stream back to `stirrup`, which renders them to the terminal.
5. On session completion, `stirrup` prints the outcome and exits. The in-process control plane shuts down.

For CI or connecting to a remote control plane:

```
stirrup -endpoint controlplane.internal:9090 -prompt "Run the review"
```

The CLI doesn't know or care whether the control plane is in-process or remote. It's a gRPC client either way.

## External API

The control plane exposes a gRPC service for external clients. This is the API that CI systems, web UIs, CLI tools, and other services call to start and interact with sessions.

### Service definition

```protobuf
syntax = "proto3";
package stirrup.controlplane.v1;

import "google/protobuf/timestamp.proto";
import "google/protobuf/duration.proto";
import "google/protobuf/struct.proto";

// StirrupControlPlane is the external-facing API for creating and managing
// coding agent sessions.
service StirrupControlPlane {
  // CreateSession provisions a sandbox and starts a harness run.
  // Returns a bidirectional stream: the client receives session events
  // and can send interactive responses (approvals, clarifications).
  rpc CreateSession(CreateSessionRequest) returns (stream SessionEvent);

  // SendMessage sends an interactive message to a running session
  // (approval, clarification, cancellation). Used when the client
  // cannot hold a long-lived stream (e.g. webhook-driven flows).
  rpc SendMessage(SendMessageRequest) returns (SendMessageResponse);

  // GetSession returns the current state of a session.
  rpc GetSession(GetSessionRequest) returns (Session);

  // ListSessions returns sessions matching the given filters.
  rpc ListSessions(ListSessionsRequest) returns (ListSessionsResponse);

  // CancelSession requests graceful cancellation of a running session.
  rpc CancelSession(CancelSessionRequest) returns (CancelSessionResponse);

  // GetSessionTrace returns the RunTrace for a completed session.
  rpc GetSessionTrace(GetSessionTraceRequest) returns (SessionTrace);
}
```

### Core messages

```protobuf
message CreateSessionRequest {
  // What to do
  string prompt = 1;
  string mode = 2;                          // "execution" | "planning" | "review" | "research" | "toil"

  // Where to work
  RepositoryRef repository = 3;             // repo + ref to clone/checkout
  string workspace_image = 4;              // container image for the sandbox (optional, has defaults per mode)

  // How to work — optional overrides to the default RunConfig for this mode.
  // Omitted fields use mode defaults.
  RunConfigOverrides config_overrides = 5;

  // Credentials — which credential sets to make available.
  // The control plane resolves these to short-lived tokens at provision time.
  repeated CredentialGrant credentials = 6;

  // Limits
  int32 max_turns = 7;                     // 0 = use mode default
  int32 timeout_seconds = 8;               // 0 = use mode default
  double max_cost_budget = 9;              // 0 = no budget cap

  // Metadata
  map<string, string> labels = 10;         // arbitrary key-value labels for filtering/grouping
  string idempotency_key = 11;            // prevents duplicate session creation on retries

  // Interactive mode
  InteractionPolicy interaction_policy = 12;
}

message RepositoryRef {
  string url = 1;                          // git clone URL (https or ssh)
  string ref = 2;                          // branch, tag, or commit SHA
  string provider = 3;                     // "github" | "gitlab" | "bitbucket" — for API-backed executor
}

message CredentialGrant {
  // Which credential to provision. The control plane maps this to a vault
  // path or credential provider.
  string credential_id = 1;               // e.g. "aws-bedrock", "github-repo-access"

  // Scope constraints — the control plane mints a token with only these
  // permissions. Principle of least privilege.
  repeated string scopes = 2;             // e.g. ["bedrock:InvokeModel", "repos:read"]

  // How the credential is delivered to the harness.
  CredentialDelivery delivery = 3;
}

enum CredentialDelivery {
  CREDENTIAL_DELIVERY_UNSPECIFIED = 0;
  // Injected as an environment variable in the sandbox.
  ENV_VAR = 1;
  // Mounted as a file in the sandbox.
  MOUNTED_FILE = 2;
  // Resolved by the harness's SecretStore via a secret:// reference.
  SECRET_REF = 3;
}

message InteractionPolicy {
  // How the control plane handles permission requests from the harness.
  InteractionMode mode = 1;

  // Maximum time to wait for a client response before auto-denying.
  google.protobuf.Duration approval_timeout = 2;

  // Auto-approve patterns — tool calls matching these patterns are
  // approved without asking the client.
  repeated AutoApproveRule auto_approve_rules = 3;
}

enum InteractionMode {
  INTERACTION_MODE_UNSPECIFIED = 0;
  // All permission requests are auto-approved. For fully autonomous runs.
  AUTO_APPROVE_ALL = 1;
  // Permission requests are streamed to the client for approval.
  INTERACTIVE = 2;
  // Side-effecting tools are denied. For read-only modes.
  DENY_SIDE_EFFECTS = 3;
}

message AutoApproveRule {
  string tool_name_pattern = 1;            // glob pattern, e.g. "read_file", "grep"
}
```

### Session events (server → client)

```protobuf
message SessionEvent {
  oneof event {
    SessionCreated session_created = 1;
    SessionProvisioning session_provisioning = 2;
    SessionRunning session_running = 3;
    TextDelta text_delta = 4;
    ToolCallEvent tool_call = 5;
    ToolResultEvent tool_result = 6;
    ApprovalRequest approval_request = 7;
    ClarificationRequest clarification_request = 8;
    SessionCompleted session_completed = 9;
    SessionFailed session_failed = 10;
    Heartbeat heartbeat = 11;
  }
}

message SessionCreated {
  string session_id = 1;
  google.protobuf.Timestamp created_at = 2;
}

message SessionProvisioning {
  string phase = 1;                        // "cloning_repo" | "building_image" | "starting_sandbox" | "injecting_credentials"
  string detail = 2;
}

message ApprovalRequest {
  string request_id = 1;
  string tool_name = 2;
  google.protobuf.Struct tool_input = 3;
  string description = 4;                 // human-readable description of what the tool will do
  google.protobuf.Duration timeout = 5;   // how long the harness will wait
}

message ClarificationRequest {
  string request_id = 1;
  string question = 2;                    // the model's question to the user
  repeated string suggested_responses = 3;
}

message SessionCompleted {
  string outcome = 1;                     // "success" | "max_turns" | "verification_failed" | "budget_exceeded"
  SessionTrace trace = 2;
  repeated Artifact artifacts = 3;
}

message SessionFailed {
  string error_code = 1;
  string error_message = 2;
  SessionTrace trace = 3;                 // partial trace, if available
}

message Artifact {
  string type = 1;                        // "git_branch" | "diff" | "plan" | "review" | "research_brief"
  string name = 2;
  bytes content = 3;
  map<string, string> metadata = 4;       // e.g. {"branch": "stirrup/run-abc123", "sha": "deadbeef"}
}
```

### Client messages (client → server, via SendMessage RPC)

```protobuf
message SendMessageRequest {
  string session_id = 1;
  oneof message {
    ApprovalResponse approval = 2;
    ClarificationResponse clarification = 3;
    UserMessage user_message = 4;
    CancelRequest cancel = 5;
  }
}

message ApprovalResponse {
  string request_id = 1;
  bool approved = 2;
  string reason = 3;
}

message ClarificationResponse {
  string request_id = 1;
  string response = 2;
}

message UserMessage {
  string text = 1;
}

message CancelRequest {
  string reason = 1;
  bool force = 2;                         // force-kill vs graceful shutdown
}

message SendMessageResponse {
  bool accepted = 1;
  string error = 2;
}
```

## Internal components

### 1. Session Manager

The session manager owns the session lifecycle state machine. Every session moves through a well-defined set of states, and only the session manager can transition between them.

#### Session state machine

```
                 CreateSession
                      │
                      ▼
               ┌─────────────┐
               │   PENDING    │
               └──────┬──────┘
                      │ sandbox provisioned
                      ▼
               ┌─────────────┐
               │ PROVISIONING │──── provision failure ──→ FAILED
               └──────┬──────┘
                      │ harness connected
                      ▼
               ┌─────────────┐
               │   RUNNING    │──── harness error ────→ FAILED
               │              │──── client cancel ────→ CANCELLING
               │              │──── timeout ──────────→ CANCELLING
               └──────┬──────┘
                      │ harness reports done
                      ▼
               ┌─────────────┐
               │  COMPLETING  │    (teardown in progress)
               └──────┬──────┘
                      │ teardown done
                      ▼
               ┌─────────────┐
               │  COMPLETED   │
               └─────────────┘

               ┌─────────────┐
               │  CANCELLING  │──── teardown done ──→ CANCELLED
               └─────────────┘

               ┌─────────────┐
               │    FAILED    │    (terminal)
               └─────────────┘

               ┌─────────────┐
               │  CANCELLED   │    (terminal)
               └─────────────┘
```

#### Session record

```go
type Session struct {
    ID             string            `json:"id"`              // UUIDv7 — sortable by creation time
    State          SessionState      `json:"state"`
    CreatedAt      time.Time         `json:"createdAt"`
    UpdatedAt      time.Time         `json:"updatedAt"`
    CompletedAt    *time.Time        `json:"completedAt,omitempty"`

    // Request (immutable after creation)
    Request        CreateSessionRequest `json:"request"`

    // Runtime state
    RunID          string            `json:"runId,omitempty"`          // harness RunConfig.RunID
    SandboxID      string            `json:"sandboxId,omitempty"`      // K8s Job name or container ID
    WorkerEndpoint string            `json:"workerEndpoint,omitempty"` // gRPC address of the harness
    CredentialIDs  []string          `json:"credentialIds,omitempty"`  // issued credential IDs for revocation

    // Outcome
    Outcome        string            `json:"outcome,omitempty"`
    Trace          *RunTrace         `json:"trace,omitempty"`
    Artifacts      []Artifact        `json:"artifacts,omitempty"`
    ErrorMessage   string            `json:"errorMessage,omitempty"`

    // Metadata
    Labels         map[string]string `json:"labels,omitempty"`
    IdempotencyKey string            `json:"idempotencyKey,omitempty"`
}
```

#### Idempotency

The `idempotency_key` field in `CreateSessionRequest` prevents duplicate sessions from retried requests. The session manager checks for an existing session with the same key before creating a new one. If found and the session is not in a terminal state, the existing session's event stream is returned. If terminal, a new session is created.

Keys are stored with a TTL (default: 24 hours). After expiry, the same key creates a new session.

### 2. Sandbox Provisioner

The provisioner is responsible for creating the isolated execution environment for each session. It translates the session request into concrete infrastructure.

#### Provisioning pipeline

```
CreateSessionRequest
        │
        ▼
┌───────────────────┐
│  1. Resolve image  │  Select container image based on mode, language hints, or explicit override.
└───────┬───────────┘
        │
        ▼
┌───────────────────┐
│  2. Clone repo     │  git clone --depth=1 --branch=<ref> into a workspace volume.
│     (or skip)      │  For API-backed executor (tier 0): skip clone, configure VCS backend.
└───────┬───────────┘
        │
        ▼
┌───────────────────┐
│  3. Mint creds     │  Request short-lived tokens from the credential vault.
│                    │  Each token is scoped to the requested permissions.
└───────┬───────────┘
        │
        ▼
┌───────────────────┐
│  4. Build RunConfig│  Merge: mode defaults + request overrides + provisioned details.
│                    │  Inject secret references, workspace path, executor config.
└───────┬───────────┘
        │
        ▼
┌───────────────────┐
│  5. Launch sandbox │  Create K8s Job (or Docker container for dev).
│                    │  Mount workspace volume. Inject credential env vars.
│                    │  Set resource limits. Configure network policy.
└───────┬───────────┘
        │
        ▼
┌───────────────────┐
│  6. Wait for ready │  The harness connects to the control plane via gRPC
│                    │  and sends a "ready" event. Timeout: 60s.
└───────────────────┘
```

#### Image selection

Default images per mode, overridable in the request:

| Mode | Default image | Rationale |
|---|---|---|
| execution | `stirrup-workspace:latest` | Full toolchain (Go, Node, Python, git, common build tools) |
| planning | `stirrup-workspace-slim:latest` | Read-only tools, smaller image, faster pull |
| review | `stirrup-workspace-slim:latest` | Read-only |
| research | `stirrup-workspace-slim:latest` | Read-only, plus web fetch |
| toil | `stirrup-workspace-slim:latest` | API-oriented, minimal tooling |

Images are pre-built and pushed to an internal registry. They include the harness binary, language runtimes, and common tools. The `workspace_image` field in the request allows callers to specify a custom image with project-specific dependencies pre-installed.

#### Workspace volume lifecycle

For container/microVM executors:

1. **Create**: ephemeral volume (emptyDir or EBS-backed PV for persistence needs).
2. **Clone**: `git clone --depth=1 --single-branch --branch=<ref> <url>` into the volume. For large repos, consider `--filter=blob:none` (treeless clone) with on-demand fetching.
3. **Mount**: volume mounted at `/workspace` inside the sandbox container, read-write.
4. **Teardown**: volume deleted when the session reaches a terminal state. If artifacts are requested (e.g. the resulting git branch), they are extracted *before* teardown.

### 3. Credential Vault

The credential vault is the control plane's interface to secret management infrastructure. It does not store secrets itself — it brokers access to external secret stores and mints short-lived, scope-limited tokens.

#### Credential lifecycle

```
Session created
    │
    ▼
┌─────────────────────┐
│  Resolve credential  │  Map credential_id to a vault path / provider.
│  grant               │  "aws-bedrock" → AWS STS AssumeRole
│                      │  "github-repo-access" → GitHub App installation token
└──────────┬──────────┘
           │
           ▼
┌─────────────────────┐
│  Mint short-lived    │  Request token with requested scopes.
│  token               │  Max TTL: session timeout + 5 min grace.
│                      │  Tag with session ID for audit trail.
└──────────┬──────────┘
           │
           ▼
┌─────────────────────┐
│  Deliver to sandbox  │  ENV_VAR: set in K8s Job spec / Docker env.
│                      │  MOUNTED_FILE: write to a tmpfs-backed secret volume.
│                      │  SECRET_REF: configure harness SecretStore backend.
└──────────┬──────────┘
           │
           ▼
   Session runs...
           │
           ▼
┌─────────────────────┐
│  Revoke on teardown  │  Explicitly revoke all tokens issued for this session.
│                      │  Belt-and-suspenders with TTL expiry.
└─────────────────────┘
```

#### Supported credential providers

| Provider | Credential type | Minting mechanism | Scope model |
|---|---|---|---|
| **AWS STS** | Temporary session credentials | `AssumeRole` with session policy | IAM policy document (inline) |
| **GitHub App** | Installation access token | `POST /app/installations/{id}/access_tokens` | Repository + permission set |
| **GitLab** | Project/group access token | GitLab API | Scopes (read_repository, etc.) |
| **Generic secret** | Static secret from vault | HashiCorp Vault / AWS Secrets Manager / GCP Secret Manager | N/A (static, TTL-bounded by wrapping token) |

#### AWS Bedrock credentials

Since the user specifies AWS Bedrock as the model provider, the credential flow is:

1. The control plane's IAM role has permission to `sts:AssumeRole` on a set of Bedrock-scoped roles.
2. For each session requesting `aws-bedrock` credentials, the vault calls `AssumeRole` with:
   - A session policy that restricts to `bedrock:InvokeModel` and `bedrock:InvokeModelWithResponseStream` only.
   - A session name containing the session ID for CloudTrail audit.
   - Duration: min(session timeout + 5min, 1 hour).
3. The resulting temporary credentials (access key, secret key, session token) are injected as environment variables (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`) in the sandbox.
4. The harness's Bedrock provider adapter picks these up via the standard AWS SDK credential chain — no special configuration needed.

### 4. Worker Dispatcher

The dispatcher manages the bidirectional gRPC connection between the control plane and the harness. It implements the `HarnessControl` service that the harness connects to.

#### Connection lifecycle

```
1. Control plane launches K8s Job with env:
   CONTROL_PLANE_ADDR=control-plane.svc.cluster.local:9090
   RUN_ID=<session-run-id>

2. Harness starts, dials CONTROL_PLANE_ADDR, opens bidi stream:
   HarnessControl.Run()

3. Control plane identifies harness by RUN_ID, sends TaskAssignment.

4. Harness streams HarnessEvents (text_delta, tool_call, tool_result, etc.)
   Control plane forwards to client's SessionEvent stream.

5. When harness needs approval (ask-upstream permission policy):
   - Harness sends ControlEvent with approval_request type
   - Control plane routes ApprovalRequest to client
   - Client responds via SendMessage RPC
   - Control plane forwards response as ControlEvent to harness

6. Harness sends RunComplete → control plane transitions session to COMPLETING.
```

#### Harness identification

When the harness connects, the control plane needs to correlate the connection to a session. Two mechanisms:

1. **Run ID in metadata**: the harness sends its `RUN_ID` as gRPC metadata on the initial connection. The control plane looks up the session by run ID.
2. **Connection timeout**: if no harness connects within 60 seconds of Job creation, the session transitions to FAILED with `error_code: HARNESS_CONNECT_TIMEOUT`.

#### Reconnection

The harness is a short-lived job — if the gRPC connection drops, the harness is likely dead. The control plane does *not* attempt to reconnect. Instead:

- If the K8s Job is still running (container restart), the harness will reconnect on restart and resume from scratch (new run, same session).
- If the Job has failed (exceeded restart limit), the session transitions to FAILED.
- Partial results from the previous connection attempt are preserved in the session's event log.

### 5. Event Broker

The event broker is the bidirectional routing layer between harness events and client streams.

#### Responsibilities

1. **Forward harness events to client**: `HarnessEvent` → `SessionEvent` translation. Enriches events with session-level context (session ID, timestamps).
2. **Route client messages to harness**: `SendMessage` → `ControlEvent` translation. Validates that the referenced session is in RUNNING state and the request_id matches a pending request.
3. **Buffer events for disconnected clients**: if the client's stream disconnects and reconnects, replay buffered events from the point of disconnection. Buffer is bounded (last 1000 events or 5 minutes, whichever is smaller).
4. **Persist events for audit**: all events are written to the session store for post-hoc debugging and compliance.
5. **Timeout management**: if an `ApprovalRequest` is not responded to within its timeout, auto-deny and notify the harness.

#### Approval flow

```
  Harness                    Control Plane                  Client
    │                             │                            │
    │  tool_call (side-effect)    │                            │
    │ ──────────────────────────► │                            │
    │                             │  ApprovalRequest           │
    │                             │ ─────────────────────────► │
    │                             │                            │
    │                             │      (client decides)      │
    │                             │                            │
    │                             │  ApprovalResponse          │
    │                             │ ◄───────────────────────── │
    │  ControlEvent (approval)    │                            │
    │ ◄────────────────────────── │                            │
    │                             │                            │
    │  tool_result                │                            │
    │ ──────────────────────────► │                            │
    │                             │  ToolResultEvent           │
    │                             │ ─────────────────────────► │
```

If the client does not respond within `approval_timeout`:

1. The control plane sends a deny `ControlEvent` to the harness.
2. The client receives a `SessionEvent` indicating the auto-denial.
3. The harness proceeds with the denial (tool returns "Permission denied: approval timeout").

### 6. Session Store

The session store persists session records, event logs, and run traces. It serves two purposes: operational state management and the trace lakehouse.

#### Storage tiers

| Data | Store | Retention | Access pattern |
|---|---|---|---|
| Active session state | PostgreSQL | Until terminal + 7 days | Read/write by session manager, read by API |
| Event log (per session) | PostgreSQL (JSONB) or append-only log | 30 days | Append during run, read for replay/debugging |
| Run traces | Object storage (S3/GCS) + metadata in Postgres | 1 year | Write on completion, read for eval/analysis |
| Aggregated metrics | TimescaleDB or Prometheus | 90 days | Write continuously, read by dashboards/alerts |

#### Trace lakehouse

Every completed session's `RunTrace` is persisted via `RunConfig.Redact()` (stripping credential references) and stored in the lakehouse. The lakehouse feeds:

1. **Eval comparisons** — compare production traces against eval suite baselines.
2. **Cost analytics** — per-team, per-mode, per-model cost breakdowns.
3. **Reliability tracking** — success rates, failure modes, turn distributions over time.
4. **Anomaly detection** — flag runs with unusual token usage, tool failure rates, or cost spikes.

## Session lifecycle: end-to-end flow

### 1. Session creation

```
Client calls CreateSession({
    prompt: "Fix the race condition in pkg/cache/lru.go",
    mode: "execution",
    repository: {url: "https://github.com/org/repo.git", ref: "main", provider: "github"},
    credentials: [
        {credential_id: "aws-bedrock", scopes: ["bedrock:InvokeModel"], delivery: ENV_VAR},
        {credential_id: "github-repo-access", scopes: ["repos:read", "repos:write"], delivery: ENV_VAR},
    ],
    interaction_policy: {mode: INTERACTIVE, approval_timeout: "300s"},
    labels: {"team": "platform", "ticket": "PLAT-1234"},
})
```

### 2. Provisioning

The session manager:

1. Creates session record (state: PENDING).
2. Checks idempotency key.
3. Transitions to PROVISIONING.
4. Calls sandbox provisioner:
   a. Selects image: `stirrup-workspace:latest` (execution mode default).
   b. Mints AWS STS temporary credentials (scoped to Bedrock invoke only).
   c. Mints GitHub installation token (scoped to the target repo, read+write).
   d. Builds RunConfig:
      ```json
      {
        "runId": "run-01JQ...",
        "mode": "execution",
        "prompt": "Fix the race condition in pkg/cache/lru.go",
        "provider": {"type": "bedrock", "region": "us-east-1"},
        "executor": {"type": "container", "workspace": "/workspace", "image": "stirrup-workspace:latest"},
        "permissionPolicy": {"type": "ask-upstream"},
        "maxTurns": 20,
        "timeout": 600
      }
      ```
   e. Creates K8s Job with:
      - Container image: `stirrup-workspace:latest`
      - Env: `CONTROL_PLANE_ADDR`, `RUN_ID`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `GITHUB_TOKEN`
      - Volume: workspace PV
      - Resource limits: 2 CPU, 4Gi memory
      - Network policy: egress only to Bedrock endpoint + control plane
5. Client receives `SessionProvisioning` events as each phase completes.

### 3. Execution

1. Harness starts, clones repo into `/workspace`, connects to control plane.
2. Control plane sends `TaskAssignment` with the RunConfig.
3. Harness runs the agentic loop, streaming events.
4. When the harness calls a side-effecting tool (e.g. `write_file`), the `ask-upstream` permission policy sends an approval request via the transport.
5. Control plane routes the approval request to the client.
6. Client approves or denies.
7. Harness proceeds accordingly.

### 4. Completion

1. Harness sends `RunComplete` with outcome and trace.
2. Control plane transitions session to COMPLETING.
3. Teardown:
   a. Extract artifacts (git branch, diff, etc.) from the workspace volume.
   b. Revoke all credentials issued for this session.
   c. Delete the K8s Job and workspace volume.
   d. Persist the RunTrace to the lakehouse.
4. Session transitions to COMPLETED.
5. Client receives `SessionCompleted` with artifacts and trace.

### 5. Teardown

Teardown is idempotent and runs regardless of how the session ended (success, failure, cancellation). Order matters:

1. **Extract artifacts** — copy any requested outputs before destroying the workspace.
2. **Revoke credentials** — explicitly revoke all short-lived tokens. Even though they have TTLs, explicit revocation is belt-and-suspenders.
3. **Delete sandbox** — delete K8s Job (cascading to Pod), delete workspace PV.
4. **Update session record** — set terminal state, persist trace, record completion time.

A background reconciler periodically scans for sessions stuck in non-terminal states (PROVISIONING or RUNNING beyond their timeout + grace period) and forces teardown.

## Scheduling and triggers

The control plane includes a scheduler for automated session creation. This is how toil mode, periodic reviews, and cron-triggered tasks work.

### Trigger types

| Trigger | Source | Example |
|---|---|---|
| **Cron** | Internal scheduler | "Run a dependency audit every Monday at 09:00 UTC" |
| **Webhook** | GitHub/GitLab events | "On new PR opened, run a review session" |
| **Manual** | CreateSession API | "Fix this bug" |
| **Event** | Internal events | "On deployment to staging, run smoke tests" |

### Trigger definition

```go
type Trigger struct {
    ID                string                `json:"id"`
    Type              string                `json:"type"`      // "cron" | "webhook" | "event"
    Schedule          string                `json:"schedule,omitempty"`   // cron expression
    WebhookFilter     *WebhookFilter        `json:"webhookFilter,omitempty"`
    EventFilter       *EventFilter          `json:"eventFilter,omitempty"`
    SessionTemplate   CreateSessionRequest  `json:"sessionTemplate"`      // template for the session to create
    Enabled           bool                  `json:"enabled"`
    MaxConcurrent     int                   `json:"maxConcurrent"`        // max concurrent sessions from this trigger
}

type WebhookFilter struct {
    Provider    string            `json:"provider"`     // "github" | "gitlab"
    EventTypes  []string          `json:"eventTypes"`   // e.g. ["pull_request.opened", "issue.labeled"]
    Repository  string            `json:"repository"`   // glob pattern, e.g. "org/*"
    Labels      map[string]string `json:"labels,omitempty"` // required labels on the PR/issue
}
```

### Webhook flow

```
GitHub webhook ──→ Control plane webhook endpoint
                        │
                        ▼
                   Match against registered triggers
                        │
                        ▼
                   Hydrate session template with webhook payload
                   (e.g. inject PR number, repo URL, branch ref)
                        │
                        ▼
                   CreateSession (internal call)
```

The webhook payload is available in the session template's `dynamic_context` for injection into the prompt. The webhook endpoint validates GitHub's HMAC signature before processing.

## Observability

### Control plane metrics

| Metric | Type | Labels | Purpose |
|---|---|---|---|
| `sessions_created_total` | Counter | mode, trigger_type | Volume tracking |
| `sessions_active` | Gauge | mode, state | Capacity planning |
| `session_duration_seconds` | Histogram | mode, outcome | Latency SLO tracking |
| `session_cost_dollars` | Histogram | mode, model | Cost tracking |
| `provisioning_duration_seconds` | Histogram | phase | Provisioning performance |
| `credential_mint_total` | Counter | provider, outcome | Credential system health |
| `credential_revoke_total` | Counter | provider, outcome | Teardown reliability |
| `approval_response_seconds` | Histogram | outcome (approved/denied/timeout) | Interaction latency |
| `worker_connect_duration_seconds` | Histogram | outcome | Harness startup performance |

### Structured logging

All control plane components emit structured JSON logs with:
- `session_id` (once assigned)
- `component` (session_manager, provisioner, dispatcher, etc.)
- `event` (session_created, credential_minted, harness_connected, etc.)

### Alerting

| Condition | Severity | Action |
|---|---|---|
| Session stuck in PROVISIONING > 5 min | Warning | Page oncall, investigate provisioner |
| Session stuck in RUNNING > timeout + 10 min | Critical | Force teardown, page oncall |
| Credential mint failure rate > 5% | Critical | Investigate vault connectivity |
| Harness connect timeout rate > 10% | Warning | Check K8s scheduling, image pull times |
| Cost per session > 2x mode average | Warning | Flag for review (possible infinite loop) |

## Security considerations

### Network architecture

```
                                    ┌─────────────────────────────┐
                                    │       VPC / Service Mesh     │
                                    │                             │
  External clients ─── mTLS ──────► │  Control Plane              │
                                    │       │                     │
                                    │       │ gRPC (internal)     │
                                    │       │ mTLS or SPIFFE      │
                                    │       ▼                     │
                                    │  Harness sandbox            │
                                    │       │                     │
                                    │       │ Network policy:     │
                                    │       │ egress only to      │
                                    │       │ model API endpoint  │
                                    │       │ + control plane     │
                                    │       ▼                     │
                                    │  ✗ No other egress          │
                                    └─────────────────────────────┘
```

### Authentication and authorization

1. **Client → Control plane**: mTLS or bearer token (OAuth2/OIDC). The control plane validates the client's identity and checks authorization against an RBAC policy (which teams can create sessions, which repos they can access, budget limits).
2. **Control plane → Harness**: mTLS within the service mesh, or a session-scoped bearer token passed as an environment variable and validated on the gRPC connection.
3. **Harness → Control plane**: same mTLS / bearer token, validated by the control plane's gRPC server.

### Audit trail

Every credential mint, session state transition, approval decision, and teardown action is logged with the session ID, actor (client identity), and timestamp. The audit log is append-only and retained for compliance requirements.

### Blast radius containment

- Each session runs in its own K8s namespace or with its own network policy. Sessions cannot communicate with each other.
- Credentials are scoped to the minimum required permissions and automatically revoked on teardown.
- Workspace volumes are ephemeral and deleted on teardown. No persistent state leaks between sessions.
- The control plane itself does not execute untrusted code. It only orchestrates — the harness does the execution, inside its sandbox.

## Implementation phases

### Phase 1: Local profile + core lifecycle

Deliver: the `local` profile works end-to-end. `stirrup -prompt "..."` runs a session through the full control plane API using in-process composition. This is the foundation — every subsequent phase adds implementations behind the same interfaces.

1. Define `ControlPlaneConfig` and profile presets (`local`, `ci`, `production`)
2. Protobuf definitions for all external API messages
3. Component interfaces: `SessionStore`, `SandboxProvisioner`, `CredentialVault`, `WorkerDispatcher`, `EventBroker`
4. In-memory session store implementation
5. Session Manager with state machine (in-memory backing)
6. In-process sandbox provisioner (calls `core.BuildLoop()` directly, bridges transport via Go channels)
7. Environment variable credential vault
8. Direct event broker (no buffering)
9. gRPC server exposing `CreateSession`, `GetSession`, `CancelSession`, `SendMessage`
10. `cmd/controlplane` binary with `-profile` flag
11. `cmd/stirrup` CLI client with embedded in-process control plane for local use
12. Integration test that exercises `CreateSession` → run → `SessionCompleted` via the gRPC API against the local profile

At the end of this phase, local development and simple CI use cases work. The same API that production will use is exercised on every run.

### Phase 2: CI profile + Docker isolation

Deliver: the `ci` profile provisions real Docker containers and connects via gRPC. CI pipelines can run Stirrup with sandbox isolation.

1. Docker sandbox provisioner (create container, mount workspace, inject env vars, connect harness via gRPC)
2. gRPC worker dispatcher (connects to harness running inside Docker container)
3. SQLite session store implementation
4. JSONL trace persistence to local filesystem
5. `cmd/stirrup -endpoint` flag for connecting to a remote control plane
6. Integration test that exercises the full Docker provisioning path
7. CI pipeline configuration (GitHub Actions) that uses the `ci` profile

### Phase 3: Production profile + cloud infrastructure

Deliver: the `production` profile runs on K8s with real credential isolation and persistent storage.

1. K8s Job-based sandbox provisioner
2. PostgreSQL session store implementation
3. AWS STS credential minting for Bedrock
4. GitHub App installation token minting
5. S3 trace persistence + PostgreSQL metadata
6. Event buffering with client reconnection replay
7. Network policies for sandbox egress control
8. ListSessions with filtering
9. Background reconciler for stuck sessions

### Phase 4: Automation and observability

Deliver: automated session triggers and production observability.

1. Webhook endpoint (GitHub events → CreateSession)
2. Cron-based trigger scheduler
3. Prometheus metrics exporter
4. Structured logging pipeline
5. Alerting rules
6. Cost analytics dashboard

### Phase 5: Multi-tenancy and hardening

Deliver: production-ready for multiple teams with strong isolation.

1. RBAC for client authorization (team-scoped repos, budget limits)
2. Per-namespace session isolation on K8s
3. mTLS between control plane and harness
4. Audit log pipeline
5. Rate limiting on the external API
6. A2A adapter for third-party agent dispatch
7. Auto-approve rule engine for reducing approval friction on trusted operations
