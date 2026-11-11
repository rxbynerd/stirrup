#!/usr/bin/env bash
# Read-only inventory. Pass the recorded VPC ID even after it has been deleted.
set -euo pipefail
exec python3 - "$0" "$@" <<'PY'
import argparse, concurrent.futures, datetime, json, pathlib, subprocess, sys

script_directory = pathlib.Path(sys.argv.pop(1)).resolve().parent
p = argparse.ArgumentParser(description='Read-only audit of the 2026-11-11 Nitro spike. No AWS mutation commands.')
p.add_argument('--profile', default='sandbox')
p.add_argument('--account', default='786874932855')
p.add_argument('--name', default='equestrian-nitro-20261111')
p.add_argument('--known-resources', default=str(script_directory / 'known-managed-resources.json'), help='Recorded ID manifest for hidden managed resources')
p.add_argument('--vpc-id', default='', help='Recorded dedicated VPC ID, retained even after VPC deletion')
a = p.parse_args()
known_path = pathlib.Path(a.known_resources)
known = json.loads(known_path.read_text()) if known_path.exists() else {}
if known and known.get('run') != a.name:
    p.error('Known-resource manifest belongs to another run')
region = 'us-east-1'
project = 'equestrian-nitro-spike'
base = ['aws', '--profile', a.profile, '--region', region, '--no-cli-pager', '--cli-connect-timeout', '10', '--cli-read-timeout', '30']

def call(label, args, query=None, absent_ok=False):
    cmd = base + args
    if query:
        cmd += ['--query', query]
    cmd += ['--output', 'json']
    try:
        proc = subprocess.run(cmd, capture_output=True, text=True, timeout=90)
    except subprocess.TimeoutExpired:
        return {'section': label, 'status': 'ERROR', 'error': 'Read-only request timed out'}
    if proc.returncode:
        err = proc.stderr.strip()
        absent = absent_ok and any(s in err for s in ['ResourceNotFoundException', 'does not exist', 'NoSuchEntity', 'InvalidInstanceID.NotFound', 'InvalidVolume.NotFound', 'InvalidNetworkInterfaceID.NotFound', 'InvalidLaunchTemplateId.NotFound'])
        return {'section': label, 'status': 'ABSENT' if absent else 'ERROR', 'error': err}
    try:
        data = json.loads(proc.stdout)
    except json.JSONDecodeError:
        return {'section': label, 'status': 'ERROR', 'error': 'AWS output was not JSON'}
    return {'section': label, 'status': 'OK', 'data': data}

identity = call('identity', ['sts', 'get-caller-identity'], '{Account:Account}')
if identity['status'] != 'OK' or identity['data'].get('Account') != a.account:
    print(json.dumps({'audit': 'REFUSED', 'reason': 'Could not verify expected AWS account', 'identity': identity}, indent=2))
    sys.exit(2)

stack = call('cloudformation', ['cloudformation', 'describe-stacks', '--stack-name', a.name],
             'Stacks[].{Id:StackId,Name:StackName,Status:StackStatus,Role:RoleARN,Outputs:Outputs}', True)
vpc = a.vpc_id or known.get('vpc_id', '')
if not vpc and stack['status'] == 'OK':
    for item in stack['data']:
        for out in item.get('Outputs') or []:
            if out['OutputKey'] == 'VpcId':
                vpc = out['OutputValue']

jobs = [
    ('eks', ['eks', 'describe-cluster', '--name', a.name], 'cluster.{Name:name,Status:status,Vpc:resourcesVpcConfig.vpcId}', True),
    ('vpcs', ['ec2', 'describe-vpcs', '--filters', f'Name=tag:Project,Values={project}'], 'Vpcs[].{Id:VpcId,State:State,Tags:Tags}', False),
    # Regional storage/address inventories deliberately include unrelated entries:
    # unattached orphan resources can lose the VPC link and need baseline comparison.
    ('regional_volumes', ['ec2', 'describe-volumes'], 'Volumes[].{Id:VolumeId,State:State,SizeGiB:Size,Type:VolumeType,IOPS:Iops,Throughput:Throughput,Created:CreateTime,Attachments:Attachments[].{Instance:InstanceId,State:State},Tags:Tags}', False),
    ('regional_eips', ['ec2', 'describe-addresses'], 'Addresses[].{Allocation:AllocationId,PublicIP:PublicIp,Instance:InstanceId,ENI:NetworkInterfaceId,Association:AssociationId,Tags:Tags}', False),
    ('regional_nat_gateways', ['ec2', 'describe-nat-gateways'], 'NatGateways[].{Id:NatGatewayId,Vpc:VpcId,State:State,Tags:Tags}', False),
    ('regional_load_balancers_v2', ['elbv2', 'describe-load-balancers'], 'LoadBalancers[].{Name:LoadBalancerName,Arn:LoadBalancerArn,Vpc:VpcId,State:State.Code,Type:Type}', False),
    ('regional_load_balancers_classic', ['elb', 'describe-load-balancers'], 'LoadBalancerDescriptions[].{Name:LoadBalancerName,Vpc:VPCId}', False),
    ('matching_instance_profiles', ['iam', 'list-instance-profiles'], f"InstanceProfiles[?contains(InstanceProfileName, '{a.name}')].{{Name:InstanceProfileName,Arn:Arn,Roles:Roles[].RoleName}}", False),
    ('matching_launch_templates', ['ec2', 'describe-launch-templates'], f"LaunchTemplates[?contains(LaunchTemplateName, '{a.name}')].{{Id:LaunchTemplateId,Name:LaunchTemplateName,Tags:Tags}}", False),
    ('named_iam_roles', ['iam', 'list-roles'], f"Roles[?starts_with(RoleName, '{a.name}')].{{Name:RoleName,Arn:Arn}}", False),
    ('named_schedules', ['scheduler', 'list-schedules', '--name-prefix', a.name], 'Schedules[].{Name:Name,State:State,Group:GroupName,Arn:Arn}', False),
    ('named_ecr_repositories', ['ecr', 'describe-repositories'], f"repositories[?contains(repositoryName, '{a.name}') || contains(repositoryName, 'nitro')].{{Name:repositoryName,Arn:repositoryArn,Created:createdAt}}", False),
    ('named_log_groups', ['logs', 'describe-log-groups', '--log-group-name-prefix', f'/aws/eks/{a.name}'], 'logGroups[].{Name:logGroupName,Bytes:storedBytes,Retention:retentionInDays}', False),
]
if vpc:
    filt = ['--filters', f'Name=vpc-id,Values={vpc}']
    jobs += [
        ('vpc_instances', ['ec2', 'describe-instances'] + filt, 'Reservations[].Instances[].{Id:InstanceId,State:State.Name,Type:InstanceType,Launch:LaunchTime,PublicIP:PublicIpAddress,Volumes:BlockDeviceMappings[].Ebs.VolumeId,Tags:Tags}', False),
        ('vpc_enis', ['ec2', 'describe-network-interfaces'] + filt, 'NetworkInterfaces[].{Id:NetworkInterfaceId,Status:Status,Description:Description,Type:InterfaceType,Tags:TagSet}', False),
        ('vpc_subnets', ['ec2', 'describe-subnets'] + filt, 'Subnets[].{Id:SubnetId,State:State}', False),
        ('vpc_security_groups', ['ec2', 'describe-security-groups'] + filt, 'SecurityGroups[].{Id:GroupId,Name:GroupName}', False),
        ('vpc_route_tables', ['ec2', 'describe-route-tables'] + filt, 'RouteTables[].{Id:RouteTableId}', False),
        ('vpc_endpoints', ['ec2', 'describe-vpc-endpoints'] + filt, 'VpcEndpoints[].{Id:VpcEndpointId,State:State,Type:VpcEndpointType}', False),
        ('vpc_internet_gateways', ['ec2', 'describe-internet-gateways', '--filters', f'Name=attachment.vpc-id,Values={vpc}'], 'InternetGateways[].{Id:InternetGatewayId,Attachments:Attachments}', False),
    ]
else:
    jobs.append(('tagged_instances', ['ec2', 'describe-instances', '--filters', f'Name=tag:eks:eks-cluster-name,Values={a.name}'], 'Reservations[].Instances[].{Id:InstanceId,State:State.Name,Type:InstanceType,Vpc:VpcId,Tags:Tags}', False))

# Direct ID requests expose resources hidden from default EC2 list operations.
for kind, operation, flag, query in [
    ('instance', 'describe-instances', '--instance-ids', 'Reservations[].Instances[].{Id:InstanceId,State:State.Name,Type:InstanceType,Vpc:VpcId,PublicIP:PublicIpAddress,Volumes:BlockDeviceMappings[].Ebs.VolumeId}'),
    ('volume', 'describe-volumes', '--volume-ids', 'Volumes[].{Id:VolumeId,State:State,SizeGiB:Size,Attachments:Attachments}'),
    ('eni', 'describe-network-interfaces', '--network-interface-ids', 'NetworkInterfaces[].{Id:NetworkInterfaceId,Status:Status,Vpc:VpcId}'),
    ('launch_template', 'describe-launch-templates', '--launch-template-ids', 'LaunchTemplates[].{Id:LaunchTemplateId,Name:LaunchTemplateName}')
]:
    for resource_id in known.get(kind + '_ids', []):
        jobs.append((f'known_{kind}:{resource_id}', ['ec2', operation, flag, resource_id], query, True))

with concurrent.futures.ThreadPoolExecutor(max_workers=5) as pool:
    results = list(pool.map(lambda j: call(*j), jobs))
results.insert(0, stack)
errors = [r['section'] for r in results if r['status'] == 'ERROR']
print(json.dumps({
    'at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
    'account': a.account, 'profile': a.profile, 'region': region, 'run': a.name, 'recorded_vpc': vpc or None,
    'audit_status': 'INCOMPLETE' if errors or not vpc or not known else 'KNOWN_IDS_CHECKED_REVIEW_REQUIRED',
    'errors': errors,
    'notes': [
        'Read-only inventory, not a deletion operation or final billing statement.',
        'CRITICAL: default EC2 lists hide new Auto Mode managed resources in this account. Empty lists do NOT prove absence. Review every known-ID result.',
        'Known-ID manifest covers resources recorded before teardown; it cannot detect an unrecorded replacement node. Independently verify EKS deletion and dedicated VPC absence.',
        'Regional volumes, EIPs, NAT gateways and LBs include unrelated resources; compare with baseline and run IDs before attributing or deleting.',
        'Instance state terminated is expected history, not running compute. Deleted NAT entries can also remain visible temporarily.',
        'Missing recorded VPC ID prevents complete VPC-scoped orphan checks. Supply --vpc-id from the pre-deletion evidence.',
        'Service-linked IAM roles shared with other clusters are deliberately not considered owned by this run.',
        'Exit 0 means requests completed with a VPC ID and known-resource manifest, not that the account is clean; inspect every returned resource.'
    ],
    'results': results
}, indent=2))
sys.exit(2 if errors or not vpc or not known else 0)
PY
