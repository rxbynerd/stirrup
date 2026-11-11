#!/usr/bin/env python3
"""Dedicated V10 Pods. Run only in the spike cluster. Records API evidence, not guest halt."""
import argparse, datetime, json, os, pathlib, subprocess, time

p=argparse.ArgumentParser()
p.add_argument('--namespace',default='nitro-spike')
p.add_argument('--image',required=True)
p.add_argument('--out',default='/tmp/nitro-probes/results/lifecycle')
a=p.parse_args()
if not os.environ.get('KUBECONFIG'): p.error('dedicated KUBECONFIG required')
if '@sha256:' not in a.image: p.error('pinned image required')
out=pathlib.Path(a.out); out.mkdir(parents=True,exist_ok=True)
base=['kubectl','--request-timeout=20s','-n',a.namespace]
events=[]
created=[]
def call(args,data=None):
    start=time.monotonic()
    try:
        r=subprocess.run(base+args,input=None if data is None else json.dumps(data).encode(),capture_output=True,timeout=30)
        row={'at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'args':args,'rc':r.returncode,'seconds':time.monotonic()-start,'stdout':r.stdout.decode(errors='replace'),'stderr':r.stderr.decode(errors='replace')}
    except subprocess.TimeoutExpired:
        row={'at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'args':args,'timeout':True,'seconds':time.monotonic()-start}
    events.append(row)
    (out/'events.json').write_text(json.dumps(events,indent=2))
    return row

def pod(name,deadline):
    subprocess.run(['python3','/tmp/nitro-probes/probes.py','generate','--namespace',a.namespace,'--name',name,'--deadline',str(deadline),'--image',a.image,'--out',str(out)],check=True,timeout=30)
    return json.loads((out/(name+'-manifest.json')).read_text())

def create(obj):
    r=call(['create','-f','-'],obj)
    if r.get('rc')!=0: raise RuntimeError('create failed; see events.json')
    created.append(obj['metadata']['name'])

def get(name):
    r=call(['get','pod',name,'-o','json','--ignore-not-found=true'])
    if r.get('rc')!=0: raise RuntimeError('GET failed; cannot infer deletion')
    return json.loads(r['stdout']) if r['stdout'].strip() else None

def ready(name):
    for _ in range(6):
        r=call(['wait','--for=condition=Ready','pod/'+name,'--timeout=20s'])
        if r.get('rc')==0: return True
    return False

results={}
names=['nitro-deadline','nitro-gc-owner','nitro-gc-child']
try:
    create(pod(names[0],120))
    deadline_start=time.monotonic()
    observed_ready=False
    for _ in range(80):
        obj=get(names[0])
        if obj is None: break
        status=obj.get('status',{})
        observed_ready |= any(c.get('type')=='Ready' and c.get('status')=='True' for c in status.get('conditions',[]))
        if status.get('phase') in ('Failed','Succeeded'):
            results['deadline']={'observed_ready':observed_ready,'elapsed_seconds':time.monotonic()-deadline_start,'status':status}
            break
        time.sleep(3)
    else: results['deadline']={'timed_out':True,'observed_ready':observed_ready}
    call(['delete','pod',names[0],'--ignore-not-found=true','--grace-period=0','--wait=false'])
    create(pod(names[1],600))
    if not ready(names[1]): raise RuntimeError('owner not Ready')
    owner=get(names[1])
    child=pod(names[2],600)
    child['metadata']['ownerReferences']=[{'apiVersion':'v1','kind':'Pod','name':names[1],'uid':owner['metadata']['uid'],'controller':False,'blockOwnerDeletion':False}]
    create(child)
    if not ready(names[2]): raise RuntimeError('child not Ready')
    child_uid=get(names[2])['metadata']['uid']
    start=time.monotonic()
    deletion=call(['delete','pod',names[1],'--grace-period=0','--wait=false','--cascade=background'])
    if deletion.get('rc')!=0: raise RuntimeError('owner delete failed')
    results['garbage_collection']={'child_uid':child_uid}
    for _ in range(30):
        if get(names[2]) is None:
            results['garbage_collection']['api_absence_seconds']=time.monotonic()-start
            break
        time.sleep(2)
    else: results['garbage_collection']['timed_out']=True
finally:
    results['guest_halt_after_delete']='UNVERIFIED: no independent guest heartbeat observer'
    (out/'summary.json').write_text(json.dumps(results,indent=2))
    for name in created:
        call(['delete','pod',name,'--ignore-not-found=true','--grace-period=0','--wait=false'])
    print(json.dumps(results,indent=2))
