"""Derive the containerd image IDs from the exact archives in a verified catalog."""
import argparse
import hashlib
import json
import tarfile
from pathlib import Path

import plugin_catalog
from plugin_bundle import _release_image_name


def manifest_id(archive, entry):
    with archive.open('rb') as stream:
        if hashlib.file_digest(stream, 'sha256').hexdigest() != entry['image_archive_sha256']:
            raise ValueError('Archive digest differs from signed catalog')
        stream.seek(0)
        with tarfile.open(fileobj=stream) as tar:
            descriptor, = json.load(tar.extractfile('index.json'))['manifests']
            digest = descriptor['digest']
            if descriptor['mediaType'] not in ('application/vnd.oci.image.manifest.v1+json',
                                               'application/vnd.docker.distribution.manifest.v2+json'):
                raise ValueError('Expected a single image manifest')
            raw = tar.extractfile('blobs/sha256/' + digest.removeprefix('sha256:')).read()
            if 'sha256:' + hashlib.sha256(raw).hexdigest() != digest:
                raise ValueError('Manifest digest differs from index')
            if json.loads(raw)['config']['digest'] != entry['image_id']:
                raise ValueError('Manifest does not reference the signed image configuration')
            return digest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('catalog', 'trust-root', 'archives', 'output'):
        parser.add_argument('--' + name, type=Path, required=True)
    args = parser.parse_args()
    catalog = plugin_catalog.load_verified(args.catalog, args.trust_root)
    arch = catalog['target_platform'].split('/')[1]
    for entry in catalog['plugins']:
        archive = args.archives / _release_image_name(entry['id'], arch)
        entry['image_id'] = manifest_id(archive, entry)
    catalog['image_store'] = 'containerd'
    args.output.write_text(json.dumps(catalog, sort_keys=True, indent=2) + '\n', encoding='utf-8')


if __name__ == '__main__':
    main()
