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

The AWS Bedrock smoke workflow lives at
[`.github/workflows/smoke-bedrock.yml`](../.github/workflows/smoke-bedrock.yml).
It targets `us.anthropic.claude-haiku-4-5-20251001-v1:0` (the
cross-region inference profile for Haiku 4.5) on Bedrock via STS
`AssumeRoleWithWebIdentity`. The harness exchanges the GitHub Actions
OIDC JWT for short-lived AWS credentials at
`sts.amazonaws.com`, then signs Bedrock `ConverseStream` requests with
those credentials — no `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` /
IAM user / static AWS credential of any kind appears in the workflow's
scope.

The federation primitive is documented under
[`docs/credential-federation.md`](credential-federation.md) as
*Cross-cloud → Bedrock via STS web-identity*. As with Azure, the
harness does not expose first-class CLI flags for the Bedrock token
source; the smoke workflow drives the credential shape entirely through
[`examples/runconfig/bedrock-wif-smoke.json`](../examples/runconfig/bedrock-wif-smoke.json),
which pins the role ARN, region, model ID, audience, and the
`github-actions-oidc` token source.

### Design choice: stirrup's `web-identity` vs. `aws-actions/configure-aws-credentials`

Two ways to plumb GHA OIDC → AWS for a smoke test exist:

- **Stirrup's `web-identity` source.** The harness performs the STS
  exchange itself via the configured `tokenSource:
  github-actions-oidc` + `roleArn`. The workflow contains no
  AWS-specific tooling and the federation code path under test is
  identical to what an operator in production deploys.
- **`aws-actions/configure-aws-credentials@v4`.** The action performs
  the STS exchange and exports
  `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`AWS_SESSION_TOKEN` to
  the runner env. The harness then uses `aws-default` (the SDK
  default chain) — no stirrup-side federation code is exercised.

The shipped workflow uses the first option, deliberately. A smoke test
exists to prove stirrup's federation code path works against live AWS;
the second option turns the run into a Bedrock-adapter-only smoke and
silently bypasses the federation layer that production deployments
depend on. The action remains a useful debugging fallback when a smoke
run fails — swapping it in disambiguates *stirrup federation broken*
from *AWS-side IAM broken* — but the committed workflow exercises
stirrup end-to-end.

### Provisioned state (stirrup sandbox account)

The setup below is **already complete on the stirrup sandbox AWS
account `786874932855`**. An operator dispatching this smoke run from
`main` against the existing fixture needs no `aws` provisioning. The
walkthrough exists as a reusable playbook for other accounts adopting
the same shape.

| Field | Value |
|---|---|
| AWS account ID | `786874932855` |
| Region (source) | `us-west-2` |
| Model (cross-region inference profile) | `us.anthropic.claude-haiku-4-5-20251001-v1:0` |
| IAM OIDC provider | `arn:aws:iam::786874932855:oidc-provider/token.actions.githubusercontent.com` |
| Role ARN | `arn:aws:iam::786874932855:role/stirrup-smoke-bedrock` |
| Trust `sub` claim | `repo:rxbynerd/stirrup:ref:refs/heads/main` |
| Trust `aud` claim | `sts.amazonaws.com` |
| Role inline policy | `BedrockInvokeHaiku45` — Invoke/ConverseStream on the `us.` inference-profile ARN plus the foundation-model ARNs in `us-west-2`, `us-east-1`, `us-east-2` |
| Bedrock model access (Haiku 4.5, `us-west-2`) | enabled |

The role's trust policy is pinned to `refs/heads/main`. Dispatching the
workflow from a feature branch, a PR, or a fork will fail at the STS
exchange — this is intentional, and the failure mode is loud (the
harness surfaces the STS error verbatim in the trace).

### Reusable setup walkthrough

For any other AWS account adopting the same shape:

#### 1. Confirm Bedrock model access

Anthropic models on Bedrock require explicit access enablement per
region (AWS Console → Bedrock → Model access). Smoke runs against an
unenabled region return `AccessDeniedException` from
`ConverseStream`. Verify with:

```sh
aws bedrock get-foundation-model-availability \
  --region us-west-2 \
  --model-id anthropic.claude-haiku-4-5-20251001-v1:0
```

The response should show `authorizationStatus: AUTHORIZED` and
`entitlementAvailability: AVAILABLE`. If not, enable the model in the
target region's Bedrock console before continuing.

#### 2. Create an IAM OIDC provider for GitHub Actions

```sh
aws iam create-open-id-connect-provider \
  --url https://token.actions.githubusercontent.com \
  --client-id-list sts.amazonaws.com
```

##### Gotcha: thumbprints are no longer required

Many older Terraform/CDK modules ship hardcoded GitHub thumbprints on
their `aws_iam_openid_connect_provider` resources. These are
**harmless leftovers from older AWS guidance and do not need
refreshing**. AWS now verifies the JWKS endpoint's TLS certificate
against its trusted root CA library, so the
`--thumbprint-list` flag is optional and the value (when supplied) is
not consulted at token-verification time. A dropped thumbprint is not
a security regression.

#### 3. Create the IAM role with a federated trust policy

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {
      "Federated": "arn:aws:iam::786874932855:oidc-provider/token.actions.githubusercontent.com"
    },
    "Action": "sts:AssumeRoleWithWebIdentity",
    "Condition": {
      "StringEquals": {
        "token.actions.githubusercontent.com:aud": "sts.amazonaws.com",
        "token.actions.githubusercontent.com:sub": "repo:rxbynerd/stirrup:ref:refs/heads/main"
      }
    }
  }]
}
```

```sh
aws iam create-role \
  --role-name stirrup-smoke-bedrock \
  --assume-role-policy-document file://trust-policy.json
```

##### Gotcha: `workflow_dispatch` uses the branch-ref subject form

A `workflow_dispatch`-triggered run does **not** mint a token with a
distinct `workflow_dispatch` subject form. The `sub` claim takes the
**branch-ref form** of the dispatching branch, e.g.
`repo:rxbynerd/stirrup:ref:refs/heads/main`. A trust policy whose
`sub` condition reads `repo:rxbynerd/stirrup:workflow_dispatch` will
fail token exchange every time with
`AccessDenied: Not authorized to perform sts:AssumeRoleWithWebIdentity`.

If smoke runs from pull requests are needed in future, add a second
trust statement whose `sub` reads `repo:rxbynerd/stirrup:pull_request`
(underscore — older AWS docs occasionally show `pull-request` with a
hyphen which is wrong). The shipped trust policy is `main`-only;
dispatching from a feature branch requires merging to `main` first.

#### 4. Attach a Bedrock invoke policy

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "bedrock:InvokeModel",
      "bedrock:InvokeModelWithResponseStream",
      "bedrock:Converse",
      "bedrock:ConverseStream"
    ],
    "Resource": [
      "arn:aws:bedrock:us-west-2:786874932855:inference-profile/us.anthropic.claude-haiku-4-5-20251001-v1:0",
      "arn:aws:bedrock:us-west-2::foundation-model/anthropic.claude-haiku-4-5-20251001-v1:0",
      "arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-haiku-4-5-20251001-v1:0",
      "arn:aws:bedrock:us-east-2::foundation-model/anthropic.claude-haiku-4-5-20251001-v1:0"
    ]
  }]
}
```

```sh
aws iam put-role-policy \
  --role-name stirrup-smoke-bedrock \
  --policy-name BedrockInvokeHaiku45 \
  --policy-document file://bedrock-policy.json
```

##### Gotcha: cross-region inference requires *both* ARN shapes

The `us.` prefix on a Bedrock model ID denotes a **cross-region
inference profile**, not a model. The source region (the `--region`
on the API call, and the region in the inference-profile ARN's
account-qualified portion) is `us-west-2`, but the request may
execute in any of `us-west-2`, `us-east-1`, or `us-east-2` depending
on Bedrock's load-balancing decision at request time.

The IAM policy must grant the action on **both**:

1. The inference-profile ARN itself
   (`arn:aws:bedrock:us-west-2:786874932855:inference-profile/us.anthropic.claude-haiku-4-5-20251001-v1:0`).
2. The destination foundation-model ARNs in every region the profile
   may route to
   (`arn:aws:bedrock:{us-west-2|us-east-1|us-east-2}::foundation-model/anthropic.claude-haiku-4-5-20251001-v1:0`).

Omitting the destination foundation-model ARNs is the most common
cause of `us.`-prefixed-model `AccessDenied` responses — IAM
evaluates permissions against the destination region the request is
routed to, not the source region the caller specified. The error
surfaces as a 403 from Bedrock with a region in the message that
differs from the configured `provider.region` — a strong signal that
the destination-region grant is what's missing.

#### 5. Allow IAM propagation

IAM role and policy changes propagate across the AWS control plane
in well under 30 seconds in practice, but the documented upper bound
is several minutes. Allow ~60 seconds between
`create-role`/`put-role-policy` and the first dispatch.

#### 6. Wire the role ARN into the fixture

Edit `examples/runconfig/bedrock-wif-smoke.json` and substitute the
new account ID into `provider.credential.roleArn` (and into
`provider.region` if a different source region is chosen). The values
committed today are bound to the stirrup sandbox account; an external
adopter forks the fixture (or maintains an account-specific overlay).

### Reading the fixture

The shipped smoke fixture pins these load-bearing fields:

| Field | Value | Rationale |
|---|---|---|
| `provider.type` | `bedrock` | The harness's first-class Bedrock adapter (`ConverseStream`). |
| `provider.region` | `us-west-2` | Historically the most reliable Bedrock region for new Anthropic launches; the source region the inference profile is queried from. `us-east-1` and `us-east-2` are also valid destinations for the `us.` profile but the *source* region must match the inference-profile ARN. |
| `provider.credential.type` | `web-identity` | Triggers the `sts:AssumeRoleWithWebIdentity` exchange. |
| `provider.credential.roleArn` | `arn:aws:iam::786874932855:role/stirrup-smoke-bedrock` | The role whose trust policy is pinned to `refs/heads/main` and whose inline policy grants Bedrock invoke on the Haiku 4.5 inference profile. |
| `provider.credential.sessionName` | `stirrup-smoke` | Human-readable session label surfaced in CloudTrail and the role-session ARN. Bounded length / printable-ASCII per stirrup's `validateSessionName`. |
| `provider.credential.tokenSource.type` | `github-actions-oidc` | Reads the GHA-injected `ACTIONS_ID_TOKEN_REQUEST_URL` + token. |
| `provider.credential.tokenSource.audience` | `sts.amazonaws.com` | What AWS expects on the OIDC token's `aud` claim, matching the IAM OIDC provider's client-id list and the trust policy's audience condition. |
| `modelRouter.model` | `us.anthropic.claude-haiku-4-5-20251001-v1:0` | The `us.` cross-region inference profile for Haiku 4.5 — the cheapest current Anthropic model on Bedrock. The `us.` prefix decouples the source region from the destination region the request actually executes in. |

The harness will load the fixture so long as the JSON validates
structurally and `ValidateRunConfig` passes; live Bedrock state
(model access, IAM trust, inference-profile availability) is only
exercised at the first provider call. A missing model-access
enablement surfaces as the workflow's "Run smoke test" step failing
with `AccessDeniedException`, not at config load.

## Vertex AI Gemini

The Vertex AI Gemini smoke workflow lives at
[`.github/workflows/smoke-vertex-gemini.yml`](../.github/workflows/smoke-vertex-gemini.yml).
It targets `gemini-2.5-flash-lite` on Vertex AI via GCP Workload
Identity Federation. The harness exchanges the GitHub Actions OIDC
JWT for a federated Google access token at `sts.googleapis.com`,
impersonates the target service account through
`iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/{SA}:generateAccessToken`,
then presents the resulting bearer token on Vertex AI's
`:streamGenerateContent` calls — no service-account JSON,
`GOOGLE_APPLICATION_CREDENTIALS`, or static GCP credential of any
kind appears in the workflow's scope.

The federation primitive is documented under
[`docs/credential-federation.md`](credential-federation.md) as
*Cross-cloud → Vertex AI Gemini via Workload Identity Federation*.
As with Azure and Bedrock, the harness does not expose first-class
CLI flags for the Vertex token source; the smoke workflow drives the
credential shape entirely through
[`examples/runconfig/vertex-gemini-wif-smoke.json`](../examples/runconfig/vertex-gemini-wif-smoke.json),
which pins the project, project number, audience, service account,
model, and the `github-actions-oidc` token source.

### Design choice: stirrup's WIF source vs. `google-github-actions/auth`

Two ways to plumb GHA OIDC → GCP for a smoke test exist:

- **Stirrup's `gcp-workload-identity-federation` source.** The
  harness performs the STS exchange itself via the configured
  `tokenSource: github-actions-oidc` + `audience` +
  `serviceAccount`. The workflow contains no GCP-specific tooling
  and the federation code path under test is identical to what an
  operator in production deploys.
- **`google-github-actions/auth@v3`.** The action performs the STS
  exchange and exports `GOOGLE_APPLICATION_CREDENTIALS` (a
  workload-identity credential file). The harness then uses
  `gcp-default` (Application Default Credentials) — no
  stirrup-side federation code is exercised.

The shipped workflow uses the first option, deliberately. A smoke
test exists to prove stirrup's federation code path works against
live GCP; the second option turns the run into a Vertex-adapter-only
smoke and silently bypasses the federation layer that production
deployments depend on. The action remains a useful debugging
fallback when a smoke run fails — swapping it in disambiguates
*stirrup federation broken* from *GCP-side IAM broken* — but the
committed workflow exercises stirrup end-to-end. The same trade-off
applies to `smoke-bedrock.yml` vs. `aws-actions/configure-aws-credentials`.

### Provisioned state (rubynerd-net project)

The setup below is **already complete on the rubynerd-net GCP
project**, reusing the shared `stirrup-gha` Workload Identity Pool
that `stirrup-publisher` already authenticates against for GAR
pushes. An operator dispatching this smoke run from `main` against
the existing fixture needs no `gcloud` provisioning. The walkthrough
exists as a reusable playbook for other projects adopting the same
shape.

| Field | Value |
|---|---|
| GCP project ID | `rubynerd-net` |
| GCP project number | `163317929648` |
| Workload Identity Pool | `stirrup-gha` (display name *Stirrup GitHub Actions*) |
| WIF provider | `stirrup-gha-provider` |
| Provider issuer URI | `https://token.actions.githubusercontent.com` |
| Provider attribute mapping | `google.subject=assertion.sub, attribute.repository=assertion.repository, attribute.ref=assertion.ref` |
| Provider attribute condition | `assertion.repository == 'rxbynerd/stirrup' && (assertion.ref == 'refs/heads/main' \|\| assertion.ref.startsWith('refs/tags/v'))` |
| Federated principalSet | `principalSet://iam.googleapis.com/projects/163317929648/locations/global/workloadIdentityPools/stirrup-gha/attribute.repository/rxbynerd/stirrup` |
| WIF audience string | `//iam.googleapis.com/projects/163317929648/locations/global/workloadIdentityPools/stirrup-gha/providers/stirrup-gha-provider` |
| Service account | `stirrup-testing@rubynerd-net.iam.gserviceaccount.com` |
| SA project role | `roles/aiplatform.user` on `rubynerd-net` |
| SA federation binding | `roles/iam.workloadIdentityUser` on the federated principalSet above |
| APIs enabled | `iam.googleapis.com`, `aiplatform.googleapis.com`, `sts.googleapis.com`, `iamcredentials.googleapis.com` |
| Model exercised | `gemini-2.5-flash-lite` |

### Deviations from the reusable walkthrough

The reusable walkthrough below describes a greenfield project
(`stirrup-smoke`) with a dedicated pool (`stirrup-smoke`) and a
provider hardened with numeric-claim-ID attribute matching for
typosquatting defence. The `rubynerd-net` provisioning diverges in
three places to reuse existing infrastructure:

- **Pool name** is `stirrup-gha`, **not** `stirrup-smoke`. The pool
  is shared with `stirrup-publisher` (GAR pushes from `ci.yml`,
  `release.yml`, `smoke-gar-publish.yml`); spinning up a second pool
  just for the Vertex smoke would have doubled the operator surface
  with no security gain.
- **Provider name** is `stirrup-gha-provider`, **not**
  `github-actions`. Same rationale as the pool name.
- **Service-account name** is `stirrup-testing`, **not**
  `stirrup-smoke`. The SA is dedicated to Vertex AI smoke testing
  and does not share roles with `stirrup-publisher`; the principle
  of least privilege is preserved at the SA level even though the
  pool is shared.
- The provider uses **name-based** attribute matching
  (`assertion.repository == 'rxbynerd/stirrup'`), **not** the
  numeric-claim-ID typosquatting defence described in step 4 below.
  This matches `stirrup-publisher`'s existing behaviour on the same
  pool. Hardening to numeric IDs is a cross-cutting change that
  touches every workflow on the pool (`ci.yml`, `release.yml`,
  `smoke-gar-publish.yml`, plus this smoke test) and is tracked
  separately — out of scope for this smoke test. Greenfield
  deployments should follow step 4's numeric-ID recipe.
- The attribute condition restricts dispatch to **`refs/heads/main`
  and `refs/tags/v*` only**. A `workflow_dispatch` triggered against
  a feature branch will fail at the STS token exchange with
  `unauthorized_client: The given credential is rejected by the
  attribute condition.`. This is the same constraint documented at
  the top of `smoke-gar-publish.yml` for the same pool; fix-forward
  to `main` (or cut a `v*` tag) to validate WIF-path changes.

### Reusable setup walkthrough

For any other GCP project adopting the same shape (greenfield
deployment with the recommended numeric-ID attribute matching):

#### 1. Provision a GCP project and enable APIs

```sh
PROJECT_ID=stirrup-smoke
PROJECT_NUMBER=$(gcloud projects describe "$PROJECT_ID" --format='value(projectNumber)')

gcloud services enable \
  iam.googleapis.com \
  aiplatform.googleapis.com \
  sts.googleapis.com \
  iamcredentials.googleapis.com \
  --project="$PROJECT_ID"
```

Both `PROJECT_ID` and `PROJECT_NUMBER` are non-secret per Google's
WIF docs and safe to commit into the fixture file.

#### 2. Create a Workload Identity Pool

```sh
gcloud iam workload-identity-pools create stirrup-smoke \
  --location=global \
  --display-name="Stirrup smoke test" \
  --project="$PROJECT_ID"
```

Pool IDs must be 4–32 lowercase alphanumeric characters with
internal hyphens. The pool's full resource name is
`projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/stirrup-smoke`.

#### 3. Create a GHA OIDC provider in the pool

```sh
REPO_ID=$(gh api repos/rxbynerd/stirrup --jq .id)
OWNER_ID=$(gh api repos/rxbynerd/stirrup --jq .owner.id)

gcloud iam workload-identity-pools providers create-oidc github-actions \
  --location=global \
  --workload-identity-pool=stirrup-smoke \
  --issuer-uri=https://token.actions.githubusercontent.com \
  --attribute-mapping="google.subject=assertion.sub,attribute.repository=assertion.repository,attribute.repository_owner=assertion.repository_owner,attribute.repository_id=assertion.repository_id,attribute.repository_owner_id=assertion.repository_owner_id,attribute.ref=assertion.ref" \
  --attribute-condition="assertion.repository_owner_id == '${OWNER_ID}' && assertion.repository_id == '${REPO_ID}'" \
  --project="$PROJECT_ID"
```

##### Gotcha: numeric-claim-ID attribute matching as typosquatting defence

The `repository_owner_id` and `repository_id` numeric-claim
condition above is Google's recommended defence against an attacker
registering `<owner>-2/<repo>` (or any name-collision squat) after
the original repository is renamed, deleted, or transferred.
Name-based attribute conditions (`assertion.repository ==
'rxbynerd/stirrup'`) are vulnerable to this squat — GitHub
recycles repository names but does **not** recycle numeric IDs, so
pinning to the immutable numeric pair fully closes the window.
This is the recommended shape for any greenfield WIF provider that
trusts GitHub Actions OIDC tokens.

##### Gotcha: `workflow_dispatch` uses the branch-ref subject form

Like Azure and AWS, a `workflow_dispatch`-triggered run does **not**
mint a token with a distinct `workflow_dispatch` subject form.
GHA's OIDC `sub` claim takes the **branch-ref form** of the
dispatching branch, e.g.
`repo:rxbynerd/stirrup:ref:refs/heads/main`. An attribute condition
written to expect `repo:rxbynerd/stirrup:workflow_dispatch` will
fail STS every time with `unauthorized_client: The given credential
is rejected by the attribute condition.`. The numeric-claim
condition above sidesteps the issue entirely because it does not
key off `sub`.

#### 4. Create the target service account

```sh
gcloud iam service-accounts create stirrup-smoke \
  --display-name="Stirrup smoke test SA" \
  --project="$PROJECT_ID"
```

#### 5. Grant the federated principal `workloadIdentityUser` on the SA

```sh
gcloud iam service-accounts add-iam-policy-binding \
  "stirrup-smoke@${PROJECT_ID}.iam.gserviceaccount.com" \
  --role=roles/iam.workloadIdentityUser \
  --member="principalSet://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/stirrup-smoke/attribute.repository/rxbynerd/stirrup" \
  --project="$PROJECT_ID"
```

This binding is what authorises the federated GHA principal to
impersonate the target SA. Without it, the
`iamcredentials.generateAccessToken` call after the STS exchange
fails with `IAM_PERMISSION_DENIED`.

#### 6. Grant the SA Vertex AI usage

```sh
gcloud projects add-iam-policy-binding "$PROJECT_ID" \
  --member="serviceAccount:stirrup-smoke@${PROJECT_ID}.iam.gserviceaccount.com" \
  --role=roles/aiplatform.user
```

`roles/aiplatform.user` is the minimal role for
`:streamGenerateContent`. Direct WIF (no impersonation) requires
this same role granted directly to the federated principalSet,
which works for some Vertex APIs but is unreliable for
`streamGenerateContent`. SA impersonation has a 1-hour token
lifetime (vs. 10 minutes for direct WIF) and full Vertex API
support — recommended even when the federated principal could
hold the grant directly.

#### 7. Wire the project, audience, and SA into the fixture

Edit `examples/runconfig/vertex-gemini-wif-smoke.json` and
substitute the new `gcpProject`, the audience string (both
`provider.credential.audience` **and**
`provider.credential.tokenSource.audience`), and the SA email. The
values committed today are bound to the rubynerd-net project; an
external adopter forks the fixture (or maintains a project-specific
overlay).

##### Gotcha: the audience double slash is required

The WIF audience starts with **two slashes**
(`//iam.googleapis.com/...`), not one. A single-slash or missing
slash fails the STS exchange with an opaque
`400 INVALID_ARGUMENT` from `sts.googleapis.com/v1/token`. The
loader validates the shape against `types.GCPWIFAudiencePatternString`
at config-load time so this catches a typo before the harness ever
talks to STS, but if the fixture is hand-edited it is the first
thing to check on a 400.

##### Gotcha: the two audience strings must match

The fixture sets `audience` in **two** places:
`provider.credential.audience` (which the harness sends in the STS
exchange's `audience` parameter) and
`provider.credential.tokenSource.audience` (which the harness
passes to GHA when requesting the OIDC JWT). The two **must** be
identical. GHA embeds the `audience` parameter into the OIDC
token's `aud` claim, and STS rejects exchanges where the token's
`aud` does not match the WIF provider's expected audience.

### Reading the fixture

The shipped smoke fixture pins these load-bearing fields:

| Field | Value | Rationale |
|---|---|---|
| `provider.type` | `gemini` | The harness's first-class Vertex AI Gemini adapter (`:streamGenerateContent`). |
| `provider.gcpProject` | `rubynerd-net` | Project hosting the SA and the Vertex AI grant. Non-secret per Google's docs. |
| `provider.gcpLocation` | `global` | Multi-region endpoint (`aiplatform.googleapis.com`). Swap to e.g. `us-central1` if the chosen model is region-restricted; document the change. |
| `provider.credential.type` | `gcp-workload-identity-federation` | Triggers the STS exchange + optional SA impersonation. |
| `provider.credential.audience` | `//iam.googleapis.com/projects/163317929648/locations/global/workloadIdentityPools/stirrup-gha/providers/stirrup-gha-provider` | The shared `stirrup-gha` pool's `stirrup-gha-provider` audience. Double slash required. |
| `provider.credential.serviceAccount` | `stirrup-testing@rubynerd-net.iam.gserviceaccount.com` | The dedicated Vertex smoke SA. Recommended (not optional) — impersonation has a 1-hour token lifetime and full Vertex API support. |
| `provider.credential.tokenSource.type` | `github-actions-oidc` | Reads the GHA-injected `ACTIONS_ID_TOKEN_REQUEST_URL` + token. |
| `provider.credential.tokenSource.audience` | (same as `provider.credential.audience`) | GHA embeds this into the OIDC token's `aud` claim; STS rejects mismatches. |
| `modelRouter.model` | `gemini-2.5-flash-lite` | Current cheap-tier Gemini Flash model. Google rotates Gemini Flash model IDs more aggressively than Anthropic does — re-pin against the [Vertex Generative AI release notes](https://cloud.google.com/vertex-ai/generative-ai/docs/release-notes) when refreshing. |

The harness will load the fixture so long as the JSON validates
structurally and `ValidateRunConfig` passes; live GCP state (API
enablement, pool / provider / SA existence, IAM bindings) is only
exercised at the first provider call. A missing role binding
surfaces as the workflow's *Run smoke test* step failing at the
STS or `iamcredentials` exchange, not at config load.

## See also

- [`docs/credential-federation.md`](credential-federation.md) — the
  cross-cloud federation primitive smoke tests exercise.
- [`docs/anthropic-wif.md`](anthropic-wif.md) — Anthropic Console
  setup and CLI flag reference.
- [`docs/azure-workload-identity.md`](azure-workload-identity.md) —
  Azure App Registration + federated identity credential reference.
