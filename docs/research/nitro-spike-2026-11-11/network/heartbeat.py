#!/usr/bin/env python3
import datetime,ipaddress,json,os,pathlib,subprocess,threading,time,uuid
if os.environ.get('KUBECONFIG')!='/tmp/nitro-spike.kubeconfig':raise SystemExit('Dedicated spike kubeconfig required')
p=pathlib.Path('/tmp/nitro-network/results');runid=uuid.uuid4().hex
base=['kubectl','--request-timeout=50s','-n','nitro-spike']
rec={'run_id':runid,'events':[],'calls':[],'scope':'Observed network activity after zero-grace DeletePod; not proof of guest CPU halt'}
lock=threading.Lock()
def stamp():return {'utc':datetime.datetime.now(datetime.timezone.utc).isoformat(),'monotonic':time.monotonic()}
def call(label,args,data=None):
 entry={'label':label,'start':stamp(),'args':base+args}
 r=subprocess.run(base+args,input=data,capture_output=True,text=True,timeout=55)
 entry.update({'end':stamp(),'rc':r.returncode,'stdout':r.stdout,'stderr':r.stderr});rec['calls'].append(entry);return r
receiver=json.loads((p/'heartbeat-receiver-admitted.json').read_text());ip=str(ipaddress.ip_address(receiver['status']['podIP']))
rec['receiver']={'pod':'nitro-v9-proxy','ip':ip,'uid':receiver['metadata']['uid']}
policy={'apiVersion':'networking.k8s.io/v1','kind':'NetworkPolicy','metadata':{'name':'nitro-network-probe','namespace':'nitro-spike'},'spec':{'podSelector':{'matchLabels':{'nitro-network-probe':'nitro-network-probe'}},'policyTypes':['Egress'],'egress':[{'to':[{'podSelector':{'matchLabels':{'app':'stirrup-egress-proxy'}}}],'ports':[{'protocol':'TCP','port':19090}]}]}}
assert call('heartbeat-policy',['apply','-f','-'],json.dumps(policy)).returncode==0
listener=subprocess.Popen(base+['exec','nitro-v9-proxy','-c','proxy','--','timeout','45','sh','-c','while :; do nc -l -p 19090; done'],stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,bufsize=1)
rec['listener_start']=stamp()
def read():
 for line in listener.stdout:
  with lock:rec['events'].append(dict(stamp(),line=line.strip()))
thread=threading.Thread(target=read,daemon=True);thread.start();time.sleep(1)
sender_script=f'i=0; while :; do i=$((i+1)); printf "{runid} heartbeat %s\\n" "$i" | nc -w 1 {ip} 19090; sleep 0.2; done'
sender=subprocess.Popen(base+['exec','nitro-network-probe','-c','agent','--','timeout','35','sh','-c',sender_script],stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
rec['sender_start']=stamp();deadline=time.monotonic()+20
while time.monotonic()<deadline:
 with lock:count=sum(e['line'].startswith(runid+' heartbeat ') for e in rec['events'])
 if count>=5:break
 time.sleep(.1)
rec['predelete_count']=count
if count>=5:
 call('delete-sender',['delete','pod','nitro-network-probe','--grace-period=0','--wait=false'])
 time.sleep(10)
 rec['post_observation_end']=stamp()
 call('receiver-control',['exec','nitro-v9-proxy','-c','proxy','--','sh','-c',f'printf "{runid} CONTROL\\n" | timeout 3 nc -w 1 127.0.0.1 19090'])
 time.sleep(1)
else:rec['failure']='Fewer than five pre-deletion heartbeats; no lifetime claim'
try:listener.wait(timeout=48)
except subprocess.TimeoutExpired:listener.terminate();rec['listener_local_timeout']=True
thread.join(timeout=2);rec['listener_rc']=listener.returncode;rec['listener_stderr']=listener.stderr.read()
try:rec['sender_stdout'],rec['sender_stderr']=sender.communicate(timeout=2)
except subprocess.TimeoutExpired:sender.terminate();rec['sender_stdout'],rec['sender_stderr']=sender.communicate(timeout=2)
rec['sender_rc']=sender.returncode
call('delete-sender-final',['delete','pod','nitro-network-probe','--grace-period=0','--wait=false','--ignore-not-found'])
call('delete-policy',['delete','networkpolicy','nitro-network-probe','--ignore-not-found'])
rec['end']=stamp();(p/'heartbeat.json').write_text(json.dumps(rec,indent=2)+'\n')
print(json.dumps({'predelete_count':count,'events':rec['events'],'calls':[{'label':x['label'],'start':x['start'],'end':x['end'],'rc':x['rc']} for x in rec['calls']],'listener_rc':rec['listener_rc'],'failure':rec.get('failure')},indent=2))
