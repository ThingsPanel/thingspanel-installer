#!/usr/bin/env python3
"""Publish and verify a signed ThingsPanel plugin catalog.

A catalog is plain JSON listing installable plugins. It is signed (DER ECDSA over SHA-256 of the exact
bytes, same format as the release manifest) so an installer can refuse a catalog that was altered or
published by someone else. The public key is the trust root the installer is given out of band.

    plugin_catalog.py publish --key release-key.pem --out DIR --version 1 [--plugin-file p.json ...]
    plugin_catalog.py verify  --trust-root trust-root.pem --catalog DIR/plugin-catalog.json

A plugin entry (see plugin_install.py) pins one local/registry image by its immutable image ID.
"""
import argparse
import importlib.util
import json
import re
from pathlib import Path
import subprocess
import sys
import time

SCHEMA = 'thingspanel.plugin-catalog.v1'
REPO = Path(__file__).resolve().parents[1]
SUPPORTED_PLUGIN_PLATFORMS = frozenset(('linux/amd64', 'linux/arm64'))
RELEASE_TAG_RE = re.compile(r'^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$')
SHA256_RE = re.compile(r'^[0-9a-f]{64}$')


def validate_bundle_metadata(target_platform, release_tag, agent_sha256):
    """Validate optional per-architecture installer bundle metadata."""
    supplied = (target_platform is not None, release_tag is not None, agent_sha256 is not None)
    if any(supplied) and not all(supplied):
        raise ValueError('target_platform, release_tag and agent_sha256 must be supplied together')
    if not any(supplied):
        return None
    if target_platform not in SUPPORTED_PLUGIN_PLATFORMS:
        raise ValueError('target_platform must be linux/amd64 or linux/arm64')
    if (not isinstance(release_tag, str) or RELEASE_TAG_RE.fullmatch(release_tag) is None or
            '..' in release_tag or release_tag.endswith('.') or release_tag.endswith('.lock') or '@{' in release_tag):
        raise ValueError('release_tag is not a safe GitHub release tag')
    if not isinstance(agent_sha256, str) or SHA256_RE.fullmatch(agent_sha256) is None:
        raise ValueError('agent_sha256 must be a lowercase SHA-256 digest')
    return {'target_platform': target_platform, 'release_tag': release_tag, 'agent_sha256': agent_sha256}


def validate_catalog_bundle(catalog):
    """Validate the optional bundle envelope and its HA target alignment."""
    bundle = validate_bundle_metadata(catalog.get('target_platform'), catalog.get('release_tag'),
                                     catalog.get('agent_sha256'))
    if bundle is None:
        return
    plugins = catalog.get('plugins')
    if not isinstance(plugins, list):
        raise ValueError('bundle catalog plugins must be a list')
    homeassistant = [entry for entry in plugins if isinstance(entry, dict) and entry.get('id') == 'homeassistant']
    if len(homeassistant) != 1 or homeassistant[0].get('image_platform') != bundle['target_platform']:
        raise ValueError('bundle catalog must contain one Home Assistant entry matching target_platform')


def _verifier():
    spec = importlib.util.spec_from_file_location('tp_release_manifest', REPO / 'config/native/release_manifest.py')
    module = importlib.util.module_from_spec(spec)
    sys.modules['tp_release_manifest'] = module
    spec.loader.exec_module(module)
    return module


_RESERVED_MOUNTS = ('/proc', '/sys', '/dev', '/etc', '/run', '/var/run', '/bin', '/sbin', '/lib', '/lib64', '/usr', '/boot')


def _check_destination(value, what):
    """Same rules as the Agent (agent/storage.go validDestination); the Agent re-checks and fails closed."""
    import posixpath
    if (not isinstance(value, str) or not value.startswith('/') or value == '/' or len(value) > 255 or posixpath.normpath(value) != value
            or any(c in value for c in ',:\\\0\r\n')):
        raise ValueError('%s: destination must be a clean absolute path' % what)
    if any(value == p or value.startswith(p + '/') for p in _RESERVED_MOUNTS):
        raise ValueError('%s: destination %s is reserved' % (what, value))
    return value


def check_storage(entry):
    """Optional generic storage: named volumes (rw/ro) and bounded tmpfs. No bind sources, drivers or options."""
    volumes, tmpfs = entry.get('volumes', []), entry.get('tmpfs', [])
    if not isinstance(volumes, list) or not isinstance(tmpfs, list) or len(volumes) > 4 or len(tmpfs) > 2:
        raise ValueError('volumes (max 4) and tmpfs (max 2) must be lists')
    keys, dests = set(), []
    for v in volumes:
        if not isinstance(v, dict) or set(v) - {'key', 'destination', 'mode'}:
            raise ValueError('volumes[]: only key, destination and mode are allowed')
        if not re.fullmatch(r'[a-z0-9][a-z0-9-]{0,30}[a-z0-9]', str(v.get('key', ''))) or v['key'] in keys:
            raise ValueError('volumes[].key must be a unique lowercase label')
        if v.get('mode') not in ('rw', 'ro'):
            raise ValueError('volumes[].mode must be rw or ro')
        keys.add(v['key'])
        dests.append(_check_destination(v.get('destination'), 'volumes[%s]' % v['key']))
    for t in tmpfs:
        if not isinstance(t, dict) or set(t) - {'destination', 'size_mib'}:
            raise ValueError('tmpfs[]: only destination and size_mib are allowed')
        if not isinstance(t.get('size_mib'), int) or isinstance(t.get('size_mib'), bool) or not 1 <= t['size_mib'] <= 256:
            raise ValueError('tmpfs[].size_mib must be 1..256')
        dests.append(_check_destination(t.get('destination'), 'tmpfs'))
    for i, a in enumerate(dests):
        for b in dests[i + 1:]:
            if a == b or a.startswith(b + '/') or b.startswith(a + '/'):
                raise ValueError('storage destinations must not overlap: %s, %s' % (a, b))


def check_entry(entry):
    for key in ('id', 'version', 'kind', 'image_id', 'entrypoint', 'platform'):
        if key not in entry:
            raise ValueError('plugin entry is missing ' + key)
    if not str(entry['image_id']).startswith('sha256:') or len(entry['image_id']) != 71:
        raise ValueError('image_id must be an immutable sha256 image ID')
    if entry['id'] == 'homeassistant' and re.fullmatch(r'[0-9a-f]{64}', str(entry.get('image_archive_sha256', ''))) is None:
        raise ValueError('homeassistant.image_archive_sha256 must be a lowercase SHA-256 digest')
    platform = entry['platform']
    binding = platform.get('binding', 'template')
    if binding not in ('template', 'none'):
        raise ValueError('platform.binding must be "template" or "none"')
    if binding == 'template':
        for key in ('protocol_type', 'device_type', 'template_name', 'secret_env'):
            if key not in platform:
                raise ValueError('platform.' + key + ' is required when binding is template')
    elif not platform.get('service_identifier'):
        raise ValueError('platform.service_identifier is required when binding is none')
    alias = platform.get('network_alias')
    if alias is not None and not re.fullmatch(r'[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?', alias):
        raise ValueError('platform.network_alias must be a lowercase DNS label')
    service_plugin = platform.get('service_plugin')
    if service_plugin is not None:
        if binding != 'none':
            raise ValueError('platform.service_plugin is only for binding none')
        if not isinstance(service_plugin.get('config'), dict) or not service_plugin.get('name'):
            raise ValueError('platform.service_plugin needs name and a config object')
        if 'http_address' in service_plugin['config'] and 'http_port' not in platform:
            raise ValueError('service_plugin.config.http_address needs platform.http_port')
    for name, spec in entry.get('env_user', {}).items():
        if not isinstance(spec, dict) or spec.get('validate') not in (None, 'url'):
            raise ValueError('env_user.%s: unsupported spec' % name)
    check_storage(entry)
    if entry['kind'] != 'service':
        raise ValueError('only service plugins are supported by this installer')


def publish(args):
    plugins = []
    for path in args.plugin_file:
        entry = json.loads(Path(path).read_text(encoding='utf-8'))
        check_entry(entry)
        plugins.append(entry)
    if not plugins:
        raise SystemExit('at least one --plugin-file is required')
    now = int(time.time())
    catalog = {'schema': SCHEMA, 'version': args.version, 'published_at': now, 'expires_at': now + args.valid_days * 86400, 'plugins': plugins}
    bundle = validate_bundle_metadata(getattr(args, 'target_platform', None),
                                     getattr(args, 'release_tag', None), getattr(args, 'agent_sha256', None))
    if bundle:
        catalog.update(bundle)
        validate_catalog_bundle(catalog)
    raw = json.dumps(catalog, sort_keys=True, indent=1).encode()
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    (out / 'plugin-catalog.json').write_bytes(raw)
    signature = subprocess.run(['openssl', 'dgst', '-sha256', '-sign', args.key], input=raw, stdout=subprocess.PIPE, check=True).stdout
    (out / 'plugin-catalog.json.sig').write_bytes(signature)
    print('published %d plugin(s), catalog version %s' % (len(plugins), args.version))


def load_verified(catalog_path, trust_root):
    """Return the catalog dict only if the signature verifies against the pinned root and it is current."""
    verifier = _verifier()
    catalog_path = Path(catalog_path)
    raw = catalog_path.read_bytes()
    signature = Path(str(catalog_path) + '.sig').read_bytes()
    verifier.verify_signature(raw, signature, verifier.load_trust_root(trust_root))
    catalog = json.loads(raw)
    if catalog.get('schema') != SCHEMA:
        raise ValueError('unsupported catalog schema')
    if int(time.time()) >= int(catalog.get('expires_at', 0)):
        raise ValueError('catalog has expired')
    for entry in catalog['plugins']:
        check_entry(entry)
    validate_catalog_bundle(catalog)
    return catalog


def verify(args):
    catalog = load_verified(args.catalog, args.trust_root)
    print('catalog v%s verified: %s' % (catalog['version'], ', '.join('%s@%s' % (p['id'], p['version']) for p in catalog['plugins'])))


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest='command', required=True)
    p = sub.add_parser('publish')
    p.add_argument('--key', required=True)
    p.add_argument('--out', required=True)
    p.add_argument('--version', required=True)
    p.add_argument('--valid-days', type=int, default=30)
    p.add_argument('--plugin-file', action='append', default=[])
    p.add_argument('--target-platform', choices=sorted(SUPPORTED_PLUGIN_PLATFORMS),
                   help='optional bundle target; must be paired with --release-tag and --agent-sha256')
    p.add_argument('--release-tag', help='optional safe GitHub Release tag for the bundle assets')
    p.add_argument('--agent-sha256', help='optional lowercase SHA-256 digest of the target Agent binary')
    p.set_defaults(run=publish)
    v = sub.add_parser('verify')
    v.add_argument('--trust-root', required=True)
    v.add_argument('--catalog', required=True)
    v.set_defaults(run=verify)
    args = parser.parse_args()
    try:
        args.run(args)
    except Exception as error:  # noqa: BLE001 - CLI boundary
        print('error: %s' % error, file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
