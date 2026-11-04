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


def _verifier():
    spec = importlib.util.spec_from_file_location('tp_release_manifest', REPO / 'config/native/release_manifest.py')
    module = importlib.util.module_from_spec(spec)
    sys.modules['tp_release_manifest'] = module
    spec.loader.exec_module(module)
    return module


def check_entry(entry):
    for key in ('id', 'version', 'kind', 'image_id', 'entrypoint', 'platform'):
        if key not in entry:
            raise ValueError('plugin entry is missing ' + key)
    if not str(entry['image_id']).startswith('sha256:') or len(entry['image_id']) != 71:
        raise ValueError('image_id must be an immutable sha256 image ID')
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
