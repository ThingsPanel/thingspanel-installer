#!/usr/bin/env python3
"""Verify the offline archive and every locally loaded image before installation."""
import hashlib
import ipaddress
import json
from pathlib import Path
import subprocess
import sys

root = Path(__file__).resolve().parent
lock = json.loads((root / 'offline-images.lock.json').read_text())
config = json.loads(subprocess.check_output(['docker', 'compose', 'config', '--no-env-resolution', '--format', 'json'], cwd=root, text=True))
if config['name'] != lock['composeProject']:
    raise SystemExit('Unexpected Compose project; never reuse a product environment')
for key, value in config.get('volumes', {}).items():
    if value.get('external') or value.get('name') != config['name'] + '_' + key:
        raise SystemExit('Preview volumes must belong exclusively to this project')
for key, value in config.get('networks', {}).items():
    if value.get('external') or value.get('name') != config['name'] + '_' + key:
        raise SystemExit('Preview networks must belong exclusively to this project')
network_ids = subprocess.check_output(['docker', 'network', 'ls', '-q'], text=True).split()
existing_networks = json.loads(subprocess.check_output(['docker', 'network', 'inspect', *network_ids], text=True)) if network_ids else []
if (root / '.preview-installed').exists():
    for key, value in config.get('networks', {}).items():
        desired = {str(ipaddress.ip_network(item['subnet'])) for item in ((value.get('ipam') or {}).get('config') or [])}
        for existing in existing_networks:
            if existing['Name'] == value['name'] and (existing.get('Labels') or {}).get('com.docker.compose.project') == config['name'] and desired:
                present = {str(ipaddress.ip_network(item['Subnet'])) for item in ((existing.get('IPAM') or {}).get('Config') or []) if item.get('Subnet')}
                if desired != present:
                    raise SystemExit('An installed preview network CIDR must not change')
for key, value in config.get('networks', {}).items():
    for ipam in ((value.get('ipam') or {}).get('config') or []):
        selected = ipaddress.ip_network(ipam['subnet'])
        for existing in existing_networks:
            for allocated in ((existing.get('IPAM') or {}).get('Config') or []):
                subnet = allocated.get('Subnet')
                if not subnet or not selected.overlaps(ipaddress.ip_network(subnet)):
                    continue
                own = (existing.get('Labels') or {}).get('com.docker.compose.project') == config['name']
                if own and existing['Name'] == value['name'] and selected == ipaddress.ip_network(subnet):
                    continue
                raise SystemExit('Preview network overlaps existing network ' + existing['Name'] + ' (' + subnet + '). Choose a free CIDR before first installation; never change an installed CIDR.')
for service in config['services'].values():
    for mount in service.get('volumes', []):
        if mount.get('type') == 'bind' and not Path(mount['source']).resolve().is_relative_to(root):
            raise SystemExit('Preview bind mounts must stay inside the bundle directory')
if not (root / '.preview-installed').exists():
    for resource in ('container', 'volume'):
        existing = subprocess.check_output(['docker', resource, 'ls', '-q', '--filter', 'label=com.docker.compose.project=' + config['name']], text=True).strip()
        if existing:
            raise SystemExit('Fresh preview requires new volumes. Review/clean only this preview project or use a new project bundle.')
actual_refs = sorted({s['image'] for s in config['services'].values()})
if actual_refs != lock['expectedImages']:
    raise SystemExit('Resolved Compose images differ from the fixed preview manifest')
for service in config['services'].values():
    if service.get('container_name'):
        raise SystemExit('Fixed container names are forbidden in the preview')
    for port in service.get('ports', []):
        if port.get('host_ip') != '127.0.0.1':
            raise SystemExit('Preview host ports must bind only to loopback')
expected_archives = {str((root / item['archive']).resolve()) for item in lock['images']}
actual_archives = {str(item.resolve()) for item in (root / 'images.d').glob('*.tar.gz')}
if actual_archives != expected_archives:
    raise SystemExit('The image archive set must exactly match the manifest')
for image in lock['images']:
    archive = (root / image['archive']).resolve()
    if not archive.is_relative_to(root / 'images.d'):
        raise SystemExit('Image archive must be inside images.d')
    h = hashlib.sha256()
    with archive.open('rb') as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b''):
            h.update(block)
    if h.hexdigest() != image['archiveSha256']:
        raise SystemExit('Image archive checksum mismatch: ' + image['archive'])
if '--archives-only' in sys.argv:
    print('Compose isolation and all archive checksums match')
    raise SystemExit(0)
for image in lock['images']:
    result = subprocess.check_output(['docker', 'image', 'inspect', image['reference']], text=True)
    actual = json.loads(result)[0]
    if actual['Id'] != image['imageId'] or actual['Architecture'] != image['architecture']:
        raise SystemExit('Image provenance mismatch: ' + image['reference'])
print('Archive and all image IDs match the fixed preview manifest')
