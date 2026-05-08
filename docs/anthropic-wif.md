# Anthropic Workload Identity Federation

Anthropic's [Workload Identity Federation](https://platform.claude.com/docs/en/manage-claude/workload-identity-federation)
lets a workload exchange an OIDC JWT — issued by its own IdP (GitHub
Actions, EKS IRSA, AKS workload identity, GKE metadata, generic
projected k8s tokens, SPIFFE) — for a short-lived Anthropic access
token bound to a service account. No static API keys, no rotation
toil, no `ANTHROPIC_API_KEY` to leak.

This document is the operator walkthrough. The architectural shape
sits alongside [`credential-federation.md`](credential-federation.md);
the two complement each other.

## Why use it

- **No static API key in your runtime.** WIF replaces the
  `ANTHROPIC_API_KEY` env var with a per-run, IdP-bound bearer token
  that expires in minutes. The harness never holds a long-lived
  credential.
- **Audit trail per workload.** Federation rules embed the
  service-account ID and the workspace into the issued token. The
  Anthropic Console's authentication-history page surfaces each
  exchange by `request_id`, IdP issuer, and JWT subject.
- **Granular scopes.** Each federation rule pins
  `token_lifetime_seconds` (60–86400) and the OAuth scope
  (`workspace:developer` at launch).

## Console-side setup

The Anthropic Console manages three resources — see Anthropic's
[WIF reference](https://platform.claude.com/docs/en/manage-claude/wif-reference)
for the click-by-click UI.

1. **Create a service account** in your Anthropic organization. The
   resulting ID is `svac_...` and goes into RunConfig as
   `serviceAccountId`.
2. **Register a federation issuer** — an OIDC IdP your workloads use.
   Configure it with the IdP's `iss` claim value and a JWKS source
   (`discovery` for IdPs that publish a `.well-known/openid-configuration`,
   `explicit_url` for ones that don't, `inline` for static keys). The
   resulting ID is `fdis_...`.
3. **Create a federation rule** that bridges the issuer to the service
   account. Specify a `subject_prefix` matching the JWT's `sub` claim
   (e.g. `repo:owner/repo:ref:refs/heads/main` for a GHA workflow,
   `arn:aws:sts::ACCOUNT:assumed-role/ROLE/SESSION` for an IRSA
   role), the workspace IDs the rule applies to, the OAuth scope, and
   `token_lifetime_seconds`. The resulting ID is `fdrl_...`.

The rule's `workspaces` list determines whether `workspaceId` is
required at exchange time: if the rule is bound to exactly one
workspace, the field is optional in RunConfig; if it covers more than
one (or all), the field is required. Anthropic accepts the literal
string `default` alongside `wrkspc_...` identifiers.

## Stirrup-side configuration

The minimal RunConfig is:

```json
{
  "provider": {
    "type": "anthropic",
    "credential": {
      "type": "anthropic-wif",
      "federationRuleId": "fdrl_example",
      "organizationId": "11111111-1111-1111-1111-111111111111",
      "serviceAccountId": "svac_example",
      "workspaceId": "default",
      "tokenSource": { "type": "github-actions-oidc", "audience": "https://api.anthropic.com" }
    }
  }
}
```

The four federation identifiers are non-secret per Anthropic's docs
(safe to commit or bake into a container image). The `tokenSource`
block selects which IdP issues the JWT — see the runtime walkthroughs
below.

`apiKeyRef` is **not** set. Anthropic WIF and static API keys are
mutually exclusive on the same provider; the validator rejects the
combination because the SDK precedence chain would silently shadow
WIF with `ANTHROPIC_API_KEY` if both were present.

### Flags + env-var fallbacks

The CLI surface accepts four ID flags plus an opt-in for the GitHub
Actions runtime:

```sh
./stirrup harness \
  --anthropic-federation-rule-id fdrl_example \
  --anthropic-organization-id 11111111-1111-1111-1111-111111111111 \
  --anthropic-service-account-id svac_example \
  --anthropic-workspace-id default \
  --anthropic-from-github-actions \
  --prompt "..."
```

Each flag has an env-var fallback honouring the
[Anthropic SDK env contract](https://platform.claude.com/docs/en/manage-claude/wif-reference#environment-variables):

| Flag | Env fallback |
|---|---|
| `--anthropic-federation-rule-id` | `ANTHROPIC_FEDERATION_RULE_ID` |
| `--anthropic-organization-id` | `ANTHROPIC_ORGANIZATION_ID` |
| `--anthropic-service-account-id` | `ANTHROPIC_SERVICE_ACCOUNT_ID` |
| `--anthropic-workspace-id` | `ANTHROPIC_WORKSPACE_ID` |

Two more env vars infer the token-source block when `--config` does
not pin one:

- `ANTHROPIC_IDENTITY_TOKEN_FILE=/path/to/jwt` →
  `tokenSource: {type: file, path: <path>}`
- `ANTHROPIC_IDENTITY_TOKEN=<jwt>` →
  `tokenSource: {type: env, envVar: ANTHROPIC_IDENTITY_TOKEN}`

The presence of GitHub Actions' `ACTIONS_ID_TOKEN_REQUEST_URL` does
**not** auto-select the GHA token source. The flag
`--anthropic-from-github-actions` is the explicit opt-in; silent IdP
selection from env presence is rejected per issue #117 risk #5
("silent IdP selection makes credential bugs unfixable").

## Runtime walkthroughs

### GitHub Actions

The workflow must declare:

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
      ANTHROPIC_FEDERATION_RULE_ID: fdrl_example
      ANTHROPIC_ORGANIZATION_ID: 11111111-1111-1111-1111-111111111111
      ANTHROPIC_SERVICE_ACCOUNT_ID: svac_example
      ANTHROPIC_WORKSPACE_ID: default
    steps:
      - uses: actions/checkout@v4
      - run: |
          ./stirrup harness \
            --anthropic-from-github-actions \
            --prompt "..."
```

The federation rule on the Anthropic side should pin
`subject_prefix` to the repository and ref the workflow runs from,
e.g. `repo:owner/repo:ref:refs/heads/main`. See the
[GitHub Actions WIF guide](https://platform.claude.com/docs/en/manage-claude/wif-providers/github-actions).

### EKS IRSA

A Kubernetes ServiceAccount annotated with the IRSA role:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: stirrup
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::ACCOUNT:role/stirrup-runtime
```

EKS injects `AWS_WEB_IDENTITY_TOKEN_FILE` into the Pod. RunConfig:

```json
{
  "provider": {
    "type": "anthropic",
    "credential": {
      "type": "anthropic-wif",
      "federationRuleId": "fdrl_example",
      "organizationId": "11111111-1111-1111-1111-111111111111",
      "serviceAccountId": "svac_example",
      "tokenSource": { "type": "aws-irsa" }
    }
  }
}
```

The `aws-irsa` token source reads
`$AWS_WEB_IDENTITY_TOKEN_FILE` directly. Anthropic's federation issuer
must be configured with the IRSA OIDC endpoint
(`https://oidc.eks.<region>.amazonaws.com/id/<cluster-id>`) and the
rule's `subject_prefix` should match the assumed-role ARN.

### AKS Workload Identity

AKS Workload Identity injects an OIDC JWT into Pods via the
`/var/run/secrets/azure/tokens/azure-identity-token` projected
volume. The simplest path is the `file` token source pointed at that
volume, but the `azure-imds` source also works for managed identities
that issue tokens via IMDS.

```json
{
  "provider": {
    "type": "anthropic",
    "credential": {
      "type": "anthropic-wif",
      "federationRuleId": "fdrl_example",
      "organizationId": "11111111-1111-1111-1111-111111111111",
      "serviceAccountId": "svac_example",
      "tokenSource": {
        "type": "file",
        "path": "/var/run/secrets/azure/tokens/azure-identity-token"
      }
    }
  }
}
```

The federation issuer on the Anthropic side is the AKS cluster's
`AZURE_AUTHORITY_HOST` issuer URL.

### Generic Kubernetes (projected service-account tokens)

For any k8s cluster (not EKS / AKS / GKE) that supports
`serviceAccountToken` projected volumes:

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
              audience: anthropic.com
              expirationSeconds: 3600
              path: token
  containers:
    - name: stirrup
      volumeMounts:
        - name: oidc-token
          mountPath: /var/run/secrets/oidc
          readOnly: true
```

Then in RunConfig:

```json
{
  "credential": {
    "type": "anthropic-wif",
    "federationRuleId": "fdrl_example",
    "organizationId": "11111111-1111-1111-1111-111111111111",
    "serviceAccountId": "svac_example",
    "tokenSource": { "type": "file", "path": "/var/run/secrets/oidc/token" }
  }
}
```

The federation issuer on the Anthropic side is the cluster's OIDC
discovery URL, exposed via `kubectl get --raw /.well-known/openid-configuration`.

## Validation errors

| Error | Meaning |
|---|---|
| `apiKeyRef must not be set when credential.type is "anthropic-wif"` | Risk #4: a leftover static API key would silently shadow federation. Remove `apiKeyRef` (or `--api-key-ref`) — under WIF the bearer token comes from the OAuth exchange. |
| `anthropic-wif requires federationRuleId, organizationId, and serviceAccountId` | One of the three identifiers is missing. The fourth field, `workspaceId`, is conditional — required when the federation rule covers more than one workspace. |
| `federationRuleId %q does not match %q` | The ID format is wrong. Federation rules are `fdrl_...`, service accounts are `svac_...`, workspaces are `wrkspc_...` (or the literal `default`). |
| `--anthropic-* federation flags imply credential.type=anthropic-wif, but credential.type is already %q` | The `--config` already named a different credential type and the operator layered WIF flags on top. Pick one — silently rewriting the explicit type would hide intent. |
| `token exchange returned 400 (request_id=req_...)` | Anthropic rejected the assertion. Common causes: clock skew (Anthropic applies 30s; check NTP on the runtime), `subject_prefix` mismatch, expired JWT, or the federation rule was archived. The `request_id` is the lookup key for the [Console authentication-history page](https://platform.claude.com/settings/workload-identity-federation?tab=history). |

## Refresh behaviour

`AnthropicWIFSource` wraps the OAuth exchange in
`oauth2.ReuseTokenSource`, so the cached access token is reused until
its `expires_in` window elapses. On every refresh the source re-reads
the underlying JWT from the `tokenSource` — projected k8s tokens and
GHA OIDC tokens rotate ahead of their nominal `exp`, and Anthropic's
docs require the assertion to be unexpired. Concurrent provider
requests under contention single-flight through `ReuseTokenSource`'s
internal mutex, so the OAuth endpoint sees one in-flight exchange
per source instance.

The exchange-endpoint timeout is 30 seconds. Stream timeouts on the
Anthropic Messages API are independent (120s for the streaming
response). A long-running stream that outlives its access token is
not currently a concern because Anthropic streams typically complete
in under five minutes; document this if you tune `token_lifetime_seconds`
below 300 for any reason.

## Risks and mitigations

These are condensed from the issue body — the full discussion is at
[issue #117](https://github.com/rxbynerd/stirrup/issues/117).

1. **`/v1/oauth/token` is on the auth path.** Bugs here surface as
   400s with consolidated `invalid_grant` errors (Anthropic
   intentionally collapses signature failures, claim-mismatch
   failures, and archived-rule errors into one code to prevent
   enumeration). The credential source surfaces `request_id` in
   the error so operators can correlate with the
   [Console authentication-history page](https://platform.claude.com/settings/workload-identity-federation?tab=history).
   The JWT assertion is never logged.
2. **`ANTHROPIC_API_KEY` shadowing.** The Anthropic SDK precedence
   chain puts `ANTHROPIC_API_KEY` above federation. Stirrup fails
   closed: `apiKeyRef` set with `credential.type=anthropic-wif` is a
   validation error, and the CLI's default
   `--api-key-ref=secret://ANTHROPIC_API_KEY` is silently cleared
   when WIF flags are present (no operator intent expressed).
3. **GHA OIDC tokens expire in 5 minutes.** Stirrup fetches the JWT
   lazily on every refresh, not eagerly at startup, so a workflow
   with a long setup before the first agent turn still works. The
   first exchange must happen within 5 minutes of the JWT being
   fetched.
4. **Clock skew.** Anthropic applies 30s skew on `exp`/`nbf`/`iat`.
   Containers and CI runners with drifted clocks will see
   `invalid_grant` from the server. Drift is operator-fixable, not
   transient — the credential source does not retry.
5. **JWKS cache during IdP key rotation.** Anthropic caches the
   JWKS for ~1 minute after fetch. If your IdP rotates and
   immediately signs with the new key, exchanges fail until the
   cache refreshes. This is operator-facing; stirrup cannot
   mitigate it.
6. **No profile-file support.** The Anthropic SDK supports a
   profile file at `<config_dir>/configs/<name>.json`; stirrup does
   not. Use `--config` + env-var fallbacks instead. Tracked as an
   intentional non-goal.

## See also

- [`credential-federation.md`](credential-federation.md) — the
  cross-cloud federation primitive that Anthropic WIF builds on.
- [`harness/internal/credential/anthropic_wif.go`](../harness/internal/credential/anthropic_wif.go)
  — the source implementation.
- [Anthropic WIF reference](https://platform.claude.com/docs/en/manage-claude/wif-reference)
  — authoritative for the OAuth wire format and error codes.
- [GitHub Actions guide](https://platform.claude.com/docs/en/manage-claude/wif-providers/github-actions),
  [AWS guide](https://platform.claude.com/docs/en/manage-claude/wif-providers/aws),
  [Azure guide](https://platform.claude.com/docs/en/manage-claude/wif-providers/azure),
  [Kubernetes guide](https://platform.claude.com/docs/en/manage-claude/wif-providers/kubernetes).
