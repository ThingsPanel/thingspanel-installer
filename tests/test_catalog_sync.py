"""R9/B4: `agent_host.py catalog-sync` installs a newer signed plugin catalog as a drop-in, or refuses."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

REPO = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(REPO / 'tools'))
import agent_host  # noqa: E402
import plugin_catalog  # noqa: E402

SOURCE = 'https://rel.example.test/beta'
HA_ID = 'sha256:' + 'a' * 64
OTHER_ID = 'sha256:' + 'b' * 64
NEW_ID = 'sha256:' + 'c' * 64


def sha(data):
    return hashlib.sha256(data).hexdigest()


class CatalogSync(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name).resolve()
        subprocess.run(['openssl', 'ecparam', '-name', 'prime256v1', '-genkey', '-noout', '-out', str(self.dir / 'k.pem')], check=True, capture_output=True)
        (self.dir / 'root.pem').write_bytes(subprocess.run(['openssl', 'ec', '-in', str(self.dir / 'k.pem'), '-pubout'], check=True, capture_output=True).stdout)
        self.agent, self.archive = b'agent-binary', b'ha-image-archive'
        self.bundle = self.dir / 'bundle'
        (self.bundle / 'bin').mkdir(parents=True)
        (self.bundle / 'plugins').mkdir()
        (self.bundle / 'bin/tp-agent').write_bytes(self.agent)
        (self.bundle / 'plugins/homeassistant-image.tar').write_bytes(self.archive)
        self.installed = self.publish(self.bundle / 'plugins', version=1, plugins=['ha'])
        self.served = {}
        self.docker_images = {HA_ID}
        self.loaded = []

    def entry(self, which):
        base = json.loads((REPO / 'plugins/homeassistant.json').read_text())
        if which == 'ha':
            base.update(image_id=HA_ID, image_platform='linux/arm64', image_archive_sha256=sha(self.archive))
        else:
            base = json.loads((REPO / 'plugins/http-adapter.json').read_text())
            base.update(image_id=OTHER_ID if which == 'other' else NEW_ID)
            if which == 'new':
                base['version'] = '9.9.9'
        return base

    def publish(self, out, *, version, plugins, agent=None, archive=None, platform='linux/arm64', days=30):
        files = []
        for which in plugins:
            entry = self.entry(which)
            if which == 'ha':
                entry['image_platform'] = platform
                if archive is not None:
                    entry['image_archive_sha256'] = sha(archive)
            path = Path(self.tmp.name) / ('entry-%s-%s.json' % (which, version))
            path.write_text(json.dumps(entry))
            files += ['--plugin-file', str(path)]
        out = Path(out)
        subprocess.run([sys.executable, str(REPO / 'tools/plugin_catalog.py'), 'publish', '--key', str(self.dir / 'k.pem'), '--out', str(out),
                        '--version', str(version), '--valid-days', str(days), '--target-platform', platform, '--release-tag', 'v1.0.0',
                        '--agent-sha256', sha(agent or self.agent)] + files, check=True, capture_output=True)
        return out

    def serve(self, directory):
        directory = Path(directory)
        self.served = {SOURCE + '/plugins/catalog-linux-arm64.json': (directory / 'plugin-catalog.json').read_bytes(),
                       SOURCE + '/plugins/catalog-linux-arm64.json.sig': (directory / 'plugin-catalog.json.sig').read_bytes()}

    def run_sync(self, **kw):
        def fake_stream(url, destination, max_bytes, **_):
            Path(destination).write_bytes(self.served[url])

        def fake_sh(args, check=True):
            if args[:3] == ['docker', 'image', 'inspect']:
                return subprocess.CompletedProcess(args, 0 if args[-1] in self.docker_images else 1, '', '')
            if args[:2] == ['docker', 'load']:
                self.loaded.append(Path(args[-1]).name)
                self.docker_images.add(self.pending_load)
                return subprocess.CompletedProcess(args, 0, '', '')
            raise AssertionError(args)
        ns = argparse.Namespace(catalog=str(self.bundle / 'plugins/plugin-catalog.json'), trust_root=str(self.dir / 'root.pem'),
                                release_source=SOURCE, release_ca=None, image_dir=None, allow_missing_images=False)
        for k, v in kw.items():
            setattr(ns, k, v)
        with mock.patch.object(agent_host, 'docker_platform', return_value='linux/arm64'), \
                mock.patch.object(agent_host, 'sh', side_effect=fake_sh), \
                mock.patch('plugin_bundle._download_stream', side_effect=fake_stream), mock.patch('sys.stdout'):
            agent_host.catalog_sync(ns)

    def installed_version(self):
        return int(json.loads((self.bundle / 'plugins/plugin-catalog.json').read_text())['version'])

    def test_newer_catalog_replaces_the_installed_one_when_images_are_present(self):
        self.docker_images.add(NEW_ID)
        self.serve(self.publish(self.dir / 'v2', version=2, plugins=['ha', 'new']))
        self.run_sync()
        self.assertEqual(self.installed_version(), 2)
        loaded = plugin_catalog.load_verified(self.bundle / 'plugins/plugin-catalog.json', self.dir / 'root.pem')
        self.assertEqual({e['version'] for e in loaded['plugins']} >= {'9.9.9'}, True)
        self.assertEqual(list((self.bundle / 'plugins').glob('.catalog-sync-*')), [])

    def test_missing_image_blocks_the_swap_unless_allowed_or_loaded_from_an_archive(self):
        self.serve(self.publish(self.dir / 'v2', version=2, plugins=['ha', 'new']))
        with self.assertRaises(RuntimeError) as caught:
            self.run_sync()
        self.assertIn('NOT replaced', str(caught.exception))
        self.assertEqual(self.installed_version(), 1)
        self.run_sync(allow_missing_images=True)
        self.assertEqual(self.installed_version(), 2)

    def test_image_is_loaded_from_an_archive_directory_and_checked(self):
        images = self.dir / 'images'
        images.mkdir()
        archive = b'new-plugin-image'
        entry = self.entry('new')
        entry['image_archive_sha256'] = sha(archive)
        # publish a catalog whose 'new' entry carries the archive digest
        original = self.entry
        self.entry = lambda which: entry if which == 'new' else original(which)
        self.serve(self.publish(self.dir / 'v2', version=2, plugins=['ha', 'new']))
        self.pending_load = NEW_ID
        (images / 'http-adapter-linux-arm64.tar').write_bytes(b'tampered')
        with self.assertRaises(RuntimeError) as caught:
            self.run_sync(image_dir=str(images))
        self.assertIn('does not match', str(caught.exception))
        self.assertEqual(self.loaded, [])
        self.assertEqual(self.installed_version(), 1)
        (images / 'http-adapter-linux-arm64.tar').write_bytes(archive)
        self.run_sync(image_dir=str(images))
        self.assertEqual(self.loaded, ['http-adapter-linux-arm64.tar'])
        self.assertEqual(self.installed_version(), 2)

    def test_refusals_leave_the_installed_catalog_alone(self):
        self.docker_images.update({NEW_ID, OTHER_ID})
        cases = {
            'different agent binary': dict(version=2, plugins=['ha', 'new'], agent=b'other-agent'),
            'different HA image archive': dict(version=2, plugins=['ha', 'new'], archive=b'other-archive'),
            'older catalog': dict(version=0, plugins=['ha', 'new']),
            'wrong platform': dict(version=2, plugins=['ha', 'new'], platform='linux/amd64'),
            'expired': dict(version=2, plugins=['ha', 'new'], days=-1),
        }
        for name, kw in cases.items():
            self.serve(self.publish(self.dir / ('c-' + name.replace(' ', '-')), **kw))
            with self.assertRaises(Exception, msg=name):
                self.run_sync()
            self.assertEqual(self.installed_version(), 1, name)

    def test_unsigned_or_foreign_signed_catalog_is_refused(self):
        self.docker_images.add(NEW_ID)
        out = self.publish(self.dir / 'v2', version=2, plugins=['ha', 'new'])
        self.serve(out)
        self.served[SOURCE + '/plugins/catalog-linux-arm64.json'] += b' '   # content no longer matches the signature
        with self.assertRaises(Exception):
            self.run_sync()
        self.assertEqual(self.installed_version(), 1)
        subprocess.run(['openssl', 'ecparam', '-name', 'prime256v1', '-genkey', '-noout', '-out', str(self.dir / 'other.pem')], check=True, capture_output=True)
        foreign = self.dir / 'foreign'
        foreign.mkdir()
        raw = (out / 'plugin-catalog.json').read_bytes()
        (foreign / 'plugin-catalog.json').write_bytes(raw)
        (foreign / 'plugin-catalog.json.sig').write_bytes(subprocess.run(['openssl', 'dgst', '-sha256', '-sign', str(self.dir / 'other.pem')], input=raw, capture_output=True, check=True).stdout)
        self.serve(foreign)
        with self.assertRaises(Exception):
            self.run_sync()
        self.assertEqual(self.installed_version(), 1)

    def test_no_release_source_is_an_explicit_error(self):
        with self.assertRaises(RuntimeError) as caught:
            self.run_sync(release_source='')
        self.assertIn('none is configured', str(caught.exception))


if __name__ == '__main__':
    unittest.main()
