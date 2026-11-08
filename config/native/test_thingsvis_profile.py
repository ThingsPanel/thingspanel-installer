import importlib.util
import json
import os
import re
from pathlib import Path
import shutil
import shlex
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).with_name('prepare.py')
spec = importlib.util.spec_from_file_location('native_prepare', SCRIPT)
prepare = importlib.util.module_from_spec(spec)
spec.loader.exec_module(prepare)


class ThingsVisDatabaseGuardTest(unittest.TestCase):
    def configuration(self, url):
        # The existing entry guards now require both platform and ThingsVis
        # datasource evidence. A ThingsVis-only fixture is unknown, not PG.
        return {'services': {'thingsvis-server': {'environment': {'DATABASE_URL': url}},
                'backend': {'environment': {'GOTP_DB_PSQL_HOST': 'postgres',
                'GOTP_DB_PSQL_PORT': '5432', 'GOTP_DB_PSQL_USERNAME': 'postgres',
                'GOTP_DB_PSQL_DBNAME': 'ThingsPanel'}}, 'postgres': {}}}

    def test_postgresql_schemes_are_allowed(self):
        for url in ('postgres://example/db', 'postgresql://example/db?schema=thingsvis'):
            with self.subTest(url=url):
                prepare.validate_thingsvis_database(self.configuration(url))


    def test_mysql_and_missing_configuration_are_rejected_without_echoing_values(self):
        for config in (self.configuration('mysql://example-secret@example/db'),
                       self.configuration(None), {'services': {}}):
            with self.subTest(config=config):
                with self.assertRaises(RuntimeError) as result:
                    prepare.validate_thingsvis_database(config)
                self.assertNotIn('example-secret', str(result.exception))

    def test_actual_compose_renders_a_supported_url(self):
        root = SCRIPT.parents[2]
        result = subprocess.run(['docker', 'compose', '-f', str(root / 'docker-compose.yml'),
                                 'config', '--no-interpolate', '--format', 'json'], check=True, capture_output=True, text=True)
        prepare.validate_thingsvis_database(json.loads(result.stdout))

        env = os.environ.copy()
        for key in ('TP_AUTOMATION_TOKEN', 'TP_MQTT_USER', 'TP_MQTT_PASSWORD',
                    'TP_JWT_KEY', 'TP_BROKER_DATABASE_DSN', 'TP_NATIVE_DB_PASSWORD',
                    'TP_PROTOCOL_MQTT_USER', 'TP_PROTOCOL_MQTT_PASSWORD',
                    'TP_NOTIFICATION_KEYRING_B64', 'TP_THINGSVIS_AUTH_SECRET', 'TP_MARKETINSTALL_PROVIDER_TOKEN',
                    'TP_MARKETINSTALL_THINGSVIS_IDENTITY_TOKEN', 'TP_MARKETINSTALL_THINGSVIS_TOKEN'):
            env[key] = 'synthetic-test-value'
        resolved = subprocess.run(['docker', 'compose', '-f', str(root / 'docker-compose.yml'),
                                   'config', '--format', 'json'], check=True, capture_output=True, text=True, env=env)
        prepare.validate_thingsvis_database(json.loads(resolved.stdout))

    def test_cli_rejects_mysql_before_creating_configuration(self):
        with tempfile.TemporaryDirectory(prefix='thingsvis-guard-test-') as directory:
            root = Path(directory)
            script = root / 'config' / 'native' / 'prepare.py'
            script.parent.mkdir(parents=True)
            shutil.copyfile(SCRIPT, script)
            fake_docker = root / 'docker'
            fake_docker.write_text('#!/bin/sh\nprintf "%s" "$TP_TEST_COMPOSE_JSON"\n')
            fake_docker.chmod(0o755)
            env = os.environ.copy()
            env['PATH'] = str(root) + os.pathsep + env['PATH']
            env['TP_TEST_COMPOSE_JSON'] = json.dumps(self.configuration('mysql://private-value@example/db'))
            result = subprocess.run(['python3', str(script), '--bootstrap'], env=env,
                                    capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('only a PostgreSQL datasource', result.stderr)
            self.assertNotIn('private-value', result.stderr + result.stdout)
            self.assertFalse((root / '.env').exists())
            self.assertFalse((root / 'config' / 'native' / 'plugin-token').exists())

    def test_unix_install_and_upgrade_check_old_compose_before_network_or_backup(self):
        repo = SCRIPT.parents[2]
        for entry in ('install.bash', 'upgrade.sh'):
            for scheme in ('mysql', 'postgresql'):
                with self.subTest(entry=entry, scheme=scheme), tempfile.TemporaryDirectory(prefix='tp-entry-guard-') as directory:
                    root = Path(directory)
                    target = root / entry
                    shutil.copyfile(repo / entry, target)
                    compose = root / 'docker-compose.yml'
                    compose.write_text('synthetic old compose - must be retained\n')
                    before = compose.read_bytes()
                    bin_dir = root / 'bin'
                    bin_dir.mkdir()
                    docker = bin_dir / 'docker'
                    docker.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$TP_TEST_DOCKER_LOG"\n'
                                      'if [ "$1" = version ]; then echo 25.0.0; exit 0; fi\n'
                                      'if [ "$1" = compose ] && [ "$2" = version ]; then echo 2.20.0; exit 0; fi\n'
                                      'if [ "$1" = compose ] && [ "$2" = config ]; then printf "%s" "$TP_TEST_COMPOSE_JSON"; exit 0; fi\n'
                                      'exit 99\n')
                    docker.chmod(0o755)
                    curl = bin_dir / 'curl'
                    curl.write_text('#!/bin/sh\nprintf "called\\n" >> "$TP_TEST_CURL_LOG"\nexit 42\n')
                    curl.chmod(0o755)
                    env = os.environ.copy()
                    env.update(PATH=str(bin_dir) + os.pathsep + env['PATH'], INSTALL_DIR=str(root), TMPDIR=str(root),
                               TP_TEST_DOCKER_LOG=str(root / 'docker.log'), TP_TEST_CURL_LOG=str(root / 'curl.log'),
                               TP_TEST_COMPOSE_JSON=json.dumps(self.configuration(scheme + '://sensitive-marker@example/db')))
                    args = ['bash', str(target)] + (['v-test'] if entry == 'upgrade.sh' else [])
                    result = subprocess.run(args, env=env, cwd=root, capture_output=True, text=True, timeout=30)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertEqual(compose.read_bytes(), before)
                    self.assertNotIn('sensitive-marker', result.stdout + result.stderr)
                    calls = (root / 'docker.log').read_text().splitlines()
                    self.assertTrue(any(line.startswith('compose config ') for line in calls))
                    self.assertFalse(any(line.startswith(('compose up', 'compose down', 'pull')) for line in calls))
                    if scheme == 'mysql':
                        self.assertFalse((root / 'curl.log').exists())
                        self.assertFalse(list(root.glob('docker-compose.yml.bak.*')))
                    else:
                        self.assertTrue((root / 'curl.log').exists(), 'valid PostgreSQL should pass the guard')

    def test_unix_entries_with_historical_compose_and_mysql_variant(self):
        repo = SCRIPT.parents[2]
        real_docker = shutil.which('docker')
        self.assertIsNotNone(real_docker)
        for tag in ('v1.1.13.6', 'v1.2.12.1'):
            original = subprocess.run(['git', '-C', str(repo), 'show', tag + ':docker-compose.yml'],
                                      check=True, capture_output=True, text=True).stdout
            url_pattern = r'(DATABASE_URL\s*(?:=|:\s*))postgresql://'
            self.assertRegex(original, url_pattern)
            for scheme in ('postgresql', 'mysql'):
                content = re.sub(url_pattern, lambda match: match.group(1) + scheme + '://', original)
                for entry in ('install.bash', 'upgrade.sh'):
                    with self.subTest(tag=tag, scheme=scheme, entry=entry), tempfile.TemporaryDirectory(prefix='tp-history-guard-') as directory:
                        root = Path(directory)
                        script = root / entry
                        shutil.copyfile(repo / entry, script)
                        compose = root / 'docker-compose.yml'
                        compose.write_text(content)
                        bin_dir = root / 'bin'
                        bin_dir.mkdir()
                        docker = bin_dir / 'docker'
                        docker.write_text('#!/bin/sh\n'
                                          'if [ "$1" = version ]; then echo 25.0.0; exit 0; fi\n'
                                          'if [ "$1" = compose ] && [ "$2" = version ]; then echo 2.20.0; exit 0; fi\n'
                                          'if [ "$1" = compose ] && [ "$2" = config ]; then exec ' + shlex.quote(real_docker) + ' "$@"; fi\n'
                                          'exit 99\n')
                        docker.chmod(0o755)
                        curl = bin_dir / 'curl'
                        curl.write_text('#!/bin/sh\nprintf "called\\n" >> "$TP_TEST_CURL_LOG"\nexit 42\n')
                        curl.chmod(0o755)
                        env = os.environ.copy()
                        env.pop('COMPOSE_FILE', None)
                        env.update(PATH=str(bin_dir) + os.pathsep + env['PATH'], INSTALL_DIR=str(root), TMPDIR=str(root),
                                   POSTGRES_PASSWORD='synthetic-test-password', TP_TEST_CURL_LOG=str(root / 'curl.log'))
                        for required in re.findall(r'\$\{([A-Z0-9_]+):\?', content):
                            env[required] = 'synthetic-test-value'
                        args = ['bash', str(script)] + (['v-test'] if entry == 'upgrade.sh' else [])
                        result = subprocess.run(args, env=env, cwd=root, capture_output=True, text=True, timeout=30)
                        self.assertNotEqual(result.returncode, 0)
                        self.assertEqual(compose.read_text(), content)
                        self.assertNotIn('synthetic-test-password', result.stdout + result.stderr)
                        if scheme == 'mysql':
                            self.assertFalse((root / 'curl.log').exists())
                            self.assertFalse(list(root.glob('docker-compose.yml.bak.*')))
                        else:
                            self.assertTrue((root / 'curl.log').exists(), 'historical PostgreSQL must pass the guard')


class BackendDatabaseGuardTest(unittest.TestCase):
    def configuration(self, *, host='postgres', password='postgresThingsPanel'):
        env = {
            'GOTP_DB_PSQL_HOST': host,
            'GOTP_DB_PSQL_PORT': '5432',
            'GOTP_DB_PSQL_USERNAME': 'postgres',
            'GOTP_DB_PSQL_DBNAME': 'ThingsPanel',
            'GOTP_RELATIONAL_DRIVER': 'postgres',
            'GOTP_RELATIONAL_POSTGRES_PLATFORM_HOST': host,
            'GOTP_RELATIONAL_POSTGRES_PLATFORM_PORT': '5432',
            'GOTP_RELATIONAL_POSTGRES_PLATFORM_DATABASE': 'ThingsPanel',
            'GOTP_RELATIONAL_POSTGRES_PLATFORM_USERNAME': 'postgres',
            'GOTP_RELATIONAL_POSTGRES_PLATFORM_PASSWORD': password,
        }
        return {'services': {'backend': {'environment': env}, 'postgres': {}}}

    def test_unresolved_password_is_allowed_before_generated_env_secrets_exist(self):
        prepare.validate_supported_backend_database(
            self.configuration(password='${TP_DATABASE_PASSWORD:-postgresThingsPanel}'))

    def test_unresolved_database_location_is_not_allowed(self):
        with self.assertRaisesRegex(RuntimeError, 'supported PostgreSQL'):
            prepare.validate_supported_backend_database(self.configuration(host='${DATABASE_HOST:-postgres}'))


if __name__ == '__main__':
    unittest.main()
