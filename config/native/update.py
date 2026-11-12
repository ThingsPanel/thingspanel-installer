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
import importlib.util
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


# Signed release verification (optional until a release trust root is supplied).
# The verifier is loaded only if its SHA-256 equals this pin, which is itself
# covered by the entry points' pin of this file.
RELEASE_VERIFIER = 'config/native/release_manifest.py'
RELEASE_VERIFIER_SHA256 = "717abbe4a5a52e949042913d5aec5ea349ce41ce1afab814965eecae717b9374"
RELEASE_MANIFEST = 'release-manifest.json'
DEFAULT_RELEASE_PUBLISHER = 'https://github.com/ThingsPanel/thingspanel-installer/.github/workflows/release.yml'

# Independent plugin trust anchor, reviewed with the installer source. The
# signed plugin catalog is never allowed to introduce or replace this key.
PLUGIN_TRUST_PEM = b'''-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEx5sN5wE3uKMeQNg/GfwdgdECr3Dl
HwERLCSb0Ng2n3l4djp5nlaxGQdYZFcwGZJ0UhdPyohfgnwpGnu6eZKicw==
-----END PUBLIC KEY-----
'''
# Helpers are independently pinned by the entry-point-pinned updater. Keep old
# profiles here when changing helper code so an offline last-good bundle can
# still be restored by a later updater. R5a: the first profile adds the system role to agent_host.py and lets
# scope_sync.py defer to the Agent; the second is the previous release's.
PLUGIN_HELPER_PROFILES = ({
    'tools/plugin_bundle.py': 'a8fa326430c7433b20dc2cc699b115063be5e498a6a45f533dcf395a34a0ecfa',
    'tools/plugin_catalog.py': 'fe0c4c4cb5ab9af8bfb08d0003e112c07d87680e62d8017b3d0996fdae0cca21',
    'config/native/release_manifest.py': '717abbe4a5a52e949042913d5aec5ea349ce41ce1afab814965eecae717b9374',
    'tools/agent_host.py': 'ff29d112d21d8ec29e3711d8b378e25919c700a96fabc66e06464be10d0454c4',
    'tools/scope_sync.py': '592b29d7401f1ad18f66b511007584c6b41688817bd4a4fea7aa7ee85a6d40c9',
}, {
    'tools/plugin_bundle.py': 'a8fa326430c7433b20dc2cc699b115063be5e498a6a45f533dcf395a34a0ecfa',
    'tools/plugin_catalog.py': 'fe0c4c4cb5ab9af8bfb08d0003e112c07d87680e62d8017b3d0996fdae0cca21',
    'config/native/release_manifest.py': '717abbe4a5a52e949042913d5aec5ea349ce41ce1afab814965eecae717b9374',
    'tools/agent_host.py': '037dd423bab55cf48b56b27eca736e28eb1e05a6cd75e4a425586b3ac56705f9',
    'tools/scope_sync.py': 'f995ff0d2fdc23225aadceb9be78709493fd1a5f8f6094173086d63e8cf5e4c1',
},)


COMMON = (
    ('docker-compose.yml', 0o644), ('niginx.conf', 0o644),
    ('config/native/prepare.py', 0o644),
    ('config/native/runtime.encore.json', 0o644),
    ('config/native/runtime.notification.json', 0o644),
    ('config/native/tick.sh', 0o644), ('config/native/README.md', 0o644),
)
LEGACY_WATCHED = ('.env', 'config/native/plugin-token')
ACTIVATION_FILES = ('.env', 'config/native/broker/plugin-token',
                    'config/native/broker/owner-tokens.json', 'config/native/broker/services.json')
WATCHED = LEGACY_WATCHED + ('config/native/owner-tokens.json', 'config/native/services.json') + ACTIVATION_FILES[1:]


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
        previous_mode = None
        if destination.exists():
            previous_mode = stat.S_IMODE(destination.stat().st_mode)
            if not previous_mode & stat.S_IWRITE:
                # Windows represents the broker's POSIX 0440 mode as the
                # read-only file attribute. Clear it only for the atomic
                # replacement and restore it if replacement fails.
                os.chmod(destination, previous_mode | stat.S_IWRITE)
        if not move(str(source), str(destination), 0x1 | 0x8):
            error_code = ctypes.get_last_error()
            if previous_mode is not None and not previous_mode & stat.S_IWRITE:
                os.chmod(destination, previous_mode)
            raise ctypes.WinError(error_code)
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
            # Keep the source writable until MoveFileExW has consumed it.
            # Windows maps POSIX 0440 to a read-only attribute, which blocks
            # both replacement and cleanup; apply the requested mode afterward.
            if os.name != 'nt':
                os.fchmod(stream.fileno(), mode) if hasattr(os, 'fchmod') else os.chmod(temporary, mode)
            if ownership is not None and os.name != 'nt':
                info = os.fstat(stream.fileno())
                if (info.st_uid, info.st_gid) != (ownership['uid'], ownership['gid']):
                    os.fchown(stream.fileno(), ownership['uid'], ownership['gid'])
            os.fsync(stream.fileno())
        replace(temporary, path)
        if os.name == 'nt':
            os.chmod(path, mode)
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


def run_command(args, root, *, data=None, extra_env=None):
    env = command_environment(root)
    if extra_env:
        env.update(extra_env)
    try:
        result = subprocess.run(args, cwd=root, input=data, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, check=False,
                                env=env)
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
        backend_service = services['backend']
        backend = environment(backend_service['environment'])
        url = vis['DATABASE_URL']
        parsed = urlsplit(url)
        if parsed.scheme not in ('postgres', 'postgresql') or not parsed.hostname or '$' in parsed.hostname:
            raise ValueError()
        required = ('GOTP_DB_PSQL_HOST', 'GOTP_DB_PSQL_PORT', 'GOTP_DB_PSQL_USERNAME', 'GOTP_DB_PSQL_DBNAME')
        if any(not isinstance(backend.get(k), (str, int)) or not str(backend[k]).strip() or '$' in str(backend[k]) for k in required):
            raise ValueError()
        if backend['GOTP_DB_PSQL_HOST'] == 'postgres' and not isinstance(services.get('postgres'), dict):
            raise ValueError()
        relational_required = ('GOTP_RELATIONAL_POSTGRES_PLATFORM_HOST', 'GOTP_RELATIONAL_POSTGRES_PLATFORM_PORT',
                               'GOTP_RELATIONAL_POSTGRES_PLATFORM_DATABASE', 'GOTP_RELATIONAL_POSTGRES_PLATFORM_USERNAME',
                               'GOTP_RELATIONAL_POSTGRES_PLATFORM_PASSWORD')
        relational_keys = [key for key in backend if key.startswith('GOTP_RELATIONAL_')]
        if relational_keys:
            if backend.get('GOTP_RELATIONAL_DRIVER') != 'postgres':
                raise ValueError()
            if any(not isinstance(backend.get(k), (str, int)) or not str(backend[k]).strip() or '$' in str(backend[k]) for k in relational_required):
                raise ValueError()
            if backend['GOTP_RELATIONAL_POSTGRES_PLATFORM_HOST'] == 'postgres' and not isinstance(services.get('postgres'), dict):
                raise ValueError()
        selectors = ('GOTP_RELATIONAL_DRIVER', 'GOTP_DB_PSQL_DBTYPE', 'GOTP_DB_DRIVER', 'GOTP_DB_PROVIDER', 'GOTP_DB_TYPE',
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
                 validator=None, checkpoint=None, release_policy=None):
        # release_policy: None (no signature check) or a dict with 'trust_root',
        # 'publisher' and 'revoked'. A policy without a usable trust root fails closed.
        self.release_policy = release_policy
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
            if data['version'] != 1 or not isinstance(data['files'], list) or not isinstance(data['root'], str):
                raise ValueError()
            if data['root'] != str(self.root):
                # A finished installation may be relocated; an unresolved one is
                # bound to the path whose files and services it describes.
                if data['phase'] not in ('complete', 'recovered'):
                    raise ValueError()
                data['root'] = str(self.root)
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
            if not isinstance(data['watched'], dict) or set(data['watched']) not in (set(WATCHED), set(LEGACY_WATCHED)):
                raise ValueError()
            for value in data['watched'].values():
                fingerprint(value, nullable=True)
            activation_files = data.get('activation_files', {})
            if not isinstance(activation_files, dict) or any(p not in ACTIVATION_FILES for p in activation_files):
                raise ValueError()
            if activation_files and data['phase'] not in ('activation_started', 'complete'):
                raise ValueError()
            activation_dirs = data.get('activation_dirs', [])
            if not isinstance(activation_dirs, list) or len(activation_dirs) != len(set(activation_dirs)) or any(p not in ('config', 'config/native', 'config/native/broker') for p in activation_dirs):
                raise ValueError()
            if activation_dirs and data['phase'] not in ('activation_started', 'complete'):
                raise ValueError()
            for relative, entry in activation_files.items():
                if not isinstance(entry, dict) or set(entry) != {'old', 'new', 'applied'} or type(entry['applied']) is not bool:
                    raise ValueError()
                fingerprint(entry['old'], nullable=True)
                fingerprint(entry['new'])
                if relative not in data['watched'] or entry['old'] != data['watched'][relative]:
                    raise ValueError()
                if data['phase'] == 'complete' and not entry['applied']:
                    raise ValueError()
                safe_target(self.root, relative)
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
            # Without interpolation the reviewed default is the part after ':-'.
            default = re.fullmatch(r'\$\{TP_BACKEND_IMAGE:-([^${}]+)\}', image)
            if default:
                image = default.group(1)
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

    def load_release_verifier(self, source, directory):
        sibling = Path(__file__).resolve().with_name('release_manifest.py')
        candidate = sibling
        if file_info(sibling) is None or hashlib.sha256(sibling.read_bytes()).hexdigest() != RELEASE_VERIFIER_SHA256:
            candidate = directory / 'verifier' / 'release_manifest.py'
            self.downloader(source.rstrip('/') + '/' + RELEASE_VERIFIER, candidate)
        content = candidate.read_bytes()
        if hashlib.sha256(content).hexdigest() != RELEASE_VERIFIER_SHA256:
            raise UpdateError('Release verifier does not match the digest pinned by this updater')
        spec = importlib.util.spec_from_loader('tp_release_manifest', loader=None)
        module = importlib.util.module_from_spec(spec)
        exec(compile(content, str(candidate), 'exec'), module.__dict__)
        return module

    def verify_signed_release(self, source, directory, stage, data, expected_version):
        policy = self.release_policy
        verifier = self.load_release_verifier(source, directory)
        release = directory / 'release'
        private_directory(release)
        base = source.rstrip('/') + '/' + RELEASE_MANIFEST
        self.downloader(base, release / 'manifest.json')
        self.downloader(base + '.sig', release / 'manifest.json.sig')
        files = {entry['path']: stage / entry['path'] for entry in data['files']}
        try:
            verifier.verify_files(
                (release / 'manifest.json').read_bytes(), (release / 'manifest.json.sig').read_bytes(),
                policy.get('trust_root'), files,
                expected_publisher=policy.get('publisher') or DEFAULT_RELEASE_PUBLISHER,
                expected_version=expected_version or None,
                revoked_versions=frozenset(policy.get('revoked') or ()))
        except verifier.ManifestError as error:
            # Only the machine-readable reason; never echo manifest content.
            raise UpdateError('Signed release verification failed: ' + error.reason) from None

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
            if self.release_policy is not None:
                self.verify_signed_release(source, directory, stage, data, expected_version)
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

    def write_activation_environment(self, content):
        self.write_activation_file('.env', content, 0o600)

    def write_activation_file(self, relative, content, mode, ownership=None):
        """Journal the native .env effect; never infer service/DB rollback.

        The formal entrypoint holds update_lock while its prepare child uses
        this method. Before activation .env remains a watched input. During
        activation its original backup and intended new bytes are recorded in
        the same private transaction before the atomic target replacement.
        """
        data = self.load()
        if data is None or data['phase'] != 'activation_started' or relative not in ACTIVATION_FILES or relative not in data['watched']:
            raise UpdateError('Configuration changes require the active installation journal and a known file')
        target = safe_target(self.root, relative)
        actual = file_info(target)
        prior = data.get('activation_files', {}).get(relative)
        if prior is not None:
            if not prior['applied'] or not same(actual, prior['new']):
                raise UpdateError('Previous configuration activation is unresolved; preserve the journal')
        elif not same(actual, data['watched'][relative]):
            raise UpdateError('Configuration changed before native activation')
        original = data['watched'][relative]
        directory = self.state / data['operation']
        backup = directory / 'backup' / relative
        if prior is None and original is not None:
            durable_bytes(backup, target.read_bytes(), original['mode'], original)
        if original is not None and not same(file_info(backup), original):
            raise UpdateError('Configuration activation backup is missing or changed')
        staged = directory / 'stage' / relative
        owner = ownership if ownership is not None else original
        durable_bytes(staged, content, mode, owner)
        entry = {'old': original, 'new': file_info(staged), 'applied': False}
        data.setdefault('activation_files', {})[relative] = entry
        missing = []
        parent = target.parent
        while parent != self.root and not parent.exists():
            missing.append(parent.relative_to(self.root).as_posix())
            parent = parent.parent
        for path in reversed(missing):
            if path not in data.setdefault('activation_dirs', []):
                data['activation_dirs'].append(path)
        self.save(data)
        self.checkpoint('activation-env-prepared' if relative == '.env' else 'activation-file-prepared:' + relative)
        expected = prior['new'] if prior is not None else original
        if not same(file_info(target), expected) or not same(file_info(staged), entry['new']):
            raise UpdateError('Configuration changed during native activation')
        durable_bytes(target, staged.read_bytes(), mode, owner)
        self.checkpoint('activation-env-written' if relative == '.env' else 'activation-file-written:' + relative)
        entry['applied'] = True
        self.save(data)

    def reconcile_activation(self, plugin_trust_root=None, *, enable_plugin_bundle=False):
        """Resume an unresolved activation only by re-running its idempotent tail
        (migration bootstrap, compose up, verification).

        The journal stays ``activation_started`` unless services come up and
        native verification passes; nothing is rolled back or re-applied.
        """
        agent_started = False
        with update_lock(self.state):
            data = self.load()
            if data is None or data['phase'] != 'activation_started':
                raise UpdateError('There is no unresolved activation to reconcile')
            for relative, entry in data.get('activation_files', {}).items():
                if not entry['applied'] or not same(file_info(safe_target(self.root, relative)), entry['new']):
                    raise UpdateError('Activation configuration is not in its recorded state; manual review is required: ' + relative)
            # The managed public flow uses the reviewed embedded trust anchor
            # and last-good bundle. Do not let an old partial root-level bundle
            # prevent recovery of the ThingsPanel core or the managed Agent.
            catalog = False if enable_plugin_bundle else verify_plugin_agent_bundle(self.root, plugin_trust_root)
            managed = None
            if catalog:
                preload_homeassistant_image(self.root, catalog)
            elif enable_plugin_bundle:
                managed = optional_plugin_for_activation(self.root, plugin_trust_root, allow_download=False)
                if managed:
                    try:
                        preload_homeassistant_image(managed[0], managed[2])
                    except Exception as error:  # noqa: BLE001 - leave an untrusted bundle inactive.
                        print('[WARN] Plugin image rejected (%s); platform recovery continues.' % type(error).__name__, file=sys.stderr)
                        managed = None
            context = {'TP_INSTALLER_ACTIVE_OPERATION': data['operation']}
            prepare = [sys.executable, str(self.root / 'config/native/prepare.py')]
            run_command(prepare + ['--bootstrap', '--refresh-devices'], self.root, extra_env=context)
            if managed:
                try:
                    agent_started = start_plugin_agent(self.root, managed[1], verified_catalog=managed[2], bundle_root=managed[0])
                    if agent_started and managed[3]:
                        record_active_plugin(self.root, managed[0])
                        clear_pending_plugin(self.root)
                except Exception as error:  # noqa: BLE001 - recover the platform even when the optional Agent fails.
                    print('[WARN] Plugin Agent could not start (%s); platform recovery continues.' % type(error).__name__, file=sys.stderr)
                    agent_started = False
            else:
                agent_started = start_plugin_agent(self.root, plugin_trust_root, verified_catalog=catalog)
            run_command(['docker', 'compose', 'up', '-d', '--wait', '--wait-timeout', '240'], self.root)
            restart_services_with_replaced_mounts(self.root)
            run_command(prepare + ['--verify'], self.root, extra_env=context)
            data['phase'] = 'complete'
            self.save(data)
        if agent_started:
            start_scope_watcher_after_activation(self.root)
        return agent_started

    def run(self, source, *, expected_version='', activate=None, recovery_only=False):
        agent_started = False
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
                    agent_started = bool(activate(self.root))
                    # Native preparation records its .env effect in this same
                    # journal. Preserve it when committing the outer result.
                    activated = self.load()
                    if activated is None or activated['operation'] != data['operation'] or activated['phase'] != 'activation_started':
                        raise UpdateError('Activation journal changed unexpectedly')
                    data = activated
                data['phase'] = 'complete'
                self.save(data)
            except BaseException:
                if data['phase'] != 'activation_started':
                    self.recover(data)
                raise
        if agent_started:
            start_scope_watcher_after_activation(self.root)
        return agent_started


def restart_services_with_replaced_mounts(root):
    """Restart services whose single-file bind mounts were replaced after they started.

    Managed files are replaced atomically (a new inode); a running container keeps
    the old inode, and some services then fail every request until restarted.
    """
    config = json.loads(run_command(['docker', 'compose', 'config', '--format', 'json'], root))
    base = os.path.realpath(str(root))
    stale = []
    for name, service in sorted(config.get('services', {}).items()):
        newest = 0.0
        for volume in service.get('volumes', []):
            source = volume.get('source') if isinstance(volume, dict) and volume.get('type') == 'bind' else None
            if not source or not os.path.isfile(source) or not os.path.realpath(source).startswith(base + os.sep):
                continue
            newest = max(newest, os.stat(source).st_mtime)
        if not newest:
            continue
        container = run_command(['docker', 'compose', 'ps', '-q', name], root).decode('utf-8', 'replace').strip().split('\n')[0]
        if not container:
            continue
        started = run_command(['docker', 'inspect', '--format', '{{.State.Running}} {{.State.StartedAt}}', container], root).decode().strip().split()
        if len(started) != 2 or started[0] != 'true':
            continue
        timestamp = started[1]
        match = re.fullmatch(r'(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d+))?Z', timestamp)
        if match is None:
            continue
        from datetime import datetime, timezone
        begun = datetime.strptime(match.group(1), '%Y-%m-%dT%H:%M:%S').replace(tzinfo=timezone.utc).timestamp()
        if newest > begun:
            stale.append(name)
    if stale:
        print('Restarting services whose mounted files were replaced: ' + ', '.join(stale), flush=True)
        run_command(['docker', 'compose', 'restart'] + stale, root)
        # Services sharing another service's network namespace need to follow it.
        run_command(['docker', 'compose', 'up', '-d', '--wait', '--wait-timeout', '240'], root)
    return stale


def plugin_bundle_parent(root):
    root = Path(root).resolve()
    state = root / '.thingspanel-update'
    private_directory(state)
    parent = state / 'plugin-bundles'
    private_directory(parent)
    return parent


def plugin_trust_root(root, supplied=None):
    """Use an operator root or materialize the installer's pinned public root outside the payload."""
    root = Path(root).resolve()
    if supplied:
        key = Path(supplied)
        if not key.is_absolute() or not key.is_file() or key.is_symlink():
            raise UpdateError('Plugin trust root is unavailable or invalid')
        key = key.resolve()
        try:
            key.relative_to(root)
        except ValueError:
            return key
        raise UpdateError('Plugin trust root must be outside the installer payload')
    key = root.parent / ('.' + root.name + '-plugin-trust.pem')
    try:
        fd = os.open(str(key), os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, 'O_NOFOLLOW', 0), 0o644)
    except FileExistsError:
        info = file_info(key)
        if info is None or key.read_bytes() != PLUGIN_TRUST_PEM:
            raise UpdateError('Existing plugin trust root does not match the installer pin')
    else:
        with os.fdopen(fd, 'wb') as out:
            out.write(PLUGIN_TRUST_PEM)
            out.flush()
            os.fsync(out.fileno())
        fsync_directory(key.parent)
    return key.resolve()


def load_plugin_helpers(helper_root):
    """Load only helper files matching an updater-pinned complete profile."""
    helper_root = Path(helper_root)
    if helper_root.is_symlink() or not helper_root.is_dir():
        raise UpdateError('Plugin helper directory is invalid')
    matched = False
    for profile in PLUGIN_HELPER_PROFILES:
        if all((info := file_info(safe_target(helper_root, name))) is not None and info['sha256'] == digest
               for name, digest in profile.items()):
            matched = True
            break
    if not matched:
        raise UpdateError('Plugin helper files do not match the installer pin')
    previous = sys.modules.get('plugin_catalog')
    try:
        catalog_spec = importlib.util.spec_from_file_location('plugin_catalog', helper_root / 'tools/plugin_catalog.py')
        catalog_module = importlib.util.module_from_spec(catalog_spec)
        sys.modules['plugin_catalog'] = catalog_module
        catalog_spec.loader.exec_module(catalog_module)
        spec = importlib.util.spec_from_file_location('tp_plugin_bundle_bootstrap', helper_root / 'tools/plugin_bundle.py')
        module = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = module
        spec.loader.exec_module(module)
        return module
    finally:
        if previous is None:
            sys.modules.pop('plugin_catalog', None)
        else:
            sys.modules['plugin_catalog'] = previous


def active_plugin_directory(root):
    """Read a private last-good pointer without following paths or symlinks."""
    return plugin_directory_from_pointer(root, 'current.json', 'Last-good')


def pending_plugin_directory(root):
    """Read a durably staged first-install bundle awaiting Agent activation."""
    return plugin_directory_from_pointer(root, 'pending.json', 'Pending')


def plugin_directory_from_pointer(root, filename, label):
    parent = plugin_bundle_parent(root)
    pointer = parent / filename
    if file_info(pointer) is None:
        return None
    try:
        value = json.loads(pointer.read_text(encoding='utf-8'))
        name = value['directory']
        if not isinstance(name, str) or re.fullmatch(r'tp-plugin-bundle-[A-Za-z0-9_-]+', name) is None:
            raise ValueError()
        directory = parent / name
        if directory.is_symlink() or not directory.is_dir():
            raise ValueError()
        return directory
    except (KeyError, TypeError, ValueError, OSError):
        raise UpdateError(label + ' plugin bundle pointer is invalid') from None


def record_active_plugin(root, bundle):
    write_plugin_pointer(root, bundle, 'current.json', 'activated')


def record_pending_plugin(root, bundle):
    write_plugin_pointer(root, bundle, 'pending.json', 'pending')


def write_plugin_pointer(root, bundle, filename, label):
    parent = plugin_bundle_parent(root)
    bundle = Path(bundle)
    if bundle.parent != parent or bundle.is_symlink() or not bundle.is_dir() or re.fullmatch(r'tp-plugin-bundle-[A-Za-z0-9_-]+', bundle.name) is None:
        raise UpdateError('Plugin bundle cannot be recorded outside its private directory')
    pointer = parent / filename
    file_info(pointer)  # Refuse an existing symlink or non-regular pointer.
    durable_bytes(pointer, (json.dumps({'directory': bundle.name}, sort_keys=True) + '\n').encode(), 0o600)


def clear_pending_plugin(root):
    pointer = plugin_bundle_parent(root) / 'pending.json'
    if file_info(pointer) is not None:
        pointer.unlink()
        fsync_directory(pointer.parent)


def prepare_optional_plugin(root, supplied_trust_root=None, *, allow_download=False,
                            source='https://install.thingspanel.io'):
    """Verify an active bundle, or stage a new one without touching the active pointer."""
    root = Path(root).resolve()
    parent = plugin_bundle_parent(root)
    active = active_plugin_directory(root)
    pending = pending_plugin_directory(root) if active is None else None
    if active is None and pending is None and not allow_download:
        return None
    trust = plugin_trust_root(root, supplied_trust_root)
    platform = normalize_platform(run_command(['docker', 'info', '--format', '{{.OSType}}/{{.Architecture}}'], root).decode().strip())
    if active is not None:
        if pending_plugin_directory(root) == active:
            clear_pending_plugin(root)
        helpers = load_plugin_helpers(active)
        catalog = helpers.verify_active_bundle(active, trust, platform)
        return active, trust, catalog, False
    if pending is not None:
        helpers = load_plugin_helpers(pending)
        try:
            catalog = helpers.verify_active_bundle(pending, trust, platform, require_unexpired=True)
        except Exception as error:  # noqa: BLE001 - only an authenticated expired marker is eligible for refresh.
            if 'pending catalog expired before activation' not in str(error) or not allow_download:
                raise
            # This pointer was created by this updater but never activated. Keep
            # the expired bundle intact for diagnosis; remove only its pointer,
            # then stage the current independently signed release.
            clear_pending_plugin(root)
            pending = None
        else:
            return pending, trust, catalog, True
    helper_root = Path(tempfile.mkdtemp(prefix='tp-plugin-helpers-', dir=parent))
    os.chmod(helper_root, 0o700)
    try:
        for relative, digest in PLUGIN_HELPER_PROFILES[0].items():
            target = safe_target(helper_root, relative)
            download(source.rstrip('/') + '/' + relative, target)
            info = file_info(target)
            if info is None or info['sha256'] != digest:
                raise UpdateError('Downloaded plugin helper does not match the installer pin: ' + relative)
        helpers = load_plugin_helpers(helper_root)
        staged = helpers.stage_plugin_bundle(platform, trust_root=trust, stage_parent=parent)
        bundle = helpers.arrange_staged_bundle(staged)
        # Move only the helper files we just downloaded and pinned; no active bundle is touched.
        for relative in PLUGIN_HELPER_PROFILES[0]:
            destination = safe_target(bundle, relative)
            make_directories(destination.parent, 0o700)
            os.replace(safe_target(helper_root, relative), destination)
        helpers = load_plugin_helpers(bundle)
        catalog = helpers.verify_active_bundle(bundle, trust, platform, require_unexpired=True)
        # Persist the candidate before Docker/Agent side effects so activation can
        # resume after a host crash without downloading or creating a new instance.
        record_pending_plugin(root, bundle)
        return bundle, trust, catalog, True
    finally:
        shutil.rmtree(helper_root, ignore_errors=True)


def optional_plugin_for_activation(root, supplied_trust_root=None, *, allow_download=False,
                                   source='https://install.thingspanel.io'):
    """Plugin delivery is optional: a rejected bundle must not stop the platform itself."""
    try:
        return prepare_optional_plugin(root, supplied_trust_root, allow_download=allow_download, source=source)
    except Exception as error:  # noqa: BLE001 - never execute/load a rejected optional bundle.
        print('[WARN] Plugin bundle unavailable (%s); platform startup continues.' % type(error).__name__, file=sys.stderr)
        return None


def activate_native(root, plugin_trust_root=None, *, enable_plugin_bundle=False,
                    source='https://install.thingspanel.io'):
    """Existing native entry point, after commit, under the same process lock."""
    prepare = [sys.executable, str(root / 'config/native/prepare.py')]
    current = Transaction(root).load()
    if current is None or current['phase'] != 'activation_started':
        raise UpdateError('Native activation requires its persistent operation')
    # New install/upgrade flow uses the pinned managed bundle. Older root-level
    # files may be partial or require an obsolete out-of-band key; they remain
    # untouched and cannot block the current platform activation.
    catalog = False if enable_plugin_bundle else verify_plugin_agent_bundle(root, plugin_trust_root)
    managed = None
    if catalog:
        preload_homeassistant_image(root, catalog)
    elif enable_plugin_bundle:
        managed = optional_plugin_for_activation(root, plugin_trust_root, allow_download=True, source=source)
        if managed:
            try:
                preload_homeassistant_image(managed[0], managed[2])
            except Exception as error:  # noqa: BLE001 - do not activate a rejected optional image.
                print('[WARN] Plugin image rejected (%s); platform startup continues.' % type(error).__name__, file=sys.stderr)
                managed = None
    context = {'TP_INSTALLER_ACTIVE_OPERATION': current['operation']}

    def native(*args):
        return run_command(prepare + list(args), root, extra_env=context)

    print('Preparing native configuration (configuration rollback ends here).', flush=True)
    native()
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
        native('--pull')
    print('Applying the existing native migration and service preparation.', flush=True)
    native('--bootstrap', '--refresh-devices')
    if managed:
        try:
            agent_started = start_plugin_agent(root, managed[1], verified_catalog=managed[2], bundle_root=managed[0])
            if agent_started and managed[3]:
                record_active_plugin(root, managed[0])
                clear_pending_plugin(root)
        except Exception as error:  # noqa: BLE001 - keep platform startup independent of the optional plugin.
            print('[WARN] Plugin Agent could not start (%s); platform startup continues.' % type(error).__name__, file=sys.stderr)
            agent_started = False
    else:
        agent_started = start_plugin_agent(root, plugin_trust_root, verified_catalog=catalog)
    run_command(['docker', 'compose', 'up', '-d', '--wait', '--wait-timeout', '240'], root)
    restart_services_with_replaced_mounts(root)
    native('--verify')
    return agent_started


def load_verified_plugin_catalog(root, trust_root):
    """Load the exact signed catalog through the same verifier used by the Agent."""
    path = Path(root) / 'tools/plugin_catalog.py'
    spec = importlib.util.spec_from_file_location('tp_installer_plugin_catalog', path)
    if spec is None or spec.loader is None:
        raise UpdateError('Plugin catalog verifier is unavailable; backend was not started')
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    try:
        return module.load_verified(Path(root) / 'plugins/plugin-catalog.json', trust_root)
    except Exception as error:  # noqa: BLE001 - fail closed at the installer boundary
        raise UpdateError('Plugin catalog signature or contents are invalid; backend was not started') from error


def normalize_platform(value):
    """Normalize Docker and Go architecture names to catalog platform names."""
    parts = value.strip().lower().split('/')
    if len(parts) != 2:
        return ''
    aliases = {'aarch64': 'arm64', 'x86_64': 'amd64'}
    parts[1] = aliases.get(parts[1], parts[1])
    return '/'.join(parts)


def preload_homeassistant_image(root, catalog):
    """Load and verify the signed HA image before the Agent or backend can start."""
    entries = [entry for entry in catalog.get('plugins', []) if entry.get('id') == 'homeassistant']
    if not entries:
        return False
    if len(entries) != 1:
        raise UpdateError('Home Assistant catalog entry is ambiguous; backend was not started')
    entry = entries[0]
    image_id = entry.get('image_id', '')
    target = normalize_platform(entry.get('image_platform', ''))
    archive_sha256 = entry.get('image_archive_sha256', '')
    if not image_id.startswith('sha256:') or len(image_id) != 71 or not target.startswith('linux/'):
        raise UpdateError('Home Assistant catalog image identity or target platform is invalid; backend was not started')
    if not isinstance(archive_sha256, str) or re.fullmatch(r'[0-9a-f]{64}', archive_sha256) is None:
        raise UpdateError('Home Assistant catalog image archive digest is invalid; backend was not started')

    archive = Path(root) / 'plugins/homeassistant-image.tar'
    if not archive.is_file() or archive.is_symlink():
        raise UpdateError('Home Assistant image archive is missing or invalid; backend was not started')

    try:
        digest = hashlib.sha256()
        with archive.open('rb') as stream:
            for block in iter(lambda: stream.read(1024 * 1024), b''):
                digest.update(block)
    except OSError:
        raise UpdateError('Unable to verify Home Assistant image archive; backend was not started') from None
    if digest.hexdigest() != archive_sha256:
        raise UpdateError('Home Assistant image archive digest does not match the signed catalog; backend was not started')

    try:
        docker_platform = run_command(
            ['docker', 'info', '--format', '{{.OSType}}/{{.Architecture}}'], root
        ).decode().strip()
    except (UpdateError, OSError):
        raise UpdateError('Unable to determine Docker target platform; backend was not started') from None
    if normalize_platform(docker_platform) != target:
        raise UpdateError('Home Assistant image target platform does not match Docker; backend was not started')

    run_command(['docker', 'load', '-i', str(archive)], root)
    try:
        inspected = run_command(
            ['docker', 'image', 'inspect', '--format', '{{.Id}} {{.Os}}/{{.Architecture}}', image_id], root
        ).decode().strip().split()
    except (UpdateError, OSError):
        raise UpdateError('Loaded Home Assistant image is unavailable; backend was not started') from None
    if len(inspected) != 2 or inspected[0] != image_id or normalize_platform(inspected[1]) != target:
        raise UpdateError('Loaded Home Assistant image digest or platform does not match the signed catalog; backend was not started')
    return True


def verify_plugin_agent_bundle(root, plugin_trust_root=None):
    """Verify an optional plugin Agent bundle before any migration or service startup."""
    root = Path(root).resolve()
    agent_bin = root / 'bin/tp-agent'
    catalog = root / 'plugins/plugin-catalog.json'
    signature = root / 'plugins/plugin-catalog.json.sig'
    bundle = (agent_bin, catalog, signature)
    present = tuple(path.is_file() for path in bundle)
    if not any(present):
        return False
    if not all(present):
        raise UpdateError('Plugin Agent release bundle is incomplete; backend was not started')
    if not plugin_trust_root:
        raise UpdateError('Plugin trust root was not supplied out of band; backend was not started')
    trust_root = Path(plugin_trust_root)
    if not trust_root.is_absolute():
        raise UpdateError('Plugin trust root path must be absolute and supplied out of band')
    trust_root = trust_root.resolve()
    if not trust_root.is_file():
        raise UpdateError('Plugin trust root is unavailable; backend was not started')
    try:
        trust_root.relative_to(root)
    except ValueError:
        pass
    else:
        raise UpdateError('Plugin trust root must be outside the installer payload')
    return load_verified_plugin_catalog(root, trust_root)


def start_plugin_agent(root, plugin_trust_root=None, verified_catalog=None, bundle_root=None):
    """Start the already-verified plugin Agent after DB/network bootstrap but before the backend."""
    root = Path(root).resolve()
    catalog_data = verified_catalog if verified_catalog is not None else verify_plugin_agent_bundle(root, plugin_trust_root)
    if not catalog_data:
        return False
    if verified_catalog is None:
        preload_homeassistant_image(root, catalog_data)
    bundle = Path(bundle_root).resolve() if bundle_root is not None else root
    agent_bin = bundle / 'bin/tp-agent'
    catalog = bundle / 'plugins/plugin-catalog.json'
    trust_root = Path(plugin_trust_root).resolve()
    run_command([
        sys.executable, str(bundle / 'tools/agent_host.py'), 'up',
        '--stack-dir', str(root), '--agent-dir', str(root / '.tp-agent'),
        '--agent-bin', str(agent_bin), '--catalog', str(catalog), '--trust-root', str(trust_root),
        '--defer-scope-watcher', '--system',
    ], root)
    return True


def start_scope_watcher_after_activation(root):
    """Start the optional Broker scope watcher after the transaction lock is released."""
    root = Path(root).resolve()
    try:
        active = active_plugin_directory(root)
        if active:
            load_plugin_helpers(active)
        scope_sync_path = (active or root) / 'tools/scope_sync.py'
    except Exception as error:  # noqa: BLE001 - do not load unverified helper code.
        print('[WARN] Plugin scope watcher is unavailable (%s).' % type(error).__name__, file=sys.stderr)
        return False
    spec = importlib.util.spec_from_file_location('tp_installer_scope_sync', scope_sync_path)
    if spec is None or spec.loader is None:
        print('[WARN] Plugin Agent is running but its Broker scope watcher could not load; retry with tools/scope_sync.py ensure.',
              file=sys.stderr)
        return False
    module = importlib.util.module_from_spec(spec)
    try:
        spec.loader.exec_module(module)
        status = module.ensure(root, root / '.tp-agent')
    except Exception as error:  # noqa: BLE001 - watcher startup is retryable and must not fail platform activation.
        print('[WARN] Plugin Agent is running but its Broker scope watcher did not start (%s); retry with tools/scope_sync.py ensure.' %
              type(error).__name__, file=sys.stderr)
        return False
    if status == 'busy':
        print('[WARN] Plugin Agent is running but another installer operation holds the lock; retry scope_sync.py ensure.',
              file=sys.stderr)
        return False
    if status not in ('started', 'running'):
        print('[WARN] Plugin Agent is running but its Broker scope watcher returned an unexpected status; retry scope_sync.py ensure.',
              file=sys.stderr)
        return False
    return True


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', required=True)
    parser.add_argument('--base-url', default='https://install.thingspanel.io')
    parser.add_argument('--expected-version', default='')
    parser.add_argument('--recover-config-only', action='store_true')
    parser.add_argument('--reconcile-activation', action='store_true',
                        help='re-run compose up and verification for an unresolved activation, then close its journal')
    parser.add_argument('--plugin-trust-root', default='',
                        help='absolute out-of-band ECDSA P-256 public key used to verify the plugin catalog')
    parser.add_argument('--release-trust-root', default='', help='pinned ECDSA P-256 public key (PEM) for signed releases')
    parser.add_argument('--require-signed-release', action='store_true',
                        help='fail closed unless the release verifies against --release-trust-root')
    parser.add_argument('--release-publisher', default=DEFAULT_RELEASE_PUBLISHER)
    parser.add_argument('--revoked-version', action='append', default=[])
    args = parser.parse_args()
    try:
        root = Path(os.path.abspath(args.root))
        if not args.recover_config_only and not (root / 'config/native/internal.crt').is_file() and shutil.which('openssl') is None:
            raise UpdateError('OpenSSL is required by native certificate preparation; no configuration was replaced')
        policy = None
        if args.release_trust_root or args.require_signed_release:
            policy = {'trust_root': args.release_trust_root or None, 'publisher': args.release_publisher,
                      'revoked': args.revoked_version}
        elif not args.recover_config_only:
            print('[WARN] No release trust root configured; the release signature is not verified.', file=sys.stderr)
        transaction = Transaction(args.root, release_policy=policy)
        if args.reconcile_activation:
            agent_started = transaction.reconcile_activation(args.plugin_trust_root or None, enable_plugin_bundle=True)
            print('Activation reconciled.')
            if not agent_started:
                print('[WARN] ThingsPanel recovery completed, but the HA plugin Agent is not active. Re-run the installer after fixing the plugin release or host issue.', file=sys.stderr)
                return 2
            return 0
        unresolved = None if args.recover_config_only else transaction.load()
        if unresolved is not None and unresolved['phase'] == 'activation_started':
            print('Resuming the previous interrupted ThingsPanel activation.', flush=True)
            agent_started = transaction.reconcile_activation(args.plugin_trust_root or None, enable_plugin_bundle=True)
            print('Interrupted activation recovered.')
            if not agent_started:
                print('[WARN] ThingsPanel recovery completed, but the HA plugin Agent is not active. Fix the plugin or host issue and re-run the installer.', file=sys.stderr)
                return 2
            return 0
        agent_started = transaction.run(args.base_url, expected_version=args.expected_version,
                        activate=lambda install_root: activate_native(install_root, args.plugin_trust_root or None,
                                                                     enable_plugin_bundle=True, source=args.base_url),
                        recovery_only=args.recover_config_only)
        if not args.recover_config_only and not agent_started:
            print('[WARN] ThingsPanel core is running, but the HA plugin Agent is not active. Fix the reported plugin or host issue and re-run the installer.', file=sys.stderr)
            return 2
        print('Configuration recovery complete.' if args.recover_config_only else 'Installation operation completed.')
    except (UpdateError, OSError) as error:
        # Avoid arbitrary subprocess output or URLs in the error channel.
        message = str(error) if isinstance(error, UpdateError) else 'Installer filesystem operation failed; preserve the update journal for recovery'
        print('[ERROR] ' + message, file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
