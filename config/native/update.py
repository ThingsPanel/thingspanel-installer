#!/usr/bin/env python3
"""One configuration transaction for the existing ThingsPanel installer.

The reviewed entry points pin this file's SHA-256 before executing it. Payload
downloads retain the installer's HTTPS distribution trust; this is not a new
package-signing system. Never execute a downloaded prepare.py to validate it.
Configuration recovery ends at activation: database and service effects are
deliberately not called a rollback of files.
"""
import argparse
import ast
import contextlib
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import sys
import tempfile
from urllib.parse import urlsplit
import uuid
import urllib.request
import urllib.error


class UpdateError(RuntimeError):
    pass


COMMON = (
    ('docker-compose.yml', 0o644), ('niginx.conf', 0o644),
    ('config/native/prepare.py', 0o644),
    ('config/native/runtime.encore.json', 0o644),
    ('config/native/runtime.notification.json', 0o644),
    ('config/native/tick.sh', 0o644), ('config/native/README.md', 0o644),
)
WATCHED = ('.env', 'config/native/plugin-token')


def make_directories(path, mode=0o755):
    """Persist each new directory entry, including the stage/backup parents."""
    missing = []
    current = path
    while not current.exists():
        missing.append(current)
        current = current.parent
    for current in reversed(missing):
        current.mkdir(mode=mode)
        fsync_directory(current.parent)


def fsync_directory(path):
    if os.name == 'nt':
        # Windows replacements use MOVEFILE_WRITE_THROUGH below. File content
        # is flushed before each replacement. POSIX directory fds are not
        # supported on Windows; do not pretend os.fsync(open(directory)) works.
        return
    fd = os.open(str(path), os.O_RDONLY | getattr(os, 'O_DIRECTORY', 0))
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def replace(source, destination):
    if os.name == 'nt':
        import ctypes
        move = ctypes.WinDLL('kernel32', use_last_error=True).MoveFileExW
        move.argtypes = [ctypes.c_wchar_p, ctypes.c_wchar_p, ctypes.c_uint]
        move.restype = ctypes.c_int
        if not move(str(source), str(destination), 0x1 | 0x8):
            raise ctypes.WinError(ctypes.get_last_error())
    else:
        os.replace(source, destination)
    fsync_directory(destination.parent)


def private_directory(path):
    if path.exists() or path.is_symlink():
        info = path.lstat()
        if not stat.S_ISDIR(info.st_mode) or path.is_symlink():
            raise UpdateError('Update state path is not a real directory')
        if os.name != 'nt' and (info.st_mode & 0o077):
            raise UpdateError('Update state directory must be private')
    else:
        path.mkdir(mode=0o700)
        fsync_directory(path.parent)


def file_info(path):
    """Read a regular file without following its final symlink; no bytes logged."""
    try:
        before = path.lstat()
    except FileNotFoundError:
        return None
    if not stat.S_ISREG(before.st_mode) or stat.S_ISLNK(before.st_mode):
        raise UpdateError('A managed file is not a regular file: ' + path.name)
    fd = os.open(str(path), os.O_RDONLY | getattr(os, 'O_NOFOLLOW', 0))
    try:
        opened = os.fstat(fd)
        if (opened.st_dev, opened.st_ino) != (before.st_dev, before.st_ino):
            raise UpdateError('A managed file changed while being read: ' + path.name)
        digest = hashlib.sha256()
        while True:
            block = os.read(fd, 1024 * 1024)
            if not block:
                break
            digest.update(block)
        after = os.fstat(fd)
        if (opened.st_size, opened.st_mtime_ns, opened.st_mode) != (after.st_size, after.st_mtime_ns, after.st_mode):
            raise UpdateError('A managed file changed while being read: ' + path.name)
        return {'sha256': digest.hexdigest(), 'size': after.st_size,
                'mode': stat.S_IMODE(after.st_mode), 'uid': after.st_uid,
                'gid': after.st_gid}
    finally:
        os.close(fd)


def same(actual, expected):
    if actual is None or expected is None:
        return actual is expected
    keys = ('sha256', 'size', 'mode') if os.name == 'nt' else ('sha256', 'size', 'mode', 'uid', 'gid')
    return all(actual[key] == expected[key] for key in keys)


def safe_target(root, relative):
    parts = relative.split('/')
    if not parts or any(p in ('', '.', '..') for p in parts) or '\\' in relative:
        raise UpdateError('Invalid managed path')
    target = root
    for part in parts[:-1]:
        target = target / part
        if target.is_symlink() or (target.exists() and not target.is_dir()):
            raise UpdateError('A managed parent is not a real directory')
    return target / parts[-1]


def durable_bytes(path, content, mode=0o600, ownership=None):
    make_directories(path.parent)
    fd, temporary = tempfile.mkstemp(prefix='.write-', dir=path.parent)
    temporary = Path(temporary)
    try:
        with os.fdopen(fd, 'wb') as stream:
            stream.write(content)
            stream.flush()
            os.fchmod(stream.fileno(), mode) if hasattr(os, 'fchmod') else os.chmod(temporary, mode)
            if ownership is not None and os.name != 'nt':
                info = os.fstat(stream.fileno())
                if (info.st_uid, info.st_gid) != (ownership['uid'], ownership['gid']):
                    os.fchown(stream.fileno(), ownership['uid'], ownership['gid'])
            os.fsync(stream.fileno())
        replace(temporary, path)
    finally:
        if temporary.exists():
            temporary.unlink()


@contextlib.contextmanager
def update_lock(state):
    path = state / 'lock'
    if path.is_symlink() or (path.exists() and not path.is_file()):
        raise UpdateError('Invalid update lock')
    fd = os.open(str(path), os.O_RDWR | os.O_CREAT | getattr(os, 'O_NOFOLLOW', 0), 0o600)
    try:
        if os.name == 'nt':
            import msvcrt
            if os.fstat(fd).st_size == 0:
                os.write(fd, b'0')
                os.fsync(fd)
            os.lseek(fd, 0, os.SEEK_SET)
            try:
                msvcrt.locking(fd, msvcrt.LK_NBLCK, 1)
            except OSError:
                raise UpdateError('Another installer operation is active') from None
        else:
            import fcntl
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except OSError:
                raise UpdateError('Another installer operation is active') from None
        yield
    finally:
        os.close(fd)  # Kernel releases the lock even after process termination.


def command_environment(root):
    # Every Compose invocation, including those in the existing prepare.py,
    # must use the same reviewed Compose file and the original project .env.
    # Preserve ordinary interpolation variables and COMPOSE_PROJECT_NAME.
    env = os.environ.copy()
    env['COMPOSE_FILE'] = str(root / 'docker-compose.yml')
    env.pop('COMPOSE_ENV_FILES', None)
    env.pop('COMPOSE_DISABLE_ENV_FILE', None)
    return env


def run_command(args, root, *, data=None):
    try:
        result = subprocess.run(args, cwd=root, input=data, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, check=False,
                                env=command_environment(root))
    except OSError:
        raise UpdateError('Required installer command could not be started') from None
    if result.returncode:
        # Compose/prepare errors can include an interpolated password or URL.
        raise UpdateError('Installer command failed (exit %d)' % result.returncode)
    return result.stdout


def environment(value):
    if isinstance(value, dict):
        return value
    if isinstance(value, list):
        result = {}
        for item in value:
            if not isinstance(item, str) or '=' not in item:
                raise UpdateError('Cannot confirm supported PostgreSQL configuration')
            key, val = item.split('=', 1)
            if key in result:
                raise UpdateError('Duplicate environment field')
            result[key] = val
        return result
    raise UpdateError('Cannot confirm supported PostgreSQL configuration')


def validate_config(config):
    """Keep the reviewed PG/unknown guard; no downloaded module is imported."""
    try:
        services = config['services']
        vis = environment(services['thingsvis-server']['environment'])
        backend = environment(services['backend']['environment'])
        url = vis['DATABASE_URL']
        parsed = urlsplit(url)
        if parsed.scheme not in ('postgres', 'postgresql') or not parsed.hostname or '$' in parsed.hostname:
            raise ValueError()
        required = ('GOTP_DB_PSQL_HOST', 'GOTP_DB_PSQL_PORT', 'GOTP_DB_PSQL_USERNAME', 'GOTP_DB_PSQL_DBNAME')
        if any(not isinstance(backend.get(k), (str, int)) or not str(backend[k]).strip() or '$' in str(backend[k]) for k in required):
            raise ValueError()
        if backend['GOTP_DB_PSQL_HOST'] == 'postgres' and not isinstance(services.get('postgres'), dict):
            raise ValueError()
        selectors = ('GOTP_DB_PSQL_DBTYPE', 'GOTP_DB_DRIVER', 'GOTP_DB_PROVIDER', 'GOTP_DB_TYPE',
                     'DB_DRIVER', 'DB_PROVIDER', 'DB_TYPE', 'DATABASE_TYPE', 'DATABASE_PROVIDER', 'DATABASE_DRIVER')
        for key, value in backend.items():
            name = str(key).upper()
            if name.startswith(('GOTP_DB_MYSQL_', 'GOTP_DB_MARIADB_')) and value not in (None, ''):
                raise ValueError()
            if name in selectors and str(value).lower() not in ('postgres', 'postgresql', 'pg'):
                raise ValueError()
            if name == 'DATABASE_URL' and (not isinstance(value, str) or urlsplit(value).scheme not in ('postgres', 'postgresql')):
                raise ValueError()
    except (KeyError, TypeError, ValueError, AttributeError):
        raise UpdateError('Cannot confirm supported PostgreSQL backend and ThingsVis configuration') from None


def compose_config(root, compose, *, interpolate=True):
    args = ['docker', 'compose', '--project-directory', str(root), '-f', str(compose)]
    if (root / '.env').is_file():
        args += ['--env-file', str(root / '.env')]
    args += ['config']
    if not interpolate:
        args += ['--no-interpolate']
    args += ['--format', 'json']
    try:
        value = json.loads(run_command(args, root))
    except (ValueError, UnicodeError):
        raise UpdateError('Compose did not return valid JSON') from None
    validate_config(value)
    return value


def render_with_fallback(root, compose):
    try:
        return compose_config(root, compose)
    except UpdateError:
        return compose_config(root, compose, interpolate=False)


def download(url, path):
    make_directories(path.parent)
    if shutil.which('curl'):
        args = ['curl', '-fsSL', '--connect-timeout', '15', '--max-time', '180', url, '-o', str(path)]
    elif shutil.which('wget'):
        args = ['wget', '-q', '--timeout=180', '-O', str(path), url]
    else:
        # Windows installations with Python but no curl.exe/wget do not need a
        # second downloader dependency. Tests never reach a real network.
        try:
            with urllib.request.urlopen(url, timeout=180) as response, path.open('wb') as output:
                size = 0
                while True:
                    block = response.read(1024 * 1024)
                    if not block:
                        break
                    size += len(block)
                    if size > 16 * 1024 * 1024:
                        raise UpdateError('Downloaded installer file is too large')
                    output.write(block)
        except (urllib.error.URLError, OSError):
            raise UpdateError('Installer file download failed') from None
        args = None
    if args is not None:
        run_command(args, path.parent)
    info = file_info(path)
    if info is None or info['size'] == 0 or info['size'] > 16 * 1024 * 1024:
        raise UpdateError('Downloaded installer file is empty or too large')
    with path.open('rb+') as stream:
        os.fsync(stream.fileno())
    fsync_directory(path.parent)


class Transaction:
    def __init__(self, root, *, platform=None, downloader=download,
                 validator=None, checkpoint=None):
        self.root = Path(os.path.abspath(root))
        if self.root.is_symlink() or (self.root.exists() and not self.root.is_dir()):
            raise UpdateError('Install directory must be a real directory')
        make_directories(self.root)
        self.state = self.root / '.thingspanel-update'
        private_directory(self.state)
        self.journal = self.state / 'journal.json'
        self.platform = platform or ('windows' if os.name == 'nt' else 'posix')
        if self.platform not in ('windows', 'posix'):
            raise UpdateError('Unsupported installer platform')
        self.artifacts = list(COMMON) + ([(x, 0o644) for x in ('upgrade.ps1', 'uninstall.ps1')]
                                       if self.platform == 'windows' else [(x, 0o755) for x in ('upgrade.sh', 'uninstall.sh')])
        self.artifacts += [('config/native/update.py', 0o644)]
        self.downloader = downloader
        self.validator = validator or self.validate
        self.checkpoint = checkpoint or (lambda event: None)

    def save(self, data):
        durable_bytes(self.journal, (json.dumps(data, sort_keys=True) + '\n').encode())

    def load(self):
        if file_info(self.journal) is None:
            return None
        try:
            data = json.loads(self.journal.read_text(encoding='utf-8'))
            if data['version'] != 1 or data['root'] != str(self.root) or not isinstance(data['files'], list):
                raise ValueError()
            if not isinstance(data['operation'], str) or uuid.UUID(data['operation']).hex != data['operation']:
                raise ValueError()
            expected = {p for p, _ in self.artifacts}
            paths = [entry['path'] for entry in data['files']]
            if len(paths) != len(set(paths)) or set(paths) != expected:
                raise ValueError()
            if data['phase'] not in ('prepared', 'applying', 'config_committed', 'activation_started', 'complete', 'recovering', 'recovered'):
                raise ValueError()
            def fingerprint(value, nullable=False):
                if value is None and nullable:
                    return
                if not isinstance(value, dict) or set(value) != {'sha256', 'size', 'mode', 'uid', 'gid'}:
                    raise ValueError()
                if not isinstance(value['sha256'], str) or re.fullmatch(r'[0-9a-f]{64}', value['sha256']) is None:
                    raise ValueError()
                for key in ('size', 'mode', 'uid', 'gid'):
                    if type(value[key]) is not int or value[key] < 0:
                        raise ValueError()
                if value['mode'] > 0o7777:
                    raise ValueError()
            if not isinstance(data['watched'], dict) or set(data['watched']) != set(WATCHED):
                raise ValueError()
            for value in data['watched'].values():
                fingerprint(value, nullable=True)
            if not isinstance(data['created_dirs'], list) or any(p not in ('config', 'config/native') for p in data['created_dirs']):
                raise ValueError()
            if len(data['created_dirs']) != len(set(data['created_dirs'])):
                raise ValueError()
            if data.get('intent') is not None and data['intent'] not in expected:
                raise ValueError()
            for entry in data['files']:
                safe_target(self.root, entry['path'])
                fingerprint(entry['old'], nullable=True)
                fingerprint(entry['new'])
                if type(entry['mode']) is not int or entry['mode'] != entry['new']['mode']:
                    raise ValueError()
                if entry['old'] is not None and entry['mode'] != entry['old']['mode']:
                    raise ValueError()
            # Stage and backup data must stay inside this real operation dir.
            for prefix in ('stage', 'backup'):
                safe_target(self.state, data['operation'] + '/' + prefix + '/placeholder')
            return data
        except (KeyError, TypeError, ValueError, UnicodeError, AttributeError):
            raise UpdateError('Update journal is invalid; preserve it for recovery') from None

    def validate(self, stage, expected_version):
        for relative, _ in self.artifacts:
            path = stage / relative
            try:
                text = path.read_text(encoding='utf-8-sig')
                if '\x00' in text or not text.strip():
                    raise ValueError()
                if path.suffix == '.json' and not isinstance(json.loads(text), dict):
                    raise ValueError()
                if path.suffix == '.py':
                    ast.parse(text)
            except (ValueError, UnicodeError, SyntaxError):
                raise UpdateError('Invalid staged installer file: ' + relative) from None
        # Do not publish a management entry point that expects a different
        # updater. The reviewed updater itself is copied into this transaction.
        entrypoint = stage / ('upgrade.ps1' if self.platform == 'windows' else 'upgrade.sh')
        pins = re.findall(r'UPDATE_ENGINE_SHA256\s*=\s*"([0-9a-f]{64})"', entrypoint.read_text(encoding='utf-8-sig'))
        if pins != [hashlib.sha256(Path(__file__).read_bytes()).hexdigest()]:
            raise UpdateError('Staged management script and reviewed updater are from different releases')
        config = render_with_fallback(self.root, stage / 'docker-compose.yml')
        if expected_version:
            image = config.get('services', {}).get('backend', {}).get('image', '')
            if '@' in image or ':' not in image or image.rsplit(':', 1)[-1] != expected_version:
                raise UpdateError('Requested version does not match the staged Compose backend image')

    def check_unchanged(self, data, *, before_apply=False):
        for relative, original in data['watched'].items():
            if not same(file_info(safe_target(self.root, relative)), original):
                raise UpdateError('Install configuration changed during update: ' + relative)
        if before_apply:
            for entry in data['files']:
                if not same(file_info(safe_target(self.root, entry['path'])), entry['old']):
                    raise UpdateError('Managed file changed after preflight: ' + entry['path'])

    def prepare(self, source, expected_version):
        parsed = urlsplit(source)
        if parsed.scheme != 'https' or not parsed.hostname or parsed.username or parsed.query or parsed.fragment:
            raise UpdateError('An HTTPS installer distribution base is required')
        if (self.root / 'docker-compose.yml').exists():
            render_with_fallback(self.root, self.root / 'docker-compose.yml')
        operation = uuid.uuid4().hex
        directory = self.state / operation
        private_directory(directory)
        stage = directory / 'stage'
        backups = directory / 'backup'
        stage.mkdir(mode=0o700)
        backups.mkdir(mode=0o700)
        fsync_directory(directory)
        data = {'version': 1, 'root': str(self.root), 'operation': operation,
                'phase': 'prepared', 'files': [], 'watched': {}, 'created_dirs': []}
        for relative in WATCHED:
            data['watched'][relative] = file_info(safe_target(self.root, relative))
        for relative, default_mode in self.artifacts:
            target = safe_target(self.root, relative)
            original = file_info(target)
            entry = {'path': relative, 'old': original, 'new': None, 'mode': original['mode'] if original else default_mode}
            data['files'].append(entry)
            if original is not None:
                durable_bytes(backups / relative, target.read_bytes(), original['mode'], original)
                if not same(file_info(backups / relative), original) or not same(file_info(target), original):
                    raise UpdateError('Managed file changed during backup: ' + relative)
        try:
            for entry in data['files']:
                path = stage / entry['path']
                if entry['path'] == 'config/native/update.py':
                    durable_bytes(path, Path(__file__).read_bytes(), entry['mode'], entry['old'])
                else:
                    self.downloader(source.rstrip('/') + '/' + entry['path'], path)
                    # Preserve existing permissions and ownership, including
                    # private generated runtime files, through the update.
                    durable_bytes(path, path.read_bytes(), entry['mode'], entry['old'])
                entry['new'] = file_info(path)
                if os.name == 'nt':
                    # Windows chmod exposes read-only/writable, not POSIX rwx.
                    entry['mode'] = entry['new']['mode']
            self.validator(stage, expected_version)
            self.checkpoint('validated')
            self.check_unchanged(data, before_apply=True)
            self.save(data)
            self.checkpoint('prepared')
            return data
        except BaseException:
            # No managed target was written. Keep only this transaction's
            # private stage/backup for diagnosis; never clean an existing stack.
            raise

    def apply(self, data):
        self.check_unchanged(data, before_apply=True)
        data['phase'] = 'applying'
        self.save(data)
        for entry in data['files']:
            relative = entry['path']
            target = safe_target(self.root, relative)
            self.check_unchanged(data)
            if not same(file_info(target), entry['old']):
                raise UpdateError('Managed file changed before replacement: ' + relative)
            missing = []
            parent = target.parent
            while parent != self.root and not parent.exists():
                missing.append(parent)
                parent = parent.parent
            for parent in reversed(missing):
                rel = parent.relative_to(self.root).as_posix()
                if rel not in data['created_dirs']:
                    data['created_dirs'].append(rel)
                    self.save(data)  # Write intent before creating anything.
                parent.mkdir(mode=0o755, exist_ok=True)
                fsync_directory(parent.parent)
            data['intent'] = relative
            self.save(data)
            self.checkpoint('before-replace:' + relative)
            self.check_unchanged(data)
            if not same(file_info(target), entry['old']):
                raise UpdateError('Managed file changed before replacement: ' + relative)
            source = self.state / data['operation'] / 'stage' / relative
            if not same(file_info(source), entry['new']):
                raise UpdateError('Staged file changed before replacement: ' + relative)
            durable_bytes(target, source.read_bytes(), entry['mode'], entry['old'])
            self.checkpoint('after-replace:' + relative)
            data['intent'] = None
            self.save(data)
        data['phase'] = 'config_committed'
        self.save(data)
        self.checkpoint('config_committed')

    def recover(self, data):
        if data['phase'] in ('complete', 'recovered'):
            return
        if data['phase'] == 'activation_started':
            raise UpdateError('Previous activation may have changed databases or services; inspect the journal and reconcile before another update. No database rollback was attempted')
        # Inspect *all* targets first. Neither a later user edit nor an unknown
        # replacement is silently overwritten during recovery.
        for entry in data['files']:
            current = file_info(safe_target(self.root, entry['path']))
            if not same(current, entry['old']) and not same(current, entry['new']):
                raise UpdateError('Recovery requires review of an externally changed file: ' + entry['path'])
            if entry['old'] is not None:
                backup = self.state / data['operation'] / 'backup' / entry['path']
                if not same(file_info(backup), entry['old']):
                    raise UpdateError('Recovery backup is missing or changed: ' + entry['path'])
        data['phase'] = 'recovering'
        self.save(data)
        for entry in reversed(data['files']):
            target = safe_target(self.root, entry['path'])
            current = file_info(target)
            if same(current, entry['old']):
                continue
            if not same(current, entry['new']):
                raise UpdateError('File changed during recovery: ' + entry['path'])
            if entry['old'] is None:
                target.unlink()
                fsync_directory(target.parent)
            else:
                backup = self.state / data['operation'] / 'backup' / entry['path']
                durable_bytes(target, backup.read_bytes(), entry['old']['mode'], entry['old'])
            self.checkpoint('recovered-file:' + entry['path'])
        for relative in reversed(data.get('created_dirs', [])):
            directory = safe_target(self.root, relative + '/placeholder').parent
            try:
                directory.rmdir()  # Never recursive; preserve foreign contents.
                fsync_directory(directory.parent)
            except (FileNotFoundError, OSError):
                pass
        data['phase'] = 'recovered'
        self.save(data)

    def run(self, source, *, expected_version='', activate=None, recovery_only=False):
        with update_lock(self.state):
            old = self.load()
            if old is not None:
                self.recover(old)
            if recovery_only:
                return
            data = self.prepare(source, expected_version)
            try:
                self.apply(data)
                if activate is not None:
                    self.check_unchanged(data)
                    for entry in data['files']:
                        if not same(file_info(safe_target(self.root, entry['path'])), entry['new']):
                            raise UpdateError('Managed file changed before activation: ' + entry['path'])
                    data['phase'] = 'activation_started'
                    self.save(data)
                    self.checkpoint('activation_started')
                    activate(self.root)
                data['phase'] = 'complete'
                self.save(data)
            except BaseException:
                if data['phase'] != 'activation_started':
                    self.recover(data)
                raise


def activate_native(root):
    """Existing native entry point, after commit, under the same process lock."""
    prepare = [sys.executable, str(root / 'config/native/prepare.py')]
    print('Preparing native configuration (configuration rollback ends here).', flush=True)
    run_command(prepare, root)
    archive = root / 'images.tar'
    parts = sorted(root.glob('images.tar.part-*'))
    loaded = False
    if archive.is_file():
        try:
            run_command(['docker', 'load', '-i', str(archive)], root)
            loaded = True
        except UpdateError:
            print('Offline image load failed; trying the configured registry.', flush=True)
    elif parts:
        # Stream parts instead of creating a second multi-GB archive.
        process = subprocess.Popen(['docker', 'load'], cwd=root, stdin=subprocess.PIPE,
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            for part in parts:
                with part.open('rb') as stream:
                    shutil.copyfileobj(stream, process.stdin)
            process.stdin.close()
            loaded = process.wait() == 0
        except (OSError, BrokenPipeError):
            if process.stdin:
                process.stdin.close()
            process.wait()
        if not loaded:
            print('Offline image load failed; trying the configured registry.', flush=True)
    if not loaded:
        print('Pulling the selected component images.', flush=True)
        run_command(prepare + ['--pull'], root)
    print('Applying the existing native migration and service preparation.', flush=True)
    run_command(prepare + ['--bootstrap', '--refresh-devices'], root)
    run_command(['docker', 'compose', 'up', '-d', '--wait', '--wait-timeout', '240'], root)
    run_command(prepare + ['--verify'], root)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', required=True)
    parser.add_argument('--base-url', default='https://install.thingspanel.io')
    parser.add_argument('--expected-version', default='')
    parser.add_argument('--recover-config-only', action='store_true')
    args = parser.parse_args()
    try:
        root = Path(os.path.abspath(args.root))
        if not args.recover_config_only and not (root / 'config/native/internal.crt').is_file() and shutil.which('openssl') is None:
            raise UpdateError('OpenSSL is required by native certificate preparation; no configuration was replaced')
        transaction = Transaction(args.root)
        transaction.run(args.base_url, expected_version=args.expected_version,
                        activate=activate_native, recovery_only=args.recover_config_only)
        print('Configuration recovery complete.' if args.recover_config_only else 'Installation operation completed.')
    except (UpdateError, OSError) as error:
        # Avoid arbitrary subprocess output or URLs in the error channel.
        message = str(error) if isinstance(error, UpdateError) else 'Installer filesystem operation failed; preserve the update journal for recovery'
        print('[ERROR] ' + message, file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
