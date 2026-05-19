# Cross-provider smoke tests

Stirrup ships a small suite of manually-dispatched GitHub Actions
workflows that exercise each provider's credential-federation path
end-to-end against the live upstream API. The intent is narrow: every
smoke run proves that a provider's Workload Identity Federation
(WIF) wiring still works against the real authority and the real
provider, with **no static API key in scope**. The runs do not
replace `go test ./...` and they do not assert correctness of the
agent's output beyond a single workspace artifact.

This document is the operator playbook. The Go-side primitives the
smoke runs exercise are documented at
[`docs/credential-federation.md`](credential-federation.md),
[`docs/anthropic-wif.md`](anthropic-wif.md), and
[`docs/azure-workload-identity.md`](azure-workload-identity.md).

## Conventions

Each provider gets its own workflow file, job key, and artifact name.
The shape is uniform so dispatch logs read clearly and a failure in
one provider's run cannot be confused with another's:

| Surface | Pattern | Example |
|---|---|---|
| Workflow file | `smoke-<provider>.yml` | `smoke-anthropic.yml` |
| Job key | `smoke-<provider>-wif` | `smoke-anthropic-wif` |
| Artifact name | `smoke-trace-<provider>-wif` | `smoke-trace-anthropic-wif` |

Each workflow:

- Is `workflow_dispatch`-only — no schedule, no push trigger. Cron
  can be added once the manual cadence is proven flake-free over a
  couple of weeks; the current discipline keeps live-API spend
  bounded to deliberate operator clicks.
- Declares `permissions: { contents: read, id-token: write }`. The
  `id-token: write` permission is what injects the GHA OIDC token
  envelope (`ACTIONS_ID_TOKEN_REQUEST_URL` and
  `ACTIONS_ID_TOKEN_REQUEST_TOKEN`) into the runner. Workflows
  without it cannot mint an OIDC JWT and the credential exchange
  fails with `ACTIONS_ID_TOKEN_REQUEST_URL is not set`.
- Builds the harness binary fresh from `go.work`. There is no
  release-binary path — the smoke run is a moving-tip integration
  test against the branch dispatching it.
- Runs the agent for ≤ 5 turns with a fixed prompt: *"Write the
  string 'smoke test passed' to a file called hello.txt"*.
- Uploads `trace.jsonl` as an artifact with `if: always()` so failed
  runs preserve the trace for post-mortem. Retention is 7 days.

### The two-verify pattern

Every smoke workflow verifies the run with **two independent
assertions**:

1. **Trace outcome**:

   ```sh
   set -euo pipefail
   [ -s trace.jsonl ] || { echo "trace.jsonl is missing or empty" >&2; exit 1; }
   jq -e '.outcome == "success"' trace.jsonl
   ```

   The `[ -s trace.jsonl ]` guard catches the case where the harness
   exited before writing the trace; `jq -e` exits non-zero when the
   expression is falsy. Direct read of the single-document JSONL — do
   **not** rewrite this as `tail -1 | jq`. Under GNU coreutils
   `tail` silently emits an empty stream for a missing file, which
   `jq` then passes; the failure becomes invisible. (This was the
   regression #132 explicitly fixed in review.)

2. **Workspace artifact**:

   ```sh
   test -f workspace/hello.txt
   grep -qF 'smoke test passed' workspace/hello.txt
   ```

   `outcome == success` only proves the agentic loop exited cleanly,
   not that the model produced useful output. Asserting the file
   contents confirms the agent actually exercised the edit tool path
   end-to-end.

The two assertions exist independently because they fail under
different conditions: a model that hallucinates a successful
`edit_file` tool call without persisting the file leaves the trace
outcome green but the workspace empty; a model that writes the file
but the harness loop crashes after leaves the workspace correct but
the trace outcome `error`. Either failure is a regression worth
catching.

### What smoke runs prove (and don't)

A green smoke run proves:

- The federated identity credential is wired correctly on both sides.
- The provider adapter handles the resulting bearer / API token.
- The agentic loop completes a real turn against the upstream API.
- The edit tool path persists files to disk.

A green smoke run does **not** prove:

- Output quality. The prompt is trivial.
- Long-running behaviour. The cap is 5 turns.
- Failure-mode handling. Adversarial inputs are not exercised here.
- Cost control. Pricing-side limits live on the provider resource,
  not in the workflow.

## Anthropic

The Anthropic smoke workflow lives at
[`.github/workflows/smoke-anthropic.yml`](../.github/workflows/smoke-anthropic.yml).
It targets `claude-haiku-4-5-20251001` over the public Anthropic
Messages API with WIF, no `ANTHROPIC_API_KEY` in scope.

The federation primitive is documented in full at
[`docs/anthropic-wif.md`](anthropic-wif.md). Anthropic's Console
walks the operator through the three resources the smoke run needs:

1. A **service account** (`svac_...`) in the Anthropic
   organization.
2. A **federation issuer** (`fdis_...`) registered against the
   GitHub Actions OIDC endpoint
   (`https://token.actions.githubusercontent.com`) with JWKS source
   `discovery`.
3. A **federation rule** (`fdrl_...`) bridging the issuer to the
   service account with `subject_prefix:
   repo:rxbynerd/stirrup:ref:refs/heads/<branch>` and the
   `workspace:developer` scope.

The four resulting identifiers — federation rule ID, organization
ID, service account ID, and workspace ID — are non-secret per
Anthropic's WIF docs and live committed in the smoke workflow as
bare CLI flags. The actual authentication factor is the per-run
OIDC JWT minted by GitHub.

The workflow uses the harness's first-class
`--anthropic-from-github-actions` opt-in alongside the four ID
flags (`--anthropic-federation-rule-id`,
`--anthropic-organization-id`, `--anthropic-service-account-id`,
plus `--model claude-haiku-4-5-20251001`). No fixture file is
needed; the CLI surface covers the full credential shape.

If the smoke run starts surfacing `token exchange returned 400`
with a `request_id=req_...`, that `request_id` is the lookup key for
the Anthropic Console's
[authentication-history page](https://platform.claude.com/settings/workload-identity-federation?tab=history).
Common causes are clock skew on the runner (Anthropic applies 30s
skew on `exp`/`nbf`/`iat`), an archived federation rule, or a
`subject_prefix` that no longer matches the dispatching branch.

## Azure OpenAI

The Azure OpenAI smoke workflow lives at
[`.github/workflows/smoke-azure-openai.yml`](../.github/workflows/smoke-azure-openai.yml).
It targets the `gpt-5.4-nano` deployment on the `stirrup-eval-resource`
Azure AI Foundry resource (`AIServices` kind, `swedencentral`) via
Entra ID Workload Identity Federation. The harness exchanges the
GitHub Actions OIDC JWT for a Microsoft Entra access token at
`login.microsoftonline.com/{tenant}/oauth2/v2.0/token` and presents
the resulting bearer on `Authorization: Bearer …` to the Azure
OpenAI `/openai/v1/responses` endpoint.

The federation primitive is documented at
[`docs/azure-workload-identity.md`](azure-workload-identity.md).
Unlike Anthropic, the harness does not expose first-class CLI flags
for the Azure token source; the smoke workflow drives the
credential shape entirely through
[`examples/runconfig/azure-openai-wif-smoke.json`](../examples/runconfig/azure-openai-wif-smoke.json),
which pins the tenant ID, client ID, AI Foundry host, `api-version`,
and the `github-actions-oidc` token source.

### Provisioned state (stirrup test tenant)

The setup below is **already complete on the Ghostworks Ltd tenant**.
An operator dispatching this smoke run from `main` against the
existing fixture needs no `az` provisioning. The walkthrough exists
as a reusable playbook for other organisations adopting the same
shape.

| Field | Value |
|---|---|
| Tenant ID | `070edf67-6378-4bb0-9f3a-dce13cf67a36` |
| App / Client ID | `3d4df370-c289-49f2-aa79-c9d83237ebd8` (`stirrup-azure-openai`) |
| Service Principal Object ID | `344e02fb-8f3b-4c57-9162-a45fa81059fb` |
| Federated credential | `stirrup-gha`, subject `repo:rxbynerd/stirrup:ref:refs/heads/main`, audience `api://AzureADTokenExchange` |
| RBAC | `Cognitive Services OpenAI User` on `stirrup-eval-resource` (granted) |
| Resource | `stirrup-eval-resource` (`AIServices` kind, `swedencentral`, RG `rg-stirrup-eval`) |
| Endpoint | `https://stirrup-eval-resource.cognitiveservices.azure.com/` |
| Deployment exercised | `gpt-5.4-nano` (GlobalStandard, capacity 100, model version `2026-03-17`) |
| Other deployments on the same resource | `gpt-5.4-mini`, `gpt-5.4` (kept for future eval / research work, not exercised by this smoke test) |

### Reusable setup walkthrough

For any other Entra tenant adopting the same shape:

#### 1. Provision a low-cost Azure OpenAI resource

Pick a region that hosts the chosen deployment (e.g. `swedencentral`,
`eastus2`). Note the **kind** — an `AIServices` resource (Azure AI
Foundry) uses the `cognitiveservices.azure.com` host; a classic
`OpenAI` resource uses `openai.azure.com`. The fixture's
`provider.baseUrl` must match. The smoke fixture pins the AI
Foundry host because the stirrup test resource is `AIServices` kind.

#### 2. Create a cheap deployment

Deploy a small model — `gpt-5.4-nano` is the choice on the stirrup
test resource. The **deployment name** (not the underlying model
name) is what goes into `modelRouter.model` in the fixture. Azure
treats these as distinct strings.

#### 3. Register an App in the Entra tenant

```sh
az ad app create --display-name stirrup-smoke
APP_ID=$(az ad app list --display-name stirrup-smoke --query "[0].appId" -o tsv)
TENANT_ID=$(az account show --query tenantId -o tsv)
az ad sp create --id "$APP_ID"
SP_ID=$(az ad sp show --id "$APP_ID" --query id -o tsv)
```

Both `APP_ID` (the Application / client ID) and `TENANT_ID` are
non-secret per Microsoft's WIF docs and safe to commit into the
fixture file.

#### 4. Add a federated identity credential for the GHA workflow

```sh
cat > federated-cred-gha.json <<EOF
{
  "name": "stirrup-smoke-gha",
  "issuer": "https://token.actions.githubusercontent.com",
  "subject": "repo:<owner>/<repo>:ref:refs/heads/main",
  "audiences": ["api://AzureADTokenExchange"]
}
EOF
az ad app federated-credential create --id "$APP_ID" --parameters federated-cred-gha.json
```

The `subject` field is matched **exactly** against the `sub` claim
GHA puts on the OIDC token — Azure does not support wildcards.

##### Gotcha: `workflow_dispatch` uses the branch-ref subject form

A `workflow_dispatch`-triggered run does **not** mint a token with a
distinct `workflow_dispatch` subject form. The `sub` claim takes
the **branch-ref form** of the dispatching branch, e.g.
`repo:rxbynerd/stirrup:ref:refs/heads/main`. A federated identity
credential whose `subject` reads `repo:rxbynerd/stirrup:workflow_dispatch`
will silently fail token exchange every time. The smoke workflow's
credential is bound to the `main` branch-ref subject; dispatching
from a feature branch requires a second federated identity
credential bound to that branch's ref, or — more commonly — merging
to `main` first.

##### Gotcha: `pull_request` uses underscore, not hyphen

If smoke runs from pull requests are needed in future, the
federated identity credential's `subject` takes the form
`repo:<owner>/<repo>:pull_request` (underscore). Older Microsoft
documentation pages show `pull-request` (hyphen) which is **wrong**
— the federated credential will be silently rejected on every PR
dispatch with `AADSTS700213` (No matching federated identity
record found). The smoke workflow's `main`-branch credential
sidesteps this, but the gotcha is worth knowing before adding a PR
trigger.

#### 5. Grant Azure OpenAI access

```sh
AOAI_RESOURCE=$(az cognitiveservices account show \
  --name <resource> --resource-group <rg> --query id -o tsv)

az role assignment create \
  --assignee-object-id "$SP_ID" \
  --assignee-principal-type ServicePrincipal \
  --role "Cognitive Services OpenAI User" \
  --scope "$AOAI_RESOURCE"
```

##### Gotcha: 5-minute RBAC propagation lag

Azure RBAC role assignments take up to **5 minutes** to propagate
across the resource graph. The first smoke run dispatched
immediately after `az role assignment create` will frequently
401-fail with a `Forbidden`-class response from Azure OpenAI; the
error is not a misconfiguration but a propagation race. Wait five
minutes and re-dispatch. A retry on the run step is acceptable for
operators who want it idempotent; the smoke workflow does not retry
by default because a real auth failure should surface loudly rather
than be retried into a green status.

#### 6. Wire the IDs into the fixture

Edit `examples/runconfig/azure-openai-wif-smoke.json` and substitute
the new tenant ID, client ID, resource host, and deployment name.
The values committed today are bound to the stirrup test tenant; an
external adopter forks the fixture (or maintains a tenant-specific
overlay).

### Reading the fixture

The shipped smoke fixture pins these load-bearing fields:

| Field | Value | Rationale |
|---|---|---|
| `provider.type` | `openai-responses` | The harness's first-class Azure AI Foundry adapter speaks the `/openai/v1/responses` surface. Use `openai-compatible` only if the chosen deployment exposes only Chat Completions. |
| `provider.baseUrl` | `https://stirrup-eval-resource.cognitiveservices.azure.com/openai/v1` | AI Foundry (`AIServices` kind) host. A classic `OpenAI` kind resource would use `openai.azure.com`. |
| `provider.queryParams.api-version` | `preview` | Latest Responses API path. Use `2024-10-21` if pinning to GA Chat Completions instead. |
| `provider.credential.type` | `azure-workload-identity` | Triggers the Entra `client_credentials` + JWT-bearer exchange. |
| `provider.credential.tokenSource.type` | `github-actions-oidc` | Reads the GHA-injected `ACTIONS_ID_TOKEN_REQUEST_URL` + token. |
| `provider.credential.tokenSource.audience` | `api://AzureADTokenExchange` | Must match `audiences[0]` on the federated identity credential. |
| `modelRouter.model` | `gpt-5.4-nano` | Deployment name on the test resource. Deployment names ≠ model names on Azure. |

The harness will refuse to load the fixture if the deployment name
references one that does not exist on the resource, but the failure
surfaces only at first provider call — not at config load.
`ValidateRunConfig` cannot validate against live Azure state.

## AWS Bedrock

Documented under sister issue #161.

## Vertex AI Gemini

Documented under sister issue #162.

## See also

- [`docs/credential-federation.md`](credential-federation.md) — the
  cross-cloud federation primitive smoke tests exercise.
- [`docs/anthropic-wif.md`](anthropic-wif.md) — Anthropic Console
  setup and CLI flag reference.
- [`docs/azure-workload-identity.md`](azure-workload-identity.md) —
  Azure App Registration + federated identity credential reference.
