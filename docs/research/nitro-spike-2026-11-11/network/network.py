#!/usr/bin/env python3
"""Explicit, bounded V6 research actions. Never changes cluster-wide settings."""
import argparse,datetime,json,os,pathlib,subprocess,time
p=argparse.ArgumentParser()
p.add_argument('action',choices=['generate','create','probe','policy','delete','snapshot'])
p.add_argument('--policy',choices=['deny','allowlike','allowall','remove'],default='deny')
p.add_argument('--name',default='nitro-network-probe')
p.add_argument('--namespace',default='nitro-spike')
p.add_argument('--stage',default='unspecified')
p.add_argument('--nodepool',default='')
p.add_argument('--image',default='public.ecr.aws/docker/library/busybox@sha256:66a6306db78bf2dbf3487f293aa8d6990d8e506fdffab9cc43fe422becf886e4')
p.add_argument('--out',default='/tmp/nitro-network/results')
a=p.parse_args()
if a.action!='generate' and not os.environ.get('KUBECONFIG'):p.error('Set dedicated KUBECONFIG; refusing default context')
if not a.name.startswith('nitro-network-'):p.error('Name must start nitro-network-')
if '@sha256:' not in a.image:p.error('Pinned image digest required')
out=pathlib.Path(a.out);out.mkdir(parents=True,exist_ok=True)
base=['kubectl','--request-timeout=35s','-n',a.namespace]
def call(label,args,data=None,timeout=50):
 start=time.monotonic()
 try:
  r=subprocess.run(base+args,input=data,capture_output=True,timeout=timeout)
  rec={'rc':r.returncode,'stdout':r.stdout.decode(errors='replace'),'stderr':r.stderr.decode(errors='replace')}
 except subprocess.TimeoutExpired as e:
  rec={'timeout':True,'stdout':(e.stdout or b'').decode(errors='replace'),'stderr':(e.stderr or b'').decode(errors='replace')}
 rec.update({'at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'elapsed':time.monotonic()-start,'args':base+args})
 (out/f'{a.stage}-{label}.json').write_text(json.dumps(rec,indent=2))
 print(label,json.dumps({k:v for k,v in rec.items() if k not in ['args','stdout','stderr']}),flush=True)
 return rec
probe='''probe() {
 label="$1"; url="$2"
 start=$(date +%s)
 result=$(timeout 4 wget -S -T 2 -O /dev/null "$url" 2>&1)
 rc=$?
 status=$(printf '%s\\n' "$result" | awk 'index($1,"HTTP/")==1 {printf "%s,", $2}')
 printf 'probe=%s start=%s end=%s wget_rc=%s http_status=%s\\n' "$label" "$start" "$(date +%s)" "$rc" "$status"
}
'''
startup=probe+'''echo network-probe-start
for i in 1 2 3 4 5 6 7 8 9 10; do
 printf 'startup_iteration=%s ' "$i"
 probe internet_literal http://1.1.1.1/
 sleep 0.2
done
sleep 1800
'''
if a.action in ['generate','create']:
 pod={'apiVersion':'v1','kind':'Pod','metadata':{'name':a.name,'namespace':a.namespace,'labels':{'nitro-network-probe':a.name,'nitro-spike-owned':'true'}},'spec':{'runtimeClassName':'isolated-sandbox','automountServiceAccountToken':False,'activeDeadlineSeconds':1200,'terminationGracePeriodSeconds':0,'restartPolicy':'Never','nodeSelector':{'kubernetes.io/arch':'amd64'},'containers':[{'name':'agent','image':a.image,'command':['/bin/sh','-c',startup],'resources':{'requests':{'cpu':'1','memory':'1Gi'},'limits':{'cpu':'1','memory':'1Gi'}},'securityContext':{'runAsUser':65532,'runAsNonRoot':True,'allowPrivilegeEscalation':False,'capabilities':{'drop':['ALL']},'seccompProfile':{'type':'RuntimeDefault'}}}]}}
 if a.nodepool:
  pod['spec']['nodeSelector']['karpenter.sh/nodepool']=a.nodepool
  if a.nodepool=='nitro-deny':pod['spec']['tolerations']=[{'key':'nitro-deny','operator':'Equal','value':'true','effect':'NoSchedule'}]
 data=json.dumps(pod,indent=2).encode();(out/f'{a.stage}-pod.json').write_bytes(data)
 if a.action=='create':
  rec=call('create',['create','-f','-'],data)
  if rec.get('rc')==0:call('ready',['wait','pod/'+a.name,'--for=condition=Ready','--timeout=300s'],timeout=310)
elif a.action=='probe':
 script=probe+'''date -u; id; cat /proc/net/dev; cat /proc/net/route
probe internet_literal http://1.1.1.1/
probe internet_dns http://example.com/
probe imds_region http://169.254.169.254/latest/meta-data/placement/region
probe pod_identity_root http://169.254.170.23/
'''
 call('probe',['exec',a.name,'-c','agent','--','/bin/sh','-c',script],timeout=45)
 call('startup-logs',['logs',a.name,'-c','agent','--tail=50'])
elif a.action=='policy':
 if a.policy=='remove':call('policy-remove',['delete','networkpolicy',a.name,'--ignore-not-found']);raise SystemExit
 spec={'podSelector':{'matchLabels':{'nitro-network-probe':a.name}},'policyTypes':['Egress'],'egress':[]}
 if a.policy=='allowall':spec['egress']=[{}]
 if a.policy=='allowlike':
  # Exact current Stirrup allowances: DNS to any destination, proxy pod peer.
  spec['egress']=[{'ports':[{'protocol':'UDP','port':53},{'protocol':'TCP','port':53}]},{'to':[{'podSelector':{'matchLabels':{'app':'stirrup-egress-proxy'}}}],'ports':[{'protocol':'TCP','port':8080}]}]
 policy={'apiVersion':'networking.k8s.io/v1','kind':'NetworkPolicy','metadata':{'name':a.name,'namespace':a.namespace,'labels':{'nitro-spike-owned':'true'}},'spec':spec}
 data=json.dumps(policy,indent=2).encode();(out/f'{a.stage}-policy.json').write_bytes(data)
 call('policy-apply',['apply','-f','-'],data)
elif a.action=='snapshot':
 for kind in ['pod','networkpolicy','policyendpoints.networking.k8s.aws']:
  call('snapshot-'+kind,['get',kind,'-o','json'])
 call('controller',['get','configmap','amazon-vpc-cni','-n','kube-system','-o','json'])
elif a.action=='delete':
 call('delete',['delete','pod',a.name,'--grace-period=0','--wait=false','--ignore-not-found'])
 call('deleted',['wait','--for=delete','pod/'+a.name,'--timeout=60s'],timeout=70)
 call('policy-remove',['delete','networkpolicy',a.name,'--ignore-not-found'])
