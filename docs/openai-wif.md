# OpenAI Workload Identity Federation

OpenAI's [Workload Identity Federation](https://developers.openai.com/api/docs/guides/workload-identity-federation)
lets a workload exchange an OIDC JWT — issued by its own platform
(GitHub Actions, EKS, AKS, GKE, generic projected Kubernetes tokens,
GCE/Azure metadata, or SPIFFE/SPIRE) — for a short-lived OpenAI access
token bound to a service account. No static `sk-...` API keys, no
rotation toil, no long-lived secret in the runtime.

This document is the operator walkthrough. The architectural shape sits
alongside [`credential-federation.md`](credential-federation.md); the
two complement each other.

## Why use it

- **No static API key in the runtime.** WIF replaces a long-lived
  `OPENAI_API_KEY` with a per-run, IdP-bound bearer token that expires
  in at most an hour and never outlives the subject token it was minted
  from. The harness never holds a durable credential.
- **Audit trail per workload.** The exchange binds the issued token to a
  specific service account and project via the OpenAI-side mapping.
- **Platform-attested identity.** The trust anchor is the IdP's OIDC
  signature over claims the platform controls (`sub`, `repository`,
  `aud`), not a copied secret.

## OpenAI-side setup

Configuration is performed by an **organization owner** at
`https://platform.openai.com/settings/organization/security/workload-identity-provider`.
Two resources are created — see OpenAI's per-provider guides (linked
under [See also](#see-also)) for the click-by-click UI.

1. **Workload Identity Provider** — registers the trusted OIDC issuer
   URL, the expected `aud`, and the key source (OIDC discovery for
   issuers that publish `.well-known/openid-configuration`, or an
   uploaded JWKS for self-hosted clusters). The resulting **identity
   provider ID** goes into RunConfig as `openaiIdentityProviderId`.
2. **Service-account mapping** — authorizes a subject-token attribute
   (for example `sub == repo:owner/repo:ref:refs/heads/main` for a
   GitHub Actions workflow, or `sub == system:serviceaccount:ns:sa` for
   a projected Kubernetes token) to mint tokens for a specific OpenAI
   service account in a specific project. The resulting **service
   account ID** goes into RunConfig as `openaiServiceAccountId`.

A single trailing wildcard is permitted in a mapping value
(`repo:owner/*`); the prefix must be non-empty. If more than one
enabled mapping matches a single exchange, OpenAI rejects it — mappings
must be unambiguous. Admin-API scopes cannot be assigned to a mapping,
so a federated token cannot call the Admin API.

## Stirrup-side configuration

The minimal RunConfig is:

```json
{
  "provider": {
    "type": "openai-responses",
    "baseUrl": "https://api.openai.com/v1",
    "credential": {
      "type": "openai-wif",
      "openaiIdentityProviderId": "idp_example",
      "openaiServiceAccountId": "sa_example",
      "tokenSource": { "type": "github-actions-oidc", "audience": "https://api.openai.com/v1" }
    }
  }
}
```

`openai-wif` pairs with the `openai-compatible` (Chat Completions) or
`openai-responses` (Responses API) provider types — the two adapters
that speak to the OpenAI API. The validator rejects it on any other
provider type so a federated access token is never aimed at a
non-OpenAI endpoint.

`apiKeyRef` is **not** set, and `apiKeyHeader` is left empty.
OpenAI WIF and static API keys are mutually exclusive on the same
provider: the bearer comes from the OAuth exchange and rides on
`Authorization: Bearer`, so the validator rejects a co-configured
`apiKeyRef` or `apiKeyHeader="api-key"`.

### The audience lives on the token source

Unlike Anthropic WIF — which carries organization and workspace
identifiers in the exchange body — the OpenAI exchange body holds only
the grant type, the subject token, and the two IDs. OpenAI validates the
**audience** from the subject token's `aud` claim against the provider
config, so the audience is configured on the `tokenSource` (the
canonical value is `https://api.openai.com/v1`, or whatever custom
audience the provider was registered with), never in the exchange
itself. Organization and project are bound by the service-account
mapping server-side and likewise do not appear in stirrup's config.

### Flags + env-var fallbacks

The CLI surface accepts the two ID flags, an optional subject-token-type
override, and an opt-in for the GitHub Actions runtime:

```sh
./stirrup harness \
  --provider openai-responses \
  --base-url https://api.openai.com/v1 \
  --openai-identity-provider-id idp_example \
  --openai-service-account-id sa_example \
  --openai-from-github-actions \
  --prompt "..."
```

Each ID flag has an env-var fallback:

| Flag | Env fallback |
|---|---|
| `--openai-identity-provider-id` | `OPENAI_IDENTITY_PROVIDER_ID` |
| `--openai-service-account-id` | `OPENAI_SERVICE_ACCOUNT_ID` |
| `--openai-subject-token-type` | `OPENAI_SUBJECT_TOKEN_TYPE` |

`--openai-subject-token-type` is optional and defaults to
`urn:ietf:params:oauth:token-type:jwt`, which every OpenAI-documented
provider uses; override it only for an IdP that issues a different token
type. Two more env vars infer the token-source block when `--config`
does not pin one:

- `OPENAI_IDENTITY_TOKEN_FILE=/path/to/jwt` →
  `tokenSource: {type: file, path: <path>}`
- `OPENAI_IDENTITY_TOKEN=<jwt>` →
  `tokenSource: {type: env, envVar: OPENAI_IDENTITY_TOKEN}`

The presence of GitHub Actions' `ACTIONS_ID_TOKEN_REQUEST_URL` does
**not** auto-select the GHA token source. The flag
`--openai-from-github-actions` is the explicit opt-in; silent IdP
selection from env presence is rejected for the same reason the
Anthropic path rejects it — a silently-selected IdP makes credential
bugs unfixable.

## Runtime walkthroughs

### GitHub Actions

The workflow must grant the job an OIDC token:

```yaml
permissions:
  id-token: write
  contents: read
```

so the runner injects `ACTIONS_ID_TOKEN_REQUEST_URL` and
`ACTIONS_ID_TOKEN_REQUEST_TOKEN`. The job then runs:

```yaml
jobs:
  stirrup:
    runs-on: ubuntu-latest
    permissions:
      id-token: write
      contents: read
    env:
      OPENAI_IDENTITY_PROVIDER_ID: idp_example
      OPENAI_SERVICE_ACCOUNT_ID: sa_example
    steps:
      - uses: actions/checkout@v4
      - run: |
          ./stirrup harness \
            --provider openai-responses \
            --base-url https://api.openai.com/v1 \
            --openai-from-github-actions \
            --prompt "..."
```

The provider registered on the OpenAI side uses the GitHub Actions
issuer (`https://token.actions.githubusercontent.com`); the mapping
pins a `sub` (or `repository`/`ref`) claim to the repository and ref the
workflow runs from. See the example at
[`examples/runconfig/openai-wif-github-actions.json`](../examples/runconfig/openai-wif-github-actions.json).

### EKS (IRSA / Pod Identity)

A Kubernetes ServiceAccount annotated with an IRSA role causes EKS to
inject `AWS_WEB_IDENTITY_TOKEN_FILE` into the Pod. The `aws-irsa` token
source reads it directly:

```json
{
  "provider": {
    "type": "openai-compatible",
    "baseUrl": "https://api.openai.com/v1",
    "credential": {
      "type": "openai-wif",
      "openaiIdentityProviderId": "idp_example",
      "openaiServiceAccountId": "sa_example",
      "tokenSource": { "type": "aws-irsa" }
    }
  }
}
```

The OpenAI provider must be registered with the EKS cluster's OIDC
issuer (`aws eks describe-cluster --query cluster.identity.oidc.issuer`)
and the mapping should match the projected token's `sub`
(`system:serviceaccount:<ns>:<sa>`). See
[`examples/runconfig/openai-wif-eks-irsa.json`](../examples/runconfig/openai-wif-eks-irsa.json).

### AKS / GKE / generic Kubernetes (projected tokens)

Any cluster that supports `serviceAccountToken` projected volumes can
mount a JWT and point the `file` token source at it. The projected
volume sets the audience the OpenAI provider expects:

```yaml
apiVersion: v1
kind: Pod
spec:
  serviceAccountName: stirrup
  volumes:
    - name: oidc-token
      projected:
        sources:
          - serviceAccountToken:
              audience: https://api.openai.com/v1
              expirationSeconds: 3600
              path: token
  containers:
    - name: stirrup
      volumeMounts:
        - name: oidc-token
          mountPath: /var/run/secrets/oidc
          readOnly: true
```

```json
{
  "credential": {
    "type": "openai-wif",
    "openaiIdentityProviderId": "idp_example",
    "openaiServiceAccountId": "sa_example",
    "tokenSource": { "type": "file", "path": "/var/run/secrets/oidc/token" }
  }
}
```

The OpenAI provider's issuer is the cluster's OIDC discovery URL
(`kubectl get --raw /.well-known/openid-configuration | jq -r .issuer`).
Self-hosted clusters whose discovery document is not publicly reachable
must upload a JWKS to the provider instead of using OIDC discovery.

On GKE with Workload Identity, the `gke-metadata` token source fetches a
Google-signed identity token from the metadata server with no projected
volume:

```json
{
  "credential": {
    "type": "openai-wif",
    "openaiIdentityProviderId": "idp_example",
    "openaiServiceAccountId": "sa_example",
    "tokenSource": { "type": "gke-metadata", "audience": "https://api.openai.com/v1" }
  }
}
```

The metadata server is reachable only from inside the cluster, so the
orchestrator must run on GKE — see
[`executors/k8s.md`](executors/k8s.md#running-the-orchestrator-in-cluster).

## Validation errors

| Error | Meaning |
|---|---|
| `openai-wif requires openaiIdentityProviderId` / `openaiServiceAccountId` | A required identifier is missing. Both come from the OpenAI dashboard when the provider and mapping are created. |
| `openai-wif requires tokenSource` | No token source was configured and none could be inferred. Set `credential.tokenSource` in `--config`, or use `--openai-from-github-actions` / the `OPENAI_IDENTITY_TOKEN*` env vars. |
| `openaiIdentityProviderId %q must be a non-empty printable identifier with no whitespace` | The value contains whitespace or control bytes — usually a copy-paste that picked up a newline. |
| `openaiSubjectTokenType %q must be an RFC 8693 token-type URN` | The override is not a `urn:ietf:params:oauth:token-type:...` value. Leave it unset to default to `jwt`. |
| `openai-wif is only supported with openai-compatible or openai-responses provider types` | The credential was paired with a non-OpenAI provider. A federated OpenAI token must not be handed to a foreign endpoint. |
| `openai-wif does not use apiKeyRef; remove it` | A static key was configured alongside WIF. Remove `apiKeyRef` (or `--api-key-ref`) — the bearer comes from the OAuth exchange. |
| `openai-wif requires Authorization: Bearer; apiKeyHeader="api-key" is mutually exclusive` | The federated token is only accepted on `Authorization: Bearer`. Leave `apiKeyHeader` empty. |
| `token exchange returned 4xx` | OpenAI rejected the exchange. Common causes: clock skew on the runtime, a `sub` that does not match any mapping, an expired subject token, or an `aud` that does not match the provider config. When OpenAI returns an `x-request-id` header it is surfaced in the error for correlation. |

## Refresh behaviour

`OpenAIWIFSource` wraps the OAuth exchange in
`oauth2.ReuseTokenSource`, so the cached access token is reused until its
`expires_in` window elapses. On every refresh the source re-reads the
underlying JWT from the `tokenSource` — projected Kubernetes tokens and
GitHub Actions OIDC tokens rotate ahead of their nominal `exp`, and the
OpenAI access token never outlives the subject token used for the
exchange. Concurrent provider requests single-flight through
`ReuseTokenSource`'s internal mutex, so the exchange endpoint sees one
in-flight exchange per source instance.

OpenAI access tokens expire after at most one hour, and the response's
`expires_in` is honoured directly rather than assumed — a subject token
with a shorter lifetime (for example a projected token with
`expirationSeconds: 600`) caps the OpenAI token's lifetime below the
one-hour ceiling. The exchange-endpoint timeout is 30 seconds.

## Notes and limits

- **Organization owner required.** Registering providers and mappings is
  an org-owner operation; there is no self-service path for other roles.
- **No audience in the exchange body.** The audience is validated from
  the subject token's `aud` claim against the provider config. Set it on
  the token source, not the credential.
- **Federated tokens cannot call the Admin API.** Admin scopes cannot be
  assigned to a mapping; use a static admin key for those operations.
- **Subject token type.** All documented providers present a JWT
  (`subject_token_type=urn:ietf:params:oauth:token-type:jwt`), which is
  the default. SPIFFE support is JWT-SVID only — X.509-SVIDs are not
  accepted.

## See also

- [`credential-federation.md`](credential-federation.md) — the
  cross-cloud federation primitive OpenAI WIF builds on.
- [`anthropic-wif.md`](anthropic-wif.md) — the sibling Anthropic WIF
  walkthrough; the two share the exchange skeleton and most of the
  runtime setup.
- [`harness/internal/credential/openai_wif.go`](../harness/internal/credential/openai_wif.go)
  — the source implementation.
- [OpenAI WIF guide](https://developers.openai.com/api/docs/guides/workload-identity-federation)
  and [token-exchange reference](https://developers.openai.com/api/reference/workload-identity-federation)
  — authoritative for the OAuth wire format.
- Per-provider OpenAI guides:
  [Kubernetes](https://developers.openai.com/api/docs/guides/workload-identity-federation/kubernetes),
  [AWS](https://developers.openai.com/api/docs/guides/workload-identity-federation/aws),
  [Azure](https://developers.openai.com/api/docs/guides/workload-identity-federation/microsoft-azure),
  [Google Cloud](https://developers.openai.com/api/docs/guides/workload-identity-federation/google-cloud),
  [GitHub Actions](https://developers.openai.com/api/docs/guides/workload-identity-federation/github-actions),
  [SPIFFE](https://developers.openai.com/api/docs/guides/workload-identity-federation/spiffe).
