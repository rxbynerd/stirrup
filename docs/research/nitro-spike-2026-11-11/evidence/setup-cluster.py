import json,os,pathlib,subprocess
base=['aws','--profile','sandbox','--region','us-east-1']
s=json.loads(subprocess.check_output(base+['cloudformation','describe-stacks','--stack-name','equestrian-nitro-20261111']))['Stacks'][0]
assert s['StackStatus']=='CREATE_COMPLETE',s['StackStatus']
out={x['OutputKey']:x['OutputValue'] for x in s['Outputs']}
pathlib.Path('/tmp/nitro-evidence/stack-outputs.json').write_text(json.dumps(out,indent=2))
subprocess.run(base+['eks','update-kubeconfig','--name',out['ClusterName'],'--kubeconfig','/tmp/nitro-spike.kubeconfig','--alias','nitro-spike'],check=True)
env=dict(os.environ,KUBECONFIG='/tmp/nitro-spike.kubeconfig')
for kind in ['runtimeclasses','nodepools','nodeclasses','nodes']:
 r=subprocess.run(['kubectl','get',kind,'-o','json'],env=env,capture_output=True,text=True)
 pathlib.Path('/tmp/nitro-evidence/initial-'+kind+'.json').write_text(r.stdout if r.returncode==0 else r.stderr)
 if r.returncode: raise RuntimeError(r.stderr)
rc=json.loads(pathlib.Path('/tmp/nitro-evidence/initial-runtimeclasses.json').read_text())
assert any(x['metadata']['name']=='isolated-sandbox' for x in rc['items'])
manifest=pathlib.Path('/tmp/nitro-node-template.json').read_text()
for key,value in {'NODE_ROLE_NAME':out['NodeRoleName'],'SUBNET_A_ID':out['SubnetA'],'SUBNET_B_ID':out['SubnetB'],'CLUSTER_SECURITY_GROUP_ID':out['ClusterSecurityGroupId']}.items():manifest=manifest.replace(key,value)
pathlib.Path('/tmp/nitro-evidence/node-config.json').write_text(manifest)
subprocess.run(['kubectl','apply','-f','/tmp/nitro-evidence/node-config.json'],env=env,check=True)
ns={'apiVersion':'v1','kind':'Namespace','metadata':{'name':'nitro-spike','labels':{'pod-security.kubernetes.io/enforce':'restricted','pod-security.kubernetes.io/enforce-version':'v1.36','project':'equestrian-nitro-spike'}}}
subprocess.run(['kubectl','create','-f','-'],env=env,input=json.dumps(ns).encode(),check=True)
