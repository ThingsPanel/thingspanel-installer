"""Focused tests for the native pre-bootstrap backend datasource guard."""
import copy
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('native_prepare_guard', ROOT / 'config/native/prepare.py')
prepare = importlib.util.module_from_spec(spec)
spec.loader.exec_module(prepare)


def render():
    text = (ROOT / 'docker-compose.yml').read_text()
    env = os.environ.copy()
    for name in re.findall(r'\$\{([A-Z0-9_]+):\?', text):
        env[name] = 'synthetic-fixture'
    proc = subprocess.run(['docker', 'compose', 'config', '--format', 'json'], cwd=ROOT,
                          env=env, text=True, capture_output=True, check=True)
    return json.loads(proc.stdout)


class BackendDatabaseGuard(unittest.TestCase):
    @classmethod
    def setUpClass(cls): cls.config = render()

    def check(self, cfg):
        with patch.object(prepare, 'command', return_value=json.dumps(cfg)):
            prepare.check_thingsvis_database()

    def test_shipped_pg_compose_is_accepted(self):
        self.check(self.config)

    def test_shipped_backend_has_matching_a1_postgres_settings(self):
        env = self.config['services']['backend']['environment']
        self.assertEqual(env['GOTP_RELATIONAL_DRIVER'], 'postgres')
        self.assertEqual(env['GOTP_RELATIONAL_POSTGRES_PLATFORM_HOST'], 'postgres')
        self.assertEqual(env['GOTP_RELATIONAL_POSTGRES_PLATFORM_PORT'], '5432')
        self.assertEqual(env['GOTP_RELATIONAL_POSTGRES_PLATFORM_DATABASE'], 'ThingsPanel')
        self.assertEqual(env['GOTP_RELATIONAL_POSTGRES_PLATFORM_USERNAME'], 'postgres')

    def test_a1_profile_missing_required_setting_is_rejected(self):
        cfg = copy.deepcopy(self.config)
        del cfg['services']['backend']['environment']['GOTP_RELATIONAL_POSTGRES_PLATFORM_HOST']
        with self.assertRaisesRegex(RuntimeError, 'Cannot confirm'):
            self.check(cfg)

    def test_legacy_only_compose_remains_upgrade_compatible(self):
        cfg = copy.deepcopy(self.config)
        env = cfg['services']['backend']['environment']
        for key in tuple(env):
            if key.startswith('GOTP_RELATIONAL_'):
                del env[key]
        self.check(cfg)

    def test_explicit_mysql_backend_with_pg_vis_is_rejected(self):
        cfg = copy.deepcopy(self.config)
        cfg['services']['backend']['environment']['GOTP_DB_MYSQL_HOST'] = 'synthetic-mysql'
        with self.assertRaisesRegex(RuntimeError, 'MySQL backend'): self.check(cfg)

    def test_mysql_driver_with_pg_fields_is_rejected(self):
        for selector in ('GOTP_DB_DRIVER', 'DB_TYPE', 'GOTP_DB_TYPE', 'DATABASE_TYPE', 'GOTP_DB_PSQL_DBTYPE'):
            with self.subTest(selector=selector):
                cfg = copy.deepcopy(self.config)
                cfg['services']['backend']['environment'][selector] = 'mysql'
                with self.assertRaisesRegex(RuntimeError, 'Cannot confirm'): self.check(cfg)

    def test_unrelated_mysql_environment_does_not_override_pg(self):
        cfg = copy.deepcopy(self.config)
        cfg['services']['backend']['environment']['MYSQL_EXPORTER_NOTE'] = 'synthetic'
        self.check(cfg)

    def test_unresolved_host_is_not_treated_as_external_pg(self):
        cfg = copy.deepcopy(self.config)
        cfg['services']['backend']['environment']['GOTP_DB_PSQL_HOST'] = '${DATABASE_HOST}'
        with self.assertRaisesRegex(RuntimeError, 'Cannot confirm'):
            self.check(cfg)

    def test_upgrade_rejects_symlinked_compose_before_download(self):
        with tempfile.TemporaryDirectory(prefix='tp-b-symlink-guard-') as directory:
            root = Path(directory)
            script = root / 'upgrade.sh'
            script.write_bytes((ROOT / 'upgrade.sh').read_bytes())
            real = root / 'original-compose'
            real.write_text('synthetic original')
            (root / 'docker-compose.yml').symlink_to(real)
            curl = root / 'curl'
            curl.write_text('#!/bin/sh\necho called > "$TP_FIXTURE_CURL_LOG"\nexit 42\n')
            curl.chmod(0o755)
            env = os.environ.copy()
            env.update(PATH=str(root) + os.pathsep + env['PATH'], TP_FIXTURE_CURL_LOG=str(root / 'curl.log'))
            proc = subprocess.run(['bash', str(script), 'v-test'], cwd=root, env=env, capture_output=True, text=True, timeout=30)
            self.assertNotEqual(proc.returncode, 0)
            self.assertEqual(real.read_text(), 'synthetic original')
            self.assertFalse((root / 'curl.log').exists())

    def test_install_rejects_symlinked_install_directory(self):
        with tempfile.TemporaryDirectory(prefix='tp-b-rootlink-guard-') as directory:
            root = Path(directory)
            script = root / 'install.bash'
            script.write_bytes((ROOT / 'install.bash').read_bytes())
            target = root / 'real'
            target.mkdir()
            link = root / 'install-link'
            link.symlink_to(target, target_is_directory=True)
            docker = root / 'docker'
            docker.write_text('#!/bin/sh\nif [ "$1" = version ]; then echo 25.0.0; exit 0; fi\nif [ "$1" = compose ] && [ "$2" = version ]; then echo 2.20.0; exit 0; fi\nexit 99\n')
            docker.chmod(0o755)
            env = os.environ.copy()
            env.update(PATH=str(root) + os.pathsep + env['PATH'], INSTALL_DIR=str(link))
            proc = subprocess.run(['bash', str(script)], cwd=root, env=env, capture_output=True, text=True, timeout=30)
            self.assertNotEqual(proc.returncode, 0)
            self.assertIn('旧配置未修改', proc.stderr)
            self.assertEqual(list(target.iterdir()), [])

    def test_missing_pg_backend_or_required_postgres_service_rejected(self):
        for change in ('backend', 'postgres'):
            with self.subTest(missing=change):
                cfg=copy.deepcopy(self.config); del cfg['services'][change]
                with self.assertRaisesRegex(RuntimeError, 'Cannot confirm'): self.check(cfg)

    def test_external_pg_host_not_rejected_merely_for_missing_local_service(self):
        cfg=copy.deepcopy(self.config)
        cfg['services']['backend']['environment']['GOTP_DB_PSQL_HOST']='synthetic-external-pg'
        cfg['services']['backend']['environment']['GOTP_RELATIONAL_POSTGRES_PLATFORM_HOST']='synthetic-external-pg'
        del cfg['services']['postgres']
        self.check(cfg)

    def test_mysql_thingsvis_still_rejected_without_leaking_url(self):
        cfg=copy.deepcopy(self.config)
        cfg['services']['thingsvis-server']['environment']['DATABASE_URL']='mysql://synthetic-secret@fixture/database'
        with self.assertRaises(RuntimeError) as error:self.check(cfg)
        self.assertNotIn('synthetic-secret',str(error.exception))

    def test_duplicate_list_environment_rejected(self):
        cfg=copy.deepcopy(self.config); values=cfg['services']['backend']['environment']
        cfg['services']['backend']['environment']=[f'{key}={value}' for key,value in values.items()]+['GOTP_DB_PSQL_HOST=other']
        with self.assertRaisesRegex(RuntimeError, 'Cannot confirm'): self.check(cfg)

    def test_cli_rejects_backend_before_creating_configuration(self):
        cfg = copy.deepcopy(self.config)
        cfg['services']['backend']['environment']['GOTP_DB_MYSQL_HOST'] = 'synthetic-mysql'
        with tempfile.TemporaryDirectory(prefix='tp-b-backend-guard-') as directory:
            root = Path(directory)
            script = root / 'config/native/prepare.py'
            script.parent.mkdir(parents=True)
            script.write_bytes((ROOT / 'config/native/prepare.py').read_bytes())
            docker = root / 'docker'
            docker.write_text('#!/bin/sh\nprintf "%s" "$TP_FIXTURE_COMPOSE"\n')
            docker.chmod(0o755)
            env = os.environ.copy()
            env.update(PATH=str(root) + os.pathsep + env['PATH'], TP_FIXTURE_COMPOSE=json.dumps(cfg))
            proc = subprocess.run(['python3', str(script), '--bootstrap'], cwd=root,
                                  env=env, capture_output=True, text=True, timeout=30)
            self.assertNotEqual(proc.returncode, 0)
            self.assertIn('MySQL backend', proc.stderr)
            self.assertFalse((root / '.env').exists())
            self.assertFalse((root / 'config/native/plugin-token').exists())

    def test_linux_entries_reject_old_mysql_backend_before_download_or_overwrite(self):
        cfg = copy.deepcopy(self.config)
        cfg['services']['backend']['environment']['DB_TYPE'] = 'mysql'
        for entry in ('install.bash', 'upgrade.sh'):
            with self.subTest(entry=entry), tempfile.TemporaryDirectory(prefix='tp-b-entry-guard-') as directory:
                root = Path(directory)
                target = root / entry
                target.write_bytes((ROOT / entry).read_bytes())
                compose = root / 'docker-compose.yml'
                compose.write_text('existing synthetic compose\n')
                before = compose.read_bytes()
                docker = root / 'docker'
                docker.write_text('#!/bin/sh\nif [ "$1" = version ]; then echo 25.0.0; exit 0; fi\nif [ "$1" = compose ] && [ "$2" = version ]; then echo 2.20.0; exit 0; fi\nif [ "$1" = compose ] && [ "$2" = config ]; then printf "%s" "$TP_FIXTURE_COMPOSE"; exit 0; fi\nexit 99\n')
                docker.chmod(0o755)
                curl = root / 'curl'
                curl.write_text('#!/bin/sh\necho called >> "$TP_FIXTURE_CURL_LOG"\nexit 42\n')
                curl.chmod(0o755)
                env = os.environ.copy()
                env.update(PATH=str(root) + os.pathsep + env['PATH'], INSTALL_DIR=str(root), TMPDIR=str(root),
                           TP_FIXTURE_COMPOSE=json.dumps(cfg), TP_FIXTURE_CURL_LOG=str(root/'curl.log'))
                args=['bash',str(target)] + (['v-test'] if entry == 'upgrade.sh' else [])
                proc=subprocess.run(args,cwd=root,env=env,capture_output=True,text=True,timeout=30)
                self.assertNotEqual(proc.returncode,0)
                self.assertEqual(compose.read_bytes(),before)
                self.assertFalse((root/'curl.log').exists())

if __name__ == '__main__': unittest.main()
