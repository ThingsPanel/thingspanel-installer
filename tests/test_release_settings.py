"""R9/B3+B7: where the host Agent finds signed releases, and how the trust root is pinned."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

REPO = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location('tp_update_release_settings', REPO / 'config/native/update.py')
update = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(update)


def make_key(path):
    private = Path(str(path) + '.private')
    subprocess.run(['openssl', 'ecparam', '-name', 'prime256v1', '-genkey', '-noout', '-out', str(private)], check=True, capture_output=True)
    pub = subprocess.run(['openssl', 'ec', '-in', str(private), '-pubout'], check=True, capture_output=True).stdout
    Path(path).write_bytes(pub)
    return pub


class ReleaseSettings(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        base = Path(self.tmp.name).resolve()
        self.root = base / 'install'
        self.root.mkdir()
        self.keys = base / 'keys'
        self.keys.mkdir()
        self.beta = self.keys / 'beta.pem'
        self.official = self.keys / 'official.pem'
        self.beta_pem = make_key(self.beta)
        self.official_pem = make_key(self.official)

    def configure(self, **kw):
        return update.configure_release_settings(self.root, **kw)

    def flags(self):
        return update.release_agent_flags(self.root)

    def test_nothing_configured_keeps_upgrade_off(self):
        result = self.configure()
        self.assertEqual(result.get('source'), '')
        self.assertEqual(self.flags(), [])

    def test_source_and_trust_root_enable_upgrade_and_are_remembered(self):
        self.configure(source='https://rel.example.test/beta/', trust_root=str(self.beta))
        flags = self.flags()
        self.assertEqual(flags[:2], ['--release-source', 'https://rel.example.test/beta'])
        trust = Path(flags[flags.index('--release-trust-root') + 1])
        self.assertEqual(trust.read_bytes(), self.beta_pem)
        self.assertEqual(oct(trust.stat().st_mode & 0o777), '0o600')
        self.assertEqual(oct(trust.parent.stat().st_mode & 0o777), '0o700')
        self.assertEqual(flags[-2:], ['--compose-image', update.DEFAULT_HELPER_IMAGE])
        # a later run without any of the inputs keeps the pinned configuration
        self.configure()
        self.assertEqual(self.flags(), flags)

    def test_source_without_trust_root_stays_off(self):
        self.configure(source='https://rel.example.test/beta')
        with mock.patch('sys.stderr'):
            self.assertEqual(self.flags(), [])

    def test_trust_root_without_source_stays_off(self):
        self.configure(trust_root=str(self.beta))
        self.assertEqual(self.flags(), [])

    def test_env_file_values_are_used_below_explicit_ones(self):
        (self.root / '.env').write_text('TP_RELEASE_SOURCE=https://from-env.example.test/r\nTP_RELEASE_TRUST_ROOT="%s"\n' % self.beta)
        self.configure()
        self.assertEqual(self.flags()[1], 'https://from-env.example.test/r')
        self.configure(source='https://explicit.example.test/r')
        self.assertEqual(self.flags()[1], 'https://explicit.example.test/r')

    def test_a_different_trust_root_is_refused_unless_replacement_is_explicit(self):
        self.configure(source='https://rel.example.test/r', trust_root=str(self.beta))
        before = (self.root / '.tp-agent/release/settings.json').read_bytes()
        with self.assertRaises(update.UpdateError) as caught:
            self.configure(trust_root=str(self.official))
        self.assertIn('TP_REPLACE_RELEASE_TRUST_ROOT', str(caught.exception))
        self.assertEqual((self.root / '.tp-agent/release/settings.json').read_bytes(), before)
        self.assertEqual((self.root / '.tp-agent/release/release-trust-root.pem').read_bytes(), self.beta_pem)
        self.configure(trust_root=str(self.official), replace=True)
        self.assertEqual((self.root / '.tp-agent/release/release-trust-root.pem').read_bytes(), self.official_pem)
        self.assertEqual(self.flags()[1], 'https://rel.example.test/r')

    def test_tampered_pinned_copy_disables_upgrade_and_blocks_reconfiguration(self):
        self.configure(source='https://rel.example.test/r', trust_root=str(self.beta))
        copy = self.root / '.tp-agent/release/release-trust-root.pem'
        copy.write_bytes(self.official_pem)   # what a swap by anything but the operator would look like
        with mock.patch('sys.stderr'):
            self.assertEqual(self.flags(), [])
        with self.assertRaises(update.UpdateError):
            self.configure()
        with self.assertRaises(update.UpdateError):
            self.configure(trust_root=str(self.official))   # supplying the swapped file is no way around the pin

    def test_trust_root_must_be_outside_the_installation_and_a_p256_key(self):
        inside = self.root / 'root.pem'
        inside.write_bytes(self.beta_pem)
        with self.assertRaises(update.UpdateError):
            self.configure(trust_root=str(inside))
        rsa = self.keys / 'rsa.pem'
        subprocess.run(['openssl', 'genrsa', '-out', str(self.keys / 'rsa.private'), '2048'], check=True, capture_output=True)
        rsa.write_bytes(subprocess.run(['openssl', 'rsa', '-in', str(self.keys / 'rsa.private'), '-pubout'], check=True, capture_output=True).stdout)
        with self.assertRaises(update.UpdateError):
            self.configure(trust_root=str(rsa))
        junk = self.keys / 'junk.pem'
        junk.write_text('not a key')
        with self.assertRaises(update.UpdateError):
            self.configure(trust_root=str(junk))
        link = self.keys / 'link.pem'
        link.symlink_to(self.beta)
        with self.assertRaises(update.UpdateError):
            self.configure(trust_root=str(link))
        with self.assertRaises(update.UpdateError):
            self.configure(trust_root='relative.pem')
        self.assertFalse((self.root / '.tp-agent/release/release-trust-root.pem').exists())

    def test_source_and_image_validation(self):
        for bad in ('http://plain.example.test/r', 'https://user:pw@x.example.test/r', 'https://x.example.test/r?a=b', 'ftp://x/y',
                    'https://x.example.test/a b', 'https://'):
            with self.assertRaises(update.UpdateError, msg=bad):
                self.configure(source=bad, trust_root=str(self.beta))
        for bad in ('docker 27', 'docker:27;id', '-x', ''):
            if bad:
                with self.assertRaises(update.UpdateError, msg=bad):
                    self.configure(helper_image=bad)

    def test_helper_image_is_configurable_and_passed_on(self):
        self.configure(source='https://rel.example.test/r', trust_root=str(self.beta), helper_image='registry.example.test/tools/docker:27-cli')
        self.assertEqual(self.flags()[-2:], ['--compose-image', 'registry.example.test/tools/docker:27-cli'])

    def test_ca_bundle_is_pinned_too(self):
        ca = self.keys / 'ca.pem'
        ca.write_text('-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n')
        self.configure(source='https://rel.example.test/r', trust_root=str(self.beta), ca=str(ca))
        flags = self.flags()
        self.assertIn('--release-ca', flags)
        Path(flags[flags.index('--release-ca') + 1]).write_text('-----BEGIN CERTIFICATE-----\nBBBB\n-----END CERTIFICATE-----\n')
        with mock.patch('sys.stderr'):
            self.assertEqual(self.flags(), [])

    def test_damaged_settings_fail_closed(self):
        self.configure(source='https://rel.example.test/r', trust_root=str(self.beta))
        (self.root / '.tp-agent/release/settings.json').write_text('{broken')
        with mock.patch('sys.stderr'):
            self.assertEqual(self.flags(), [])
        with self.assertRaises(update.UpdateError):
            self.configure()

    def test_official_root_constant_is_the_default_once_it_exists(self):
        with mock.patch.object(update, 'OFFICIAL_RELEASE_TRUST_PEM', self.official_pem):
            self.configure(source='https://rel.example.test/r')
            flags = self.flags()
            self.assertEqual(Path(flags[flags.index('--release-trust-root') + 1]).read_bytes(), self.official_pem)
            # a beta machine switches to the official root explicitly; switching back and forth is never silent
            with self.assertRaises(update.UpdateError):
                self.configure(trust_root=str(self.beta))
            self.configure(trust_root=str(self.beta), replace=True)
            self.assertEqual(Path(self.flags()[3]).read_bytes(), self.beta_pem)
            self.configure(replace=True)    # replace with nothing supplied = back to the built-in official root
            self.assertEqual(Path(self.flags()[3]).read_bytes(), self.official_pem)

    def test_release_flags_reach_agent_host(self):
        self.configure(source='https://rel.example.test/r', trust_root=str(self.beta))
        agent_dir = self.root / 'bundle'
        (agent_dir / 'bin').mkdir(parents=True)
        (agent_dir / 'plugins').mkdir()
        (agent_dir / 'tools').mkdir()
        calls = []
        with mock.patch.object(update, 'run_command', side_effect=lambda args, root, **kw: calls.append(args) or b''), \
                mock.patch.object(update, 'verify_plugin_agent_bundle', return_value=True):
            trust = self.keys / 'plugin.pem'
            trust.write_bytes(self.beta_pem)
            update.start_plugin_agent(self.root, str(trust), verified_catalog={'x': 1}, bundle_root=agent_dir)
        host = [c for c in calls if any('agent_host.py' in str(a) for a in c)]
        self.assertEqual(len(host), 1)
        args = host[0]
        for flag in ('--system', '--release-source', '--release-trust-root', '--compose-image'):
            self.assertIn(flag, args)
        self.assertEqual(args[args.index('--release-source') + 1], 'https://rel.example.test/r')
        # the helper image was checked (and pulled when absent) before the Agent was started
        self.assertTrue(any(c[:3] == ['docker', 'image', 'inspect'] for c in calls))

    def test_unconfigured_agent_gets_no_release_flags(self):
        calls = []
        with mock.patch.object(update, 'run_command', side_effect=lambda args, root, **kw: calls.append(args) or b''), \
                mock.patch.object(update, 'verify_plugin_agent_bundle', return_value=True):
            trust = self.keys / 'plugin.pem'
            trust.write_bytes(self.beta_pem)
            update.start_plugin_agent(self.root, str(trust), verified_catalog={'x': 1}, bundle_root=self.root)
        self.assertEqual(len(calls), 1)
        self.assertNotIn('--release-source', calls[0])
        self.assertNotIn('--compose-image', calls[0])
        self.assertIn('--system', calls[0])

    def test_helper_image_prefetch_outcomes(self):
        runs = []

        def present(args, root, **kw):
            runs.append(args)
            return b''
        with mock.patch.object(update, 'run_command', side_effect=present):
            self.assertTrue(update.ensure_upgrade_helper_image(self.root, ''))
        self.assertEqual(runs, [['docker', 'image', 'inspect', 'docker:27-cli']])

        def missing_then_pull(args, root, **kw):
            runs.append(args)
            if args[:3] == ['docker', 'image', 'inspect']:
                raise update.UpdateError('x')
            return b''
        runs.clear()
        with mock.patch.object(update, 'run_command', side_effect=missing_then_pull):
            self.assertTrue(update.ensure_upgrade_helper_image(self.root, 'h.example.test/docker:27'))
        self.assertEqual(runs[-1], ['docker', 'pull', 'h.example.test/docker:27'])

        def offline(args, root, **kw):
            raise update.UpdateError('x')
        with mock.patch.object(update, 'run_command', side_effect=offline), mock.patch('sys.stderr') as err:
            self.assertFalse(update.ensure_upgrade_helper_image(self.root, ''))
        written = ''.join(call.args[0] for call in err.write.call_args_list)
        self.assertIn('docker save', written)
        self.assertIn('docker load', written)


if __name__ == '__main__':
    unittest.main()


class FreshInstallCompose(unittest.TestCase):
    """R9/B6 finding: on a clean machine there is no .env, Compose cannot interpolate, and the unrendered file used to
    fail the updater's PostgreSQL guard because the password is a ${...} reference. A fresh install never got past it."""

    def test_repository_compose_passes_the_guard_without_an_env_file(self):
        if subprocess.run(['docker', 'compose', 'version'], capture_output=True).returncode:
            self.skipTest('docker compose is not available')
        with tempfile.TemporaryDirectory() as directory:
            update.render_with_fallback(Path(directory), REPO / 'docker-compose.yml')

    def test_endpoint_fields_must_still_be_literal(self):
        config = {'services': {
            'postgres': {},
            'thingsvis-server': {'environment': {'DATABASE_URL': 'postgresql://u:p@postgres:5432/d'}},
            'backend': {'environment': {
                'GOTP_DB_PSQL_HOST': 'postgres', 'GOTP_DB_PSQL_PORT': '5432', 'GOTP_DB_PSQL_USERNAME': 'postgres', 'GOTP_DB_PSQL_DBNAME': 'd',
                'GOTP_RELATIONAL_DRIVER': 'postgres', 'GOTP_RELATIONAL_POSTGRES_PLATFORM_HOST': 'postgres',
                'GOTP_RELATIONAL_POSTGRES_PLATFORM_PORT': '5432', 'GOTP_RELATIONAL_POSTGRES_PLATFORM_DATABASE': 'd',
                'GOTP_RELATIONAL_POSTGRES_PLATFORM_USERNAME': 'postgres',
                'GOTP_RELATIONAL_POSTGRES_PLATFORM_PASSWORD': '${TP_DATABASE_PASSWORD:-x}'}}}}
        update.validate_config(config)     # an unresolved password reference is fine
        config['services']['backend']['environment']['GOTP_RELATIONAL_POSTGRES_PLATFORM_HOST'] = '${HOST}'
        with self.assertRaises(update.UpdateError):
            update.validate_config(config)  # an unresolved host is not


class FreshInstallPrepareGuard(unittest.TestCase):
    def test_prepare_guard_accepts_unresolved_password_reference_only(self):
        spec = importlib.util.spec_from_file_location('tp_prepare_guard', REPO / 'config/native/prepare.py')
        prepare = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(prepare)
        env = {'GOTP_DB_PSQL_HOST': 'postgres', 'GOTP_DB_PSQL_PORT': '5432', 'GOTP_DB_PSQL_USERNAME': 'postgres', 'GOTP_DB_PSQL_DBNAME': 'd',
               'GOTP_RELATIONAL_DRIVER': 'postgres', 'GOTP_RELATIONAL_POSTGRES_PLATFORM_HOST': 'postgres',
               'GOTP_RELATIONAL_POSTGRES_PLATFORM_PORT': '5432', 'GOTP_RELATIONAL_POSTGRES_PLATFORM_DATABASE': 'd',
               'GOTP_RELATIONAL_POSTGRES_PLATFORM_USERNAME': 'postgres',
               'GOTP_RELATIONAL_POSTGRES_PLATFORM_PASSWORD': '${TP_DATABASE_PASSWORD:-x}'}
        config = {'services': {'postgres': {}, 'backend': {'environment': env}}}
        prepare.validate_supported_backend_database(config)
        env['GOTP_RELATIONAL_POSTGRES_PLATFORM_DATABASE'] = '${DB}'
        with self.assertRaises(RuntimeError):
            prepare.validate_supported_backend_database(config)


class BundleMirror(unittest.TestCase):
    """R9: a beta (or mirror) can serve its own plugin bundle; the default stays pinned to the official locations."""

    def test_default_bases_stay_pinned_and_custom_ones_need_the_flag(self):
        sys_path = str(REPO / 'tools')
        import sys
        sys.path.insert(0, sys_path)
        import plugin_bundle
        with self.assertRaises(plugin_bundle.PluginBundleError):
            plugin_bundle._validate_https_base('https://beta.example.test/p')
        with self.assertRaises(plugin_bundle.PluginBundleError):
            plugin_bundle._validate_https_base('https://beta.example.test/r', release=True)
        self.assertEqual(plugin_bundle._validate_https_base('https://beta.example.test/p/', custom=True), 'https://beta.example.test/p')
        with self.assertRaises(plugin_bundle.PluginBundleError):
            plugin_bundle._validate_https_base('http://beta.example.test/p', custom=True)   # https is never optional
        with self.assertRaises(plugin_bundle.PluginBundleError):
            plugin_bundle._validate_https_base('https://u:p@beta.example.test/p', custom=True)

    def test_bases_are_saved_validated_and_remembered(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / 'install'
            root.mkdir()
            update.configure_release_settings(root, bundle_pages_base='https://beta.example.test/release',
                                              bundle_release_base='https://beta.example.test/assets/')
            saved = update.load_release_settings(root)
            self.assertEqual(saved['bundle_pages_base'], 'https://beta.example.test/release')
            self.assertEqual(saved['bundle_release_base'], 'https://beta.example.test/assets')
            update.configure_release_settings(root)       # remembered
            self.assertEqual(update.load_release_settings(root)['bundle_pages_base'], 'https://beta.example.test/release')
            with self.assertRaises(update.UpdateError):
                update.configure_release_settings(root, bundle_pages_base='http://plain.example.test')
