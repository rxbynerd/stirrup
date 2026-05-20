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
on every new image without further configuration — and bills per
scan. Cost-control implication: dev images from `ci.yml` are
deliberately published only to GHCR (not GAR) so the auto-scan
cost surface is bounded to release tags. See
[Phase 3 — Vulnerability gating on releases](#phase-3--vulnerability-gating-on-releases)
below for the gate flow and the on-demand dispatch wrapper.

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
**Workload Identity Federation through a Service Account** path: the
GitHub principalSet impersonates a short-lived service account that
holds the GAR-push role bindings. There is no service-account JSON
key file at any point — the SA exists purely as the actor whose roles
the federated GitHub identity is authorised to assume.

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

### 4. Create the publisher service account and bind IAM roles

Create the dedicated publisher SA. It owns no key file, has no
console login, and exists solely as the actor whose role bindings
the GitHub principalSet is authorised to assume via WIF.

```sh
gcloud iam service-accounts create stirrup-publisher \
  --display-name="Stirrup container publisher" \
  --description="Impersonated by the rxbynerd/stirrup GitHub Actions WIF principal to push to GAR." \
  --project=rubynerd-net
```

The full SA email is
`stirrup-publisher@rubynerd-net.iam.gserviceaccount.com`. This value
goes into GitHub as `vars.GAR_PUBLISHER_SA`.

Grant the GitHub principalSet permission to impersonate the SA. This
is the only binding the principalSet itself receives — every GAR /
Artifact Analysis role binds to the SA, not to the principalSet.

`<PROJECT_NUMBER>` is the numeric project number (not the project ID).
Find it with `gcloud projects describe rubynerd-net --format='value(projectNumber)'`.

```sh
PRINCIPAL="principalSet://iam.googleapis.com/projects/<PROJECT_NUMBER>/locations/global/workloadIdentityPools/stirrup-gha/attribute.repository/rxbynerd/stirrup"

gcloud iam service-accounts add-iam-policy-binding \
  stirrup-publisher@rubynerd-net.iam.gserviceaccount.com \
  --role=roles/iam.workloadIdentityUser \
  --member="$PRINCIPAL" \
  --project=rubynerd-net
```

#### Phase 1 (bind now)

Phase 1 needs exactly one role on the SA: the right to push
container images.

```sh
gcloud projects add-iam-policy-binding rubynerd-net \
  --role=roles/artifactregistry.writer \
  --member="serviceAccount:stirrup-publisher@rubynerd-net.iam.gserviceaccount.com"
```

| Role | Purpose | Phase |
|---|---|---|
| `roles/artifactregistry.writer` | Push container images. | 1 |

If you are reading this after the operator already ran the full
bootstrap, the Phase 2 bindings below are already in place — that is
intentional. The Phase 2 release-time `gcloud artifacts sbom load`
steps in `release.yml::publish-container` consume them; see the
[Phase 2 — Image SBOMs in Artifact Analysis](#phase-2--image-sboms-in-artifact-analysis)
section below for how they are exercised.

#### Phase 2 (bind now — `gcloud artifacts sbom load` ships in `release.yml`)

The four roles below are exercised by the release-time SBOM upload
steps. They were pre-bound by the Phase 1 bootstrap (the spec
above lists them in the same `gcloud projects add-iam-policy-binding`
loop run alongside the Phase 1 `roles/artifactregistry.writer`
grant), so an operator who ran the full bootstrap once does not
need to re-run this block when Phase 2 ships.

```sh
for role in \
  roles/containeranalysis.notes.editor \
  roles/containeranalysis.occurrences.editor \
  roles/containeranalysis.notes.attacher \
  roles/storage.objectAdmin; do
  gcloud projects add-iam-policy-binding rubynerd-net \
    --role="$role" \
    --member="serviceAccount:stirrup-publisher@rubynerd-net.iam.gserviceaccount.com"
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

#### Transition note

The original Phase 1 PR (#163) bound these roles directly to the
principalSet. Issue #167 migrated to SA impersonation. Operators who
ran the original bootstrap will have *both* sets of bindings in place
during the transition; the follow-up cleanup PR revokes the
principalSet bindings once the SA path is verified.

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
| `GAR_PUBLISHER_SA` | Email of the publisher service account the GitHub principalSet impersonates (e.g. `stirrup-publisher@rubynerd-net.iam.gserviceaccount.com`). Holds all GAR / Artifact Analysis role bindings; the GitHub principalSet only holds `roles/iam.workloadIdentityUser` on this SA. |
| `GAR_LOCATION` | Artifact Registry location, e.g. `europe-west4`. Determines the registry hostname `<location>-docker.pkg.dev`. |
| `GAR_REPOSITORY` | Repository name within Artifact Registry (e.g. `stirrup`). |
| `GAR_IMAGE` | Image name within the repository (e.g. `stirrup`). The final image path is `<GAR_LOCATION>-docker.pkg.dev/<GCP_PROJECT_ID>/<GAR_REPOSITORY>/<GAR_IMAGE>`. |

`release.yml::publish-container` reads these variables for the
release-time GHCR + GAR dual-publish. `ci.yml::publish-container`
no longer reads them: dev images on main go to GHCR only (so
auto-scan cost on GAR is bounded to releases), and the GAR-side
auth + login steps have been removed from the ci job. The smoke
workflows (`smoke-gar-publish.yml`,`smoke-cloud-run-job.yml`) and
`release.yml` are now the only callers. If any variable is unset,
those workflows will fail at run time with a clear "registry
hostname empty" or "workload identity provider empty" error —
there is no silent fallback to a different identity.

### Why short-lived service-account impersonation

The publish workflows pass `service_account: ${{ vars.GAR_PUBLISHER_SA }}`
and `token_format: access_token` to `google-github-actions/auth`. The
action exchanges the GitHub OIDC JWT for STS-issued external-account
credentials, then impersonates the SA to mint a short-lived OAuth2
access token that `docker/login-action` consumes via
`steps.gcp-auth.outputs.access_token`. No `gcloud` invocation, no
`::add-mask::`, no extra step output — the canonical pattern. The
predecessor design (PR #163, #166) used Direct WIF with no SA and
piped `gcloud auth print-access-token` into a masked step output;
issue #167 migrated to impersonation for parity with the upstream
docs and to keep `gcloud`-based Phase 2 / Phase 3 work
(SBOM upload, vulnerability gating) ergonomic. The blast radius is
unchanged: an attacker who lands a malicious workflow on `main` or a
`v*` tag still mints a token with the same five role grants — those
roles now bind to the SA instead of the principalSet. Upstream
reference:
[`google-github-actions/auth` — Workload Identity Federation through a Service Account](https://github.com/google-github-actions/auth#workload-identity-federation-through-a-service-account).

## Verification

After the first tagged release merges with the publish workflow,
confirm the dual-publish worked end to end. (A push to `main` no
longer touches GAR — only release tags do.)

### 0. (Optional) Run the smoke workflow on `main`

The smoke workflow at `.github/workflows/smoke-gar-publish.yml` mints
a short-lived access token via the same WIF + SA-impersonation chain
the publish workflows use, then runs `gcloud artifacts repositories
describe` against the target repo without pushing or pulling any
image. Dispatch with `gh workflow run smoke-gar-publish.yml --ref
main`. It is the fastest way to confirm the WIF provider, the
`roles/iam.workloadIdentityUser` impersonation grant on
`vars.GAR_PUBLISHER_SA`, and the SA's role bindings are healthy
after a provider rotation, IAM rebind, or after editing the `auth`
step in `ci.yml` / `release.yml`. Note that it can only be
dispatched from main or a v* tag (the same WIF
`attributeCondition` that gates the real publish path also gates the
smoke). This is intentional, and means pre-merge verification of
auth-surface changes on a feature branch is infeasible by design.

### 1. Confirm the image landed in GAR

```sh
gcloud artifacts docker images list \
  europe-west4-docker.pkg.dev/rubynerd-net/stirrup/stirrup \
  --project=rubynerd-net
```

After a tagged release, expect entries for `:latest`, `:vX.Y.Z`,
and (on non-prerelease tags) `:X.Y`, each with a populated
`DIGEST` column. Multi-arch builds show one entry per manifest
list, not per platform. The pre-2026-05 `:main` / `:sha-<7>` dev
tags are no longer pushed to GAR — those live on GHCR
(`ghcr.io/rxbynerd/stirrup`) only.

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

## Phase 2 — Image SBOMs in Artifact Analysis

Phase 1 only puts the container in GAR. Phase 2 makes the container's
contents *queryable* by Google's
[Artifact Analysis](https://docs.cloud.google.com/artifact-analysis/docs/artifact-analysis)
service: after each release tag pushes its image, the
`release.yml::publish-container` job generates a per-image SBOM in
both SPDX and CycloneDX formats and uploads them via
`gcloud artifacts sbom load`. Once loaded, the SBOMs surface as
[Container Analysis](https://docs.cloud.google.com/container-analysis/docs)
occurrences against the image digest, alongside the discovery and
vulnerability occurrences that the continuous-scanning service emits
on its own.

### Why a separate image SBOM

The release workflow already ships SPDX + CycloneDX SBOMs as GitHub
Release assets (`release.yml::sbom`). Those describe the *source
tree* — the Go module graph that `anchore/sbom-action` builds by
scanning `path: .`. They are the right input for someone auditing
which versions of `aws-sdk-go-v2`, `google.golang.org/grpc`, or
`cedar-go` are pinned in any given release, but they say nothing
about what actually shipped in the container.

The image SBOM is the inverse: `anchore/sbom-action` runs against
the pushed image digest and walks every layer, including the
`gcr.io/distroless/static-debian12:nonroot` base. The resulting
package list is what Artifact Analysis maps to CVEs — vulnerabilities
in libc, openssl, or any other base-layer package only show up in
the image SBOM, not in the source SBOM. Both ship; both are
necessary; neither is a substitute for the other.

### What the workflow does

The Phase 2 steps in `release.yml::publish-container` (in order):

1. **`docker/build-push-action`** keeps `sbom: true` and exposes
   `id: docker-push`. The OCI in-toto SBOM attestation written by
   buildx is for cosign/syft consumers walking the SLSA chain off
   the registry directly; it does *not* substitute for `sbom load`.
2. **`google-github-actions/setup-gcloud`** installs the `gcloud`
   CLI on PATH. The earlier `gcp-auth` step's access token is
   already exported into the runner environment, so `gcloud`
   commands authenticate as the impersonated `vars.GAR_PUBLISHER_SA`
   without an explicit `gcloud auth activate` call.
3. **Two `anchore/sbom-action` calls** generate
   `stirrup-vX.Y.Z.image.spdx.json` and `.image.cdx.json`. The
   `.image.` infix distinguishes them from the source SBOMs in the
   release-asset bundle. `upload-artifact` and
   `upload-release-assets` are both `false`: these files go *only*
   to Artifact Analysis.
4. **Two `gcloud artifacts sbom load` calls** push each file to
   Artifact Analysis. The `--uri` is the digest form
   `<location>-docker.pkg.dev/<project>/<repo>/<image>@sha256:...`
   — `gcloud` rejects the tag form because an SBOM must bind to a
   specific manifest, and a tag is mutable.

### IAM (already bound)

The four roles `sbom load` needs were pre-bound in the Phase 1
bootstrap and are listed in the [Phase 2 IAM table](#phase-2-bind-now--gcloud-artifacts-sbom-load-ships-in-releaseyml)
above:

- `roles/containeranalysis.notes.editor`
- `roles/containeranalysis.occurrences.editor`
- `roles/containeranalysis.notes.attacher`
- `roles/storage.objectAdmin` (for the managed Cloud Storage bucket
  that backs Artifact Analysis SBOM storage — without this, `sbom
  load` returns exit 0 but the SBOM never appears in `sbom list`)

This is intentional, not a coincidence: the bootstrap was scoped to
the full set of roles up front so Phase 2 would not require a
second IAM pass. No `gcloud` / IAM changes are needed when Phase 2
ships.

### Verification

After a `vX.Y.Z` tag push completes its release run, list the SBOMs
loaded against the image digest:

```sh
DIGEST=$(gcloud artifacts docker images describe \
  europe-west4-docker.pkg.dev/rubynerd-net/stirrup/stirrup:vX.Y.Z \
  --format='value(image_summary.digest)' \
  --project=rubynerd-net)

gcloud artifacts sbom list \
  --uri="europe-west4-docker.pkg.dev/rubynerd-net/stirrup/stirrup@${DIGEST}" \
  --project=rubynerd-net
```

Expect two rows — one SPDX, one CycloneDX — both pointing at the
same digest the release workflow pushed. A single row means one
upload step errored: the `sbom load` script runs under
`set -euo pipefail`, so a non-zero `gcloud` exit surfaces a workflow
failure rather than failing silently. Check the `publish-container`
job in the release run for the failed step, then re-dispatch
`release.yml` against the tag once the cause is resolved.

For cross-checking: `grype sbom:stirrup-vX.Y.Z.image.spdx.json`
locally (against the file the workflow uploaded) produces the same
vulnerability set Artifact Analysis stores against the digest.

### Operator's mental model

Two SBOM streams exist for the same release, served to different
audiences:

| Stream | Lives in | Describes | Consumer |
|---|---|---|---|
| Source SBOM | GitHub Release assets (`*.spdx.json`, `*.cdx.json`) | Go module graph at the tagged commit | Anyone auditing the dependency pin set without `docker pull` |
| Image SBOM | Artifact Analysis Container Analysis occurrences | Everything in the pushed multi-arch image, including the distroless base | The vulnerability scanner; future BinAuthz attestation chains; Phase 3's release gate |

The OCI in-toto attestation written by `sbom: true` is a third,
adjacent stream that lives in the image registry alongside the
manifest itself; it's structurally close to the image SBOM but
formatted for SLSA / cosign consumers rather than for Container
Analysis ingestion.

## Phase 3 — Vulnerability gating on releases

The `release.yml::vulnerability-gate` job runs after
`publish-container` on every tag push. It queries Container Analysis
for vulnerability occurrences on the just-pushed image and reports
findings. **The current mode is "warn + file follow-up issue", not
block.** Promotion to a blocking gate is intentionally a separate,
small follow-up PR so that the calibration window can produce real
data about scanner timing and false-positive rates against actual
release images.

The job body lives in `.github/workflows/_vuln-scan.yml`
(reusable, `workflow_call`). Both the release-time caller and the
ad-hoc `vuln-scan.yml` dispatcher share the same polling, query,
normalisation, summary, and issue-filing logic — see
[On-demand vulnerability scans](#on-demand-vulnerability-scans)
below for the dispatcher's use case.

### What the gate does

1. Re-authenticates to GCP via WIF + SA impersonation (per-job OIDC
   tokens mean `publish-container`'s access token is not reusable —
   the gate has to mint its own). The impersonated SA is still
   `vars.GAR_PUBLISHER_SA`; no new bindings are required (see
   "IAM observation" below).
2. Polls `gcloud artifacts docker images list --show-occurrences
   --occurrence-filter='kind="DISCOVERY"' ...` every 15s for up to
   10 minutes, waiting for the discovery analysis to reach
   `FINISHED_SUCCESS` or `FINISHED_FAILED`. Timeout is **warn, not
   fail** (see the scan-latency note below).
3. Queries `kind="VULNERABILITY"` occurrences for the same digest,
   parses them with `jq`, and emits step outputs for CRITICAL /
   HIGH / MEDIUM / LOW totals plus CRITICAL / HIGH fixable counts.
4. Writes a Markdown summary table to `$GITHUB_STEP_SUMMARY`,
   including the `gcloud` invocation an operator can run locally to
   reproduce the query.
5. Files a tracking issue via `gh issue create` if any
   CRITICAL **and** fixable vulnerability exists (the warn-mode
   threshold). HIGH-fixable findings appear in the summary but are
   not auto-filed — the noise/signal balance during calibration
   leans toward fewer auto-issues.
6. Emits a single-line "would block with N" / "would pass cleanly"
   calibration log so a later promote-to-block change has data to
   reason against.

### Warn-vs-block calibration

The job is deliberately **not** in `release.needs`, so the warn
posture is structural rather than just behavioural — even an
accidental `exit 1` from the reusable workflow's `Gate result`
step would not block the release today. When the calibration
window closes, the promote-to-block change is two edits:

1. Flip `mode: warn` to `mode: block` in
   `release.yml::vulnerability-gate`'s `with:` block.
2. Add `vulnerability-gate` to `release.needs`.

The `Gate result` step in `_vuln-scan.yml` already honours
`block` mode (exits 1 on CRITICAL+fixable when the override is
inactive), so the caller flip is sufficient.

Until then, the release pipeline ships regardless of what the gate
finds. The tracking issue is the audit trail.

### Override variable

`vars.STIRRUP_RELEASE_VULN_OVERRIDE` short-circuits the
issue-creation step for one specific tag. Set it to the literal tag
string (e.g. `v1.2.3`) when:

- A release is already cut, the finding is known and triaged, and
  rolling back the tag is high-cost.
- A noisy scanner false-positive on a specific image is producing
  duplicate tracking issues you don't want to keep closing.

**The override does not skip the query or the summary** — they
still run so the calibration data and operator-facing summary
remain intact. The match is also intentionally narrow (one literal
tag, no globs, no env-var fallback) so a forgotten override cannot
silently suppress future releases.

**Operator responsibility:** every use of the override is paired
with a durable override-audit issue that the gate auto-files when
the bypass fires (title: `vuln gate override used: <tag>`, label:
`security`). The operator who set the override MUST add a rationale
comment on that auto-filed issue — that comment is the durable
audit trail. The auto-filed issue body does not contain CVE detail;
it exists only so the bypass leaves a record in the issue tracker
after the workflow run's 90-day log retention expires.

`vars.STIRRUP_RELEASE_VULN_OVERRIDE` is **settable by any repo
collaborator with write access** and leaves no trace in git history.
That surface is intentional (an emergency operator action must not
require a PR round-trip), but it is also why the auto-filed
override-audit issue and the rationale comment are non-negotiable:
they are the only durable record an auditor can query.

### Permissions

`vulnerability-gate` holds a minimal set:

| Permission | Why |
|---|---|
| `contents: read` | Default; kept explicit to fence future edits. |
| `id-token: write` | Per-job OIDC; required for re-auth via WIF. |
| `issues: write` | `gh issue create` for the tracking issue. **New capability vs `publish-container`.** |

`packages: write` is **deliberately omitted** — the gate never
pushes anything to GHCR. The split keeps the principle that any job
holding `packages: write` is the only thing that ever does, and the
introspection job is read-only on registries.

### Scan-wait timeout policy

The 10-minute discovery wait treats timeout as warn-and-continue,
never fail. Reasoning:

- Container Analysis scan latency is variable. Internal Google
  outages of the scanner have historically taken hours, not minutes.
- The release is already cut by the time the gate runs — failing on
  scanner latency would block the GitHub Release publish for an
  outage the project has no leverage over.
- During the warn calibration window we are explicitly tolerating
  early false negatives. If the gate misses a vulnerability because
  discovery hadn't finished, the next periodic re-scan picks it up
  and the next manual re-query (or the next release of the same
  image base) catches it.

Operators can re-run the query manually at any time. For any past
release tag, resolve the digest first and then query Container
Analysis for vulnerability occurrences against that immutable
digest:

```sh
# 1. Resolve the digest for any released tag:
DIGEST=$(gcloud artifacts docker images describe \
  <GAR_LOCATION>-docker.pkg.dev/rubynerd-net/stirrup/stirrup:vX.Y.Z \
  --format='value(image_summary.digest)' \
  --project=rubynerd-net)

# 2. Query vulnerability occurrences against that digest:
gcloud artifacts docker images list \
  <GAR_LOCATION>-docker.pkg.dev/rubynerd-net/stirrup/stirrup \
  --show-occurrences \
  --occurrence-filter='kind="VULNERABILITY"' \
  --filter="uri=\"<GAR_LOCATION>-docker.pkg.dev/rubynerd-net/stirrup/stirrup@${DIGEST}\"" \
  --format=json
```

The same digest is also surfaced in the workflow run's summary
table, so for the most recent release a copy-paste from the Actions
UI works without step 1.

### IAM observation

No new IAM bindings are required for Phase 3. The Phase 1 bootstrap
already grants `containeranalysis.occurrences.editor` to
`vars.GAR_PUBLISHER_SA` (so `gcloud artifacts sbom load` can write
occurrences during `publish-container`). That role is a superset
of the `occurrences.get` / `occurrences.list` permissions the gate
needs, so the same impersonated identity reads back what the
earlier job wrote — and what Container Analysis itself emitted as
discovery occurrences. Verified during the Phase 1 bootstrap; the
existing `gcloud projects get-iam-policy ...` audit command in this
doc surfaces it.

`issues: write` is GitHub-side, not GCP-side; the existing PAT-free
posture is unchanged.

### On-demand vulnerability scans

`.github/workflows/vuln-scan.yml` is a `workflow_dispatch`
wrapper around `_vuln-scan.yml`. Use it to re-query Artifact
Analysis findings for any digest already in GAR without cutting a
new release tag — typical triggers:

- A fresh CVE drops against a base image that a still-supported
  release ships. Re-query to see whether the existing image is
  now flagged.
- A vendor announces a vulnerability in a Go module the harness
  embeds; the operator wants to confirm whether the latest
  released image picks it up.
- Investigating a specific digest reported by an external scanner
  for cross-validation.

Dispatch from `main` or an existing `v*` tag (the WIF provider's
attribute-condition pins `assertion.ref` to those two refs;
feature-branch dispatches fail at the auth step with a clean
WIF error — the documented security boundary working as
designed):

```sh
gh workflow run vuln-scan.yml --ref main \
  -f image_uri='europe-west4-docker.pkg.dev/rubynerd-net/stirrup/stirrup@sha256:...' \
  -f tag='v1.2.3' \
  -f mode='warn'
```

The dispatcher accepts the same `mode` input as the release
caller. `block` is useful for one-off strict checks where a
CRITICAL+fixable finding should produce a red Actions UI marker
without touching `release.yml`. The
`STIRRUP_RELEASE_VULN_OVERRIDE` variable is matched literally
against `inputs.tag`; ad-hoc dispatches with arbitrary labels do
not collide with a release-tag override.

The dispatcher does **not** trigger a scan. Scans are triggered
by GAR's auto-scan on push (Phase 1 enabled
`containerscanning.googleapis.com`); the dispatcher only queries
already-completed results. Re-evaluation against the loaded
SBOMs happens periodically inside Container Analysis at no
additional billable scan cost.

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
4. **Container Analysis scan latency is variable.** Phase 3's
   `vulnerability-gate` polls for the discovery occurrence to reach
   `FINISHED_SUCCESS` for up to 10 minutes and then warns-and-
   continues. The 10-minute floor is a known unknown — internal
   Google scanner backlogs can exceed it, in which case the gate
   reports incomplete data and the release ships. The timeout is
   deliberately tuned for "scanner outage must not wedge releases";
   re-evaluate once the calibration window has produced enough runs
   to characterise actual scan-completion times for stirrup images.
5. **Warn calibration tolerates false negatives early.** During the
   Phase 3 warn window, the gate explicitly accepts that a
   vulnerability may slip past — discovery may not have finished, a
   gcloud response shape may not match the jq normalisation, or
   `containeranalysis.googleapis.com` may be having a bad day. The
   release ships anyway and a tracking issue is filed retroactively
   when the same image base re-runs through the gate on the next
   tag. The block-mode promotion (separate follow-up PR) is what
   tightens this; until then, treat the gate as defence in depth,
   not as the primary vulnerability control.
