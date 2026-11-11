# Nitro spike resource and cost review

Reviewed 2026-11-11 around 23:10–23:15 UTC, AWS account `786874932855`, region `us-east-1`, profile `sandbox`. All AWS calls in this review were read-only. No Cost Explorer request was made. This is a resource-based estimate, not an observed invoice.

## Observed inventory

Cluster/stack `equestrian-nitro-20261111`; dedicated VPC `vpc-028a9b46aba88edba`. Control-plane creation time supplied by the parent run: 22:44:43 UTC.

| Instance | Type | Launch time UTC | Storage | Public IPv4 at first direct check |
|---|---|---|---|---|
| `i-0a0a2ea59026805e3` | `m5d.xlarge` | 22:59:50 | 4 + 20 GiB gp3 | One |
| `i-00de16c7f9f410e84` | `m5d.xlarge` | 23:07:09 | 4 + 20 GiB gp3 | One |

Each of the four EBS volumes had baseline 3,000 IOPS / 125 MiBps, with `DeleteOnTermination: true`. There was no extra provisioned-performance charge in the observed configuration. Instance-store cost is included in the EC2 hourly rate. Each node also had a generated IAM instance profile. Default regional reads returned no NAT gateways, EIPs, load balancers or VPC endpoints. Ordinary auto-assigned public IPv4 addresses do not appear in `describe-addresses`, so this empty EIP inventory does not eliminate the node IPv4 charge.

A second exact-ID audit around 23:15 found `i-00de16c7f9f410e84` already terminated; its two volume IDs and ENI returned explicit NotFound. The other node was still running. Both launch-template IDs recorded on the instances returned explicit NotFound individually; generated instance profiles remained present at that audit. These observations are time-bound and not final teardown evidence.

Evidence:

- `known-instances-before-teardown.json`
- `known-volumes-before-teardown.json`
- `instance-profiles-before-teardown.json`
- `inventory-known-ids-pre-teardown.json`
- `known-managed-resources.json`

## Cost estimate and forecast

Rates verified by live AWS Pricing API earlier in this run: EC2 m5d.xlarge $0.226/node-hour; EKS Auto Mode $0.02712/node-hour; gp3 $0.08/GiB-month. Published standard EKS control-plane rate $0.10/hour and public IPv4 $0.005/address-hour. No credits/discounts assumed. EBS estimates use 720 hours per month, a conservative approximation for this forecast.

| Assumed stop time | Modeled cumulative base cost |
|---|---:|
| 23:13 UTC | $0.130 |
| 23:30 UTC | $0.306 |
| Scheduled deletion begins 00:45 UTC | $1.083 |
| 01:15 UTC, allowing 30 minutes after the schedule | $1.394 |

The forecasts deliberately charge **both** recorded nodes, both public IPv4s and all 48 GiB of storage through each stop time, even though the second node was observed terminated around 23:15. They include control-plane time from the supplied creation timestamp. They exclude unmeasured transfer, tax and any new resources launched after these reads. Small ordinary transfers and a generous deletion margin remain comfortably inside the original $5 working envelope and the owner's $25 soft / $40 hard limits. Do not describe these numbers as the final AWS charge.

At two running nodes the modeled steady hourly rate is about $0.622/hour including the observed storage. The backstop is operational risk reduction, not an enforceable billing cap. Verify deletion and retain timestamps; delayed bills must be reconciled later.

## Critical inventory finding: managed resources are hidden

Default EC2 `describe-instances`, `describe-volumes` and `describe-launch-templates` returned empty lists while direct queries proved that two billable Auto Mode instances and four volumes existed. The initial `inventory-pre-teardown.json` has been relabeled `INCOMPLETE_MANAGED_RESOURCES_HIDDEN`; its empty lists are not evidence of cleanup.

AWS documents that managed-resource visibility can hide Auto Mode resources from list/filter operations without affecting operation or billing. Direct queries by known resource ID still work. The installed AWS CLI 2.34.30 model has no `IncludeManagedResources` input in its `describe-instances` skeleton, although current AWS documentation describes an `include-managed-resources` parameter. We did not change account-wide visibility or issue a custom signed query; direct IDs were sufficient for the recorded resources.

The updated `cleanup-audit.sh` reads `known-managed-resources.json` and checks each instance, volume, ENI and launch template separately. It also inventories instance profiles containing the cluster name. One deleted ID cannot mask a still-live ID because calls are per resource. Explicit NotFound is classified separately from authorization/network errors. Empty general lists still cannot prove that an unrecorded replacement node does not exist: final proof should combine all recorded-ID checks, EKS absence and dedicated-VPC absence.

Source: https://docs.aws.amazon.com/eks/latest/userguide/automode-learn-instances.html and https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/amazon-ec2-managed-instances.html

## Cleanup checks

Run the updated audit after Terraform destruction:

```sh
/tmp/nitro-evidence/cleanup-audit.sh \
  --vpc-id vpc-028a9b46aba88edba \
  --known-resources /tmp/nitro-evidence/known-managed-resources.json \
  > /tmp/nitro-evidence/inventory-final.json
```

Exit 0 means the inventory requests succeeded with the known-resource manifest; it is not an automated clean-account verdict. Review every section. Check instance profiles after cluster deletion because attached profiles can prevent NodeRole deletion. Never remove a profile merely because its name matches: confirm it is part of this run and unused. No AWS cleanup mutation was performed by this reviewer.
