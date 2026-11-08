"""Installer filesystem transactions only; no network, Docker daemon or DB.

The real Transaction implementation is exercised. Download and Compose output
are the explicit I/O substitutes. Crash workers are ordinary child processes.
"""
import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

REPO = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location('tp_update', REPO / 'config/native/update.py')
update = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(update)
CONFIG = {'services': {'backend': {'image': 'registry/backend:v-fixture', 'environment': {
    'GOTP_DB_PSQL_HOST': 'postgres', 'GOTP_DB_PSQL_PORT': '5432',
    'GOTP_DB_PSQL_USERNAME': 'postgres', 'GOTP_DB_PSQL_DBNAME': 'ThingsPanel',
    'GOTP_RELATIONAL_DRIVER': 'postgres',
    'GOTP_RELATIONAL_POSTGRES_PLATFORM_HOST': 'postgres',
    'GOTP_RELATIONAL_POSTGRES_PLATFORM_PORT': '5432',
    'GOTP_RELATIONAL_POSTGRES_PLATFORM_DATABASE': 'ThingsPanel',
    'GOTP_RELATIONAL_POSTGRES_PLATFORM_USERNAME': 'postgres',
    'GOTP_RELATIONAL_POSTGRES_PLATFORM_PASSWORD': 'synthetic-fixture'}},
    'postgres': {}, 'thingsvis-server': {'environment': {
        'DATABASE_URL': 'postgresql://fixture:fixture@postgres/ThingsPanel'}}}}
BASE = 'https://installer.invalid'


def payload_download(url, path):
    relative = url[len(BASE) + 1:]
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes((REPO / relative).read_bytes())


class TransactionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='tp-update-unit-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / 'install'
        self.root.mkdir()
        self.render = mock.patch.object(update, 'render_with_fallback', return_value=copy.deepcopy(CONFIG))
        self.render.start()
        self.addCleanup(self.render.stop)

    def tx(self, **kwargs):
        return update.Transaction(self.root, downloader=payload_download, **kwargs)

    def originals(self, tx=None):
        tx = tx or self.tx()
        for relative, _ in tx.artifacts:
            path = self.root / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text('original ' + relative + '\n', encoding='utf-8')
            path.chmod(0o600)
        for relative in update.WATCHED:
            path = self.root / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text('synthetic-local-only\n', encoding='utf-8')
            path.chmod(0o600)
        result = {p: update.file_info(self.root / p) for p, _ in tx.artifacts}
        result.update({p: update.file_info(self.root / p) for p in update.WATCHED})
        return result

    def assertOriginals(self, original):
        for path, expected in original.items():
            self.assertTrue(update.same(update.file_info(self.root / path), expected), path)

    def test_complete_payload_staged_before_any_target_change(self):
        tx = self.tx()
        old = self.originals(tx)
        count = 0
        def download(url, path):
            nonlocal count
            self.assertOriginals(old)
            count += 1
            payload_download(url, path)
        tx.downloader = download
        activated = []
        def activate(root):
            activated.append(root)
            self.assertEqual(tx.load()['phase'], 'activation_started')
            with self.assertRaisesRegex(update.UpdateError, 'Another installer'):
                with update.update_lock(tx.state):
                    pass
        tx.run(BASE, activate=activate, expected_version='v-fixture')
        self.assertEqual(count, 9)
        self.assertEqual(activated, [self.root])
        self.assertEqual(tx.load()['phase'], 'complete')
        for relative, _ in tx.artifacts:
            self.assertEqual((self.root / relative).read_bytes(), (REPO / relative).read_bytes())
            self.assertEqual(update.file_info(self.root / relative)['mode'], old[relative]['mode'])
        for relative in update.WATCHED:
            self.assertTrue(update.same(update.file_info(self.root / relative), old[relative]))

    def test_scope_watcher_starts_after_activation_releases_update_lock(self):
        tx = self.tx()
        self.originals(tx)
        activated = []

        def activate(root):
            self.assertEqual(root, self.root)
            with self.assertRaisesRegex(update.UpdateError, 'Another installer'):
                with update.update_lock(tx.state):
                    pass
            return True

        def start_watcher(root):
            self.assertEqual(root, self.root)
            with update.update_lock(tx.state):
                activated.append(root)

        with mock.patch.object(update, 'start_scope_watcher_after_activation', side_effect=start_watcher):
            self.assertTrue(tx.run(BASE, activate=activate, expected_version='v-fixture'))
        self.assertEqual(activated, [self.root])

    def test_no_plugin_agent_keeps_old_install_without_scope_sync_dependency(self):
        tx = self.tx()
        self.originals(tx)
        with mock.patch.object(update, 'start_scope_watcher_after_activation') as watcher:
            self.assertFalse(tx.run(BASE, activate=lambda _: False, expected_version='v-fixture'))
        watcher.assert_not_called()

    def test_management_file_download_failures_leave_entire_payload_unchanged(self):
        tx = self.tx()
        old = self.originals(tx)
        for boundary in (8, 9):
            count = 0
            def download(url, path):
                nonlocal count
                count += 1
                payload_download(url, path)
                if count == boundary:
                    raise update.UpdateError('synthetic download interruption')
            tx.downloader = download
            with self.subTest(boundary=boundary), self.assertRaisesRegex(update.UpdateError, 'interruption'):
                tx.run(BASE)
            self.assertOriginals(old)

    def test_invalid_payload_does_not_execute_staged_prepare(self):
        tx = self.tx()
        old = self.originals(tx)
        marker = self.root / 'must-not-exist'
        def download(url, path):
            payload_download(url, path)
            if path.name == 'prepare.py':
                path.write_text('from pathlib import Path\nPath(%r).touch()\n' % str(marker))
            if path.name == 'runtime.encore.json':
                path.write_text('partial invalid JSON')
        tx.downloader = download
        with self.assertRaisesRegex(update.UpdateError, 'Invalid staged'):
            tx.run(BASE)
        self.assertFalse(marker.exists())
        self.assertOriginals(old)

    def test_requested_version_mismatch_is_precommit(self):
        tx = self.tx()
        old = self.originals(tx)
        with self.assertRaisesRegex(update.UpdateError, 'Requested version'):
            tx.run(BASE, expected_version='another-version')
        self.assertOriginals(old)

    def test_mixed_updater_and_management_script_rejected(self):
        tx = self.tx()
        old = self.originals(tx)
        def download(url, path):
            payload_download(url, path)
            if path.name.startswith('upgrade.'):
                path.write_text('UPDATE_ENGINE_SHA256="' + '0' * 64 + '"\n')
        tx.downloader = download
        with self.assertRaisesRegex(update.UpdateError, 'different releases'):
            tx.run(BASE)
        self.assertOriginals(old)

    def test_all_replacement_failures_restore_original_files_and_modes(self):
        tx = self.tx()
        old = self.originals(tx)
        for relative, _ in tx.artifacts:
            for prefix in ('before-replace:', 'after-replace:'):
                event = prefix + relative
                def checkpoint(actual):
                    if actual == event:
                        raise RuntimeError('synthetic replacement interruption')
                tx.checkpoint = checkpoint
                with self.subTest(event=event), self.assertRaisesRegex(RuntimeError, 'interruption'):
                    tx.run(BASE)
                self.assertOriginals(old)
                self.assertEqual(tx.load()['phase'], 'recovered')

    def test_originally_absent_files_and_new_empty_directories_are_removed(self):
        tx = self.tx(checkpoint=lambda event: (_ for _ in ()).throw(RuntimeError('stop'))
                     if event == 'after-replace:config/native/prepare.py' else None)
        with self.assertRaises(RuntimeError):
            tx.run(BASE)
        for relative, _ in tx.artifacts:
            self.assertFalse((self.root / relative).exists())
        self.assertFalse((self.root / 'config').exists())
        self.assertEqual(tx.load()['phase'], 'recovered')

    def test_user_edit_after_validation_is_preserved(self):
        tx = self.tx()
        old = self.originals(tx)
        edited = self.root / 'niginx.conf'
        tx.checkpoint = lambda event: edited.write_text('operator edit') if event == 'validated' else None
        with self.assertRaisesRegex(update.UpdateError, 'changed after preflight'):
            tx.run(BASE)
        self.assertEqual(edited.read_text(), 'operator edit')
        self.assertOriginals({k: v for k, v in old.items() if k != 'niginx.conf'})

    def test_env_edit_immediately_before_replacement_is_preserved(self):
        tx = self.tx()
        old = self.originals(tx)
        tx.checkpoint = lambda event: (self.root / '.env').write_text('operator env') if event == 'before-replace:docker-compose.yml' else None
        with self.assertRaisesRegex(update.UpdateError, 'configuration changed'):
            tx.run(BASE)
        self.assertOriginals({k: v for k, v in old.items() if k != '.env'})
        self.assertEqual((self.root / '.env').read_text(), 'operator env')

    def test_foreign_edit_during_partial_replacement_blocks_recovery(self):
        tx = self.tx()
        self.originals(tx)
        def checkpoint(event):
            if event == 'after-replace:niginx.conf':
                (self.root / 'docker-compose.yml').write_text('operator changed new compose')
                raise RuntimeError('stop')
        tx.checkpoint = checkpoint
        with self.assertRaisesRegex(update.UpdateError, 'externally changed'):
            tx.run(BASE)
        self.assertEqual((self.root / 'docker-compose.yml').read_text(), 'operator changed new compose')
        self.assertEqual((self.root / 'niginx.conf').read_bytes(), (REPO / 'niginx.conf').read_bytes())
        with self.assertRaisesRegex(update.UpdateError, 'externally changed'):
            self.tx().run(BASE, recovery_only=True)

    def test_edit_of_already_replaced_file_prevents_activation(self):
        tx = self.tx()
        self.originals(tx)
        tx.checkpoint = lambda event: (self.root / 'docker-compose.yml').write_text('operator after commit') if event == 'config_committed' else None
        activate = mock.Mock()
        with self.assertRaisesRegex(update.UpdateError, 'externally changed'):
            tx.run(BASE, activate=activate)
        activate.assert_not_called()
        self.assertEqual((self.root / 'docker-compose.yml').read_text(), 'operator after commit')

    def test_activation_failure_preserves_committed_configuration_for_review(self):
        tx = self.tx()
        self.originals(tx)
        def activate(_):
            raise RuntimeError('synthetic external failure')
        with self.assertRaises(RuntimeError):
            tx.run(BASE, activate=activate)
        self.assertEqual(tx.load()['phase'], 'activation_started')
        self.assertEqual((self.root / 'docker-compose.yml').read_bytes(), (REPO / 'docker-compose.yml').read_bytes())
        with self.assertRaisesRegex(update.UpdateError, 'No database rollback'):
            self.tx().run(BASE, recovery_only=True)

    def test_activation_environment_backup_and_receipt_survive_completion(self):
        tx = self.tx()
        old = self.originals(tx)
        before = (self.root / '.env').read_bytes()
        after = b'TP_AUTH=synthetic-new-identity\nTP_AUTH_POLICY=fixture-v1\n'
        tx.run(BASE, activate=lambda _: tx.write_activation_environment(after))
        data = tx.load()
        self.assertEqual(data['phase'], 'complete')
        entry = data['activation_files']['.env']
        self.assertEqual(entry['old'], old['.env'])
        self.assertTrue(entry['applied'])
        self.assertTrue(update.same(entry['new'], update.file_info(self.root / '.env')))
        self.assertEqual((tx.state / data['operation'] / 'backup/.env').read_bytes(), before)
        self.tx().run(BASE, recovery_only=True)
        self.assertEqual((self.root / '.env').read_bytes(), after)

    @unittest.skipIf(os.name == 'nt', 'SIGKILL semantics are checked on POSIX')
    def test_activation_environment_kill9_never_claims_external_rollback(self):
        parent = self.root
        for event in ('activation-env-prepared', 'activation-env-written'):
            with self.subTest(event=event):
                self.root = parent / event
                self.root.mkdir()
                old = self.originals()
                process = self.worker('kill-env', event)
                _, err = process.communicate(timeout=20)
                self.assertEqual(process.returncode, -signal.SIGKILL, err)
                data = self.tx().load()
                self.assertEqual(data['phase'], 'activation_started')
                self.assertFalse(data['activation_files']['.env']['applied'])
                expected = old['.env'] if event == 'activation-env-prepared' else data['activation_files']['.env']['new']
                self.assertTrue(update.same(update.file_info(self.root / '.env'), expected))
                recover = self.worker('recover', '')
                _, err = recover.communicate(timeout=20)
                self.assertNotEqual(recover.returncode, 0)
                self.assertIn('No database rollback', err)
                self.assertTrue(update.same(update.file_info(self.root / '.env'), expected))
        self.root = parent

    @unittest.skipIf(os.name == 'nt', 'SIGKILL semantics are checked on POSIX')
    def test_broker_directory_interruption_is_recorded_and_never_accepted_as_complete(self):
        process = self.worker('kill-broker', 'activation-file-written:config/native/broker/plugin-token')
        _, err = process.communicate(timeout=20)
        self.assertEqual(process.returncode, -signal.SIGKILL, err)
        data = self.tx().load()
        self.assertEqual(data['phase'], 'activation_started')
        entry = data['activation_files']['config/native/broker/plugin-token']
        self.assertIsNone(entry['old'])
        self.assertFalse(entry['applied'])
        self.assertIn('config/native/broker', data['activation_dirs'])
        with self.assertRaisesRegex(update.UpdateError, 'No database rollback'):
            self.tx().run(BASE, recovery_only=True)

    def test_corrupt_journal_is_rejected_without_target_changes(self):
        tx = self.tx()
        old = self.originals(tx)
        data = tx.prepare(BASE, '')
        corruptions = [
            ('created_dirs', ['../outside']), ('watched', []), ('intent', '../outside'),
            ('operation', '../outside'), ('phase', 'unrecognized'),
            ('activation_files', {'.env': {'old': data['watched']['.env'],
                                         'new': data['watched']['.env'], 'applied': True}})]
        for key, value in corruptions:
            bad = copy.deepcopy(data); bad[key] = value
            tx.journal.write_text(json.dumps(bad))
            with self.subTest(key=key), self.assertRaisesRegex(update.UpdateError, 'journal is invalid'):
                tx.run(BASE, recovery_only=True)
            self.assertOriginals(old)

    def test_missing_backup_fails_closed(self):
        tx = self.tx()
        old = self.originals(tx)
        data = tx.prepare(BASE, '')
        (tx.state / data['operation'] / 'backup/docker-compose.yml').unlink()
        with self.assertRaisesRegex(update.UpdateError, 'backup is missing'):
            tx.run(BASE, recovery_only=True)
        self.assertOriginals(old)

    @unittest.skipIf(os.name == 'nt', 'creating symlinks needs Windows developer privileges')
    def test_symlink_target_and_state_are_not_followed(self):
        outside = Path(self.temp.name) / 'outside'
        outside.write_text('outside')
        (self.root / 'docker-compose.yml').symlink_to(outside)
        with self.assertRaisesRegex(update.UpdateError, 'regular file'):
            self.tx().run(BASE)
        self.assertEqual(outside.read_text(), 'outside')
        (self.root / 'docker-compose.yml').unlink()
        state = self.root / '.thingspanel-update'
        # The attempted operation made a private journal directory only.
        import shutil
        shutil.rmtree(state)
        state.symlink_to(Path(self.temp.name))
        with self.assertRaisesRegex(update.UpdateError, 'real directory'):
            self.tx()

    def test_windows_payload_includes_native_files_and_powershell_management(self):
        tx = self.tx(platform='windows')
        self.originals(tx)
        tx.run(BASE)
        paths = {p for p, _ in tx.artifacts}
        self.assertIn('upgrade.ps1', paths)
        self.assertIn('uninstall.ps1', paths)
        self.assertIn('config/native/prepare.py', paths)
        self.assertNotIn('upgrade.sh', paths)

    def worker(self, mode, event):
        return subprocess.Popen([sys.executable, str(REPO / 'tests/transaction_worker.py'),
                                 str(self.root), mode, event], stdin=subprocess.PIPE,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                                env=dict(os.environ, PYTHONDONTWRITEBYTECODE='1'))

    @unittest.skipIf(os.name == 'nt', 'SIGKILL semantics are checked on POSIX')
    def test_kill9_during_replacements_is_recovered_by_new_process(self):
        old = self.originals()
        for event in ('prepared', 'before-replace:docker-compose.yml',
                      'after-replace:docker-compose.yml', 'after-replace:config/native/prepare.py',
                      'after-replace:config/native/update.py', 'config_committed'):
            with self.subTest(event=event):
                process = self.worker('kill', event)
                out, err = process.communicate(timeout=20)
                self.assertEqual(process.returncode, -signal.SIGKILL, err)
                recover = self.worker('recover', '')
                out, err = recover.communicate(timeout=20)
                self.assertEqual(recover.returncode, 0, err)
                self.assertOriginals(old)
                self.assertEqual(self.tx().load()['phase'], 'recovered')

    @unittest.skipIf(os.name == 'nt', 'SIGKILL semantics are checked on POSIX')
    def test_recovery_itself_can_be_killed_and_retried(self):
        old = self.originals()
        process = self.worker('kill', 'after-replace:config/native/prepare.py')
        process.communicate(timeout=20)
        self.assertEqual(process.returncode, -signal.SIGKILL)
        process = self.worker('kill-recover', 'recovered-file:config/native/prepare.py')
        process.communicate(timeout=20)
        self.assertEqual(process.returncode, -signal.SIGKILL)
        self.tx().run(BASE, recovery_only=True)
        self.assertOriginals(old)

    @unittest.skipIf(os.name == 'nt', 'POSIX signal delivery is checked on Linux and macOS')
    def test_sigterm_during_update_is_recovered_by_new_process(self):
        old = self.originals()
        for event in ('prepared', 'after-replace:config/native/prepare.py', 'config_committed'):
            with self.subTest(event=event):
                process = self.worker('terminate', event)
                _, err = process.communicate(timeout=20)
                self.assertEqual(process.returncode, -signal.SIGTERM, err)
                recover = self.worker('recover', '')
                _, err = recover.communicate(timeout=20)
                self.assertEqual(recover.returncode, 0, err)
                self.assertOriginals(old)

    @unittest.skipIf(os.name == 'nt', 'SIGKILL semantics are checked on POSIX')
    def test_kill9_restores_original_absence_and_recovery_is_idempotent(self):
        process = self.worker('kill', 'after-replace:config/native/update.py')
        _, err = process.communicate(timeout=20)
        self.assertEqual(process.returncode, -signal.SIGKILL, err)
        for _ in range(2):
            recover = self.worker('recover', '')
            _, err = recover.communicate(timeout=20)
            self.assertEqual(recover.returncode, 0, err)
            for relative, _ in self.tx().artifacts:
                self.assertFalse((self.root / relative).exists(), relative)
            self.assertFalse((self.root / 'config').exists())
            self.assertEqual(self.tx().load()['phase'], 'recovered')

    def test_completed_installation_can_be_relocated_but_unresolved_cannot(self):
        tx = self.tx()
        self.originals(tx)
        tx.run(BASE)
        journal = self.root / '.thingspanel-update' / 'journal.json'
        data = json.loads(journal.read_text(encoding='utf-8'))
        unresolved = copy.deepcopy(data)
        unresolved['phase'] = 'activation_started'
        moved = Path(self.temp.name) / 'moved'
        self.root.rename(moved)
        relocated = update.Transaction(moved, downloader=payload_download).load()
        self.assertEqual(relocated['phase'], 'complete')
        self.assertEqual(relocated['root'], str(moved))
        (moved / '.thingspanel-update' / 'journal.json').write_text(json.dumps(unresolved, sort_keys=True) + '\n', encoding='utf-8')
        with self.assertRaises(update.UpdateError):
            update.Transaction(moved, downloader=payload_download).load()

    def test_completed_journal_is_not_rolled_back_on_restart(self):
        tx = self.tx()
        self.originals(tx)
        tx.run(BASE)
        complete = {p: update.file_info(self.root / p) for p, _ in tx.artifacts}
        for _ in range(2):
            recover = self.worker('recover', '')
            _, err = recover.communicate(timeout=20)
            self.assertEqual(recover.returncode, 0, err)
            self.assertOriginals(complete)
            self.assertEqual(self.tx().load()['phase'], 'complete')

    def test_second_process_cannot_enter_during_config_replacement(self):
        old = self.originals()
        for event in ('prepared', 'after-replace:config/native/prepare.py', 'config_committed'):
            with self.subTest(event=event):
                ready = self.root / '.worker-ready'
                if ready.exists():
                    ready.unlink()
                process = self.worker('hold', event)
                try:
                    deadline = time.monotonic() + 10
                    while not ready.exists() and process.poll() is None and time.monotonic() < deadline:
                        time.sleep(0.02)
                    self.assertTrue(ready.exists())
                    competitor = self.worker('recover', '')
                    _, err = competitor.communicate(timeout=20)
                    self.assertNotEqual(competitor.returncode, 0)
                    self.assertIn('Another installer operation is active', err)
                finally:
                    process.kill()
                    process.communicate(timeout=20)
                recover = self.worker('recover', '')
                _, err = recover.communicate(timeout=20)
                self.assertEqual(recover.returncode, 0, err)
                self.assertOriginals(old)

    def test_process_lock_covers_activation_and_is_released_by_termination(self):
        self.originals()
        process = self.worker('hold', 'activation_started')
        try:
            # A bounded wait on a synthetic readiness file; never wait forever
            # if the child fails before acquiring its actual transaction lock.
            deadline = time.monotonic() + 10
            while not (self.root / '.worker-ready').exists() and process.poll() is None and time.monotonic() < deadline:
                time.sleep(0.02)
            self.assertTrue((self.root / '.worker-ready').exists())
            with self.assertRaisesRegex(update.UpdateError, 'Another installer'):
                self.tx().run(BASE)
        finally:
            process.kill()
            process.communicate(timeout=20)
        with self.assertRaisesRegex(update.UpdateError, 'No database rollback'):
            self.tx().run(BASE, recovery_only=True)


class ValidatorTests(unittest.TestCase):
    def test_packagers_do_not_copy_configuration_before_transaction(self):
        mac = (REPO / 'packaging/macos/build.sh').read_text()
        win = (REPO / 'packaging/windows/ThingsPanel-Setup.iss').read_text()
        mac_sources = re.findall(r'^cp "\$ROOT_DIR/([^"]+)"', mac, flags=re.MULTILINE)
        win_sources = re.findall(r'^Source: "\.\.\\\.\.\\([^"]+)"', win, flags=re.MULTILINE)
        self.assertEqual(set(mac_sources), {'install.sh', 'install.bash'})
        self.assertEqual(set(win_sources), {'install.ps1', 'install.core.ps1'})
        postinstall = (REPO / 'packaging/macos/scripts/postinstall').read_text()
        self.assertNotIn('"$INSTALL_DIR"/*.sh', postinstall)
        self.assertIn('"$INSTALL_DIR/install.bash"', postinstall)

    def test_entrypoint_helper_pins_match_reviewed_bytes(self):
        digest = hashlib.sha256((REPO / 'config/native/update.py').read_bytes()).hexdigest()
        for name in ('install.bash', 'upgrade.sh', 'install.core.ps1', 'upgrade.ps1'):
            pins = re.findall(r'UPDATE_ENGINE_SHA256\s*=\s*"([0-9a-f]{64})"', (REPO / name).read_text(encoding='utf-8-sig'))
            self.assertEqual(pins, [digest], name)

    def test_compose_uses_original_project_and_env_for_staged_file(self):
        with tempfile.TemporaryDirectory() as name:
            root = Path(name)
            (root / '.env').write_text('synthetic=only')
            stage = root / '.thingspanel-update/stage/docker-compose.yml'
            with mock.patch.object(update, 'run_command', return_value=json.dumps(CONFIG).encode()) as command:
                update.compose_config(root, stage)
            args, cwd = command.call_args.args
            self.assertEqual(cwd, root)
            self.assertEqual(args[args.index('--project-directory') + 1], str(root))
            self.assertEqual(args[args.index('--env-file') + 1], str(root / '.env'))
            self.assertEqual(args[args.index('-f') + 1], str(stage))

    def test_pg_unknown_and_mysql_guards_remain_closed(self):
        update.validate_config(copy.deepcopy(CONFIG))
        for change in ('mysql', 'missing', 'selector', 'unknown', 'duplicate'):
            config = copy.deepcopy(CONFIG)
            env = config['services']['backend']['environment']
            if change == 'mysql':
                config['services']['thingsvis-server']['environment']['DATABASE_URL'] = 'mysql://fixture/db'
            elif change == 'missing':
                del env['GOTP_DB_PSQL_HOST']
            elif change == 'selector':
                env['DB_DRIVER'] = 'mysql'
            elif change == 'unknown':
                env['GOTP_DB_PSQL_HOST'] = '${UNCONFIRMED_HOST}'
            else:
                config['services']['backend']['environment'] = ['GOTP_DB_PSQL_HOST=a', 'GOTP_DB_PSQL_HOST=b']
            with self.subTest(change=change), self.assertRaises(update.UpdateError):
                update.validate_config(config)

    def test_a1_relational_pg_profile_is_required_when_declared(self):
        config = copy.deepcopy(CONFIG)
        env = config['services']['backend']['environment']
        env.update({
            'GOTP_RELATIONAL_DRIVER': 'postgres',
            'GOTP_RELATIONAL_POSTGRES_PLATFORM_HOST': 'postgres',
            'GOTP_RELATIONAL_POSTGRES_PLATFORM_PORT': '5432',
            'GOTP_RELATIONAL_POSTGRES_PLATFORM_DATABASE': 'ThingsPanel',
            'GOTP_RELATIONAL_POSTGRES_PLATFORM_USERNAME': 'postgres',
            'GOTP_RELATIONAL_POSTGRES_PLATFORM_PASSWORD': 'synthetic-fixture',
        })
        update.validate_config(config)
        del env['GOTP_RELATIONAL_POSTGRES_PLATFORM_HOST']
        with self.assertRaises(update.UpdateError):
            update.validate_config(config)

    def test_legacy_only_compose_remains_upgrade_compatible(self):
        config = copy.deepcopy(CONFIG)
        env = config['services']['backend']['environment']
        for key in tuple(env):
            if key.startswith('GOTP_RELATIONAL_'):
                del env[key]
        update.validate_config(config)

    def test_staged_legacy_only_compose_remains_supported(self):
        config = copy.deepcopy(CONFIG)
        env = config['services']['backend']['environment']
        for key in tuple(env):
            if key.startswith('GOTP_RELATIONAL_'):
                del env[key]
        with tempfile.TemporaryDirectory() as directory:
            stage = Path(directory)
            transaction = update.Transaction(stage / 'install')
            for relative, _ in transaction.artifacts:
                target = stage / relative
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_bytes((REPO / relative).read_bytes())
            with mock.patch.object(update, 'render_with_fallback', return_value=config):
                transaction.validate(stage, '')

    def test_external_compose_path_is_not_inherited_by_prepare(self):
        with mock.patch.dict(os.environ, {'COMPOSE_FILE': '/foreign/compose.yml', 'COMPOSE_ENV_FILES': '/foreign/.env', 'COMPOSE_DISABLE_ENV_FILE': '1'}):
            env = update.command_environment(Path('/original/install'))
        self.assertEqual(env['COMPOSE_FILE'], str(Path('/original/install/docker-compose.yml')))
        self.assertNotIn('COMPOSE_ENV_FILES', env)
        self.assertNotIn('COMPOSE_DISABLE_ENV_FILE', env)

    def test_subprocess_errors_do_not_expose_args_or_stderr(self):
        result = subprocess.CompletedProcess([], 9, b'', b'password=synthetic-secret')
        with mock.patch.object(update.subprocess, 'run', return_value=result):
            with self.assertRaises(update.UpdateError) as raised:
                update.run_command(['command', 'password=synthetic-secret'], REPO)
        self.assertEqual(str(raised.exception), 'Installer command failed (exit 9)')


if __name__ == '__main__':
    unittest.main()
