# Credential federation

Stirrup's credential layer separates **where am I running** (the
TokenSource) from **what am I calling** (the credential Source). This
two-layer design lets a harness on any cloud authenticate to any
provider's IAM without bundling provider-specific SDKs everywhere or
mounting raw service-account keys into the runtime.

```
TokenSource  →  credential.Source  →  Resolved.BearerToken closure
```

The `BearerToken` returned by `Resolved` is itself a closure: provider
adapters call it on every request, and the closure caches and refreshes
the underlying token internally (see the package doc on
`harness/internal/credential/source.go` for the full closure contract).
Static-secret sources resolve once and reuse the value; OAuth2-shaped
federation sources wrap their refresh in
`oauth2.ReuseTokenSource` so the cached access token is reused until
expiry without the adapter knowing the difference.

## TokenSources

A TokenSource produces a short-lived OIDC token (or an arbitrary
identity bearer) from the runtime environment.

| Type | Runtime it targets | Required fields |
|---|---|---|
| `gke-metadata` | GKE Workload Identity / GCE metadata server | `audience` |
| `file` | Anywhere — Kubernetes projected token volumes, mounted secrets | `path` |
| `env` | Anywhere — pre-injected token in an environment variable | `envVar` |
| `aws-irsa` | EKS Pod Identity / IRSA (reads `AWS_WEB_IDENTITY_TOKEN_FILE`) | none |
| `azure-imds` | Azure VMs / AKS pods with managed identity | `resource`; optional `clientId` for user-assigned identities |
| `github-actions-oidc` | GitHub Actions workflows with `permissions: id-token: write` | `audience` |

Token sources are **reusable across targets**: the same EKS IRSA
projected token can be exchanged for AWS credentials via STS, GCP
credentials via Workload Identity Federation, or (in the future) any
other OIDC-aware relying party. Token-source types do not encode their
destination.

## Credential sources

A credential `Source` consumes a TokenSource (or a static SecretStore
reference) and returns a `Resolved` carrying provider-ready
authentication material.

| Type | Targets | Required fields |
|---|---|---|
| `static` | Any provider with an API key | `apiKeyRef` on the provider config |
| `aws-default` | Bedrock | none — uses the AWS SDK default chain |
| `web-identity` | Bedrock from a non-AWS runtime | `roleArn`, `tokenSource` |
| `gcp-default` | Vertex AI from GCP / mounted SA key | none — Application Default Credentials |
| `gcp-service-account` | Vertex AI from anywhere | `gcpCredentialsFile` on the provider config |
| `gcp-workload-identity` | Vertex AI from GKE/GCE | none — uses the metadata server |
| `gcp-workload-identity-federation` | Vertex AI from a non-GCP runtime | `audience`, `tokenSource`; optional `serviceAccount` |

## Cross-cloud → Vertex AI Gemini via Workload Identity Federation

GCP Workload Identity Federation lets a non-GCP runtime authenticate
to Google APIs by trading an OIDC token from its native IAM (AWS,
Azure, GHA, Okta, …) for a short-lived Google access token. The harness
implements the full flow in
`harness/internal/credential/google_federation.go`:

1. `tokenSource.Token(ctx)` — fetch an OIDC proof from the runtime.
2. `POST https://sts.googleapis.com/v1/token` — exchange it for a
   federated access token bound to the configured WIF audience.
3. (Optional) `POST https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/{SA}:generateAccessToken`
   — impersonate a target service account so operators can grant
   narrower IAM than the federated identity itself holds.
4. Wrap the result in `oauth2.ReuseTokenSource` for caching + lazy
   refresh.

### GCP-side setup

The audience that goes into the RunConfig identifies the Workload
Identity Pool **provider** that will accept your runtime's tokens. The
shape is fixed:

```
//iam.googleapis.com/projects/{PROJECT_NUMBER}/locations/global/workloadIdentityPools/{POOL_ID}/providers/{PROVIDER_ID}
```

Stirrup validates this format at config-load time so a typo surfaces
before the harness ever talks to STS.

Setup is the same regardless of which runtime is the source IdP:

1. **Create a Workload Identity Pool**:
   ```sh
   gcloud iam workload-identity-pools create stirrup-pool \
     --location=global --display-name="Stirrup federation pool"
   ```
2. **Create a provider inside the pool** with attribute mapping that
   matches your source IdP's token claims:
   - **AWS** (IRSA / EKS): use the AWS provider type;
     `attribute-mapping="google.subject=assertion.arn,attribute.aws_role=assertion.arn"`.
   - **Azure**: use the OIDC provider type with Azure AD's issuer
     URL; map `assertion.sub` to `google.subject`.
   - **GitHub Actions**: use the OIDC provider type with issuer
     `https://token.actions.githubusercontent.com`; map
     `assertion.repository` and `assertion.ref` to attributes you can
     reference in IAM conditions.
3. **Grant the federated principal `roles/iam.workloadIdentityUser`**
   on a target service account:
   ```sh
   gcloud iam service-accounts add-iam-policy-binding \
     stirrup-vertex@my-project.iam.gserviceaccount.com \
     --role=roles/iam.workloadIdentityUser \
     --member="principalSet://iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/stirrup-pool/attribute.aws_role/arn:aws:iam::ACCOUNT:role/stirrup-runtime"
   ```
4. **Grant the target service account Vertex usage**:
   ```sh
   gcloud projects add-iam-policy-binding my-project \
     --member=serviceAccount:stirrup-vertex@my-project.iam.gserviceaccount.com \
     --role=roles/aiplatform.user
   ```

### RunConfig snippets

**EKS / IRSA → Vertex Gemini** (see
[`examples/runconfig/vertex-gemini-wif.json`](../examples/runconfig/vertex-gemini-wif.json)):

```json
{
  "provider": {
    "type": "gemini",
    "gcpProject": "my-project-id",
    "gcpLocation": "global",
    "credential": {
      "type": "gcp-workload-identity-federation",
      "audience": "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/aws-pool/providers/aws-provider",
      "serviceAccount": "stirrup-vertex@my-project-id.iam.gserviceaccount.com",
      "tokenSource": { "type": "aws-irsa" }
    }
  }
}
```

**Azure (system-assigned managed identity) → Vertex Gemini**:

```json
{
  "provider": {
    "type": "gemini",
    "gcpProject": "my-project-id",
    "gcpLocation": "us-central1",
    "credential": {
      "type": "gcp-workload-identity-federation",
      "audience": "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/azure-pool/providers/azure-provider",
      "serviceAccount": "stirrup-vertex@my-project-id.iam.gserviceaccount.com",
      "tokenSource": {
        "type": "azure-imds",
        "resource": "api://AzureADTokenExchange"
      }
    }
  }
}
```

For a user-assigned managed identity, add
`"clientId": "<UAMI client ID>"` to the `tokenSource` block.

**GitHub Actions → Vertex Gemini**:

```json
{
  "provider": {
    "type": "gemini",
    "gcpProject": "my-project-id",
    "gcpLocation": "global",
    "credential": {
      "type": "gcp-workload-identity-federation",
      "audience": "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/gha-pool/providers/gha-provider",
      "serviceAccount": "stirrup-vertex@my-project-id.iam.gserviceaccount.com",
      "tokenSource": {
        "type": "github-actions-oidc",
        "audience": "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/gha-pool/providers/gha-provider"
      }
    }
  }
}
```

For GitHub Actions, the token source's `audience` and the credential's
`audience` are the same string by convention — GHA's OIDC issuer
embeds the audience claim into the token, and STS rejects exchanges
where the claim does not match the WIF provider's expected audience.

The workflow must declare:

```yaml
permissions:
  id-token: write
  contents: read
```

so the runner injects `ACTIONS_ID_TOKEN_REQUEST_URL` and
`ACTIONS_ID_TOKEN_REQUEST_TOKEN`.

**Pre-mounted token file** (e.g. SPIFFE / sidecar-issued JWT):

```json
{
  "credential": {
    "type": "gcp-workload-identity-federation",
    "audience": "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/spiffe-pool/providers/spiffe-provider",
    "tokenSource": { "type": "file", "path": "/var/run/secrets/spiffe/jwt" }
  }
}
```

The `serviceAccount` field is optional. When omitted, the federated
access token from STS is used directly — useful when the WIF
principal itself has the IAM grants you need. Including it adds an
extra impersonation hop so the federated principal can hold only
`iam.serviceAccountTokenCreator` on the target SA, and the target SA
holds the actual Vertex grants.

## Cross-cloud → Bedrock via STS web-identity

The same pattern applies for AWS Bedrock from a non-AWS runtime, using
the older `web-identity` credential type. The TokenSource catalog above
is reusable: a GHA OIDC token, a GKE Workload Identity token, or a
projected token file all work as the subject token to
`sts:AssumeRoleWithWebIdentity`.

```json
{
  "provider": {
    "type": "bedrock",
    "region": "us-east-1",
    "credential": {
      "type": "web-identity",
      "roleArn": "arn:aws:iam::123456789012:role/StirrupBedrock",
      "tokenSource": {
        "type": "github-actions-oidc",
        "audience": "sts.amazonaws.com"
      }
    }
  }
}
```

## GCP-native paths

When the runtime is itself on GCP, prefer the GCP-native sources over
WIF — they avoid the STS round-trip entirely:

- `gcp-default` — Application Default Credentials. Looks at
  `GOOGLE_APPLICATION_CREDENTIALS`, then the metadata server. User-mode
  `gcloud auth application-default login` credentials are rejected
  outright; the harness must not run on a single human's identity.
- `gcp-service-account` — explicit service-account key file mounted
  into the runtime. Use when ADC's search order is too magical for
  your deployment story.
- `gcp-workload-identity` — fail-fast variant of `gcp-default` that
  goes straight to the metadata server. Errors loudly if there is no
  metadata server reachable, so a misconfigured pod does not silently
  fall through to environment-variable credentials.

## See also

- [`harness/internal/credential/source.go`](../harness/internal/credential/source.go)
  — the full closure contract on `Resolved.BearerToken`.
- [`harness/internal/credential/google_federation.go`](../harness/internal/credential/google_federation.go)
  — the WIF source implementation.
- [Google's WIF setup walkthrough](https://cloud.google.com/iam/docs/workload-identity-federation)
  — authoritative reference for pool/provider creation and attribute
  mapping.
