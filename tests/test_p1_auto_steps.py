"""P1: migrations (33.sql) and Broker device scope run without manual commands. No Docker, no database."""
import hashlib
import importlib.util
import concurrent.futures
import subprocess
import tempfile
import threading
import time
import unittest
from pathlib import Path
from unittest import mock

REPO = Path(__file__).resolve().parents[1]


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, REPO / path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


prepare = load('tp_prepare_p1', 'config/native/prepare.py')
scope_sync = load('tp_scope_sync_p1', 'tools/scope_sync.py')

ENV = {'TP_MQTT_USER': 'tp-community-platform', 'TP_MQTT_PASSWORD': 'p' * 32, 'TP_PROTOCOL_MQTT_USER': 'tp-community-protocols',
       'TP_PROTOCOL_MQTT_PASSWORD': 'q' * 32, 'TP_BROKER_CELL': 'community', 'TP_BROKER_INSTANCE': 'community-mosquitto',
       'TP_MQTT_CLIENT_ID': 'tp-community-backend'}
MQTT = [{'id': 'mqtt-1', 'tenant_id': 't1'}]
PLUGIN = [{'id': 'ha-1', 'tenant_id': 't1'}, {'id': 'ha-2', 'tenant_id': 't2'}]


class ScopeServicesTests(unittest.TestCase):
    def test_platform_account_covers_plugin_devices_as_well_as_mqtt_devices(self):
        platform, protocols = prepare.scope_services(ENV, MQTT, PLUGIN)
        self.assertEqual(platform['username'], 'tp-community-platform')
        self.assertEqual(platform['allowed_device_ids'], ['mqtt-1', 'ha-1', 'ha-2'])
        self.assertEqual(platform['allowed_tenant_ids'], ['t1', 't2'])
        self.assertIn('devices/command/+/+', platform['policy']['publish'])
        self.assertEqual(protocols['allowed_device_ids'], ['ha-1', 'ha-2'])
        self.assertTrue(protocols['plugin_socket'] and protocols['allow_any_client_id'])

    def test_only_plugin_devices_still_gives_the_platform_account_publish_rights(self):
        # T7b finding 3: with no plain MQTT device the publish list was empty and commands were dropped silently.
        platform, _ = prepare.scope_services(ENV, [], PLUGIN)
        self.assertTrue(platform['policy']['publish'])
        self.assertEqual(platform['allowed_device_ids'], ['ha-1', 'ha-2'])

    def test_no_devices_at_all_is_an_empty_scope_without_publish_rights(self):
        platform, protocols = prepare.scope_services(ENV, [], [])
        self.assertEqual((platform['allowed_device_ids'], platform['policy']['publish']), ([], []))
        self.assertEqual(protocols['allowed_device_ids'], [])

    def test_a_device_in_both_lists_is_listed_once(self):
        platform, _ = prepare.scope_services(ENV, MQTT + PLUGIN[:1], PLUGIN)
        self.assertEqual(platform['allowed_device_ids'], ['mqtt-1', 'ha-1', 'ha-2'])

    def test_plugin_scope_does_not_leak_mqtt_devices(self):
        _, protocols = prepare.scope_services(ENV, MQTT, PLUGIN)
        self.assertNotIn('mqtt-1', protocols['allowed_device_ids'])


class SyncScopeTests(unittest.TestCase):
    def run_sync(self, current, targets, plugins):
        calls = []
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        config = root / 'config/native'
        (config / 'broker').mkdir(parents=True)
        patches = [mock.patch.object(prepare, 'query_scopes', return_value=(targets, plugins)),
                   mock.patch.object(prepare, 'current_services', return_value=current),
                   mock.patch.object(prepare, 'save', side_effect=lambda *a, **k: calls.append(('save', a[0]))),
                   mock.patch.object(prepare, 'ROOT', root),
                   mock.patch.object(prepare, 'CONFIG', config)]
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)
        changed = prepare.sync_scope(ENV, restart=lambda name: calls.append(('restart', name)))
        return changed, calls

    def test_unchanged_scope_neither_writes_nor_restarts(self):
        same = prepare.scope_services(ENV, MQTT, PLUGIN)
        changed, calls = self.run_sync(same, MQTT, PLUGIN)
        self.assertFalse(changed)
        self.assertEqual(calls, [])

    def test_new_plugin_device_writes_then_restarts_owner_before_mosquitto(self):
        old = prepare.scope_services(ENV, MQTT, PLUGIN[:1])
        changed, calls = self.run_sync(old, MQTT, PLUGIN)
        self.assertTrue(changed)
        self.assertEqual(calls, [('save', 'services.json'), ('restart', 'broker-owner'), ('restart', 'mosquitto')])

    def test_deleted_plugin_device_also_triggers_a_refresh(self):
        old = prepare.scope_services(ENV, MQTT, PLUGIN)
        changed, calls = self.run_sync(old, MQTT, PLUGIN[:1])
        self.assertTrue(changed)
        self.assertEqual([c for c in calls if c[0] == 'restart'], [('restart', 'broker-owner'), ('restart', 'mosquitto')])

    def test_first_run_without_a_services_file_writes_it(self):
        changed, calls = self.run_sync(None, [], PLUGIN)
        self.assertTrue(changed)
        self.assertEqual(calls[0], ('save', 'services.json'))

    def test_a_failed_save_restarts_nothing(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = root / 'config/native'
            (config / 'broker').mkdir(parents=True)
            patches = [mock.patch.object(prepare, 'ROOT', root),
                mock.patch.object(prepare, 'CONFIG', config),
                mock.patch.object(prepare, 'query_scopes', return_value=([], PLUGIN)),
                mock.patch.object(prepare, 'current_services', return_value=None), \
                mock.patch.object(prepare, 'save', side_effect=RuntimeError('locked'))]
            for patch in patches:
                patch.start()
                self.addCleanup(patch.stop)
            restarted = []
            with self.assertRaises(RuntimeError):
                prepare.sync_scope(ENV, restart=restarted.append)
            self.assertEqual(restarted, [])
            self.assertTrue((config / 'broker/.scope-sync-restart.json').is_file())

    def test_failed_restart_is_retried_even_when_scope_file_is_already_current(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = root / 'config/native'
            (config / 'broker').mkdir(parents=True)
            current = {'value': prepare.scope_services(ENV, MQTT, PLUGIN[:1])}
            calls = []
            patches = [
                mock.patch.object(prepare, 'ROOT', root),
                mock.patch.object(prepare, 'CONFIG', config),
                mock.patch.object(prepare, 'query_scopes', return_value=(MQTT, PLUGIN)),
                mock.patch.object(prepare, 'current_services', side_effect=lambda: current['value']),
                mock.patch.object(prepare, 'save', side_effect=lambda _name, value, _broker, **_kw: current.update(value=value)),
            ]
            for patch in patches:
                patch.start()
                self.addCleanup(patch.stop)

            def fail_mosquitto_once(name):
                calls.append(name)
                if calls == ['broker-owner', 'mosquitto']:
                    raise RuntimeError('simulated mosquitto restart failure')

            with self.assertRaisesRegex(RuntimeError, 'simulated'):
                prepare.sync_scope(ENV, restart=fail_mosquitto_once)
            self.assertEqual(current['value'], prepare.scope_services(ENV, MQTT, PLUGIN))
            self.assertTrue((config / 'broker/.scope-sync-restart.json').is_file())

            calls.clear()
            self.assertFalse(prepare.sync_scope(ENV, restart=calls.append))
            self.assertEqual(calls, ['broker-owner', 'mosquitto'])
            self.assertFalse((config / 'broker/.scope-sync-restart.json').exists())

    def test_concurrent_scope_pass_cannot_overlap_restart_sequence(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = root / 'config/native'
            (config / 'broker').mkdir(parents=True)
            current = {'value': prepare.scope_services(ENV, MQTT, PLUGIN[:1])}
            patches = [
                mock.patch.object(prepare, 'ROOT', root),
                mock.patch.object(prepare, 'CONFIG', config),
                mock.patch.object(prepare, 'query_scopes', return_value=(MQTT, PLUGIN)),
                mock.patch.object(prepare, 'current_services', side_effect=lambda: current['value']),
                mock.patch.object(prepare, 'save', side_effect=lambda _name, value, _broker, **_kw: current.update(value=value)),
            ]
            for patch in patches:
                patch.start()
                self.addCleanup(patch.stop)
            entered, release = threading.Event(), threading.Event()

            def blocked_restart(name):
                entered.set()
                self.assertTrue(release.wait(2))

            with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
                first = pool.submit(prepare.sync_scope, ENV, blocked_restart)
                self.assertTrue(entered.wait(2))
                second = pool.submit(prepare.sync_scope, ENV, lambda _name: self.fail('overlapping restart'))
                with self.assertRaises(Exception) as raised:
                    second.result(timeout=2)
                self.assertEqual(type(raised.exception).__name__, 'UpdateError')
                release.set()
                self.assertTrue(first.result(timeout=2))

    def test_refresh_devices_uses_the_same_scope_with_plugins(self):
        saved = {}
        with mock.patch.object(prepare, 'query_scopes', return_value=([], PLUGIN)), \
                mock.patch.object(prepare, 'save', side_effect=lambda name, value, broker=False: saved.update({name: value})):
            prepare.refresh(ENV)
        self.assertEqual(saved['services.json'][0]['allowed_device_ids'], ['ha-1', 'ha-2'])


class FakeLedgerDatabase:
    """Just enough of psql for apply_migrations: records installer_migrations rows, counts executed migrations."""
    def __init__(self):
        self.rows, self.executed = {}, []

    def sql(self, database, statement):
        if statement.startswith('CREATE TABLE IF NOT EXISTS installer_migrations'):
            return ''
        if statement.startswith('SELECT checksum'):
            key = tuple(statement.split("component='")[1].split("' AND version=")[i] for i in (0, 1))
            return self.rows.get((key[0], int(key[1])), '')
        if statement.startswith('BEGIN'):
            component = statement.split("INSERT INTO installer_migrations VALUES('")[1].split("'")[0]
            number = int(statement.split("',")[1].split(",")[0])
            checksum = statement.split("'")[-2]
            self.rows[(component, number)] = checksum
            self.executed.append(number)
            return ''
        raise AssertionError('unexpected statement: ' + statement[:60])


class ExtraMigrationTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.file = Path(self.directory.name) / '33.sql'
        self.file.write_text('CREATE TABLE IF NOT EXISTS plugin_runtime_identities (instance_id varchar(64) PRIMARY KEY);\n')
        self.db = FakeLedgerDatabase()
        patch = mock.patch.object(prepare, 'sql', side_effect=self.db.sql)
        patch.start()
        self.addCleanup(patch.stop)

    def test_33_sql_is_applied_once_and_recorded_with_its_checksum(self):
        prepare.apply_migrations('ThingsPanel', prepare.EXTRA_COMMUNITY_COMPONENT, [self.file])
        self.assertEqual(self.db.executed, [33])
        expected = hashlib.sha256(self.file.read_text().encode()).hexdigest()
        self.assertEqual(self.db.rows[(prepare.EXTRA_COMMUNITY_COMPONENT, 33)], expected)

    def test_repeating_install_or_upgrade_does_not_run_it_again(self):
        for _ in range(3):
            prepare.apply_migrations('ThingsPanel', prepare.EXTRA_COMMUNITY_COMPONENT, [self.file])
        self.assertEqual(self.db.executed, [33])

    def test_an_edited_file_is_refused_instead_of_silently_skipped(self):
        prepare.apply_migrations('ThingsPanel', prepare.EXTRA_COMMUNITY_COMPONENT, [self.file])
        self.file.write_text('DROP TABLE plugin_runtime_identities;')
        with self.assertRaises(RuntimeError):
            prepare.apply_migrations('ThingsPanel', prepare.EXTRA_COMMUNITY_COMPONENT, [self.file])

    def test_the_extra_list_names_the_plugin_identity_schema_only(self):
        self.assertEqual(prepare.EXTRA_COMMUNITY_SQL, ('33.sql',))


class BootstrapAppliesExtraSqlTests(unittest.TestCase):
    """bootstrap() end to end with Docker and psql faked: 33.sql comes out of the backend image's sql directory."""
    def test_bootstrap_runs_33_sql_and_the_broker_files_and_skips_it_for_an_older_image(self):
        for with_33 in (True, False):
            applied = []
            real_copy = {}

            def command(*args, data=None):
                if args[:3] == ('docker', 'compose', 'config'):
                    return '{"services": {"backend": {"image": "backend:test"}}}'
                if args[:2] == ('docker', 'create'):
                    return 'cid'
                if args[:2] == ('docker', 'cp'):
                    source, target = args[2].split(':', 1)[1], Path(args[3])
                    real_copy[source] = target
                    if source.endswith('/sql'):
                        (target / 'broker').mkdir(parents=True)
                        (target / 'broker' / '001_x.sql').write_text('select 1;')
                        if with_33:
                            (target / '33.sql').write_text('select 33;')
                    elif source.endswith('global.go'):
                        target.write_text('VERSION_NUMBER = 0\n')
                    else:
                        target.mkdir(parents=True)
                        (target / '001_x.up.sql').write_text('select 1;')
                return ''

            def apply(database, component, paths):
                applied.append((component, [p.name for p in paths]))

            with mock.patch.object(prepare, 'command', side_effect=command), \
                    mock.patch.object(prepare, 'sql', return_value='0'), \
                    mock.patch.object(prepare, 'image_has_marker', return_value=with_33), \
                    mock.patch.object(prepare, 'apply_migrations', side_effect=apply):
                prepare.bootstrap()
            self.assertEqual(applied[0], (prepare.EXTRA_COMMUNITY_COMPONENT, ['33.sql'] if with_33 else []), with_33)
            self.assertEqual(applied[1], ('broker', ['001_x.sql']))


class ScopeSyncToolTests(unittest.TestCase):
    def result(self, code=0, out=b''):
        return subprocess.CompletedProcess([], code, stdout=out, stderr=b'')

    def test_run_once_reports_success_and_failure_and_never_raises(self):
        self.assertEqual(scope_sync.run_once('/s', runner=lambda *a, **k: self.result(0, b'ok\n')), (True, 'ok'))
        self.assertFalse(scope_sync.run_once('/s', runner=lambda *a, **k: self.result(1, b'locked'))[0])

        def boom(*a, **k):
            raise subprocess.TimeoutExpired('x', 1)
        ok, text = scope_sync.run_once('/s', runner=boom)
        self.assertFalse(ok)
        self.assertNotIn('/s', text)

    def test_run_once_calls_the_same_entrypoint_as_the_installer(self):
        seen = {}
        scope_sync.run_once('/stack', runner=lambda cmd, **k: seen.update(cmd=cmd, cwd=k['cwd']) or self.result())
        self.assertEqual(seen['cmd'][1:], ['config/native/prepare.py', '--sync-scope'])
        self.assertEqual(seen['cwd'], '/stack')

    def test_watch_keeps_going_after_a_failed_pass_and_logs_only_changes(self):
        outputs = iter([self.result(0, b'unchanged'), self.result(0, b'unchanged'), self.result(1, b'locked'), self.result(0, b'changed; restarting')])
        lines, sleeps = [], []
        scope_sync.watch('/s', 7, runner=lambda *a, **k: next(outputs), sleep=sleeps.append, rounds=4, log=lines.append)
        self.assertEqual(len(lines), 3)
        self.assertIn('FAILED', lines[1])
        self.assertEqual(sleeps, [7, 7, 7])

    def test_ensure_starts_once_and_reuses_a_live_watcher(self):
        with tempfile.TemporaryDirectory() as temporary:
            stack = Path(temporary) / 'stack'
            (stack / 'config/native').mkdir(parents=True)
            (stack / 'config/native/update.py').write_bytes((REPO / 'config/native/update.py').read_bytes())
            state = Path(temporary) / 'agent'
            spawned = []

            class Process:
                pid = 4242

            def spawn(cmd, **kwargs):
                spawned.append((cmd, kwargs))
                return Process()

            alive = lambda pid: 'python3 /x/tools/scope_sync.py watch --stack-dir /s'
            self.assertEqual(scope_sync.ensure(stack, state, spawn=spawn, ps=alive), 'started')
            self.assertEqual(scope_sync.ensure(stack, state, spawn=spawn, ps=alive), 'running')
            self.assertEqual(len(spawned), 1)
            self.assertTrue(spawned[0][1]['start_new_session'])
            self.assertEqual(spawned[0][0][2], 'watch')
            # a recycled pid that now belongs to something else is not a watcher
            self.assertEqual(scope_sync.ensure(stack, state, spawn=spawn, ps=lambda pid: 'nginx'), 'started')
            self.assertEqual(len(spawned), 2)

    def test_concurrent_ensure_starts_at_most_one_watcher(self):
        with tempfile.TemporaryDirectory() as temporary:
            stack = Path(temporary) / 'stack'
            (stack / 'config/native').mkdir(parents=True)
            (stack / 'config/native/update.py').write_bytes((REPO / 'config/native/update.py').read_bytes())
            state = Path(temporary) / 'agent'
            spawned, entered, release = [], threading.Event(), threading.Event()

            class Process:
                pid = 9876

            def spawn(_cmd, **_kwargs):
                spawned.append(True)
                entered.set()
                release.wait(2)
                return Process()

            alive = lambda _pid: 'python3 scope_sync.py watch --stack-dir ' + str(stack)
            with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
                first = pool.submit(scope_sync.ensure, stack, state, 20, spawn, alive)
                self.assertTrue(entered.wait(2))
                second = pool.submit(scope_sync.ensure, stack, state, 20, spawn, alive)
                time.sleep(0.05)
                release.set()
                results = [first.result(timeout=2), second.result(timeout=2)]
            self.assertEqual(len(spawned), 1)
            self.assertEqual(results.count('started'), 1)
            other = next(result for result in results if result != 'started')
            self.assertIn(other, ('busy', 'running'))


if __name__ == '__main__':
    unittest.main()
