# Container publishing

Stirrup publishes its container image to two registries from the same
build: GitHub Container Registry (GHCR) and Google Cloud Artifact
Registry (GAR). This document is the operator walkthrough for the
GCP-side bootstrap, the GitHub-side configuration, and the verification
steps that confirm the dual-publish is working.

The architectural sibling is [`credential-federation.md`](credential-federation.md);
this doc covers a Workload Identity Federation deployment in the
specific shape stirrup's CI workflow consumes.

## Why dual-publish

- **GHCR is the canonical public mirror.** Anyone with a GitHub account
  can `docker pull ghcr.io/rxbynerd/stirrup:<tag>` without auth; that
  remains the recommended path for external users.
- **GAR exists for the `native-runtime` GKE cluster.** The forthcoming
  `K8sExecutor` work runs sub-agent jobs on the existing
  `native-runtime` cluster in the `rubynerd-net` GCP project. The
  cluster has no pull credentials for `ghcr.io`. Granting it a
  long-lived GitHub PAT just to fix pulls would walk back the
  keyless / Rule-of-Two posture documented in
  [`safety-rings.md`](safety-rings.md) and
  [`credential-federation.md`](credential-federation.md). With the
  image in GAR, the cluster pulls using its own Google identity — no
  cross-cloud PAT, no rotation toil.
- **GAR unlocks Artifact Analysis.** Once images live in GAR, Google's
  [Artifact Analysis](https://docs.cloud.google.com/artifact-analysis/docs/artifact-analysis)
  service performs continuous vulnerability scanning on every push,
  and `gcloud artifacts sbom load` lets the SPDX + CycloneDX bundles
  produced by `release.yml::sbom` become queryable Container Analysis
  occurrences instead of inert release-asset blobs. This is a Phase 2
  follow-up; the GCP-side bootstrap below pre-binds the IAM roles so
  Phase 2 does not need a second bootstrap pass.

## GCP-side bootstrap (one-time)

These resources must exist in the `rubynerd-net` GCP project before
the workflow changes can succeed. All identifiers produced here are
non-secret — they go into GitHub Actions as **repository variables**
(`vars.*`), not secrets, mirroring how `google-github-actions/auth`
documents the pattern.

### 1. Enable required APIs

```sh
gcloud services enable \
  artifactregistry.googleapis.com \
  containerscanning.googleapis.com \
  containeranalysis.googleapis.com \
  iamcredentials.googleapis.com \
  sts.googleapis.com \
  --project rubynerd-net
```

`containerscanning.googleapis.com` is what activates *automatic*
vulnerability scanning on push; once enabled, scanning is triggered
on every new image without further configuration.

### 2. Create the Artifact Registry Docker repository

Pick the region the `native-runtime` GKE cluster lives in to avoid
cross-region pull latency and egress. The example below uses
`europe-west4`; revise if the cluster moves.

```sh
gcloud artifacts repositories create stirrup \
  --repository-format=docker \
  --location=europe-west4 \
  --description="Stirrup harness container images" \
  --project=rubynerd-net
```

Final image path:
`europe-west4-docker.pkg.dev/rubynerd-net/stirrup/stirrup`.

### 3. Create the Workload Identity Pool and Provider

Use the [`google-github-actions/auth`](https://github.com/google-github-actions/auth)
**Direct Workload Identity Federation** path: no intermediate service
account, IAM roles bind to the external identity directly. This avoids
the long-lived service-account-key foot-gun entirely.

The provider's `attribute-condition` pins the GitHub repository so a
token minted by any other repository — even one in the same
organisation — cannot exchange for a usable access token.

```sh
gcloud iam workload-identity-pools create stirrup-gha \
  --location=global \
  --project=rubynerd-net

gcloud iam workload-identity-pools providers create-oidc stirrup-gha-provider \
  --location=global \
  --workload-identity-pool=stirrup-gha \
  --issuer-uri="https://token.actions.githubusercontent.com" \
  --attribute-mapping="google.subject=assertion.sub,attribute.repository=assertion.repository,attribute.ref=assertion.ref" \
  --attribute-condition="assertion.repository == 'rxbynerd/stirrup' && (assertion.ref == 'refs/heads/main' || assertion.ref.startsWith('refs/tags/v'))" \
  --project=rubynerd-net
```

The ref constraint pushes the trust decision from the workflow YAML
(which any collaborator with write access can edit) down to the GCP
credential layer (which they cannot). Token issuance is locked to
`main` pushes and `v*` tag pushes — feature branches, scheduled runs,
and `workflow_dispatch` against arbitrary refs cannot exchange their
GHA OIDC JWT for a GCP access token even if a future workflow change
requests `id-token: write`.

After creation, verify the attribute condition is present:

```sh
gcloud iam workload-identity-pools providers describe stirrup-gha-provider \
  --location=global \
  --workload-identity-pool=stirrup-gha \
  --project=rubynerd-net \
  --format='value(attributeCondition)'
```

A missing or wrong condition is the single biggest configuration risk
on this path — without it, any GitHub-hosted runner from any repo can
exchange for an access token. Confirm the condition before binding
IAM roles.

### 4. Bind IAM roles to the external identity

Direct WIF binds roles to the *principalSet* representing the GitHub
OIDC subject — not to a service account.

`<PROJECT_NUMBER>` is the numeric project number (not the project ID).
Find it with `gcloud projects describe rubynerd-net --format='value(projectNumber)'`.
The number itself does NOT need to be saved as a GitHub repository
variable — only the fully-constructed `GCP_WORKLOAD_IDENTITY_PROVIDER`
string (which embeds it) does.

```sh
PRINCIPAL="principalSet://iam.googleapis.com/projects/<PROJECT_NUMBER>/locations/global/workloadIdentityPools/stirrup-gha/attribute.repository/rxbynerd/stirrup"
```

#### Phase 1 (bind now)

Phase 1 needs exactly one role on the WIF principal: the right to push
container images.

```sh
gcloud projects add-iam-policy-binding rubynerd-net \
  --role=roles/artifactregistry.writer \
  --member="$PRINCIPAL"
```

| Role | Purpose | Phase |
|---|---|---|
| `roles/artifactregistry.writer` | Push container images. | 1 |

If you are reading this after the operator already ran the full
bootstrap, the Phase 2 bindings below are already in place — that is
fine for now; they will be narrowed when Phase 2 ships.

#### Phase 2 (defer until `gcloud artifacts sbom load` ships)

The four roles below are needed only when the SBOM-upload step lands
(Phase 2: `gcloud artifacts sbom load` from `release.yml::sbom`).
**Do not apply them until Phase 2 ships** — binding them earlier is
a permanent over-grant for capabilities that aren't exercised yet.

```sh
# DO NOT RUN UNTIL PHASE 2 SHIPS.
for role in \
  roles/containeranalysis.notes.editor \
  roles/containeranalysis.occurrences.editor \
  roles/containeranalysis.notes.attacher \
  roles/storage.objectAdmin; do
  gcloud projects add-iam-policy-binding rubynerd-net \
    --role="$role" \
    --member="$PRINCIPAL"
done
```

| Role | Purpose | Phase |
|---|---|---|
| `roles/containeranalysis.notes.editor` | Create SBOM-reference notes. | 2 |
| `roles/containeranalysis.occurrences.editor` | Create SBOM-reference occurrences. | 2 |
| `roles/containeranalysis.notes.attacher` | Attach SBOM-reference notes to images. | 2 |
| `roles/storage.objectAdmin`[^1] | Write to the Artifact Analysis managed bucket. | 2 |

[^1]: `gcloud artifacts sbom load` (Phase 2) writes SBOMs to a managed
Cloud Storage bucket. Without this binding the load command exits 0
but the SBOM never appears in `gcloud artifacts sbom list`. The
project-scope binding is wider than strictly needed; a follow-up
will narrow it to the Artifact Analysis bucket once that bucket's
name is known.

## GitHub-side configuration

Set the following **repository variables** under
*Settings → Secrets and variables → Actions → Variables*. These are
`vars.*`, not `secrets.*`: every value is a non-secret resource
identifier, and surfacing them in workflow YAML is consistent with
how `google-github-actions/auth` documents the pattern.

| Variable | Description |
|---|---|
| `GCP_PROJECT_ID` | GCP project ID hosting the GAR repository (e.g. `rubynerd-net`). |
| `GCP_WORKLOAD_IDENTITY_PROVIDER` | Full resource path of the provider, e.g. `projects/<NUMBER>/locations/global/workloadIdentityPools/stirrup-gha/providers/stirrup-gha-provider`. The numeric project number is only needed to construct this string during bootstrap — it does not itself need to be stored as a separate repository variable. |
| `GAR_LOCATION` | Artifact Registry location, e.g. `europe-west4`. Determines the registry hostname `<location>-docker.pkg.dev`. |
| `GAR_REPOSITORY` | Repository name within Artifact Registry (e.g. `stirrup`). |
| `GAR_IMAGE` | Image name within the repository (e.g. `stirrup`). The final image path is `<GAR_LOCATION>-docker.pkg.dev/<GCP_PROJECT_ID>/<GAR_REPOSITORY>/<GAR_IMAGE>`. |

`ci.yml::publish-container` and `release.yml::publish-container` both
read these variables. If any of them are unset, the workflow steps
that depend on them will fail at run time with a clear "registry
hostname empty" or "workload identity provider empty" error — there
is no silent fallback to a different identity.

### Why we mint the access token via `gcloud` rather than the `auth` action's output

The publish workflows do **not** use `google-github-actions/auth`'s
`token_format: access_token` output to feed
`docker/login-action`. Direct WIF deliberately has no intermediate
service account, and the `access_token` output path requires SA
impersonation — the action exits with "the GitHub Action workflow
must specify a service_account to use when generating an OAuth 2.0
Access Token" if you try. The federated `external_account` credentials
file the action *does* write is sufficient for Google client libraries
that consume Application Default Credentials, but `docker login`
needs a literal bearer token. The canonical Direct-WIF pattern is
therefore: run `auth` without `token_format`, then
`setup-gcloud`, then `gcloud auth print-access-token` to transparently
perform the STS exchange and emit a usable bearer token. The minting
step pipes `::add-mask::` before writing the token to
`GITHUB_OUTPUT` so any accidental log echo downstream is redacted by
the runner. Upstream reference:
[`google-github-actions/auth` — Direct Workload Identity Federation](https://github.com/google-github-actions/auth#direct-workload-identity-federation).

## Verification

After the first `main` push merges with the new workflow, confirm the
dual-publish worked end to end.

### 0. (Optional) Run the smoke workflow on `main`

The smoke workflow at `.github/workflows/smoke-gar-publish.yml` mints
a federated access token and runs `gcloud artifacts repositories
describe` against the target repo without pushing or pulling any
image. Dispatch with `gh workflow run smoke-gar-publish.yml --ref
main`. It is the fastest way to confirm the WIF provider, IAM
bindings, and access-token minting path are healthy after a provider
rotation, IAM rebind, or after editing the `auth` / `setup-gcloud` /
token-minting steps in `ci.yml` / `release.yml`. Note that it can
only be dispatched from main or a v* tag (the same WIF
`attributeCondition` that gates the real publish path also gates the
smoke). This is intentional, and means pre-merge verification of
auth-surface changes on a feature branch is infeasible by design.

### 1. Confirm the image landed in GAR

```sh
gcloud artifacts docker images list \
  europe-west4-docker.pkg.dev/rubynerd-net/stirrup/stirrup \
  --project=rubynerd-net
```

You should see entries for `:latest`, `:main`, and `:sha-<7>`, each
with a populated `DIGEST` column. Multi-arch builds show one entry
per manifest list, not per platform.

### 2. Confirm both registries serve the same digest

```sh
docker buildx imagetools inspect ghcr.io/rxbynerd/stirrup:latest \
  --format '{{ json .Manifest.Digest }}'

docker buildx imagetools inspect \
  europe-west4-docker.pkg.dev/rubynerd-net/stirrup/stirrup:latest \
  --format '{{ json .Manifest.Digest }}'
```

The two digests must be identical. They are by construction —
`docker/build-push-action` pushes the same OCI manifest to every
registry in its `tags:` list — but verifying after the first run
catches any silent push failure that the action might have hidden.

### 3. Confirm multi-arch

```sh
docker buildx imagetools inspect \
  europe-west4-docker.pkg.dev/rubynerd-net/stirrup/stirrup:latest \
  --format '{{ range .Manifest.Manifests }}{{ .Platform.OS }}/{{ .Platform.Architecture }}{{ "\n" }}{{ end }}'
```

Expect `linux/amd64` and `linux/arm64`. A single platform means
QEMU did not run; check the workflow's `Set up QEMU` step output.

### 4. Confirm a GKE node can pull

From a node in the `native-runtime` cluster (or any GCE VM running
under the project's default service account):

```sh
gcloud auth configure-docker europe-west4-docker.pkg.dev
docker pull europe-west4-docker.pkg.dev/rubynerd-net/stirrup/stirrup:latest
```

A successful pull with no `gcloud auth login` and no PAT is the proof
the Phase 1 goal is met.

## Rolling the WIF provider

The provider's attribute condition is the *only* repo-level guard
preventing token theft via a misconfigured GitHub Actions issuer.
Roll it under any of the following conditions:

- **Repository rename or transfer.** The condition pins
  `assertion.repository`; the new owner/name must be reflected
  before the next workflow run. There is no rolling rename — push a
  provider update first, merge the workflow change, then push the
  repo rename.
- **Org-wide policy change.** If the org adopts a new shape for OIDC
  subjects (e.g. ref-scoped pinning), roll the condition to match.
- **Suspected leak of the provider's principalSet path.** The path is
  not secret, but if it ends up in a context where someone might
  attempt token replay, rotate the pool ID (`stirrup-gha-<n+1>`) and
  re-bind IAM roles. Update `vars.GCP_WORKLOAD_IDENTITY_PROVIDER` in
  the same change.

`google-github-actions/auth` handles JWKS caching transparently, so
GitHub-side issuer key rotation is invisible to the workflow.
GitHub does not pre-announce JWKS rotations; if a workflow run
suddenly fails with `invalid_token` despite no config change, suspect
JWKS first and check the
[GitHub Actions OIDC token reference](https://docs.github.com/actions/deployment/security-hardening-your-deployments/about-security-hardening-with-openid-connect).
The issuer URL itself (`https://token.actions.githubusercontent.com`)
is stable; if it ever changes, GitHub will announce well in advance.

## Risks

These are the failure modes worth keeping in mind. None block Phase 1
ship.

1. **Forked PRs cannot publish, intentionally.** GitHub does not
   expose `vars.*` to fork runs, and the WIF provider's attribute
   condition rejects tokens not minted by `rxbynerd/stirrup`. Both
   `publish-container` jobs are also gated on
   `github.repository == 'rxbynerd/stirrup'` as belt-and-braces, so
   forked runs of either workflow skip the job rather than failing
   in a confusing way.
2. **Attribute condition is the only repo guard.** A future operator
   who edits the provider and drops the condition removes the only
   thing preventing org-wide token-exchange. `gcloud iam
   workload-identity-pools providers describe` should surface a
   non-empty `attributeCondition` field on every audit.
3. **Cross-region pull cost.** If the `native-runtime` cluster moves
   away from `europe-west4`, in-region GAR pulls become cross-region
   and add egress cost. The bootstrap should be re-run in the new
   region (or the existing repo recreated) before the cluster
   migration cuts over.
