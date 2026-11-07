import argparse
import hashlib
import json
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import plugin_bundle
import plugin_catalog


REPO = Path(__file__).resolve().parents[1]
HA_ENTRY = json.loads((REPO / 'plugins/homeassistant.json').read_text(encoding='utf-8'))
PAGES = plugin_bundle.DEFAULT_PAGES_BASE_URL
RELEASE = plugin_bundle.DEFAULT_RELEASE_BASE_URL


class PluginBundle(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix='tp-plugin-bundle-test-'))
        self.addCleanup(lambda: shutil.rmtree(self.tmp, ignore_errors=True))
        self.stage_parent = self.tmp / 'stage-parent'
        self.stage_parent.mkdir(mode=0o700)
        self.key = self.tmp / 'signing-key.pem'
        self.trust_root = self.tmp / 'trust-root.pem'
        subprocess.run(['openssl', 'ecparam', '-name', 'prime256v1', '-genkey', '-noout', '-out', str(self.key)],
                       check=True, capture_output=True)
        subprocess.run(['openssl', 'pkey', '-in', str(self.key), '-pubout', '-out', str(self.trust_root)],
                       check=True, capture_output=True)
        self.agent = b'linux agent executable fixture'
        self.image = b'HA docker image archive fixture'

    def signed_files(self, target='linux/amd64', *, release_tag='plugin-v1.2.3', agent=None, image=None):
        agent = self.agent if agent is None else agent
        image = self.image if image is None else image
        entry = json.loads(json.dumps(HA_ENTRY))
        entry['image_platform'] = target
        entry['image_archive_sha256'] = hashlib.sha256(image).hexdigest()
        catalog = {
            'schema': plugin_catalog.SCHEMA,
            'version': 'bundle-1',
            'published_at': int(time.time()),
            'expires_at': int(time.time()) + 3600,
            'target_platform': target,
            'release_tag': release_tag,
            'agent_sha256': hashlib.sha256(agent).hexdigest(),
            'plugins': [entry],
        }
        raw = json.dumps(catalog, sort_keys=True, indent=1).encode()
        signature = subprocess.run(['openssl', 'dgst', '-sha256', '-sign', str(self.key)],
                                   input=raw, stdout=subprocess.PIPE, check=True).stdout
        assets = {
            PAGES + '/plugins/catalog-linux-' + target.split('/')[1] + '.json': raw,
            PAGES + '/plugins/catalog-linux-' + target.split('/')[1] + '.json.sig': signature,
            RELEASE + '/' + release_tag + '/tp-agent-linux-' + target.split('/')[1]: agent,
            RELEASE + '/' + release_tag + '/ha-image-linux-' + target.split('/')[1] + '.tar': image,
        }
        return catalog, assets

    def fetcher(self, assets, calls):
        def fetch(url, destination, max_bytes):
            calls.append((url, max_bytes))
            if url not in assets:
                raise AssertionError('unexpected asset URL: ' + url)
            data = assets[url]
            if len(data) > max_bytes:
                raise plugin_bundle.PluginBundleError('bundle asset exceeds its configured size limit')
            Path(destination).write_bytes(data)
        return fetch

    def stage(self, target, assets, calls, **kwargs):
        return plugin_bundle.stage_plugin_bundle(
            target, trust_root=self.trust_root, stage_parent=self.stage_parent,
            downloader=self.fetcher(assets, calls), **kwargs)

    def test_success_stages_both_linux_architectures_without_touching_active_paths(self):
        for target in ('linux/amd64', 'linux/arm64'):
            with self.subTest(target=target):
                catalog, assets = self.signed_files(target)
                calls = []
                staged = self.stage(target, assets, calls)
                self.assertEqual(staged.catalog['target_platform'], target)
                self.assertTrue(staged.agent_path.is_file())
                self.assertTrue(staged.image_archive_path.is_file())
                self.assertEqual(stat.S_IMODE(staged.stage_dir.stat().st_mode), 0o700)
                self.assertEqual(staged.agent_path.read_bytes(), self.agent)
                self.assertEqual(staged.image_archive_path.read_bytes(), self.image)
                self.assertEqual([call[0] for call in calls], list(assets))
                self.assertEqual([path.resolve() for path in self.stage_parent.iterdir()], [staged.stage_dir.resolve()])
                shutil.rmtree(staged.stage_dir)

    def test_bad_signature_stops_before_release_asset_download(self):
        _, assets = self.signed_files()
        sig_url = PAGES + '/plugins/catalog-linux-amd64.json.sig'
        assets[sig_url] = b'not a signature'
        calls = []
        with self.assertRaisesRegex(plugin_bundle.PluginBundleError, 'signature or schema'):
            self.stage('linux/amd64', assets, calls)
        self.assertEqual(len(calls), 2)
        self.assertEqual(list(self.stage_parent.iterdir()), [])

    def test_architecture_mismatch_stops_before_release_assets(self):
        _, assets = self.signed_files('linux/arm64')
        assets[PAGES + '/plugins/catalog-linux-amd64.json'] = assets.pop(PAGES + '/plugins/catalog-linux-arm64.json')
        assets[PAGES + '/plugins/catalog-linux-amd64.json.sig'] = assets.pop(PAGES + '/plugins/catalog-linux-arm64.json.sig')
        calls = []
        with self.assertRaisesRegex(plugin_bundle.PluginBundleError, 'target platform'):
            self.stage('linux/amd64', assets, calls)
        self.assertEqual(len(calls), 2)
        self.assertEqual(list(self.stage_parent.iterdir()), [])

    def test_release_tag_path_injection_is_rejected_before_release_assets(self):
        _, assets = self.signed_files(release_tag='../evil')
        calls = []
        with self.assertRaisesRegex(plugin_bundle.PluginBundleError, 'signature or schema'):
            self.stage('linux/amd64', assets, calls)
        self.assertEqual(len(calls), 2)

    def test_agent_tampering_is_rejected_before_image_download(self):
        _, assets = self.signed_files()
        agent_url = RELEASE + '/plugin-v1.2.3/tp-agent-linux-amd64'
        assets[agent_url] = b'changed Agent'
        calls = []
        with self.assertRaisesRegex(plugin_bundle.PluginBundleError, 'Agent binary digest'):
            self.stage('linux/amd64', assets, calls)
        self.assertEqual(len(calls), 3)
        self.assertEqual(list(self.stage_parent.iterdir()), [])

    def test_image_archive_tampering_is_rejected(self):
        _, assets = self.signed_files()
        image_url = RELEASE + '/plugin-v1.2.3/ha-image-linux-amd64.tar'
        assets[image_url] = b'changed image archive'
        calls = []
        with self.assertRaisesRegex(plugin_bundle.PluginBundleError, 'image archive digest'):
            self.stage('linux/amd64', assets, calls)
        self.assertEqual(len(calls), 4)
        self.assertEqual(list(self.stage_parent.iterdir()), [])

    def test_active_bundle_accepts_signed_expired_catalog_but_rejects_tampering(self):
        _, assets = self.signed_files()
        staged = self.stage('linux/amd64', assets, [])
        root = plugin_bundle.arrange_staged_bundle(staged)
        catalog_path = root / 'plugins/plugin-catalog.json'
        signature_path = root / 'plugins/plugin-catalog.json.sig'
        catalog = json.loads(catalog_path.read_bytes())
        catalog['expires_at'] = int(time.time()) - 1
        raw = json.dumps(catalog, sort_keys=True, indent=1).encode()
        catalog_path.write_bytes(raw)
        signature_path.write_bytes(subprocess.run(['openssl', 'dgst', '-sha256', '-sign', str(self.key)],
                                                  input=raw, stdout=subprocess.PIPE, check=True).stdout)
        with self.assertRaisesRegex(ValueError, 'expired'):
            plugin_catalog.load_verified(catalog_path, self.trust_root)
        self.assertEqual(plugin_bundle.verify_active_bundle(root, self.trust_root, 'linux/amd64'), catalog)
        (root / 'bin/tp-agent').write_bytes(b'tampered')
        with self.assertRaisesRegex(plugin_bundle.PluginBundleError, 'digest mismatch'):
            plugin_bundle.verify_active_bundle(root, self.trust_root, 'linux/amd64')
        (root / 'bin/tp-agent').write_bytes(self.agent)
        (root / 'plugins/homeassistant-image.tar').write_bytes(b'tampered')
        with self.assertRaisesRegex(plugin_bundle.PluginBundleError, 'digest mismatch'):
            plugin_bundle.verify_active_bundle(root, self.trust_root, 'linux/amd64')
        (root / 'plugins/homeassistant-image.tar').write_bytes(self.image)
        signature_path.write_bytes(b'tampered')
        with self.assertRaisesRegex(plugin_bundle.PluginBundleError, 'signature or schema'):
            plugin_bundle.verify_active_bundle(root, self.trust_root, 'linux/amd64')

    def test_active_bundle_rejects_symlinked_assets(self):
        _, assets = self.signed_files()
        root = plugin_bundle.arrange_staged_bundle(self.stage('linux/amd64', assets, []))
        agent = root / 'bin/tp-agent'
        agent.unlink()
        agent.symlink_to(self.tmp / 'external-agent')
        with self.assertRaisesRegex(plugin_bundle.PluginBundleError, 'invalid asset'):
            plugin_bundle.verify_active_bundle(root, self.trust_root, 'linux/amd64')

    def test_size_limit_rejected_and_private_stage_removed(self):
        _, assets = self.signed_files()
        calls = []
        with self.assertRaisesRegex(plugin_bundle.PluginBundleError, 'size limit'):
            self.stage('linux/amd64', assets, calls, max_agent_bytes=8)
        self.assertEqual(len(calls), 3)
        self.assertEqual(list(self.stage_parent.iterdir()), [])

    def test_streaming_downloader_enforces_limit_without_content_length(self):
        class Response:
            headers = {}

            def __init__(self):
                self.data = [b'1234', b'5678', b'']

            def __enter__(self):
                return self

            def __exit__(self, *_):
                return False

            def geturl(self):
                return 'https://install.thingspanel.io/asset'

            def read(self, size):
                return self.data.pop(0)

        dest = self.tmp / 'oversize'
        with mock.patch.object(plugin_bundle, 'urlopen', return_value=Response()):
            with self.assertRaisesRegex(plugin_bundle.PluginBundleError, 'size limit'):
                plugin_bundle._download_stream('https://install.thingspanel.io/asset', dest, 5)
        self.assertFalse(dest.exists())

    def test_publish_adds_optional_bundle_metadata_and_legacy_catalogs_remain_valid(self):
        plugin_file = self.tmp / 'ha.json'
        publish_entry = json.loads(json.dumps(HA_ENTRY))
        publish_entry['image_platform'] = 'linux/amd64'
        plugin_file.write_text(json.dumps(publish_entry), encoding='utf-8')
        out = self.tmp / 'published'
        args = argparse.Namespace(key=str(self.key), out=str(out), version='bundle-1', valid_days=2,
                                  plugin_file=[str(plugin_file)], target_platform='linux/amd64',
                                  release_tag='plugin-v1.2.3', agent_sha256=hashlib.sha256(self.agent).hexdigest())
        plugin_catalog.publish(args)
        verified = plugin_catalog.load_verified(out / 'plugin-catalog.json', self.trust_root)
        self.assertEqual(verified['target_platform'], 'linux/amd64')
        self.assertEqual(verified['release_tag'], 'plugin-v1.2.3')
        self.assertEqual(verified['agent_sha256'], hashlib.sha256(self.agent).hexdigest())

        legacy = self.tmp / 'legacy'
        legacy.mkdir()
        legacy_args = argparse.Namespace(key=str(self.key), out=str(legacy), version='legacy', valid_days=2,
                                         plugin_file=[str(plugin_file)])
        plugin_catalog.publish(legacy_args)
        old = plugin_catalog.load_verified(legacy / 'plugin-catalog.json', self.trust_root)
        self.assertNotIn('target_platform', old)

    def test_publish_metadata_arguments_are_all_or_none_and_path_safe(self):
        for metadata in (
            ('linux/amd64', None, 'a' * 64),
            ('linux/other', 'plugin-v1.2.3', 'a' * 64),
            ('linux/amd64', '../escape', 'a' * 64),
            ('linux/amd64', 'plugin-v1.2.3', 'A' * 64),
        ):
            with self.subTest(metadata=metadata):
                with self.assertRaises(ValueError):
                    plugin_catalog.validate_bundle_metadata(*metadata)


if __name__ == '__main__':
    unittest.main()
