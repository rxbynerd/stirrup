# Nitro spike auth and cost review — 2026-11-11

Read-only review by auth_budget agent. No AWS resources created or modified. Queries used AWS CLI 2.34.30, profile `sandbox`, region `us-east-1`. STS verified account `786874932855` and its SSO AdministratorAccess assumed role at about 22:40 UTC. Initial network-sandbox request could not reach STS; approved network access succeeded.

## Authentication

Local config uses modern `[sso-session ghostworks]`, SSO region `eu-west-1`, scope `sso:account:access`; both configured profiles point to the sandbox account. The profile default region is `eu-west-2`, so every spike AWS call should pin `us-east-1`.

Read only the expiry metadata, never printed token/key values:

- Access token expires 2026-11-11 23:32:21 UTC.
- Refresh token is present.
- OIDC client registration expires 2026-12-20 21:54:06 UTC. This is NOT the access portal session expiry.
- STS request caused CLI to acquire role credentials expiring 2026-11-11 23:40:49 UTC.

AWS documents that the modern SSO provider auto-refreshes until the extended portal session expires. Portal expiry is not discoverable from this cache metadata. Therefore 23:32 is not a guaranteed loss of access, but continued access after that time is also not proven. No login prompt is currently required. If refresh fails, user must run `aws sso login --profile sandbox`; refreshing the portal before sleep may restart the session, but changing organization session policy is unnecessary for a bounded spike and was not attempted. Permission-set session duration and access portal session duration are separate. Exporting static credentials would discard refresh; use the profile throughout.

Sources:
- https://docs.aws.amazon.com/sdkref/latest/guide/feature-sso-credentials.html
- https://docs.aws.amazon.com/sdkref/latest/guide/access-sso.html
- https://docs.aws.amazon.com/singlesignon/latest/userguide/authconcept.html

## Pricing verified by live AWS Pricing API

Linux, shared tenancy, on-demand, US East (N. Virginia); no credits or discounts assumed.

| Item | USD rate |
|---|---:|
| m5.xlarge EC2 | 0.192/hour |
| m5.xlarge Auto Mode surcharge | 0.02304/hour |
| m5d.xlarge EC2 | 0.226/hour |
| m5d.xlarge Auto Mode surcharge | 0.02712/hour |
| c6i.xlarge EC2 (alternative only) | 0.170/hour |
| gp3 storage | 0.08/GB-month |
| gp3 additional IOPS | 0.005/IOPS-month |
| gp3 additional throughput | 0.04/MiBps-month |

Raw non-secret Pricing API responses are `/tmp/nitro-price-AmazonEC2-m5.xlarge.json`, `/tmp/nitro-price-AmazonEC2-m5d.xlarge.json`, `/tmp/nitro-price-AmazonEKS-m5.xlarge.json`, `/tmp/nitro-price-AmazonEKS-m5d.xlarge.json`, `/tmp/nitro-price-ebs-gp3.json`; c6i response `/tmp/nitro-c6i-price.json`.

Published EKS standard control plane: $0.10/hour. Public IPv4: $0.005/address-hour. The m5d instance-store component is included in EC2 pricing; provisioned EBS remains additional.

For two m5d.xlarge nodes, two public IPv4 addresses, standard control plane, two hours: 2 × (2 × (0.226 + 0.02712 + 0.005) + 0.10) = $1.23248, before EBS/data/tax. Two m5.xlarge nodes produce $1.08016 on the same basis. Even 200 GB total gp3 for two hours is about $0.044 (730h-month approximation). A $5 working envelope is conservative for this bounded 2-hour small-transfer/no-NAT/no-LB setup; it is not a billing guarantee. The $25 soft budget and $40 hard ceiling should be enforced operationally through workload bounds, time limits and verified deletion, not billing alarms, which lag.

Sources:
- https://aws.amazon.com/eks/pricing/
- https://aws.amazon.com/vpc/pricing/
- https://aws.amazon.com/ebs/pricing/
- AWS Pricing `get-products` service AmazonEC2, instanceType=m5.xlarge/m5d.xlarge; service AmazonEKS, instancetype=m5.xlarge/m5d.xlarge; location=US East (N. Virginia).

## Operational guardrails

1. No default general-purpose NodePool. Restrict the custom pool to one exact tested instance type and on-demand capacity, CPU limit 8 and matching memory limit. Run demand sequentially. EKS docs call limits a hard ceiling, but upstream Karpenter documents eventual limit checking and possible rapid-scaleout overrun; do not treat CPU 8 as an independently proven two-node invariant.
2. No NAT gateway, LB, interface endpoints, paid EKS capabilities, provisioned control-plane tier or unbounded workload. Public-subnet nodes with outbound internet can use public IPs. No ingress is required for this spike.
3. Install a cloud-side scheduled teardown before billable provisioning. A local timer alone dies with the laptop/session. Cluster deletion protection must be disabled.
4. Scheduler calling CloudFormation DeleteStack needs an adequate CloudFormation service role OR underlying deletion privileges on its own execution role. DeleteStack alone is insufficient when no CFN role is associated. Keep cleanup IAM available until resource deletion completes. Ideally schedule lives outside the stack it deletes.
5. EKS docs explicitly state Auto Mode cluster deletion deletes its managed EC2 instances and load balancers. Still verify deletion completed: EKS absent, no managed instances, volumes, ENIs, IPs, NAT/LB, ECR images/repos, or stack DELETE_FAILED resources attributable to this spike. Dedicated VPC deletion is useful completion evidence.
6. Teardown owner remains parent agent, with commands/resource IDs retained for the user if blocked. Leave no recurring schedule or IAM cleanup privilege after verified teardown.

Sources:
- https://docs.aws.amazon.com/eks/latest/userguide/auto-cost-control.html
- https://karpenter.sh/docs/concepts/nodepools/index.md
- https://docs.aws.amazon.com/eks/latest/userguide/delete-cluster.html
- https://docs.aws.amazon.com/sdk-for-ruby/v3/api/Aws/CloudFormation/Types/DeleteStackInput.html

## Instance support caveats

AWS's EC2 supported family list includes m5/m5d except .large and .metal. Auto Mode has additional exclusions (FIPS, GPU/Neuron AMIs, multi-NUMA or unvalidated types). At launch r5a/r5ad/m5ad are temporarily unavailable. Family listing alone does not prove Auto Mode scheduling succeeds; record actual AMI/type/runtime of a live node.

- https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/amazon-ec2-nitro-sandboxes.html
- https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-limitations.html
