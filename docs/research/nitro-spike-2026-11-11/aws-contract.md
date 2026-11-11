# Nitro Sandbox setup contract checked 2026-11-11

This note records source checks and read-only AWS calls. It is not runtime validation.

## Verified API facts

`aws --profile sandbox --region us-east-1 eks describe-cluster-versions --query 'clusterVersions[].{version:clusterVersion,status:versionStatus}' --output json` returned 1.37, 1.36, 1.35, 1.34 STANDARD_SUPPORT; 1.33 through 1.31 EXTENDED_SUPPORT. The spike selects 1.36, the minimum documented Nitro version.

`iam list-roles --path-prefix /aws-reserved/sso.amazonaws.com/` identified operator IAM role `arn:aws:iam::786874932855:role/aws-reserved/sso.amazonaws.com/eu-west-1/AWSReservedSSO_AdministratorAccess_a648af0910587e53`. This is distinct from its STS session ARN and is suitable for the EKS STANDARD admin AccessEntry.

AWS `cloudformation validate-template` accepted `/tmp/nitro-infra.json` after creation and again after the explicit admin entry was added. This checks template structure, not successful provisioning. No cloud resources were created by this research agent.

## Current source corrections and deployment constraints

- The EKS sandbox overview now says **all commercial AWS Regions**. The plan's US-only conflict has resolved in the currently retrieved docs. This is documentation evidence; the spike must still observe the RuntimeClasses in its chosen region. [Sandbox overview](https://docs.aws.amazon.com/eks/latest/userguide/sandboxing.html)
- EKS Auto Mode includes the RuntimeClasses and runtime. Custom/self-managed node groups need a custom AMI and EnclaveOptions.Enabled=true. Merely choosing an Enclaves-supported EC2 type is insufficient. [Setup](https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-isolated-setup.html)
- m5d.xlarge is listed as EC2 Nitro Sandbox supported. m5d.large and m5d.metal are excluded. Flex variants are unsupported. EKS Auto Mode further excludes FIPS AMIs, GPU/Neuron AMIs, multi-NUMA and unvalidated types; r5a, r5ad, m5ad have a temporary launch restriction. EC2 support alone is not proof Auto Mode can provision a specific size. [EC2 table](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/amazon-ec2-nitro-sandboxes.html), [EKS restrictions](https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-limitations.html)
- On SMT x86 types, core scheduling confines each physical core to one sandbox at a time. Documented advice is multiples of two requested vCPUs; fractional and one-vCPU experiments remain useful as probes. Each distinct image takes 16MiB from fixed sandbox overhead; larger guests have some kernel memory bookkeeping loss. [EKS restrictions](https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-limitations.html)

## Prepared setup

`/tmp/nitro-infra.json`: dedicated VPC 10.87.0.0/16; public /20 subnets in us-east-1a and us-east-1b; IGW and routes; cluster/node IAM roles; EKS Auto Mode 1.36 IPv4; no built-in NodePools; EC2 node access entry + AmazonEKSAutoNodePolicy; STANDARD explicit operator admin entry + AmazonEKSClusterAdminPolicy.

The template's required AdminPrincipalArn parameter avoids accidentally granting only the CloudFormation service role cluster access. Bootstrap creator admin is also enabled. Cluster DependsOn includes NodeRole so deletion retains the node role until after cluster deletion. Cluster role's implicit dependency is retained too. The template creates no NAT gateway, ECR repository, load balancer, managed node group, logging subscription, or persistent PVC.

Cluster IAM trust allows sts:AssumeRole and sts:TagSession for eks.amazonaws.com. Policies: AmazonEKSClusterPolicy, AmazonEKSComputePolicy, AmazonEKSBlockStoragePolicyV2, AmazonEKSLoadBalancingPolicy, AmazonEKSNetworkingPolicy. Node role trusts ec2.amazonaws.com and attaches AmazonEKSWorkerNodeMinimalPolicy plus AmazonEC2ContainerRegistryPullOnly. [CLI setup](https://docs.aws.amazon.com/eks/latest/userguide/automode-get-started-cli.html)

When no built-in NodePools are enabled, omit nodeRoleArn and supply an empty nodePools list. No default NodeClass is then created. Custom nodes need their own NodeClass and EC2-type AccessEntry with AmazonEKSAutoNodePolicy. [NodePool](https://docs.aws.amazon.com/eks/latest/userguide/create-node-pool.html), [NodeClass](https://docs.aws.amazon.com/eks/latest/userguide/create-node-class.html)

`/tmp/nitro-node-template.json`: replace NODE_ROLE_NAME, SUBNET_A_ID, SUBNET_B_ID, CLUSTER_SECURITY_GROUP_ID from stack outputs. NodeClass `nitro-spike`, public IP enabled, 80Gi ephemeral storage, networkPolicy DefaultAllow. NodePool restricts exact m5d.xlarge, amd64, on-demand; CPU8/memory32Gi limits; expireAfter2h; terminationGracePeriod60s; WhenEmpty consolidation after30s. This gives space for two 4-vCPU parents. Limits are scheduling controls, not a guaranteed financial cap; parent owns independent timed stack deletion.

The RuntimeClass supplies the sandbox scheduling constraint. The documented selector is eks.amazonaws.com/nitro-sandbox=true, and podFixed includes one sandbox slot plus 256Mi memory for isolated or 512Mi for confidential. Do not forge a capability label. Read actual RuntimeClass and resulting NodeClaim/Node labels as live evidence. [AMI RuntimeClass reference](https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-ami-create-update.html), [Auto scaling](https://docs.aws.amazon.com/eks/latest/userguide/autoscaling.html)

## Isolation and policy probes

Annotation sandbox.amazonaws.com/network-isolated="true" belongs on the Pod (or controller's template), not the Deployment metadata. It removes the guest NIC. Documented exec/stdin/logs/stats continue through vsock; HTTP/TCP probes and port-forward do not work. An exec probe is appropriate. This still leaves a possible parent-side vsock channel; do not call it complete isolation from a malicious node operator. [Network isolation](https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-network-isolation.html)

Auto Mode network policy needs kube-system/amazon-vpc-cni ConfigMap data enable-network-policy-controller="true". NodeClass DefaultAllow/DefaultDeny configures initial agent behavior. Preserve off/on states independently for V5/V6; a successfully applied NetworkPolicy is not proof of enforcement. CoreDNS is local on Auto Mode nodes, at service CIDR .10 for IPv4; do not assume ordinary CoreDNS Deployment selectors. [Network policy](https://docs.aws.amazon.com/eks/latest/userguide/auto-net-pol.html)

## Cleanup backstop

AWS explicitly documents that deleting an Auto Mode cluster deletes its Auto Mode nodes including EC2 Managed Instances and all load balancers. No managed node groups, Fargate profiles or EKS Capabilities should be created by this spike; those would add deletion prerequisites. DeletionProtection is false. [Delete cluster](https://docs.aws.amazon.com/eks/latest/userguide/delete-cluster.html)

Timed EventBridge Scheduler DeleteStack needs an independent CloudFormation deletion service role, or a scheduler role that also grants the downstream operations. If DeleteStack has no RoleARN and the stack has no service role, CloudFormation uses a temporary session derived from caller credentials. A scheduler role with only cloudformation:DeleteStack would then be inadequate. Keep the service role and scheduler outside the target stack until deletion is confirmed. [DeleteStack API](https://docs.aws.amazon.com/AWSCloudFormation/latest/APIReference/API_DeleteStack.html)

Verify actual EC2 termination, EBS deletion, ENIs, security groups, subnets/VPC and IAM cleanup after stack deletion. Documentation is not teardown evidence. A CFN DELETE_FAILED VPC dependency still needs inspection even after the billable nodes are gone.

## Live setup blocker: custom NodeClass tags

At 22:58:15 UTC the admitted NodeClass had ValidationSucceeded=False / CreateLaunchTemplateAuthCheckFailed / `Controller isn't authorized to call ec2:CreateLaunchTemplate`. Live IAM AmazonEKSComputePolicy default version v7 shows CreateLaunchTemplate (also RunInstances/CreateFleet) conditioned on ForAllValues:StringLike aws:TagKeys containing only eks:eks-cluster-name, eks:kubernetes-node-class-name, eks:kubernetes-node-pool-name, kubernetes.io/cluster/*. Our template's Project/Expires custom tags violated that condition. CreateTags was already allowed during creation, so adding CreateTags alone is insufficient. The narrow recommended fix is removing optional NodeClass spec.tags before nodes exist. Required AWS tags continue to identify cluster/nodeclass/nodepool. Alternative: a narrow supplementary Compute statement that also allows these two custom keys, retaining cluster RequestTag/PrincipalTag equality. Parent owns mutation and confirmation; this note establishes likely cause from policy and docs, not observed remediation success. Managed policy captured at `/tmp/nitro-network/AmazonEKSComputePolicy-v7.json`.

Source: https://docs.aws.amazon.com/eks/latest/userguide/auto-cluster-iam-role.html
