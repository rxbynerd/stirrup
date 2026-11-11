#!/usr/bin/env python3
"""Bounded research probes. Each action is explicit; never creates cloud infrastructure."""
import argparse, datetime, io, json, os, pathlib, subprocess, tarfile, time

def main():
    p = argparse.ArgumentParser()
    p.add_argument('action', choices=['generate', 'create', 'inspect', 'disk', 'delete', 'snapshot'])
    p.add_argument('--namespace', default='nitro-spike')
    p.add_argument('--name', default='nitro-offline')
    p.add_argument('--image', default='')
    p.add_argument('--cpu', default='1')
    p.add_argument('--memory', default='1Gi')
    p.add_argument('--networked', action='store_true')
    p.add_argument('--no-resources', action='store_true')
    p.add_argument('--deadline', type=int, default=1200)
    p.add_argument('--disk-observe-seconds', type=int, default=90)
    p.add_argument('--out', default='/tmp/nitro-probes/results')
    a = p.parse_args()
    out = pathlib.Path(a.out); out.mkdir(parents=True, exist_ok=True)
    base = ['kubectl', '--request-timeout=35s', '-n', a.namespace]
    if a.action != 'generate' and not os.environ.get('KUBECONFIG'):
        p.error('Set dedicated KUBECONFIG; refusing default context')
    def run(label, args, data=None, timeout=45):
        start = time.time()
        try:
            r = subprocess.run(base + args, input=data, capture_output=True, timeout=timeout)
            record = {'at': datetime.datetime.now(datetime.timezone.utc).isoformat(), 'command': base + args,
                      'seconds': time.time()-start, 'returncode':r.returncode,
                      'stdout':r.stdout.decode(errors='replace'), 'stderr':r.stderr.decode(errors='replace')}
        except subprocess.TimeoutExpired as e:
            record = {'seconds':time.time()-start, 'timeout':True, 'command':base+args,
                      'stdout':(e.stdout or b'').decode(errors='replace'), 'stderr':(e.stderr or b'').decode(errors='replace')}
        (out / (a.name+'-'+label+'.json')).write_text(json.dumps(record,indent=2))
        print(label, json.dumps({k:v for k,v in record.items() if k not in ('stdout','stderr','command')}), flush=True)
        return record
    def exe(label, command):
        return run(label,['exec',a.name,'-c','agent','--','/bin/sh','-c',command])
    if a.action in ('generate','create'):
        if '@sha256:' not in a.image: p.error('Provide pinned --image digest')
        pod = {'apiVersion':'v1','kind':'Pod','metadata':{'name':a.name,'namespace':a.namespace,
               'labels':{'app':'nitro-spike','nitro-spike-owned':'true'},
               'annotations':{} if a.networked else {'sandbox.amazonaws.com/network-isolated':'true'}},
               'spec':{'runtimeClassName':'isolated-sandbox','restartPolicy':'Never',
               'automountServiceAccountToken':False,'activeDeadlineSeconds':a.deadline,'terminationGracePeriodSeconds':0,
               'nodeSelector':{'kubernetes.io/arch':'amd64'},'securityContext':{'fsGroup':65532},
               'volumes':[{'name':'workspace','emptyDir':{}},
                          {'name':'sandbox-identity','emptyDir':{'medium':'Memory','sizeLimit':'64Ki'}}],
               'containers':[{'name':'agent','image':a.image,'imagePullPolicy':'IfNotPresent',
                'command':['/bin/sh','-c','echo spike-started; sleep 3600'],'workingDir':'/workspace',
                'securityContext':{'runAsNonRoot':True,'runAsUser':65532,'allowPrivilegeEscalation':False,
                                   'capabilities':{'drop':['ALL']},'seccompProfile':{'type':'RuntimeDefault'}},
                'volumeMounts':[{'name':'workspace','mountPath':'/workspace'},
                               {'name':'sandbox-identity','mountPath':'/run/stirrup/sandbox-identity'}]}]}}
        if not a.no_resources:
            pod['spec']['containers'][0]['resources']={k:{'cpu':a.cpu,'memory':a.memory,'ephemeral-storage':'64Mi'} for k in ('requests','limits')}
        data=json.dumps(pod,indent=2).encode(); (out/(a.name+'-manifest.json')).write_bytes(data)
        if a.action=='create':
            r=run('create',['create','-f','-'],data)
            if r.get('returncode')!=0: return
            ready_start=time.monotonic()
            for attempt in range(1,11):
                ready=run('wait-'+str(attempt),['wait','--for=condition=Ready','pod/'+a.name,'--timeout=50s'],timeout=60)
                if ready.get('returncode')==0: break
            (out/(a.name+'-ready-duration.json')).write_text(json.dumps({'seconds':time.monotonic()-ready_start,'ready':ready.get('returncode')==0}))
            run('admitted',['get','pod',a.name,'-o','json'])
    elif a.action=='inspect':
        run('admitted',['get','pod',a.name,'-o','json'])
        exe('guest',"id; uname -a; cat /sys/class/dmi/id/product_name; ls /sys/block; cat /proc/meminfo; getconf _NPROCESSORS_ONLN 2>/dev/null || grep -c '^processor' /proc/cpuinfo; cat /proc/net/dev; cat /proc/net/route; cat /proc/mounts; ls -ld /workspace /run/stirrup/sandbox-identity; printf writable > /workspace/uid-check; printf dummy-probe-token > /run/stirrup/sandbox-identity/token; stat /workspace/uid-check /run/stirrup/sandbox-identity/token")
        exe('network',"for u in http://example.com/ http://1.1.1.1/ http://169.254.169.254/ http://169.254.170.23/; do echo PROBE:$u; timeout 5 wget -S -T 3 -O /dev/null \"$u\" 2>&1; echo EXIT:$?; done")
        payload=b'ASCII\nUTF8: \xe4\xb8\x96\xe7\x95\x8c\nNUL:\x00after\n'
        archive=io.BytesIO()
        with tarfile.open(fileobj=archive,mode='w') as t:
            info=tarfile.TarInfo('roundtrip.bin'); info.size=len(payload); t.addfile(info,io.BytesIO(payload))
        run('tar-write',['exec','-i',a.name,'-c','agent','--','tar','xf','-','-C','/workspace'],archive.getvalue())
        r=run('tar-read-content',['exec',a.name,'-c','agent','--','cat','/workspace/roundtrip.bin'])
        print('byte_roundtrip',r.get('stdout','').encode()==payload)
        run('logs',['logs',a.name,'-c','agent','--tail=20'])
    elif a.action=='disk':
        if not 0 <= a.disk_observe_seconds <= 180: p.error('disk observation must be 0..180 seconds')
        exe('disk-before','df -k /workspace; du -k /workspace')
        exe('disk-write','dd if=/dev/zero of=/workspace/disk-probe.bin bs=1048576 count=128; r=$?; du -k /workspace/disk-probe.bin; df -k /workspace; exit "$r"')
        run('disk-pod-immediate',['get','pod',a.name,'-o','json'])
        for elapsed in range(15,a.disk_observe_seconds+1,15):
            time.sleep(15)
            run('disk-pod-'+str(elapsed)+'s',['get','pod',a.name,'-o','json'])
        exe('disk-remove','rm -f /workspace/disk-probe.bin')
    elif a.action=='delete':
        run('delete',['delete','pod',a.name,'--grace-period=0','--wait=false'])
        run('deleted',['wait','--for=delete','pod/'+a.name,'--timeout=50s'],timeout=60)
    elif a.action=='snapshot':
        for kind in ('pods','events','nodes','runtimeclasses','nodepools','nodeclasses','networkpolicies'):
            run('snapshot-'+kind,['get',kind,'-o','json'])

if __name__=='__main__': main()
