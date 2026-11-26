import hashlib
import importlib.util
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

import plugin_bundle


spec = importlib.util.spec_from_file_location(
    'installer_update', Path(__file__).resolve().parents[1] / 'config/native/update.py')
update = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = update
spec.loader.exec_module(update)


class PluginBundleTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.stage = self.root / 'stage'
        self.stage.mkdir()
        self.trust = self.root / 'trust.pem'
        self.trust.touch()
        self.downloads = []

    def test_file_info_hashes_all_binary_archive_bytes(self):
        archive = self.stage / 'plugin-image.tar'
        payload = b'header\r\nlayer\x1atrailer'
        archive.write_bytes(payload)
        self.assertEqual(update.file_info(archive)['sha256'], hashlib.sha256(payload).hexdigest())

    def catalog(self, arch, plugins):
        target = 'linux/' + arch
        entries = [{
            'id': plugin_id,
            'image_platform': target,
            'image_id': 'sha256:' + hashlib.sha256((plugin_id + ':image').encode()).hexdigest(),
            'image_archive_sha256': hashlib.sha256(plugin_id.encode()).hexdigest(),
        } for plugin_id in plugins]
        return {
            'target_platform': target,
            'release_tag': 'test-v1.0.0',
            'agent_sha256': hashlib.sha256(b'agent').hexdigest(),
            'plugins': entries,
        }

    def download_bundle(self, arch, catalog, *, corrupt=None, accept_catalog=None, containerd=False):
        suffix = '-containerd' if containerd else ''
        name = f'catalog-linux-{arch}{suffix}.json'
        assets = {
            name: b'catalog',
            name + '.sig': b'signature',
            'tp-agent-linux-%s' % arch: b'agent',
            'ha-image-linux-%s.tar' % arch: b'homeassistant',
            'http-image-linux-%s.tar' % arch: b'http-adapter',
            'modbus-image-linux-%s.tar' % arch: b'modbus',
        }
        if corrupt:
            assets['%s-image-linux-%s.tar' % (corrupt, arch)] = b'bad archive'

        def download(url, path, _limit):
            name = url.rsplit('/', 1)[1]
            self.downloads.append(name)
            path.write_bytes(assets[name])

        return plugin_bundle.stage_plugin_bundle(
            'linux/' + arch, trust_root=self.trust, stage_parent=self.stage, downloader=download,
            catalog_loader=lambda *_: catalog, accept_catalog=accept_catalog, containerd=containerd)

    def test_containerd_requires_its_own_signed_catalog(self):
        catalog = self.catalog('amd64', ('modbus',))
        with self.assertRaisesRegex(plugin_bundle.PluginBundleError, 'image store'):
            self.download_bundle('amd64', catalog, containerd=True)
        self.assertEqual(self.downloads, ['catalog-linux-amd64-containerd.json',
                                          'catalog-linux-amd64-containerd.json.sig'])
        catalog['image_store'] = 'containerd'
        bundle = self.download_bundle('amd64', catalog, containerd=True)
        self.assertEqual(bundle.catalog, catalog)

    def test_all_signed_images_are_arranged_for_each_architecture(self):
        plugins = ('homeassistant', 'http-adapter', 'modbus')
        for arch in ('amd64', 'arm64'):
            with self.subTest(arch=arch):
                bundle = self.download_bundle(arch, self.catalog(arch, plugins))
                root = plugin_bundle.arrange_staged_bundle(bundle)
                for plugin_id in plugins:
                    self.assertEqual(plugin_bundle.image_archive(root, plugin_id).read_bytes(), plugin_id.encode())
                self.assertEqual((root / 'bin/tp-agent').read_bytes(), b'agent')

    def test_wrong_modbus_archive_rejects_the_whole_candidate(self):
        catalog = self.catalog('amd64', ('homeassistant', 'http-adapter', 'modbus'))
        with self.assertRaisesRegex(plugin_bundle.PluginBundleError, 'modbus image archive digest'):
            self.download_bundle('amd64', catalog, corrupt='modbus')
        self.assertEqual(list(self.stage.iterdir()), [])

    def test_unchanged_catalog_skips_all_release_assets(self):
        catalog = self.catalog('arm64', ('homeassistant',))
        self.assertIsNone(self.download_bundle('arm64', catalog, accept_catalog=lambda _: False))
        self.assertEqual(self.downloads, ['catalog-linux-arm64.json', 'catalog-linux-arm64.json.sig'])
        self.assertEqual(list(self.stage.iterdir()), [])

    def test_asset_name_collision_is_rejected_before_release_download(self):
        catalog = self.catalog('arm64', ('ha', 'homeassistant'))
        with self.assertRaisesRegex(plugin_bundle.PluginBundleError, 'asset names collide'):
            self.download_bundle('arm64', catalog)
        self.assertEqual(len(self.downloads), 2)

    def test_preload_imports_missing_images_then_checks_id_and_architecture(self):
        catalog = self.catalog('amd64', ('homeassistant', 'http-adapter', 'modbus'))
        bundle = self.download_bundle('amd64', catalog)
        root = plugin_bundle.arrange_staged_bundle(bundle)
        loaded = []
        available = set()
        image_by_filename = {
            entry['id'] + '-image.tar': entry['image_id'] for entry in catalog['plugins']
        }

        def docker(command, _root):
            if command[1] == 'info':
                return b'linux/x86_64'
            if command[1] == 'load':
                name = Path(command[-1]).name
                loaded.append(name)
                available.add(image_by_filename[name])
                return b''
            image = command[-1]
            if image not in available:
                raise update.UpdateError('missing image')
            return (image + ' linux/amd64').encode()

        with patch.object(update, 'run_command', side_effect=docker):
            self.assertTrue(update.preload_plugin_images(root, catalog))
            self.assertTrue(update.preload_plugin_images(root, catalog))
        self.assertEqual(loaded, list(image_by_filename))

    def test_preload_rejects_invalid_archive_before_importing_any_image(self):
        catalog = self.catalog('amd64', ('homeassistant', 'modbus'))
        bundle = self.download_bundle('amd64', catalog)
        root = plugin_bundle.arrange_staged_bundle(bundle)
        plugin_bundle.image_archive(root, 'modbus').write_bytes(b'tampered')
        with patch.object(update, 'run_command', return_value=b'linux/amd64') as docker:
            with self.assertRaisesRegex(update.UpdateError, 'modbus image archive'):
                update.preload_plugin_images(root, catalog)
        docker.assert_called_once()


    def test_failed_new_agent_start_restores_last_good_bundle(self):
        catalog = self.catalog('amd64', ('homeassistant', 'modbus'))
        previous_catalog = self.catalog('amd64', ('homeassistant',))
        candidate, previous = self.stage / 'new', self.stage / 'old'
        before = (previous, self.trust, previous_catalog, False)
        after = (candidate, self.trust, catalog, True)
        with patch.object(update, 'start_plugin_agent', side_effect=[update.UpdateError('start failed'), True]) as start, \
                patch.object(update, 'load_previous_plugin_bundle', return_value=before), \
                patch.object(update, 'record_active_plugin') as record:
            self.assertTrue(update.activate_plugin_agent(self.root, self.trust, after, False))
        record.assert_not_called()
        self.assertEqual(start.call_args.kwargs['bundle_root'], previous)
        self.assertEqual(start.call_args.kwargs['verified_catalog'], previous_catalog)


if __name__ == '__main__':
    unittest.main()
