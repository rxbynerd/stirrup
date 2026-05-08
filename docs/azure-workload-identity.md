# Azure Entra ID Workload Identity Federation

Stirrup can authenticate to Azure OpenAI / Foundry without a static
client secret or expiring Bearer token by federating an OIDC identity
proof from the runtime (AKS projected token, GitHub Actions OIDC,
EKS IRSA, Azure IMDS) into a short-lived Microsoft Entra access token
via the OAuth2 `client_credentials` grant with a JWT client assertion.

The flow is:

```
TokenSource (OIDC JWT)  →  AzureWorkloadIdentitySource  →  Resolved.BearerToken
                              POST login.microsoftonline.com/{tenant}/oauth2/v2.0/token
                              client_assertion_type=jwt-bearer
```

The resulting access token is scoped to the Azure OpenAI / Cognitive
Services audience (`https://cognitiveservices.azure.com/.default`) by
default and refreshed automatically by `oauth2.ReuseTokenSource` —
adapters call `BearerToken` on every provider request without re-hitting
the Entra token endpoint.

For the broader cross-cloud federation story (Vertex AI Gemini, AWS
Bedrock), see [`docs/credential-federation.md`](./credential-federation.md).

## Section 1 — Running on AKS with Entra ID Workload Identity

This walkthrough uses AKS Workload Identity (the supported successor to
Pod Identity). Reference: <https://azure.github.io/azure-workload-identity/docs/>.

### 1. Azure-side IAM

1. **Register an Application** in the target Entra tenant. Note the
   Application (client) ID and Directory (tenant) ID — both are UUIDs
   that go directly into the RunConfig:

   ```sh
   az ad app create --display-name stirrup-azure-openai
   APP_ID=$(az ad app list --display-name stirrup-azure-openai --query "[0].appId" -o tsv)
   TENANT_ID=$(az account show --query tenantId -o tsv)
   ```

2. **Create a service principal** for the App so it can hold role
   assignments:

   ```sh
   az ad sp create --id "$APP_ID"
   SP_ID=$(az ad sp show --id "$APP_ID" --query id -o tsv)
   ```

3. **Add a federated identity credential** pointing at the AKS OIDC
   issuer and the service-account subject the workload pod will use:

   ```sh
   AKS_OIDC_ISSUER=$(az aks show --name <cluster> --resource-group <rg> \
     --query "oidcIssuerProfile.issuerUrl" -o tsv)

   cat > federated-cred.json <<EOF
   {
     "name": "stirrup-aks",
     "issuer": "$AKS_OIDC_ISSUER",
     "subject": "system:serviceaccount:stirrup:stirrup",
     "audiences": ["api://AzureADTokenExchange"]
   }
   EOF
   az ad app federated-credential create --id "$APP_ID" --parameters federated-cred.json
   ```

4. **Grant the App the `Cognitive Services OpenAI User` role** on the
   Azure OpenAI resource:

   ```sh
   AOAI_RESOURCE=$(az cognitiveservices account show \
     --name <aoai-resource> --resource-group <rg> --query id -o tsv)

   az role assignment create \
     --assignee-object-id "$SP_ID" \
     --assignee-principal-type ServicePrincipal \
     --role "Cognitive Services OpenAI User" \
     --scope "$AOAI_RESOURCE"
   ```

### 2. Kubernetes side

1. **Enable Workload Identity on the cluster** (one-time per cluster):

   ```sh
   az aks update --resource-group <rg> --name <cluster> \
     --enable-oidc-issuer --enable-workload-identity
   ```

2. **Annotate the service account** with the App's client ID and label
   the namespace:

   ```yaml
   apiVersion: v1
   kind: ServiceAccount
   metadata:
     name: stirrup
     namespace: stirrup
     annotations:
       azure.workload.identity/client-id: "<APP_ID>"
   ```

3. **Label the workload pod** so the mutating admission webhook
   injects the projected token volume:

   ```yaml
   metadata:
     labels:
       azure.workload.identity/use: "true"
   spec:
     serviceAccountName: stirrup
   ```

   The injected volume mounts at
   `/var/run/secrets/azure/tokens/azure-identity-token`.

### 3. RunConfig

```json
{
  "provider": {
    "type": "openai-compatible",
    "baseUrl": "https://<aoai-resource>.openai.azure.com/openai/v1",
    "queryParams": {"api-version": "preview"},
    "credential": {
      "type": "azure-workload-identity",
      "azureTenantId": "<TENANT_ID>",
      "azureClientId": "<APP_ID>",
      "tokenSource": {
        "type": "file",
        "path": "/var/run/secrets/azure/tokens/azure-identity-token"
      }
    }
  }
}
```

A complete fixture lives at
[`examples/runconfig/azure-openai-wif-aks.json`](../examples/runconfig/azure-openai-wif-aks.json).

### 4. Verify

After deploying the pod, tail logs and confirm the harness emits no
auth errors at boot:

```sh
kubectl logs -n stirrup -l app=stirrup --tail=200 | jq 'select(.level=="ERROR")'
```

A successful exchange surfaces no error events; the first provider
request will return a 200 from Azure OpenAI. If you see
`Azure WIF: token endpoint returned 401` with a `correlation_id`, the
federated identity credential is misconfigured — most commonly a
`subject` mismatch between the federated credential and the
`system:serviceaccount:<ns>:<sa>` of the pod.

## Section 2 — GitHub Actions to Azure OpenAI

GitHub Actions can mint OIDC tokens that Entra accepts as a federated
identity proof, no static secret in the workflow. Useful for
short-lived eval runs, scheduled regressions, and bot pull requests
against an Azure-hosted model.

### 1. Workflow

```yaml
permissions:
  id-token: write
  contents: read

jobs:
  run-stirrup:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: |
          ./stirrup harness --config azure-openai-wif-github-actions.json \
            --prompt "Run the eval suite"
```

The `id-token: write` permission is mandatory — it injects
`ACTIONS_ID_TOKEN_REQUEST_URL` and `ACTIONS_ID_TOKEN_REQUEST_TOKEN`,
which the `github-actions-oidc` token source reads at exchange time.

### 2. Azure-side IAM

Same App Registration + role assignment shape as Section 1; the
federated identity credential differs:

```sh
cat > federated-cred-gha.json <<EOF
{
  "name": "stirrup-gha",
  "issuer": "https://token.actions.githubusercontent.com",
  "subject": "repo:<owner>/<repo>:ref:refs/heads/main",
  "audiences": ["api://AzureADTokenExchange"]
}
EOF
az ad app federated-credential create --id "$APP_ID" --parameters federated-cred-gha.json
```

The `subject` is matched exactly against the `sub` claim GHA puts in
the OIDC token. Common shapes:

| Workflow trigger | Subject |
|---|---|
| Push to a branch | `repo:owner/repo:ref:refs/heads/<branch>` |
| Tag | `repo:owner/repo:ref:refs/tags/<tag>` |
| Pull request | `repo:owner/repo:pull_request` |
| Specific environment | `repo:owner/repo:environment:<env>` |

Create separate federated identity credentials for every subject your
workflows need; Azure does not support wildcards on the `sub` claim.

### 3. RunConfig

```json
{
  "provider": {
    "type": "openai-compatible",
    "baseUrl": "https://<aoai-resource>.openai.azure.com/openai/v1",
    "queryParams": {"api-version": "preview"},
    "credential": {
      "type": "azure-workload-identity",
      "azureTenantId": "<TENANT_ID>",
      "azureClientId": "<APP_ID>",
      "tokenSource": {
        "type": "github-actions-oidc",
        "audience": "api://AzureADTokenExchange"
      }
    }
  }
}
```

A complete fixture lives at
[`examples/runconfig/azure-openai-wif-github-actions.json`](../examples/runconfig/azure-openai-wif-github-actions.json).

The `tokenSource.audience` here is the audience claim *requested* on
the OIDC token — it must match the `audiences[0]` value of the
federated identity credential on the Azure side.

## Mutual exclusion with `apiKeyHeader: "api-key"`

Entra access tokens are accepted only on the `Authorization: Bearer`
header. Static Azure OpenAI keys go on the `api-key` header. Mixing the
two — `credential.type=azure-workload-identity` alongside
`provider.apiKeyHeader=api-key` — is rejected at config-load time
because the resulting request would 401 from Azure with no useful
diagnostic. Leave `apiKeyHeader` empty (the default) when using WIF.

## References

- Microsoft, *Workload Identity Federation*: <https://learn.microsoft.com/en-us/entra/workload-id/workload-identity-federation>
- Azure AD Workload Identity for AKS: <https://azure.github.io/azure-workload-identity/docs/>
- Microsoft, *Authenticate to Azure OpenAI with Microsoft Entra ID*: <https://learn.microsoft.com/en-us/azure/ai-services/openai/how-to/managed-identity>
- GitHub Actions OIDC: <https://docs.github.com/en/actions/deployment/security-hardening-your-deployments/about-security-hardening-with-openid-connect>
