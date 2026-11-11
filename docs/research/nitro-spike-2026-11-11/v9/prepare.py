#!/usr/bin/env python3
"""Generate only. No Kubernetes or AWS calls."""
import argparse,copy,hashlib,json,pathlib,tarfile
p=argparse.ArgumentParser(); p.add_argument('--namespace',default='nitro-spike'); a=p.parse_args()
out=pathlib.Path('/tmp/nitro-v9')
busy='public.ecr.aws/docker/library/busybox@sha256:66a6306db78bf2dbf3487f293aa8d6990d8e506fdffab9cc43fe422becf886e4'
golang='public.ecr.aws/docker/library/golang@sha256:025eefc1e8dbe63201734a72aaa0a55d7a077f2d8410432f8ae2f3e939f32871'
security={'runAsUser':65532,'runAsNonRoot':True,'allowPrivilegeEscalation':False,'capabilities':{'drop':['ALL']},'seccompProfile':{'type':'RuntimeDefault'}}
def meta(name,labels=None): return {'name':name,'namespace':a.namespace,'labels':dict({'nitro-spike-owned':'true'},**(labels or {}))}
proxy={'apiVersion':'v1','kind':'Pod','metadata':meta('nitro-v9-proxy',{'app':'stirrup-egress-proxy'}),'spec':{
 'restartPolicy':'Never','automountServiceAccountToken':False,'activeDeadlineSeconds':1800,'terminationGracePeriodSeconds':0,
 'nodeSelector':{'kubernetes.io/arch':'amd64'},'securityContext':{'fsGroup':65532},
 'volumes':[{'name':'workspace','emptyDir':{}},{'name':'allowlist','configMap':{'name':'nitro-v9-allowlist'}}],
 'containers':[{'name':'proxy','image':busy,'command':['/bin/sh','-c','while [ ! -f /workspace/start ]; do sleep 1; done; exec /workspace/stirrup egress-proxy --listen :8080 --allowlist-file /etc/stirrup/allowlist.txt --log-level info'],
 'ports':[{'name':'proxy','containerPort':8080}],'readinessProbe':{'tcpSocket':{'port':'proxy'},'periodSeconds':2},
 'securityContext':security,'resources':{'requests':{'cpu':'50m','memory':'64Mi'},'limits':{'cpu':'500m','memory':'256Mi'}},
 'volumeMounts':[{'name':'workspace','mountPath':'/workspace'},{'name':'allowlist','mountPath':'/etc/stirrup','readOnly':True}]}]}}
allowlist={'apiVersion':'v1','kind':'ConfigMap','metadata':meta('nitro-v9-allowlist'),'data':{'allowlist.txt':'github.com\n*.githubusercontent.com\nproxy.golang.org\nsum.golang.org\nstorage.googleapis.com\n'}}
service={'apiVersion':'v1','kind':'Service','metadata':meta('stirrup-egress-proxy'),'spec':{'type':'ClusterIP','selector':{'app':'stirrup-egress-proxy'},'ports':[{'name':'proxy','port':8080,'targetPort':'proxy'}]}}
policy={'apiVersion':'networking.k8s.io/v1','kind':'NetworkPolicy','metadata':meta('nitro-v9-allowlist'),'spec':{'podSelector':{'matchLabels':{'app':'nitro-v9-sandbox'}},'policyTypes':['Egress'],'egress':[{'ports':[{'protocol':'UDP','port':53},{'protocol':'TCP','port':53}]},{'to':[{'podSelector':{'matchLabels':{'app':'stirrup-egress-proxy'}}}],'ports':[{'protocol':'TCP','port':8080}]}]}}
url='http://stirrup-egress-proxy.'+a.namespace+'.svc:8080'
env=[{'name':k,'value':url} for k in ['http_proxy','https_proxy','HTTP_PROXY','HTTPS_PROXY']]
env += [{'name':k,'value':v} for k,v in {'NO_PROXY':'','no_proxy':'','HOME':'/workspace','GOPATH':'/workspace/go','GOMODCACHE':'/workspace/modcache','GOCACHE':'/workspace/buildcache','GOTOOLCHAIN':'local','GIT_TERMINAL_PROMPT':'0','GOWORK':'off','GOPROXY':'https://proxy.golang.org','GOSUMDB':'sum.golang.org'}.items()]
workload={'apiVersion':'v1','kind':'Pod','metadata':meta('nitro-v9-workload',{'app':'nitro-v9-sandbox'}),'spec':{'runtimeClassName':'isolated-sandbox','restartPolicy':'Never','automountServiceAccountToken':False,'activeDeadlineSeconds':1800,'terminationGracePeriodSeconds':0,'nodeSelector':{'kubernetes.io/arch':'amd64'},'securityContext':{'fsGroup':65532},'volumes':[{'name':'workspace','emptyDir':{}}],'containers':[{'name':'agent','image':golang,'command':['/bin/sh','-c','sleep 1800'],'workingDir':'/workspace','env':env,'securityContext':copy.deepcopy(security),'resources':{'requests':{'cpu':'1','memory':'1Gi','ephemeral-storage':'1Gi'},'limits':{'cpu':'1','memory':'1Gi','ephemeral-storage':'1Gi'}},'volumeMounts':[{'name':'workspace','mountPath':'/workspace'}]}]}}
for name,obj in [('proxy',proxy),('allowlist',allowlist),('service',service),('network-policy',policy),('workload',workload)]:
 (out/(name+'.json')).write_text(json.dumps(obj,indent=2))
with tarfile.open(out/'proxy-binary.tar.gz','w:gz',compresslevel=5) as archive:
 archive.add(out/'stirrup',arcname='stirrup',recursive=False)
(out/'binary-sha256.txt').write_text(hashlib.sha256((out/'stirrup').read_bytes()).hexdigest()+'  stirrup\n')
print('Generated manifests and proxy-binary.tar.gz; no cluster calls made')
