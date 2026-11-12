"""Extend the existing main47 allowlist with HA targets only, preserving other identities."""
import pathlib,json,subprocess,fcntl,copy,os,time
root=pathlib.Path('/opt/tp-community-main-20261028');base=root/'ha-runtime'
lock=open('/opt/tp-community-actions/deployment.lock','a')
try:fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
except BlockingIOError:raise SystemExit(0)
def run(args):return subprocess.check_output(args,universal_newlines=True).strip()
sql="""SELECT coalesce(json_agg(t),'[]'::json) FROM (
SELECT d.id,d.tenant_id FROM devices d JOIN service_access sa ON sa.id=d.service_access_id
JOIN service_plugins sp ON sp.id=sa.service_plugin_id
WHERE sp.service_identifier='homeassistant' AND d.is_enabled='enabled' AND d.activate_flag='active'
AND d.tenant_id=sa.tenant_id ORDER BY d.id)t;"""
targets=json.loads(run(['docker','exec','timescaledb','psql','-X','-U','postgres','-d','ThingsPanel','-Atc',sql]))
assert len(targets)<=256
path=root/'active-broker-services.private.json';current=json.loads(path.read_text());wanted=copy.deepcopy(current)
creds=json.loads((base/'mqtt.private.json').read_text())
platform=next(x for x in wanted if x['username']=='tp-main47-platform')
history=base/'ha-scope.previous.json';previous=json.loads(history.read_text()) if history.exists() else []
# The current publisher resolves device scope dynamically in Broker Owner.
if not platform.get('target_scoped_publisher'):
 platform['allowed_device_ids']=sorted((set(platform.get('allowed_device_ids',[]))-set(previous))|{t['id'] for t in targets})
 platform['allowed_tenant_ids']=sorted(set(platform.get('allowed_tenant_ids',[]))|{t['tenant_id'] for t in targets})
plugin=next((x for x in wanted if x['username']==creds['username']),None)
if plugin is None and targets:
 plugin=copy.deepcopy(platform)
 plugin.pop('target_scoped_publisher',None)
 plugin.pop('target_tenant_scope',None)
 wanted.append(plugin)
if plugin is not None:
 if not targets:
  wanted.remove(plugin)
 else:
  plugin.update(username=creds['username'],password=creds['password'],allowed_client_ids=[],allow_any_client_id=True,plugin_socket=True,
   allowed_device_ids=[t['id'] for t in targets],allowed_tenant_ids=sorted({t['tenant_id'] for t in targets}))
  plugin['principal']['device_id']='main47-homeassistant'
  plugin['policy']={'publish':[],'subscribe':[],'max_payload':1048576,'max_qos':1,'retain_profile':'live-ingress'}
if wanted!=current or (base/'scope-restart.pending').exists():
 (base/'scope-restart.pending').write_text('pending')
 # The live services file is bind-mounted as a file: preserve its inode.
 with path.open('r+') as f:
  f.seek(0);f.write(json.dumps(wanted,indent=2));f.truncate();f.flush();os.fsync(f.fileno())
 run(['docker','restart','tp-community-main-47-broker-owner-1'])
 for _ in range(30):
  h=run(['docker','inspect','--format','{{.State.Health.Status}}','tp-community-main-47-broker-owner-1'])
  if h=='healthy':break
  time.sleep(1)
 else:raise RuntimeError('Broker owner did not recover')
 run(['docker','restart','tp-community-main-47-broker-mosquitto-1'])
 for _ in range(30):
  if run(['docker','inspect','--format','{{.State.Health.Status}}','tp-community-main-47-broker-mosquitto-1'])=='healthy':break
  time.sleep(1)
 else:raise RuntimeError('Broker did not recover')
 (base/'scope-restart.pending').unlink()
 history.write_text(json.dumps([t['id'] for t in targets]))
 print('HA scope updated; targets='+str(len(targets)))
