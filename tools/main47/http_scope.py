"""Reconcile only the managed HTTP account and its owned platform scope additions."""
import pathlib,json,subprocess,fcntl,copy,os,time
root=pathlib.Path('/opt/tp-community-main-20261028');base=root/'http-runtime'
lock=open('/opt/tp-community-actions/deployment.lock','a')
try:fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
except BlockingIOError:raise SystemExit(0)
def run(args):return subprocess.check_output(args,text=True).strip()
def query(q):return run(['docker','exec','timescaledb','psql','-X','-U','postgres','-d','ThingsPanel','-Atc',q])
live=query("SELECT count(*) FROM plugin_installations i JOIN plugin_runtime_identities r ON r.plugin_instance=i.plugin_id AND r.tenant_id=i.tenant_id AND r.generation=i.generation WHERE i.plugin_id='http-adapter' AND i.tenant_id='' AND i.desired='enabled' AND i.state IN ('installing','enabled') AND r.service_identifier='HTTP' AND r.revoked_at IS NULL AND r.expires_at>CURRENT_TIMESTAMP;")=='1'
targets=json.loads(query("SELECT coalesce(json_agg(t),'[]'::json) FROM (SELECT d.id,d.tenant_id FROM devices d JOIN device_configs c ON c.id=d.device_config_id AND c.tenant_id=d.tenant_id WHERE c.protocol_type='HTTP' AND c.device_type='1' AND d.is_enabled='enabled' AND d.activate_flag='active' AND coalesce(d.service_access_id,'')='' ORDER BY d.id)t;")) if live else []
assert len(targets)<=256
path=root/'active-broker-services.private.json';current=json.loads(path.read_text());wanted=copy.deepcopy(current)
creds=json.loads((base/'mqtt.private.json').read_text());platform=next(x for x in wanted if x['username']=='tp-main47-platform')
history=base/'http-scope.previous.json';previous=set(json.loads(history.read_text())) if history.exists() else set()
baseline=set(platform.get('allowed_device_ids',[]))-previous;ids={t['id'] for t in targets}
# The current publisher resolves device scope dynamically in Broker Owner.
if not platform.get('target_scoped_publisher'):
 platform['allowed_device_ids']=sorted(baseline|ids)
 platform['allowed_tenant_ids']=sorted(set(platform.get('allowed_tenant_ids',[]))|{t['tenant_id'] for t in targets})
plugin=next((x for x in wanted if x['username']==creds['username']),None)
if live:
 if plugin is None:
  plugin=copy.deepcopy(platform)
  plugin.pop('target_scoped_publisher',None)
  plugin.pop('target_tenant_scope',None)
  wanted.append(plugin)
 plugin.update(username=creds['username'],password=creds['password'],allowed_client_ids=[],allow_any_client_id=True,plugin_socket=True,allowed_device_ids=sorted(ids),allowed_tenant_ids=sorted({t['tenant_id'] for t in targets}))
 plugin['principal']['device_id']='main47-http'
 plugin['policy']={'publish':[],'subscribe':[],'max_payload':1048576,'max_qos':1,'retain_profile':'live-ingress'}
elif plugin is not None:wanted.remove(plugin)
if wanted!=current or (base/'scope-restart.pending').exists():
 (base/'scope-restart.pending').write_text('pending')
 with path.open('r+') as f:f.seek(0);f.write(json.dumps(wanted,indent=2));f.truncate();f.flush();os.fsync(f.fileno())
 for name in ('tp-community-main-47-broker-owner-1','tp-community-main-47-broker-mosquitto-1'):
  run(['docker','restart',name])
  for _ in range(30):
   if run(['docker','inspect','--format','{{.State.Health.Status}}',name])=='healthy':break
   time.sleep(1)
  else:raise RuntimeError('broker health check failed')
 (base/'scope-restart.pending').unlink()
 history.write_text(json.dumps(sorted(ids-baseline)))
 print('HTTP scope updated; active='+str(live)+' targets='+str(len(targets)))
