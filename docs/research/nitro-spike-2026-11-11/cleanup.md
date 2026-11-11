# Nitro spike cleanup review

**PASS for the recorded run. No surviving run-owned resource was found.** Final AWS audit completed at **2026-11-11 23:25:29 UTC** in account `786874932855`, region `us-east-1`, profile `sandbox`. Supplemental exact-ID and CloudFormation history checks completed at 23:25:47 UTC. This reviewer made read-only AWS calls; the root agent owned deletion.

Terraform reports **6 resources destroyed**, and its final state contains zero resources. All 14 resource logical IDs in `stack.json` have a CloudFormation `DELETE_COMPLETE` event. The final audit checked all 31 returned sections with no request errors. The AWS account identity was verified before each read batch.

## Cleanup evidence

| Audit section | Observed final state |
|---|---|
| `cloudformation` | Name lookup explicitly absent. ARN lookup retains the deleted stack with `DELETE_COMPLETE`. |
| `eks` | Explicit `ResourceNotFoundException`. |
| `vpcs` | Empty tagged inventory; separate exact `vpc-028a9b46aba88edba` lookup returned `InvalidVpcID.NotFound`. |
| `regional_volumes` | Empty. Each of the four recorded managed volume IDs also explicitly returned `InvalidVolume.NotFound`. |
| `regional_eips` | Empty. Neither recorded instance retains a public IPv4. |
| `regional_nat_gateways` | Empty. |
| `regional_load_balancers_v2`, `regional_load_balancers_classic` | Both empty. |
| `matching_instance_profiles` | Empty. Both previously recorded generated profiles are gone. |
| `matching_launch_templates` | Empty. Each of the two recorded template IDs also explicitly returned `InvalidLaunchTemplateId.NotFound`. |
| `named_iam_roles` | Empty, including the CFN, cleanup, ClusterRole and NodeRole roles. |
| `named_schedules` | Empty; the deletion backstop is removed after successful destruction. |
| `named_ecr_repositories`, `named_log_groups` | Both empty, matching the initial inventory. |
| `vpc_instances` | Empty; independent exact instance queries show both recorded nodes terminated. |
| `vpc_enis` | Empty; each recorded node ENI also explicitly returned `InvalidNetworkInterfaceID.NotFound`. |
| `vpc_subnets`, `vpc_security_groups`, `vpc_route_tables` | All empty. |
| `vpc_endpoints`, `vpc_internet_gateways` | Both empty. |
| `known_instance:i-0a0a2ea59026805e3`, `known_instance:i-00de16c7f9f410e84` | Both `terminated`, with no VPC, public IPv4 or attached volume references. Terminated instance history is expected. |
| Four `known_volume:*`, two `known_eni:*`, two `known_launch_template:*` sections | All explicit NotFound. No permission or network errors were classified as absence. |

Empty default EC2 lists alone would be inadequate: this account hides Auto Mode managed resources in list/filter operations. The recorded-ID results, explicit EKS absence, exact dedicated-VPC absence and complete CFN deletion history provide the cleanup proof together. The initial regional EIP/NAT/load-balancer inventories were also empty; no new visible regional storage or network residue appeared.

## Confirmed times

| Event | UTC on 2026-11-11 | Evidence |
|---|---|---|
| Cluster earliest CFN creation request | 22:44:40.148 | `cleanup-extra-final.json`, stack events |
| Cluster resource creation initiated | 22:44:43.509 | Same |
| Primary node launch | 22:59:50 | `known-instances-before-teardown.json` |
| Secondary node launch | 23:07:09 | Same |
| Secondary node shutdown initiated | 23:11:45 | `cleanup-observation-during-destroy.json`, state transition reason |
| Secondary already terminated; its volumes/ENI absent | By 23:13:05 | `inventory-known-ids-pre-teardown.json` |
| Primary node shutdown initiated | 23:19:32 | `cleanup-observation-during-destroy.json`, state transition reason |
| Both nodes observed terminated | Read batch began 23:20:51 | Same; timestamp denotes the read batch, not exact termination |
| Stack deletion began | 23:20:16.810 | `cleanup-extra-final.json`, stack event |
| EKS cluster deletion complete | 23:24:24.428 | Same |
| VPC deletion complete | 23:24:46.379 | Same |
| Stack deletion complete | 23:24:46.884 | Same |
| Final 31-section audit complete | 23:25:29.305 | `inventory-final.json` |

The stack's `DeletionTime` field is 23:20:16.810, matching **deletion initiation**. Use the `DELETE_COMPLETE` event at 23:24:46.884 when reporting completion. The earlier cost note's approximate 23:15 secondary-node cleanup observation is superseded by the raw 23:13:05 audit.

## Conservative cost model

**Modeled base cost: $0.25925, rounded to $0.26.** This deliberately charges both recorded nodes, both public IPv4s and all 48 GiB gp3 storage through final audit completion, although the nodes and secondary storage were already gone earlier. It also charges the control plane from the earliest CFN creation request through the final audit, past its confirmed deletion. This is a conservative model of the recorded resources, not an observed AWS bill.

| Component | Modeled cost USD |
|---|---:|
| EC2 m5d.xlarge | 0.165709 |
| EKS Auto Mode node fee | 0.019885 |
| Public IPv4 | 0.003666 |
| gp3, 48 GiB total | 0.001955 |
| EKS control plane | 0.068032 |
| Total | **0.259247** |

Inputs inherited from `cost-review.md`: EC2 $0.226/node-hour, Auto Mode $0.02712/node-hour, IPv4 $0.005/address-hour, gp3 $0.08/GiB-month, control plane $0.10/hour; storage uses 720 hours/month. The node-hour total is 0.733225 and the control-plane duration is 0.680321 hours. The machine-readable calculation is `cleanup-cost-final.json`.

As an additional buffer, charging one full hour for each recorded node, its IPv4 and storage, plus one control-plane hour gives **$0.62158**, still below the $5 working envelope and the authorized $25 soft / $40 hard limits. Neither estimate is an enforceable spending cap. Unmeasured transfer, tax and unrelated account spending are excluded. No Cost Explorer or invoice reconciliation was performed, and this reviewer did not repeat the earlier price lookup. Final billed cost remains unverified until AWS billing data settles.

## Review limits and retained files

The audit is scoped to this recorded run and region. It is not a whole-account clean bill of health. A finite known-ID manifest cannot enumerate an unrecorded managed replacement, which is why exact VPC absence and EKS/CFN deletion were independently required. Shared AWS service-linked IAM roles were outside run ownership. No remaining cleanup action was identified.

Primary evidence: `inventory-final.json`, `cleanup-extra-final.json`, `cleanup-observation-during-destroy.json`, `cleanup-cost-final.json`, `terraform-destroy.txt`, `known-managed-resources.json`, `inventory-known-ids-pre-teardown.json`, `known-instances-before-teardown.json`, `known-volumes-before-teardown.json`, `initial-inventory.json` and the empty final Terraform state in `/tmp/equestrian-infra-lab/nitro-spike/terraform.tfstate`.
