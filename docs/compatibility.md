# Compatibility matrix

Which combinations of executor, git strategy, workspace export, verifier,
transport, result sink and provider feature work together, which are
refused at validation, which fail later, and which are accepted and then
skipped without a diagnostic. Every restriction cites the function that
enforces it in a footnote; `TestCompatibilityDoc_CitedRulesExist` fails
when a cited function, message or test no longer exists, so a removed rule
cannot leave a stale row.

The tables describe the code as of this page's last edit. For flag-level
detail see [`configuration.md`](configuration.md); the per-executor pages
are [`executors/k8s.md`](executors/k8s.md) and
[`executors/k8s-agent-sandbox.md`](executors/k8s-agent-sandbox.md).

## Reading the tables

The host-process executor is `executor.type: "local"` (the default when
the type is unset).

| Status | Meaning |
|---|---|
| Accepted | Passes validation and construction. The row says what the run does. |
| Rejected at validation | `ValidateRunConfig` returns an error before any component is built. `stirrup run-config --validate` and `stirrup harness` exit non-zero, and a `task_assignment` over `stirrup job` is refused. |
| Fails at build | Validation passes, but building the loop fails before the first turn. `stirrup harness --dry-run` reports it only where the row says so. |
| Fails at run time | The run starts and the combination fails afterwards, usually at the end of the run. |
| Ignored | Accepted, and the setting has no effect. No diagnostic is emitted. |

The Evidence column names the test that pins the row. Tests run in CI under
`just test` unless marked "(live)", which need a container engine or a
cluster and are skipped there. `none` means no test pins the row. Live
evidence is collected in [Live-test evidence](#live-test-evidence).

## Executor capabilities

The capability flags are what the factory consults when it registers
tools and what the validators mirror in `executorExecCapable` and
`executorWorkspaceArchiver`.[^executorExecCapable][^executorWorkspaceArchiver]

| Executor | Read | Write | Exec | Archive workspace | Sandbox identity and git proxy | Where the workspace lives |
|---|---|---|---|---|---|---|
| `local` | yes | yes | yes | yes | no | A directory on the harness host (`executor.workspace`, default the working directory). |
| `container` | yes | yes | yes | yes | yes | `/workspace` in the container, with `executor.workspace` bind-mounted from the host. |
| `k8s` | yes | yes | yes | yes | yes | `/workspace` in the Pod. `executor.workspace` is rejected. |
| `k8s-sandbox` | yes | yes | yes | yes | yes | `/workspace` in the Pod. `executor.workspace` is rejected. |
| `api` | yes | no | no | no | no | None. Files are read from the forge named by `executor.vcsBackend`. |
| `none` | no | no | no | no | no | None. |

## Executor and git strategy

`gitStrategy.type: "none"` works with every executor. The rows below
cover `deterministic`, which runs every git command through the
executor's exec capability.

| Combination | Status | What happens | Evidence |
|---|---|---|---|
| `deterministic` with `local` | Accepted | Git runs on the harness host through `sh -c` in the workspace. The harness host needs `git` on `PATH`. | `TestValidateRunConfig_DeterministicGitRequiresExecCapableExecutor` |
| `deterministic` with `container`, `k8s` or `k8s-sandbox` | Accepted | Git runs inside the sandbox, so the image needs `git`. A missing binary ends the run before the first turn with outcome `error` and the text `git setup: ...`. `--dry-run` probes `git --version` through the executor for `local`, `k8s` and `k8s-sandbox`, and skips the probe for `container` because no container starts in a dry-run, so a missing binary shows only at run start. | `TestValidateRunConfig_DeterministicGitRequiresExecCapableExecutor` |
| `deterministic` with `api` or `none` | Rejected at validation[^validateGitStrategyExecutorCompat] | Neither executor can run commands. | `TestValidateRunConfig_NoneExecutorRejectsDeterministicGit` |
| `deterministic` in a read-only mode | Accepted | Branch creation and checkpoint commits are operator-declared exec outside the agent's tools, like hooks. See [`configuration.md`](configuration.md#lifecycle-hooks). | none |

With the deterministic strategy, setup initialises a repository with a
base commit when the workspace is not one, and refuses a workspace nested
inside an enclosing repository.[^prepareBranch] Checkpoints follow each
turn; the final checkpoint precedes `postRun` hooks and the workspace
export.

## Executor and workspace export

`executor.workspaceExportTo` archives the workspace through the executor,
not from a host path, so a sandboxed executor exports the sandbox's files.

| Combination | Status | What happens | Evidence |
|---|---|---|---|
| `gs://` destination with `local`, `container`, `k8s` or `k8s-sandbox` | Accepted | The executor streams a tar archive that is uploaded with `gcp-workload-identity` credentials. The export runs after the git final checkpoint and `postRun` hooks, even when the run failed. A failed export turns a successful run into `handoff_failed` unless `workspaceExportRequired` is `false`. | `TestExportWorkspace_ArchivesSandboxNotHarnessCwd` |
| Any destination with `api` or `none` | Rejected at validation[^validateExecutorWorkspaceExportTo] | Neither executor can archive a workspace. | `TestValidateExecutorWorkspaceExportTo_ArchiverTable` |
| Any destination with `executor.type` unset | Rejected at validation[^validateExecutorWorkspaceExportTo] | The message asks for an explicit type. | none |
| A scheme other than `gs://`, or an empty bucket | Rejected at validation[^validateExecutorWorkspaceExportTo-scheme] | Only `gs://` is implemented. | `TestValidateRunConfig_WorkspaceExportTo_RejectsNonGSScheme` |
| `workspaceExportRequired` without a destination | Rejected at validation[^validateExecutorWorkspaceExportTo-required] | The flag has nothing to govern. | `TestValidateRunConfig_WorkspaceExportRequired_RejectedWithoutDestination` |
| A destination when the loop is embedded through `harnessapi` | Ignored | See [Embedding](#embedding-through-harnessapi). | none |

The export is a command-line behaviour: the `stirrup harness` and
`stirrup job` entry points inject the exporter, and a loop run without
one skips the step.[^handoffWorkspace][^workspaceHandoff] An executor that
cannot archive reaches the same refusal at run time through
`exportWorkspace`, which backs up the validator for callers that bypass
it.[^exportWorkspace]

## Executor and verifier

| Combination | Status | What happens | Evidence |
|---|---|---|---|
| `test-runner` with `local`, `container`, `k8s` or `k8s-sandbox` | Accepted | `verifier.command` runs through the executor after the model's final answer. A non-zero exit feeds the output back and retries. | none |
| `test-runner` with `api` or `none` | Fails at run time[^apiExecutorExec][^noneExecutorExec] | Validation passes. `Exec` returns an error, so the run ends with outcome `verification_error` after the model has finished. | `TestLoop_VerificationError`[^loopRun] |
| `test-runner` with an empty `command` | Ignored | `sh -c ""` exits 0, so the verifier always passes. The `--verifier test-runner` flag cannot set a command; only `--config` can. | none |
| `llm-judge` with any executor | Accepted | The judge needs no executor. It calls the run's top-level provider with `verifier.model`. | none |
| `llm-judge` with a non-Anthropic provider and no `verifier.model` | Fails at run time[^buildVerifier] | The default model name is `claude-haiku-5-5`, sent to the configured provider as-is. For Bedrock the Bedrock-format ID is required. Set `verifier.model`. | none |
| `composite` | Accepted | Each sub-verifier follows the rows above. Validation checks only the `type` values.[^validateVerifierConfig] | none |

## Executor and exec-dependent features

| Combination | Status | What happens | Evidence |
|---|---|---|---|
| `hooks.preRun` or `hooks.postRun` with `api` or `none` | Rejected at validation[^validateHooksConfig] | Hooks run through the executor. | `TestValidateRunConfig_Hooks_RejectsAPIExecutorWithHooks` |
| `tools.postEdit` with `api` or `none` | Rejected at validation[^validatePostEditChecks] | Post-edit checks run commands through the executor. | `TestValidateRunConfig_PostEdit_RejectedForExecutorsWithoutExec` |
| `repositoryGuidance` with `none` | Rejected at validation[^validateRepositoryGuidanceConfig] | The files are read through the executor. `api` is accepted. | `TestValidateRunConfig_RepositoryGuidance_RejectsInvalid` |
| `tools.builtIn` naming a read, write or exec tool with `none` | Rejected at validation[^validateNoneExecutorTools] | The `none` executor registers no filesystem or shell tools. `web_fetch` and `spawn_agent` are accepted. | `TestValidateRunConfig_NoneExecutorBuiltInToolFailFast` |
| `tools.builtIn` naming `write_file`, an edit tool or `run_command` with `api` | Ignored[^buildToolRegistry] | Tools are registered by capability. `api` can read but not write or exec, so these tools are not registered and no diagnostic is emitted. | none |
| `mode: "execution"` with `api` or `none` | Accepted | The run starts with whatever tools the capabilities permit, which excludes every write and exec tool. | none |
| The `git_*` read tools with `api` | Fails at run time[^buildToolRegistry] | Registered because `api` can read, but each call fails because the executor cannot exec. | none |

## Executor field requirements

| Combination | Status | What happens | Evidence |
|---|---|---|---|
| `k8s` or `k8s-sandbox` without `image`, `k8sNamespace` or `network` | Rejected at validation[^validateK8sExecutor] | All three are required. | `TestValidateRunConfig_K8sExecutorFields` |
| `k8s` or `k8s-sandbox` with `executor.workspace` | Rejected at validation[^validateK8sExecutor] | The Pod workspace is fixed at `/workspace`. | `TestValidateRunConfig_K8sExecutorFields` |
| `k8s` or `k8s-sandbox` in `allowlist` mode without `k8sEgressProxyUrl`, or the URL in any other mode | Rejected at validation[^validateK8sEgressProxy] | The NetworkPolicy confines the Pod to the proxy. | `TestValidateRunConfig_K8sExecutorFields` |
| `k8s-sandbox` with a `runtime` other than `gvisor` | Rejected at validation[^validateExecutorRuntime] | Empty selects gVisor. | `TestValidateRunConfig_K8sSandboxExecutorFields` |
| `runtime` with `local`, `api` or `none` | Rejected at validation[^validateExecutorRuntime] | Only `container`, `k8s` and `k8s-sandbox` take a runtime. | `TestValidateRunConfig_NoneExecutorRuntimeAndRegistryAllowlistRejectedOnce` |
| `registryAllowlist` with any executor but `container` | Rejected at validation[^validateExecutorRegistryAllowlist] | Only the container executor enforces it. | `TestValidateRunConfig_NoneExecutorRuntimeAndRegistryAllowlistRejectedOnce` |
| `container` without `image` | Fails at build[^buildExecutor] | Validation passes. `--dry-run` also passes, because the container executor is not built in a dry-run and the engine probe skips the image check when the image is empty. The run fails with `build executor: container executor requires image`. | none |
| `api` without `vcsBackend` | Fails at build[^buildExecutor-api] | Validation passes and `--dry-run` reports the failure. When the block is present, `vcsBackend.type` must be `github` or `gitlab`.[^validateVcsBackend] | `TestValidateRunConfig_VcsBackendTypeIsClosedSet` |
| `local` or `api` with `image`, `network`, `resources` or `k8s*` fields | Ignored | Only the `none` executor rejects them.[^validateNoneExecutor] The `local` factory case reads only `workspace`. | none |
| `none` with `workspace`, `image`, `vcsBackend`, `network`, `resources` or `k8s*` fields | Rejected at validation[^validateNoneExecutor] | The `none` executor has no execution surface. | `TestValidateRunConfig_NoneExecutorRejectsExecutionOnlyFields` |

## Transport

`transport.type` is `stdio` or `grpc`. A config received in a
`task_assignment` over `stirrup job` always has `grpc`: the translation
layer sets it, and the wire has no field for it. Every gRPC-only row below
is therefore satisfied for control-plane-assigned runs.

| Combination | Status | What happens | Evidence |
|---|---|---|---|
| `tools.controlPlane` with `stdio` | Rejected at validation[^validateControlPlaneTools] | Only a control plane on the stream can answer `tool_result_request`. | `TestValidateRunConfig_ControlPlaneTools_RequiresGRPC` |
| `executor.sandboxIdentity` or `executor.gitProxy` with `stdio` | Rejected at validation[^validateSandboxIdentity] | There is no control plane to issue the token. | `TestValidateRunConfig_SandboxIdentityRequiresGRPC` |
| `ruleOfTwo.runtime.onDetect: "ask-upstream"` with `stdio` | Rejected at validation[^validateRuleOfTwoRuntime] | There is no control plane to answer the permission request. | `TestValidateRunConfig_RuleOfTwoRuntime_AskUpstreamRequiresGRPC` |
| `batch` with `stdio` and without `harnessSidePolling` | Rejected at validation[^validateBatchConfig-stdio] | See [Batch](#batch). | `TestValidateRunConfig_Batch_StdioRequiresHarnessSidePolling` |
| `grpc` without `transport.address` | Fails at build[^buildTransport] | Validation passes. The run fails with `build transport: gRPC transport requires an address`. | none |
| `--transport-tls-*` flags with `stdio` | Rejected at flag parsing[^resolveTransportTLSFlags] | A command-line usage error. These flags are not `RunConfig` fields. | none |
| `transport.type` omitted from a `--config` file | Accepted | The loop builds a stdio transport. Rules written as "requires `grpc`" reject the empty string, while the two `batch` rules written as "rejects `stdio`" pass it. See the `batch` row in [Batch](#batch). | none |

## Batch

`provider.batch.enabled` submits each turn to the provider's batch API.
See [`batch.md`](batch.md) for the behaviour.

| Combination | Status | What happens | Evidence |
|---|---|---|---|
| `mode: "execution"` | Rejected at validation[^validateBatchConfig-mode] | An editing run must not block between turns. | `TestValidateRunConfig_Batch_RejectsExecutionMode` |
| `mode: "planning"` or `"review"` without `allowInteractiveModes` | Rejected at validation[^validateBatchConfig-interactive] | The opt-in acknowledges the latency. | `TestValidateRunConfig_Batch_InteractiveModesRequireOptIn` |
| `mode: "research"` or `"toil"` | Accepted | | none |
| `anthropic`, `openai-compatible` or `openai-responses` provider | Accepted | | `TestValidateRunConfig_Batch_UnsupportedProviderType` |
| `bedrock` or `gemini` provider | Rejected at validation[^validateBatchConfig-provider] | Neither adapter has a batch client. | `TestValidateRunConfig_Batch_UnsupportedProviderType` |
| `harnessSidePolling` with `grpc` | Rejected at validation[^validateBatchConfig-polling-grpc] | The control plane owns polling on the stream. | `TestValidateRunConfig_Batch_HarnessSidePollingRejectedWithGRPC` |
| `cancelBundleOnRunCancel` with `stdio` | Rejected at validation[^validateBatchConfig-cancel] | There is no bundle to cancel. | `TestValidateRunConfig_Batch_CancelBundleRejectedWithStdio` |
| `harnessSidePolling` with `anthropic-wif` credentials | Rejected at validation[^validateBatchConfig-wif] | The polling client sends `x-api-key`, not a bearer token. | `TestValidateRunConfig_Batch_HarnessSidePollingRejectsAnthropicWIF` |
| `fallbackOnTimeout` with `maxWaitSeconds` equal to the run timeout | Rejected at validation[^validateBatchConfig-fallback] | The fallback could never fire. | `TestValidateRunConfig_Batch_FallbackOnTimeoutNeedsHeadroom` |
| `batch` with `transport.type` omitted from a `--config` file | Fails at build[^BuildLoopWithTransport] | Validation checks `stdio` and `grpc` literally and passes the empty string. Building the batch client then fails with `batch is not supported for transport type ""`. `--dry-run` passes. Set `transport.type` explicitly. | none |
| Models routed to a named entry in `providers{}` | Accepted | Only the top-level provider is wrapped, so turns routed to a named provider stream instead of batching.[^BuildLoopWithTransport] | none |

## Lazy tool loading and provider

`tools.lazyLoading.enabled` is never rejected for a provider or executor
combination; validation checks only the field values.[^validateLazyToolLoading]
The provider decides which search the model uses. The full model list is
in [`configuration.md`](configuration.md#strategies-and-providers).

| Combination | Status | What happens | Evidence |
|---|---|---|---|
| `anthropic` on a model with native tool search | Accepted | The provider's search tool with deferred tools sent with `defer_loading`. | none |
| `openai-responses` on the `openai` endpoint profile and a supporting model | Accepted | The hosted `tool_search`. | none |
| `openai-responses` on a `generic`, `azure-openai` or `bedrock-mantle` profile | Accepted | The harness `tool_search`. | none |
| `bedrock`, `gemini`, `openai-compatible`, and older models | Accepted | The harness `tool_search`. | none |
| Only `tool_search` eager | Accepted | A native search needs one other eager tool, so `auto` falls back to the harness search. | none |
| No MCP, control-plane or deferred tools | Accepted | Lazy loading changes nothing and `tool_search` is not sent. | none |

`stirrup harness --dry-run` prints a `lazy-tools` step stating which
search each routable provider and model would use.[^buildToolSearchSupport]
Control-plane tools are deferred unless named in `eager`; they require
gRPC regardless (see [Transport](#transport)). They do not need a
filesystem, so they work with the `none` executor and in read-only modes.

## Sandbox identity and git proxy

`executor.sandboxIdentity` and `executor.gitProxy` request a
control-plane-issued token and route git traffic through a credential
proxy. See [`configuration.md`](configuration.md#sandbox-identity-and-git-proxy-wiring).

| Combination | Status | What happens | Evidence |
|---|---|---|---|
| With `container`, `k8s` or `k8s-sandbox` over `grpc` | Accepted | The token is delivered to the sandbox at creation and refreshed through a token file. | `TestContainerExecutor_SandboxIdentityToken_LiveEngine` (live) |
| With `local`, `api` or `none` | Rejected at validation[^validateSandboxIdentity-executor] | Only executors that create a sandbox at run start can receive the token. | `TestValidateRunConfig_SandboxIdentityExecutorTypeGating` |
| `gitProxy` without `sandboxIdentity`, or with a mismatched `tokenEnvVar` | Rejected at validation[^validateSandboxIdentity-proxy] | No other token source exists. | `TestValidateRunConfig_GitProxyRequiresSandboxIdentity` |
| `gitProxy.url` missing from `network.allowlist` in `allowlist` mode | Accepted | A warning is logged and the sandbox has no route to the proxy.[^warnGitProxyAllowlistGap] | none |

## Result sink

| Combination | Status | What happens | Evidence |
|---|---|---|---|
| `none` or `stdout-json` | Accepted | `stdout-json` writes one `STIRRUP_RESULT ` line, last on stdout. With `transport: "stdio"` the event stream shares stdout. The sink is independent of executor, provider and git strategy. | none |
| `gcp-pubsub` or `gcs` | Rejected at validation[^validateResultSinkConfig] | Reserved and not implemented. | `TestValidateRunConfig_ResultSink_GCPPubsubReserved` |
| A sink failure | Accepted | Logged as a warning. The run's outcome and exit code are unaffected. | none |
| A sink when the loop is embedded through `harnessapi` | Ignored | See [Embedding](#embedding-through-harnessapi). | none |

## Embedding through `harnessapi`

`harnessapi.BuildLoopWithTransport` builds and runs the loop only. The
workspace export and the result sink are attached by the command-line
entry points, so an embedder that sets `executor.workspaceExportTo` or
`resultSink` passes validation and receives neither.[^harnessapi]
Git strategy, hooks, verifiers and the trace emitter run inside the loop
and are unaffected.

## Operations that run on the harness host

Whatever the executor, these run in the harness process or on its
filesystem. The sandbox boundary and `executor.network` do not apply.

| Operation | Where it runs |
|---|---|
| `web_fetch` | The harness process.[^WebFetchTool] |
| Cedar `permissionPolicy.policyFile` | Read from the harness host's filesystem.[^parsePolicySetFile] |
| The `semgrep` code scanner | A `semgrep` binary on the harness host's `PATH`, fed the edit content on stdin.[^defaultSemgrepRun] |
| Command output capture spool | A temporary directory on the harness host.[^commandOutputNew] |
| `local` and `container` workspaces | A directory on the harness host. |

Deterministic git and the workspace export are not in this table: both run
through the executor.

## Live-test evidence

Nothing in this section runs in CI. The workflows are dispatched by hand,
and the integration suites are skipped unless an environment variable
names a container engine or a cluster, which no CI job sets.

| Pair | Evidence |
|---|---|
| `local` with Anthropic, OpenAI, Azure OpenAI, Bedrock or Vertex Gemini | The `smoke-*.yml` workflows (`workflow_dispatch`) run `stirrup harness` against each provider with the matching `examples/runconfig/*-smoke.json` or `*-github-actions.json` file. Each uses the `local` executor over `stdio`. See [`smoke-tests.md`](smoke-tests.md). |
| `local` with Vertex Gemini, `stdout-json` sink and `gs://` workspace export | `smoke-cloud-run-job.yml` deploys [`cloud-run-vertex-gemini.json`](../examples/runconfig/cloud-run-vertex-gemini.json) as a Cloud Run job. See [`cloud-run-jobs.md`](cloud-run-jobs.md). |
| `container` executor, no model | `container_integration_test.go` under `harness/internal/executor` and `harness/internal/workspaceexport` (skipped unless `STIRRUP_TEST_CONTAINER_SOCKET` is set) and under `harness/internal/git` and `harness/internal/prompt` (also the `integration_container` build tag and `STIRRUP_TEST_CONTAINER_IMAGE`, an image containing git). Covers exec, symlink containment, workspace archive, sandbox identity tokens, deterministic git and prompt reads. |
| `k8s` and `k8s-sandbox` executors, no model | `harness/internal/executor/k8s_test.go` and `agentsandbox_integration_test.go` (build tag `integration_k8s`, `STIRRUP_TEST_KUBECONFIG`). See [`executors/k8s.md`](executors/k8s.md#testing-the-executor-against-a-real-cluster). |

No repository-tracked workflow pairs a sandboxing executor with a live
provider. Rows elsewhere on this page marked "Accepted" with Evidence
`none` are validated by reading the code, not by a recorded live run.

## Not covered

The Rule of Two, the permission policies and the guardrail classifiers
constrain what a run may do rather than which components combine; see
[`safety-rings.md`](safety-rings.md). Per-field validation that involves a
single component (value ranges, closed sets, secret references) is listed
with each field in [`configuration.md`](configuration.md).

## Maintaining this page

Each footnote id below is a row in the rule list that
`TestCompatibilityDoc_CitedRulesExist` reads. A row names the file, the
declaration, a substring of the message the declaration produces and,
optionally, a test in the same directory. When a validator is renamed or
a message is reworded, update the rule list and the footnote together.
`TestCompatibilityDoc_FootnotesMatchRuleList` fails when a footnote and a
rule list entry do not pair up.

<!-- compat-rules:start -->
```text
id | path | declaration | message substring | test
executorExecCapable | types/runconfig.go | executorExecCapable | |
executorWorkspaceArchiver | types/runconfig.go | executorWorkspaceArchiver | |
validateGitStrategyExecutorCompat | types/runconfig.go | validateGitStrategyExecutorCompat | is not valid with executor.type | TestValidateRunConfig_NoneExecutorRejectsDeterministicGit
validateExecutorWorkspaceExportTo | types/runconfig.go | validateExecutorWorkspaceExportTo | cannot archive its workspace | TestValidateExecutorWorkspaceExportTo_ArchiverTable
validateExecutorWorkspaceExportTo-scheme | types/runconfig.go | validateExecutorWorkspaceExportTo | must use the gs:// scheme | TestValidateRunConfig_WorkspaceExportTo_RejectsNonGSScheme
validateExecutorWorkspaceExportTo-required | types/runconfig.go | validateExecutorWorkspaceExportTo | requires executor.workspaceExportTo | TestValidateRunConfig_WorkspaceExportRequired_RejectedWithoutDestination
handoffWorkspace | harness/internal/core/handoff.go | AgenticLoop.handoffWorkspace | | TestRun_HandoffSkippedWithoutWorkspaceOrDestination
workspaceHandoff | harness/cmd/stirrup/cmd/root.go | workspaceHandoff | |
exportWorkspace | harness/cmd/stirrup/cmd/root.go | exportWorkspace | cannot archive its workspace | TestExportWorkspace_ExecutorWithoutArchiver
apiExecutorExec | harness/internal/executor/api.go | APIExecutor.Exec | command execution not supported |
noneExecutorExec | harness/internal/executor/none.go | NoneExecutor.Exec | exec not supported |
loopRun | harness/internal/core/loop.go | AgenticLoop.Run | verification_error | TestLoop_VerificationError
buildVerifier | harness/internal/core/factory.go | buildVerifier | claude-haiku-5-5 |
validateVerifierConfig | types/runconfig.go | validateVerifierConfig | |
validateHooksConfig | types/runconfig.go | validateHooksConfig | hooks require an exec-capable executor | TestValidateRunConfig_Hooks_RejectsAPIExecutorWithHooks
validatePostEditChecks | types/runconfig.go | validatePostEditChecks | tools.postEdit requires an exec-capable executor | TestValidateRunConfig_PostEdit_RejectedForExecutorsWithoutExec
validateRepositoryGuidanceConfig | types/repositoryguidance.go | validateRepositoryGuidanceConfig | requires an executor that can read the workspace | TestValidateRunConfig_RepositoryGuidance_RejectsInvalid
validateNoneExecutorTools | types/runconfig.go | validateNoneExecutorTools | requires an execution capability the none executor lacks | TestValidateRunConfig_NoneExecutorBuiltInToolFailFast
buildToolRegistry | harness/internal/core/factory.go | buildToolRegistry | |
validateK8sExecutor | types/runconfig.go | validateK8sExecutor | executor.network is required for executor.type | TestValidateRunConfig_K8sExecutorFields
validateK8sEgressProxy | types/runconfig.go | validateK8sEgressProxy | executor.k8sEgressProxyUrl is required when | TestValidateRunConfig_K8sExecutorFields
validateExecutorRuntime | types/runconfig.go | validateExecutorRuntime | executor.runtime must be "gvisor" or empty | TestValidateRunConfig_K8sSandboxExecutorFields
validateExecutorRegistryAllowlist | types/runconfig.go | validateExecutorRegistryAllowlist | executor.registryAllowlist is only valid when executor.type is "container" | TestValidateRunConfig_NoneExecutorRuntimeAndRegistryAllowlistRejectedOnce
buildExecutor | harness/internal/core/factory.go | buildExecutor | container executor requires image |
buildExecutor-api | harness/internal/core/factory.go | buildExecutor | api executor requires vcsBackend configuration |
validateVcsBackend | types/runconfig.go | validateVcsBackend | executor.vcsBackend | TestValidateRunConfig_VcsBackendTypeIsClosedSet
validateNoneExecutor | types/runconfig.go | validateNoneExecutor | is not valid for executor.type="none" | TestValidateRunConfig_NoneExecutorRejectsExecutionOnlyFields
validateControlPlaneTools | types/runconfig.go | validateControlPlaneTools | tools.controlPlane requires transport=grpc | TestValidateRunConfig_ControlPlaneTools_RequiresGRPC
validateSandboxIdentity | types/runconfig.go | validateSandboxIdentity | requires transport=grpc | TestValidateRunConfig_SandboxIdentityRequiresGRPC
validateSandboxIdentity-executor | types/runconfig.go | validateSandboxIdentity | is only valid for executor.type | TestValidateRunConfig_SandboxIdentityExecutorTypeGating
validateSandboxIdentity-proxy | types/runconfig.go | validateSandboxIdentity | executor.gitProxy requires executor.sandboxIdentity | TestValidateRunConfig_GitProxyRequiresSandboxIdentity
warnGitProxyAllowlistGap | types/runconfig.go | warnGitProxyAllowlistGap | is not present in executor.network.allowlist |
validateRuleOfTwoRuntime | types/runconfig.go | validateRuleOfTwoRuntime | ruleOfTwo.runtime.onDetect "ask-upstream" requires transport=grpc | TestValidateRunConfig_RuleOfTwoRuntime_AskUpstreamRequiresGRPC
validateBatchConfig-mode | types/runconfig.go | validateBatchConfig | batch cannot be used with mode=execution | TestValidateRunConfig_Batch_RejectsExecutionMode
validateBatchConfig-interactive | types/runconfig.go | validateBatchConfig | requires allowInteractiveModes=true | TestValidateRunConfig_Batch_InteractiveModesRequireOptIn
validateBatchConfig-provider | types/runconfig.go | validateBatchConfig | is not supported for provider type | TestValidateRunConfig_Batch_UnsupportedProviderType
validateBatchConfig-stdio | types/runconfig.go | validateBatchConfig | batch with transport=stdio requires harnessSidePolling=true | TestValidateRunConfig_Batch_StdioRequiresHarnessSidePolling
validateBatchConfig-polling-grpc | types/runconfig.go | validateBatchConfig | batch.harnessSidePolling must not be set with transport=grpc | TestValidateRunConfig_Batch_HarnessSidePollingRejectedWithGRPC
validateBatchConfig-cancel | types/runconfig.go | validateBatchConfig | batch.cancelBundleOnRunCancel requires transport=grpc | TestValidateRunConfig_Batch_CancelBundleRejectedWithStdio
validateBatchConfig-wif | types/runconfig.go | validateBatchConfig | does not support anthropic-wif credentials | TestValidateRunConfig_Batch_HarnessSidePollingRejectsAnthropicWIF
validateBatchConfig-fallback | types/runconfig.go | validateBatchConfig | batch.fallbackOnTimeout requires batch.maxWaitSeconds strictly below the run timeout | TestValidateRunConfig_Batch_FallbackOnTimeoutNeedsHeadroom
BuildLoopWithTransport | harness/internal/core/factory.go | BuildLoopWithTransport | batch is not supported for transport type |
buildTransport | harness/internal/core/factory.go | buildTransport | gRPC transport requires an address |
resolveTransportTLSFlags | harness/cmd/stirrup/cmd/transporttls.go | resolveTransportTLSFlags | require --transport grpc | TestResolveTransportTLSFlags_RejectsInvalidSettings
prepareBranch | harness/internal/git/deterministic.go | DeterministicGitStrategy.prepareBranch | refusing to operate on an enclosing repository | TestSetup_RejectsEnclosingRepository
validateLazyToolLoading | types/runconfig.go | validateLazyToolLoading | tools.lazyLoading.strategy | TestValidateLazyToolLoading_RejectsBadValues
buildToolSearchSupport | harness/internal/core/factory.go | buildToolSearchSupport | |
validateResultSinkConfig | types/runconfig.go | validateResultSinkConfig | is reserved but not yet implemented | TestValidateRunConfig_ResultSink_GCPPubsubReserved
harnessapi | harness/harnessapi/harnessapi.go | BuildLoopWithTransport | |
WebFetchTool | harness/internal/tool/builtins/webfetch.go | WebFetchTool | |
parsePolicySetFile | harness/internal/permission/policyengine.go | parsePolicySetFile | |
defaultSemgrepRun | harness/internal/security/codescanner/semgrep.go | defaultSemgrepRun | |
commandOutputNew | harness/internal/commandoutput/store.go | New | |
```
<!-- compat-rules:end -->

[^executorExecCapable]: `types/runconfig.go`, `executorExecCapable` and `ExecutorCanExec`: the types-side mirror of which executors can run commands. The hook, post-edit and deterministic-git checks all read it.
[^executorWorkspaceArchiver]: `types/runconfig.go`, `executorWorkspaceArchiver` and `ExecutorCanArchiveWorkspace`: which executors implement the workspace archive capability. A parity test in the executor package pins the table against the implementations.
[^validateGitStrategyExecutorCompat]: `types/runconfig.go`, `validateGitStrategyExecutorCompat`: `gitStrategy.type="deterministic" is not valid with executor.type=...`.
[^validateExecutorWorkspaceExportTo]: `types/runconfig.go`, `validateExecutorWorkspaceExportTo`: `executor.workspaceExportTo is not valid for executor.type=... (the ... executor cannot archive its workspace)`, and, for an unset type, `executor.workspaceExportTo requires an explicit executor.type other than 'api' or 'none'`.
[^validateExecutorWorkspaceExportTo-scheme]: `types/runconfig.go`, `validateExecutorWorkspaceExportTo`: `executor.workspaceExportTo ... must use the gs:// scheme`, and a message for an empty bucket path.
[^validateExecutorWorkspaceExportTo-required]: `types/runconfig.go`, `validateExecutorWorkspaceExportTo`: `executor.workspaceExportRequired requires executor.workspaceExportTo`.
[^handoffWorkspace]: `harness/internal/core/handoff.go`, `AgenticLoop.handoffWorkspace`: returns the run's outcome unchanged when `Handoff` is nil or the executor type is `api` or `none`.
[^workspaceHandoff]: `harness/cmd/stirrup/cmd/root.go`, `workspaceHandoff`: the `core.WorkspaceHandoff` that `stirrup harness` and `stirrup job` assign to the loop.
[^exportWorkspace]: `harness/cmd/stirrup/cmd/root.go`, `exportWorkspace`: refuses an executor that does not implement `executor.WorkspaceArchiver`.
[^apiExecutorExec]: `harness/internal/executor/api.go`, `APIExecutor.Exec`: returns `api executor: command execution not supported`. The GitLab backend returns the same error.
[^noneExecutorExec]: `harness/internal/executor/none.go`, `NoneExecutor.Exec`: returns `none executor: exec not supported`.
[^loopRun]: `harness/internal/core/loop.go`, `AgenticLoop.Run`: a verifier error ends the run with outcome `verification_error` and the text `verifier: ...`.
[^buildVerifier]: `harness/internal/core/factory.go`, `buildVerifier`: an empty `verifier.model` becomes `claude-haiku-5-5`; the verifier is built over the run's top-level provider adapter.
[^validateVerifierConfig]: `types/runconfig.go`, `validateVerifierConfig`: validates `type` only, recursively for `composite`. No field of a `test-runner` verifier is required.
[^validateHooksConfig]: `types/runconfig.go`, `validateHooksConfig`: `hooks require an exec-capable executor; executor.type "api" & "none" are read-only`.
[^validatePostEditChecks]: `types/runconfig.go`, `validatePostEditChecks`: `tools.postEdit requires an exec-capable executor; ...`.
[^validateRepositoryGuidanceConfig]: `types/repositoryguidance.go`, `validateRepositoryGuidanceConfig`: `repositoryGuidance requires an executor that can read the workspace; executor.type "none" cannot`.
[^validateNoneExecutorTools]: `types/runconfig.go`, `validateNoneExecutorTools`: `tools.builtIn entry ... requires an execution capability the none executor lacks`.
[^buildToolRegistry]: `harness/internal/core/factory.go`, `buildToolRegistry`: each built-in is registered only when the executor's declared capability allows it, and the function emits no diagnostic for a tool it skips. The `git_*` tools register on `CanRead` and fail on use when the executor cannot exec.
[^validateK8sExecutor]: `types/runconfig.go`, `validateK8sExecutor`: `executor.image`, `executor.k8sNamespace` and `executor.network` are required, and `executor.workspace` is refused, for `k8s` and `k8s-sandbox`.
[^validateK8sEgressProxy]: `types/runconfig.go`, `validateK8sEgressProxy`: `executor.k8sEgressProxyUrl is required when executor.network.mode is "allowlist"`, and `... is only valid when executor.network.mode is "allowlist"`.
[^validateExecutorRuntime]: `types/runconfig.go`, `validateExecutorRuntime`: a closed set per executor type, `gvisor` only for `k8s-sandbox`, and `executor.runtime is only valid when executor.type is "container", "k8s", or "k8s-sandbox"` otherwise.
[^validateExecutorRegistryAllowlist]: `types/runconfig.go`, `validateExecutorRegistryAllowlist`: `executor.registryAllowlist is only valid when executor.type is "container"`.
[^buildExecutor]: `harness/internal/core/factory.go`, `buildExecutor`: `container executor requires image`. The preflight skips constructing a container executor, and `probeContainerEngine` skips the image check for an empty image.
[^buildExecutor-api]: `harness/internal/core/factory.go`, `buildExecutor`: `api executor requires vcsBackend configuration`. `validateVcsBackend` returns early for a nil block.
[^validateVcsBackend]: `types/runconfig.go`, `validateVcsBackend`: `executor.vcsBackend type` must be a member of the closed set.
[^validateNoneExecutor]: `types/runconfig.go`, `validateNoneExecutor`: `executor.<field> is not valid for executor.type="none" (the none executor has no execution surface)`. The function returns early for every other type.
[^validateControlPlaneTools]: `types/runconfig.go`, `validateControlPlaneTools`: `tools.controlPlane requires transport=grpc`.
[^validateSandboxIdentity]: `types/runconfig.go`, `validateSandboxIdentity`: `executor.sandboxIdentity/executor.gitProxy requires transport=grpc`. Every rule in the function is gated on one of the two blocks being present.
[^validateSandboxIdentity-executor]: `types/runconfig.go`, `validateSandboxIdentity` with `sandboxIdentityCapableExecutors`: `... is only valid for executor.type "container", "k8s", or "k8s-sandbox"`.
[^validateSandboxIdentity-proxy]: `types/runconfig.go`, `validateSandboxIdentity`: `executor.gitProxy requires executor.sandboxIdentity to be set`, and a check that the two env var names agree.
[^warnGitProxyAllowlistGap]: `types/runconfig.go`, `warnGitProxyAllowlistGap`: a `slog.Warn` only; the check is an exact match, so a wildcard entry covering the proxy host also warns.
[^validateRuleOfTwoRuntime]: `types/runconfig.go`, `validateRuleOfTwoRuntime`: `ruleOfTwo.runtime.onDetect "ask-upstream" requires transport=grpc`.
[^validateBatchConfig-mode]: `types/runconfig.go`, `validateBatchConfig`: `batch cannot be used with mode=execution`.
[^validateBatchConfig-interactive]: `types/runconfig.go`, `validateBatchConfig`: `batch requires allowInteractiveModes=true for mode=...`.
[^validateBatchConfig-provider]: `types/runconfig.go`, `validateBatchConfig` with `validBatchProviderTypes`: `batch is not supported for provider type ... in v1`.
[^validateBatchConfig-stdio]: `types/runconfig.go`, `validateBatchConfig`: `batch with transport=stdio requires harnessSidePolling=true`.
[^validateBatchConfig-polling-grpc]: `types/runconfig.go`, `validateBatchConfig`: `batch.harnessSidePolling must not be set with transport=grpc`. Checked even when `enabled` is false.
[^validateBatchConfig-cancel]: `types/runconfig.go`, `validateBatchConfig`: `batch.cancelBundleOnRunCancel requires transport=grpc`. Checked even when `enabled` is false.
[^validateBatchConfig-wif]: `types/runconfig.go`, `validateBatchConfig`: `batch.harnessSidePolling does not support anthropic-wif credentials in v1`.
[^validateBatchConfig-fallback]: `types/runconfig.go`, `validateBatchConfig`: `batch.fallbackOnTimeout requires batch.maxWaitSeconds strictly below the run timeout`. A `maxWaitSeconds` above the run timeout is rejected by the same function.
[^buildTransport]: `harness/internal/core/factory.go`, `buildTransport`: `gRPC transport requires an address`. No validator checks `transport.address`.
[^resolveTransportTLSFlags]: `harness/cmd/stirrup/cmd/transporttls.go`, `resolveTransportTLSFlags`: `--transport-tls-* flags require --transport grpc`, reported as a usage error.
[^prepareBranch]: `harness/internal/git/deterministic.go`, `DeterministicGitStrategy.prepareBranch`: `workspace ... is inside repository ...; refusing to operate on an enclosing repository`, and initialisation of a non-repository workspace.
[^BuildLoopWithTransport]: `harness/internal/core/factory.go`, `BuildLoopWithTransport`: the batch client is selected by `config.Transport.Type`, with `batch is not supported for transport type` as the default case, and only the top-level provider is wrapped with the batch adapter.
[^validateLazyToolLoading]: `types/runconfig.go`, `validateLazyToolLoading`: checks `strategy`, `maxResults` and the `eager` names. It reads no other part of the config.
[^buildToolSearchSupport]: `harness/internal/core/factory.go`, `buildToolSearchSupport`: resolves native search support from the quirks registry; the preflight `lazy-tools` step reports its result.
[^validateResultSinkConfig]: `types/runconfig.go`, `validateResultSinkConfig`: `resultSink type ... is reserved but not yet implemented in this release`. `resultSink.topic` and `resultSink.attributes` are rejected for every type but `gcp-pubsub`.
[^harnessapi]: `harness/harnessapi/harnessapi.go`, `BuildLoopWithTransport`: returns a `Loop` that exposes `Run` and `Close`. The `Handoff` field that `workspaceHandoff` fills is on the unexported inner loop, and the result sink is emitted from `harness/cmd/stirrup/cmd/root.go`.
[^WebFetchTool]: `harness/internal/tool/builtins/webfetch.go`, `WebFetchTool`: constructed with no executor argument.
[^parsePolicySetFile]: `harness/internal/permission/policyengine.go`, `parsePolicySetFile`: reads the policy file with `os.ReadFile`.
[^defaultSemgrepRun]: `harness/internal/security/codescanner/semgrep.go`, `defaultSemgrepRun`: starts the `semgrep` binary with `exec.CommandContext` in the harness process.
[^commandOutputNew]: `harness/internal/commandoutput/store.go`, `New`: creates the spool directory with `os.MkdirTemp`.
