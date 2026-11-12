"""Build metadata only. The workflow signs it with the existing release key."""
import hashlib,json,os,pathlib,time
arch=os.environ['ARCH']; tmp=pathlib.Path(os.environ['RUNNER_TEMP']); out=pathlib.Path('out')
catalog=json.loads((tmp/'ha.json').read_text())
ha=next(x for x in catalog['plugins'] if x['id']=='homeassistant')
def sha(path): return hashlib.sha256(path.read_bytes()).hexdigest()
assert sha(out/('tp-agent-linux-'+arch))==catalog['agent_sha256']
assert sha(out/('ha-image-linux-'+arch+'.tar'))==ha['image_archive_sha256']
image=json.loads((tmp/'image.json').read_text())[0]
assert image['Os']=='linux' and image['Architecture']==arch
assert not (image['Config'].get('Labels') or image['Config'].get('Volumes') or image['Config'].get('OnBuild'))
entry=json.loads(pathlib.Path('plugins/http-managed.json').read_text())
entry.update(image_id=image['Id'], image_platform='linux/'+arch, image_archive_sha256=sha(out/('http-image-linux-'+arch+'.tar')),source_commit=os.environ['SOURCE_COMMIT'],installer_commit=os.environ['INSTALLER_COMMIT'])
catalog.update(version='1.2.0',release_tag='plugin-http-v1.2.0',published_at=int(time.time()),expires_at=int(time.time())+3650*86400,plugins=[ha,entry])
(out/('catalog-linux-'+arch+'.json')).write_text(json.dumps(catalog,sort_keys=True,indent=1)+'\n')
