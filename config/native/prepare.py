"""Prepare this installer's native service identities; never copy test-host secrets."""
import argparse
import base64
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import hashlib
import importlib.util
import re
import tempfile
import time
import urllib.request
import urllib.error
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[2]
CONFIG = ROOT / 'config/native'
CELL = 'community'
INSTANCE = 'community-mosquitto'
CLIENT = 'tp-community-backend'
_UPDATE_FILES = None
# sql/33.sql: managed plugin runtime identities (T4). Not applied by the backend's own startup migrations.
EXTRA_COMMUNITY_SQL = ('33.sql',)
EXTRA_COMMUNITY_COMPONENT = 'community-ext'
# Marker that tells a new image from an old one: the plugin-identity source ships in the same image as 33.sql.
# VERSION_NUMBER is not usable (it stays 25 while 33.sql exists) and image tags may be arbitrary local tags.
EXTRA_SQL_MARKER = 'internal/pluginidentity'


def command(*args, data=None):
    result = subprocess.run(args, input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=ROOT)
    if result.returncode:
        # Child output and arguments may contain interpolated credentials (for
        # example a database URL). Do not propagate either into installer logs.
        raise RuntimeError(f'Native preparation command failed (exit {result.returncode})')
    return result.stdout.decode().strip()


def image_has_marker(container, marker=EXTRA_SQL_MARKER):
    """True/False when the image clearly has/lacks the marker; raises if that cannot be determined."""
    with tempfile.TemporaryDirectory(prefix='tp-marker-') as temp:
        result = subprocess.run(['docker', 'cp', container + ':/go/src/app/' + marker, temp + '/m'],
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=ROOT)
    if result.returncode == 0: return True
    if b'could not find the file' in result.stderr.lower() or b'no such file' in result.stderr.lower(): return False
    raise RuntimeError('Cannot tell whether the backend image ships plugin-identity migrations (marker probe failed); refusing to guess')


def required_extra_sql(community, has_marker, image, names=EXTRA_COMMUNITY_SQL):
    """Return the extra migration files to apply. A new image (marker present) must ship every one of them;
    an old image (marker absent) does not, and they are skipped with a log line."""
    present = [community / name for name in names if (community / name).is_file()]
    missing = [name for name in names if not (community / name).is_file()]
    if missing and has_marker:
        raise RuntimeError('Backend image ' + image + ' contains ' + EXTRA_SQL_MARKER + ' but is missing required migration '
                           + ', '.join('sql/' + n for n in missing) + '; the image was packaged incorrectly. Refusing to continue.')
    if missing:
        print('Backend image ' + image + ' predates ' + EXTRA_SQL_MARKER + '; skipping ' + ', '.join('sql/' + n for n in missing), flush=True)
    return present


def validate_thingsvis_database(config):
    """Reject unsupported datasource before generating credentials or starting services.

    This guard covers the current ThingsVis service only; it does not certify a
    platform storage profile or inspect any external database.
    """
    service = config.get('services', {}).get('thingsvis-server')
    if not isinstance(service, dict):
        raise RuntimeError('ThingsVis service is missing from the selected Compose configuration')
    environment = service.get('environment')
    if isinstance(environment, list):
        values = [entry.partition('=') for entry in environment if isinstance(entry, str)]
        matches = [value for key, separator, value in values if key == 'DATABASE_URL' and separator]
        if len(matches) != 1:
            raise RuntimeError('ThingsVis DATABASE_URL is missing or ambiguous in the selected Compose configuration')
        environment = {'DATABASE_URL': matches[0]}
    if not isinstance(environment, dict):
        raise RuntimeError('ThingsVis database configuration is missing from the selected Compose configuration')
    database_url = environment.get('DATABASE_URL')
    if not isinstance(database_url, str):
        raise RuntimeError('ThingsVis DATABASE_URL is missing from the selected Compose configuration')
    # Do not echo DATABASE_URL: it can contain credentials.
    parsed_url = urlsplit(database_url)
    if parsed_url.scheme not in ('postgres', 'postgresql') or not parsed_url.hostname or '$' in parsed_url.hostname:
        raise RuntimeError('ThingsVis currently supports only a PostgreSQL datasource in this installer')


def validate_supported_backend_database(config):
    """Conservative guard for the currently shipped PG backend configuration.

    This does not certify the complete platform profile or an external server.
    New provider configuration requires an explicit reviewed contract.
    """
    services = config.get('services', {})
    backend = services.get('backend') if isinstance(services, dict) else None
    if not isinstance(backend, dict):
        raise RuntimeError('Cannot confirm the supported PostgreSQL backend configuration')
    env = backend.get('environment')
    if isinstance(env, list):
        parsed = {}
        for item in env:
            if not isinstance(item, str) or '=' not in item:
                raise RuntimeError('Cannot confirm the supported PostgreSQL backend configuration')
            key, value = item.split('=', 1)
            if key in parsed:
                raise RuntimeError('Cannot confirm the supported PostgreSQL backend configuration')
            parsed[key] = value
        env = parsed
    if not isinstance(env, dict):
        raise RuntimeError('Cannot confirm the supported PostgreSQL backend configuration')
    # Keep accepting legacy-only installed Compose files during upgrade preflight.
    # New defaults carry both contracts until the published v1.3.0 image is retired.
    legacy_required = ('GOTP_DB_PSQL_HOST', 'GOTP_DB_PSQL_PORT', 'GOTP_DB_PSQL_USERNAME', 'GOTP_DB_PSQL_DBNAME')
    if any(not isinstance(env.get(key), (str, int)) or not str(env[key]).strip() or '$' in str(env[key]) for key in legacy_required):
        raise RuntimeError('Cannot confirm the supported PostgreSQL backend configuration')
    if env['GOTP_DB_PSQL_HOST'] == 'postgres' and not isinstance(services.get('postgres'), dict):
        raise RuntimeError('Cannot confirm the supported PostgreSQL backend configuration')
    relational_required = (
        'GOTP_RELATIONAL_POSTGRES_PLATFORM_HOST', 'GOTP_RELATIONAL_POSTGRES_PLATFORM_PORT',
        'GOTP_RELATIONAL_POSTGRES_PLATFORM_DATABASE', 'GOTP_RELATIONAL_POSTGRES_PLATFORM_USERNAME',
        'GOTP_RELATIONAL_POSTGRES_PLATFORM_PASSWORD',
    )
    relational_keys = [key for key in env if key.startswith('GOTP_RELATIONAL_')]
    if relational_keys:
        if env.get('GOTP_RELATIONAL_DRIVER') != 'postgres' or any(
            not isinstance(env.get(key), (str, int)) or not str(env[key]).strip() or '$' in str(env[key])
            for key in relational_required
        ):
            raise RuntimeError('Cannot confirm the supported PostgreSQL backend configuration')
        if env['GOTP_RELATIONAL_POSTGRES_PLATFORM_HOST'] == 'postgres' and not isinstance(services.get('postgres'), dict):
            raise RuntimeError('Cannot confirm the supported PostgreSQL backend configuration')
    for key, value in env.items():
        upper = str(key).upper()
        if upper.startswith(('GOTP_DB_MYSQL_', 'GOTP_DB_MARIADB_')) and value not in (None, ''):
            raise RuntimeError('A MySQL backend is not supported by this installer')
        if upper in ('GOTP_RELATIONAL_DRIVER', 'GOTP_DB_PSQL_DBTYPE', 'GOTP_DB_DRIVER', 'GOTP_DB_PROVIDER', 'GOTP_DB_TYPE', 'DB_DRIVER', 'DB_PROVIDER', 'DB_TYPE', 'DATABASE_TYPE', 'DATABASE_PROVIDER', 'DATABASE_DRIVER') and str(value).lower() not in ('postgres', 'postgresql', 'pg'):
            raise RuntimeError('Cannot confirm the supported PostgreSQL backend configuration')
        if upper == 'DATABASE_URL' and (not isinstance(value, str) or urlsplit(value).scheme not in ('postgres', 'postgresql')):
            raise RuntimeError('Cannot confirm the supported PostgreSQL backend configuration')


def check_thingsvis_database(uninterpolated=False):
    args = ['docker', 'compose', 'config']
    try:
        rendered = command(*args, '--format', 'json')
    except RuntimeError:
        if not uninterpolated:
            raise
        rendered = command(*args, '--no-interpolate', '--format', 'json')
    config = json.loads(rendered)
    validate_thingsvis_database(config)
    validate_supported_backend_database(config)


def sql(database, statement):
    return command('docker', 'compose', 'exec', '-T', 'postgres', 'psql', '-X', '-qAt',
                   '-U', 'postgres', '-d', database, '-v', 'ON_ERROR_STOP=1', data=statement.encode())


def update_files():
    """Load the existing reviewed updater only after datasource preflight."""
    global _UPDATE_FILES
    if _UPDATE_FILES is None:
        spec = importlib.util.spec_from_file_location('tp_native_update_files', Path(__file__).with_name('update.py'))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        _UPDATE_FILES = module
    return _UPDATE_FILES


def save(name, value, broker=False, lock_held=False):
    # Formal install/upgrade holds the updater's process lock. Reuse its
    # durable replacement implementation: writing an existing 0440 broker
    # file in place fails for supported non-root Docker Desktop installs.
    files = update_files()
    path = files.safe_target(CONFIG, ('broker/' if broker else '') + name)
    original = files.file_info(path)
    ownership = original
    if broker and os.name == 'posix' and os.geteuid() == 0:
        ownership = {'uid': 1883, 'gid': 1883}
    write_configuration_file(path, (json.dumps(value, indent=2) + '\n').encode('utf-8'),
                             0o440 if broker else 0o600, ownership, lock_held=lock_held)


def write_configuration_file(path, content, mode, ownership=None, lock_held=False):
    files = update_files()
    original = files.file_info(path)
    owner = ownership if ownership is not None else original
    # Avoid unnecessary inode changes for configuration that is already exact.
    if original is not None and path.read_bytes() == content and original['mode'] == mode:
        if owner is None or os.name == 'nt' or (original['uid'], original['gid']) == (owner['uid'], owner['gid']):
            return
    if (ROOT / '.thingspanel-update/journal.json').exists():
        transaction = files.Transaction(ROOT)
        relative = path.relative_to(ROOT).as_posix()
        data = transaction.load()
        if data and data['phase'] == 'activation_started' and os.environ.get('TP_INSTALLER_ACTIVE_OPERATION') == data['operation']:
            transaction.write_activation_file(relative, content, mode, owner)
        elif data and data['phase'] in ('complete', 'recovered') and relative == 'config/native/broker/services.json':
            # Preserve the existing explicit device-scope refresh command.
            # This is one atomic resource projection, not an install retry or
            # identity change; serialize it against any new installer run.
            if lock_held:
                latest = transaction.load()
                if latest is None or latest['phase'] not in ('complete', 'recovered'):
                    raise RuntimeError('Installation state changed; broker refresh was not applied')
                files.durable_bytes(path, content, mode, owner)
            else:
                with files.update_lock(transaction.state):
                    latest = transaction.load()
                    if latest is None or latest['phase'] not in ('complete', 'recovered'):
                        raise RuntimeError('Installation state changed; broker refresh was not applied')
                    files.durable_bytes(path, content, mode, owner)
        else:
            raise RuntimeError('Configuration requires an active installer operation; preserve unresolved installation state')
    else:
        files.durable_bytes(path, content, mode, owner)


def secret_value(value, key, from_dotenv=False):
    if value is None:
        return None
    if from_dotenv:
        value = value.strip()
        literal = value.startswith("'") and value.endswith("'")
        if len(value) >= 2 and value[0] == value[-1] and value[0] in ('\"', "'"):
            value = value[1:-1]
        if ('$' in value and not literal) or '\\' in value or ' #' in value:
            raise RuntimeError(key + ' must be a stable literal .env value; existing configuration was not changed')
    if len(value) < 32 or any(character in value for character in ('\n', '\r', '\x00')):
        raise RuntimeError(key + ' must contain at least 32 characters; existing configuration was not changed')
    return value


def deployment_secret(current, key):
    """Keep a deployment service credential stable across install and upgrade."""
    saved = current.get(key)
    supplied = os.environ.get(key)

    saved_value, supplied_value = secret_value(saved, key, True), secret_value(supplied, key)
    if saved_value is not None and supplied_value is not None and saved_value != supplied_value:
        raise RuntimeError(key + ' conflicts with the existing deployment; existing configuration was not changed')
    if saved is not None:
        return saved  # Preserve quoting; removing it can enable Compose interpolation.
    if supplied_value is not None:
        if re.fullmatch(r'[A-Za-z0-9_+=./-]+', supplied_value) is None:
            raise RuntimeError(key + ' needs an unambiguous .env encoding; existing configuration was not changed')
        return supplied_value
    return secrets.token_hex(32)


def thingsvis_auth_settings(current):
    """Rotate once only when the verified-SSO artifact is explicitly selected.

    The target is a release-owner assertion bound to its reviewed image lock;
    it is not a claim that this installer has verified a registry signature.
    """
    policy = 'verified-sso-v1'
    recorded = current.get('TP_THINGSVIS_AUTH_POLICY', '')
    target = os.environ.get('TP_THINGSVIS_AUTH_POLICY_TARGET', current.get('TP_THINGSVIS_AUTH_POLICY_TARGET', ''))
    selected = os.environ.get('TP_THINGSVIS_IMAGE', current.get('TP_THINGSVIS_IMAGE', ''))
    local_id = os.environ.get('TP_THINGSVIS_LOCAL_IMAGE_ID', current.get('TP_THINGSVIS_LOCAL_IMAGE_ID', ''))
    if local_id and (not selected or re.fullmatch(r'sha256:[0-9a-f]{64}', local_id) is None):
        raise RuntimeError('A local ThingsVis image requires its exact Docker image ID and selected image')
    selection = {'TP_THINGSVIS_IMAGE': selected} if selected else {}
    if local_id or 'TP_THINGSVIS_LOCAL_IMAGE_ID' in current or 'TP_THINGSVIS_LOCAL_IMAGE_ID' in os.environ:
        selection['TP_THINGSVIS_LOCAL_IMAGE_ID'] = local_id
    if recorded not in ('', policy) or target not in ('', policy):
        raise RuntimeError('Unknown ThingsVis auth policy; preserve the existing deployment')
    if recorded and target != policy:
        raise RuntimeError('A secured ThingsVis deployment cannot silently downgrade its auth policy')
    if target == policy:
        if not selected or selected.split('@', 1)[0].endswith(':v1.0.23'):
            raise RuntimeError('Select the reviewed secure ThingsVis image before upgrading the auth policy')
        # Prior accept-all SSO could mint signed tokens even with a strong
        # custom key. No old key is safe to carry across this policy boundary.
        if not recorded:
            if 'TP_THINGSVIS_AUTH_SECRET' in os.environ:
                raise RuntimeError('An exported auth secret would override the rotated .env identity; remove that override before the policy upgrade')
            values = {'TP_THINGSVIS_AUTH_SECRET': secrets.token_hex(32),
                      'TP_THINGSVIS_AUTH_POLICY': policy, 'TP_THINGSVIS_AUTH_POLICY_TARGET': policy}
            values.update(selection)
            return values
        if not current.get('TP_THINGSVIS_AUTH_SECRET'):
            raise RuntimeError('The secured ThingsVis auth identity is missing; preserve the deployment')
    values = {'TP_THINGSVIS_AUTH_SECRET': deployment_secret(current, 'TP_THINGSVIS_AUTH_SECRET')}
    values.update(selection)
    return values


def configure():
    if (ROOT / '.thingspanel-update/journal.json').exists():
        transaction = update_files().Transaction(ROOT)
        data = transaction.load()
        active = data and data['phase'] == 'activation_started' and os.environ.get('TP_INSTALLER_ACTIVE_OPERATION') == data['operation']
        if not active and (data is None or data['phase'] not in ('complete', 'recovered')):
            raise RuntimeError('Native preparation requires the active installer operation; preserve unresolved installation state')
    CONFIG.mkdir(parents=True, exist_ok=True)
    envfile = ROOT / '.env'
    text = envfile.read_text(encoding='utf-8') if envfile.exists() else ''
    current = dict(line.split('=', 1) for line in text.splitlines() if '=' in line and not line.startswith('#'))
    auth_keys = ('TP_THINGSVIS_AUTH_SECRET', 'TP_THINGSVIS_AUTH_POLICY', 'TP_THINGSVIS_AUTH_POLICY_TARGET', 'TP_THINGSVIS_IMAGE', 'TP_THINGSVIS_LOCAL_IMAGE_ID')
    for key in auth_keys:
        matches = [line.split('=', 1)[1] for line in text.splitlines()
                   if '=' in line and line.split('=', 1)[0].strip() == key]
        if len(matches) > 1:
            raise RuntimeError('Ambiguous ThingsVis auth configuration; existing configuration was not changed')
        if matches:
            current[key] = matches[0]
    auth = thingsvis_auth_settings(current)
    integration_keys = ('TP_MARKETINSTALL_PROVIDER_TOKEN', 'TP_MARKETINSTALL_THINGSVIS_IDENTITY_TOKEN',
                        'TP_MARKETINSTALL_THINGSVIS_TOKEN')
    integration = {key: deployment_secret(current, key) for key in integration_keys}
    if len({secret_value(value, key, True) for key, value in integration.items()}) != len(integration):
        raise RuntimeError('Market installation service credentials must be distinct; existing configuration was not changed')
    defaults = {'TP_DATABASE_PASSWORD': 'postgresThingsPanel', 'TP_NATIVE_DB_PASSWORD': current.get('TP_DATABASE_PASSWORD', 'postgresThingsPanel'),
                'TP_BROKER_DATABASE_DSN': 'host=postgres user=postgres password=' + current.get('TP_DATABASE_PASSWORD', 'postgresThingsPanel') + ' dbname=ThingsPanel port=5432 sslmode=disable',
                'TP_JWT_KEY': secrets.token_hex(32), 'TP_AUTOMATION_TOKEN': secrets.token_hex(32),
                'TP_MQTT_USER': 'tp-community-platform', 'TP_MQTT_PASSWORD': secrets.token_hex(32),
                'TP_PROTOCOL_MQTT_USER': 'tp-community-protocols', 'TP_PROTOCOL_MQTT_PASSWORD': secrets.token_hex(32),
                'TP_DEPLOYMENT_ID': 'community', 'TP_BROKER_CELL': CELL, 'TP_BROKER_INSTANCE': INSTANCE, 'TP_MQTT_CLIENT_ID': CLIENT,
                'TP_NOTIFICATION_KEYRING_B64': base64.b64encode(json.dumps({'main': base64.b64encode(secrets.token_bytes(32)).decode()}).encode()).decode()}
    defaults.update(auth)
    defaults.update(integration)
    lines = text.splitlines()
    for key, value in defaults.items():
        if key not in current:
            lines.append(key + '=' + value)
            current[key] = value
        elif key in auth and current[key] != value:
            lines = [key + '=' + value if '=' in line and line.split('=', 1)[0].strip() == key else line for line in lines]
            current[key] = value
    updated = ('\n'.join(lines) + '\n').encode('utf-8')
    files = update_files()
    tokenfile = files.safe_target(CONFIG, 'broker/plugin-token')
    legacy_token = files.safe_target(CONFIG, 'plugin-token')
    token_source = tokenfile if files.file_info(tokenfile) is not None else legacy_token
    token = token_source.read_text(encoding='ascii').strip() if files.file_info(token_source) is not None else secrets.token_hex(32)
    if len(token) < 32 or any(ch.isspace() for ch in token):
        raise RuntimeError('Existing broker identity is invalid; preserve it for review')
    services_file = files.safe_target(CONFIG, 'broker/services.json')
    legacy_services = files.safe_target(CONFIG, 'services.json')
    services_source = services_file if files.file_info(services_file) is not None else legacy_services
    initial_services = json.loads(services_source.read_text(encoding='utf-8')) if files.file_info(services_source) is not None else [service(current, [])]
    if not isinstance(initial_services, list):
        raise RuntimeError('Existing broker services configuration is invalid; preserve it for review')
    owners_file = files.safe_target(CONFIG, 'broker/owner-tokens.json')
    legacy_owners = files.safe_target(CONFIG, 'owner-tokens.json')
    owners_source = owners_file if files.file_info(owners_file) is not None else legacy_owners
    owners = json.loads(owners_source.read_text(encoding='utf-8')) if files.file_info(owners_source) is not None else [
        {'token': token, 'broker_cell': current['TP_BROKER_CELL'], 'broker_instance': current['TP_BROKER_INSTANCE']}]
    matching = [item for item in owners if isinstance(item, dict) and item.get('token') == token] if isinstance(owners, list) else []
    if len(matching) != 1 or matching[0].get('broker_cell') != current['TP_BROKER_CELL'] or matching[0].get('broker_instance') != current['TP_BROKER_INSTANCE']:
        raise RuntimeError('Existing broker owner identity conflicts with configuration; preserve it for review')
    if updated != text.encode('utf-8'):
        write_configuration_file(envfile, updated, 0o600)
    else:
        envfile.chmod(0o600)
    token_owner = {'uid': 1883, 'gid': 1883} if os.name == 'posix' and os.geteuid() == 0 else files.file_info(tokenfile)
    write_configuration_file(tokenfile, token.encode('ascii'), 0o440, token_owner)
    save('owner-tokens.json', owners, True)
    # An empty initial scope only subscribes; it cannot issue unscoped commands.
    if files.file_info(services_file) is None:
        save('services.json', initial_services, True)
    certificate = CONFIG / 'internal.crt'
    if not certificate.exists():
        command('openssl', 'req', '-x509', '-nodes', '-newkey', 'rsa:2048', '-days', '3650',
                '-subj', '/CN=frontend', '-addext', 'subjectAltName=DNS:frontend',
                '-keyout', str(CONFIG / 'internal.key'), '-out', str(certificate))
        (CONFIG / 'internal.key').chmod(0o600)
    system_ca = Path('/etc/ssl/certs/ca-certificates.crt')
    bundle = (system_ca.read_bytes() if system_ca.exists() else b'') + b'\n' + certificate.read_bytes()
    (CONFIG / 'internal-ca.pem').write_bytes(bundle)
    return current


def service(env, targets):
    publish = ['devices/command/+/+', 'gateway/command/+/+', 'devices/telemetry/control/+', 'gateway/telemetry/control/+',
               'devices/attributes/get/+', 'gateway/attributes/get/+', 'devices/attributes/set/+/+', 'gateway/attributes/set/+/+',
               'devices/attributes/response/+/+', 'gateway/attributes/response/+/+', 'devices/event/response/+/+', 'gateway/event/response/+/+'] if targets else []
    return {'username': env['TP_MQTT_USER'], 'password': env['TP_MQTT_PASSWORD'], 'broker_cell': env['TP_BROKER_CELL'], 'broker_instance': env['TP_BROKER_INSTANCE'],
            'allowed_client_ids': [env['TP_MQTT_CLIENT_ID'], 'tp-community-readiness'], 'allowed_device_ids': [item['id'] for item in targets],
            'allowed_tenant_ids': sorted({item['tenant_id'] for item in targets}),
            'principal': {'tenant_id': 'platform', 'device_id': 'community-backend', 'principal_kind': 'service'},
            'policy': {'publish': publish, 'subscribe': ['tp-internal/ingress/v1/#', 'tp-internal/state/v1/#'], 'max_payload': 1048576, 'max_qos': 1, 'retain_profile': 'live-ingress'},
            'policy_version': 'installer-v1', 'mapping_version': 'standard-v1'}


def scope_services(env, targets, plugins):
    """Broker service identities for the given device scopes (pure; no I/O).

    The platform account issues commands to MQTT devices *and* to plugin (service
    access) devices: a command for a plugin device is published by the Broker owner
    on the plugin's prefixed topic, and the owner silently drops it when the device
    is not in the platform account's scope (T7b finding 3). The protocol account
    only ever covers plugin devices.
    """
    platform_scope, seen = [], set()
    for item in list(targets) + list(plugins):
        if item['id'] not in seen:
            seen.add(item['id'])
            platform_scope.append(item)
    plugin = service(env, [])
    plugin.update(username=env['TP_PROTOCOL_MQTT_USER'], password=env['TP_PROTOCOL_MQTT_PASSWORD'],
                  allowed_client_ids=[], allow_any_client_id=True, plugin_socket=True,
                  allowed_device_ids=[item['id'] for item in plugins],
                  allowed_tenant_ids=sorted({item['tenant_id'] for item in plugins}))
    plugin['principal']['device_id'] = 'community-protocols'
    plugin['policy']['subscribe'] = []
    return [service(env, platform_scope), plugin]


def query_scopes():
    targets = json.loads(sql('ThingsPanel', "SELECT coalesce(json_agg(t),'[]') FROM (SELECT d.id,d.tenant_id FROM devices d LEFT JOIN device_configs c ON c.id=d.device_config_id AND c.tenant_id=d.tenant_id WHERE d.is_enabled='enabled' AND d.activate_flag='active' AND (d.parent_id IS NULL OR d.parent_id='') AND (d.service_access_id IS NULL OR d.service_access_id='') AND coalesce(d.access_way,'A')<>'B' AND coalesce(c.protocol_type,'MQTT') IN ('MQTT','') ORDER BY d.id)t;"))
    if len(targets) > 256: raise RuntimeError('Broker service target scope exceeds 256; provision separate reviewed service identities')
    plugins = json.loads(sql('ThingsPanel', "SELECT coalesce(json_agg(t),'[]') FROM (SELECT d.id,d.tenant_id FROM devices d LEFT JOIN device_configs c ON c.id=d.device_config_id AND c.tenant_id=d.tenant_id WHERE d.is_enabled='enabled' AND d.activate_flag='active' AND ((d.service_access_id IS NOT NULL AND d.service_access_id<>'') OR (c.protocol_type IS NOT NULL AND c.protocol_type NOT IN ('MQTT',''))) ORDER BY d.id)t;"))
    if len(plugins) > 256: raise RuntimeError('Protocol plugin scope exceeds 256 devices')
    return targets, plugins


def refresh(env):
    targets, plugins = query_scopes()
    save('services.json', scope_services(env, targets, plugins), True)
    print('MQTT command allowlist refreshed for ' + str(len(targets)) + ' devices and ' + str(len(plugins)) + ' plugin devices; restart broker-owner and mosquitto to load it.')


def current_services():
    files = update_files()
    path = files.safe_target(CONFIG, 'broker/services.json')
    if files.file_info(path) is None:
        path = files.safe_target(CONFIG, 'services.json')
    return json.loads(path.read_text(encoding='utf-8')) if files.file_info(path) is not None else None


def sync_scope(env, restart=None):
    """Refresh the Broker device scope; only when it changed, restart broker-owner and then mosquitto.

    mosquitto shares broker-owner's network namespace, so the order is fixed:
    owner first (it reads services.json at start), mosquitto second. Returns True when something changed.
    """
    restart = restart or (lambda service_name: command('docker', 'compose', 'restart', service_name))
    files = update_files()
    state = ROOT / '.thingspanel-update'
    files.private_directory(state)
    # Serialize the full compare/write/restart/ack sequence with installer
    # activation and other scope workers. The watcher retries a busy lock.
    with files.update_lock(state):
        targets, plugins = query_scopes()
        wanted = scope_services(env, targets, plugins)
        current = current_services()
        changed = current != wanted
        fingerprint = hashlib.sha256(json.dumps(wanted, sort_keys=True, separators=(',', ':')).encode()).hexdigest()
        pending = read_scope_restart_pending(files)
        if not changed and pending is None:
            print('Broker device scope unchanged (' + str(len(targets)) + ' devices, ' + str(len(plugins)) + ' plugin devices).')
            return False

        if changed or pending != fingerprint:
            # Record intent before changing services.json. A process crash or
            # partial Docker restart leaves this marker for the next watcher pass.
            write_scope_restart_pending(files, fingerprint)
        if changed:
            save('services.json', wanted, True, lock_held=True)
            print('Broker device scope changed (' + str(len(targets)) + ' devices, ' + str(len(plugins)) + ' plugin devices); restarting broker-owner, then mosquitto.', flush=True)
        else:
            print('Retrying pending Broker scope restarts.', flush=True)
        for name in ('broker-owner', 'mosquitto'):
            restart(name)
        clear_scope_restart_pending(files)
        return changed


def read_scope_restart_pending(files=None):
    files = files or update_files()
    path = files.safe_target(ROOT, 'config/native/broker/.scope-sync-restart.json')
    if files.file_info(path) is None:
        return None
    value = json.loads(path.read_text(encoding='utf-8'))
    fingerprint = value.get('sha256') if isinstance(value, dict) else None
    if not isinstance(fingerprint, str) or re.fullmatch(r'[0-9a-f]{64}', fingerprint) is None:
        raise RuntimeError('Invalid pending Broker scope restart record')
    return fingerprint


def write_scope_restart_pending(files, fingerprint):
    path = files.safe_target(ROOT, 'config/native/broker/.scope-sync-restart.json')
    files.durable_bytes(path,
                        (json.dumps({'sha256': fingerprint}, sort_keys=True) + '\n').encode(), 0o600)


def clear_scope_restart_pending(files):
    path = files.safe_target(ROOT, 'config/native/broker/.scope-sync-restart.json')
    if files.file_info(path) is None:
        return
    path.unlink()
    files.fsync_directory(path.parent)


def bootstrap():
    """Apply SQL shipped in the selected backend image; never reinitialize data."""
    command('docker', 'compose', 'up', '-d', '--wait', '--wait-timeout', '120', 'postgres', 'redis')
    with tempfile.TemporaryDirectory(prefix='tp-installer-') as temp:
        stage = Path(temp)
        image = command('docker', 'compose', 'config', '--format', 'json')
        image = json.loads(image)['services']['backend']['image']
        container = command('docker', 'create', '--entrypoint', '/bin/true', image)
        try:
            has_marker = image_has_marker(container)
            for source, target in [('sql', 'community'), ('encore-api/automation/migrations', 'automation'),
                                   ('encore-api/devicemetadata/migrations', 'device_metadata'),
                                   ('encore-api/l3-core/notification/migrations', 'notification'), ('pkg/global/global.go', 'global.go')]:
                command('docker', 'cp', container + ':/go/src/app/' + source, str(stage / target))
        finally:
            command('docker', 'rm', container)
        target_version = int(re.search(r'VERSION_NUMBER\s*=\s*(\d+)', (stage / 'global.go').read_text()).group(1))
        # CasbinInit runs before CheckVersion in the backend and creates this adapter table.
        sql('ThingsPanel', 'CREATE TABLE IF NOT EXISTS casbin_rule(id bigserial PRIMARY KEY,ptype varchar(100),v0 varchar(100),v1 varchar(100),v2 varchar(100),v3 varchar(100),v4 varchar(100),v5 varchar(100));')
        # Match the application's own sys_version protocol; 1.sql only runs on a new database.
        sql('ThingsPanel', "CREATE TABLE IF NOT EXISTS sys_version(version_number INT NOT NULL DEFAULT 0 PRIMARY KEY,version varchar(255) NOT NULL); INSERT INTO sys_version SELECT 0,'0.0.0' WHERE NOT EXISTS(SELECT 1 FROM sys_version);")
        current = int(sql('ThingsPanel', 'SELECT version_number FROM sys_version'))
        if current > target_version: raise RuntimeError('Installed database is newer than selected backend image')
        for number in range(current + 1, target_version + 1):
            content = (stage / 'community' / (str(number) + '.sql')).read_text()
            sql('ThingsPanel', "BEGIN; SET LOCAL lock_timeout='30s';\n" + content + "\nUPDATE sys_version SET version_number=" + str(number) + ",version='v1.3.0'; COMMIT;")
        # Schema files newer than sys_version's VERSION_NUMBER that the backend does not apply by itself at startup.
        # Idempotent (CREATE ... IF NOT EXISTS) and checksum-recorded. Old image: skip + log. New image missing it: fail.
        apply_migrations('ThingsPanel', EXTRA_COMMUNITY_COMPONENT, required_extra_sql(stage / 'community', has_marker, image))
        apply_migrations('ThingsPanel', 'broker', sorted((stage / 'community/broker').glob('[0-9]*.sql'), key=lambda p: int(p.name.split('_')[0])))
        for database in ['device_metadata', 'automation', 'notification']:
            if sql('postgres', "SELECT count(*) FROM pg_database WHERE datname='" + database + "'") == '0':
                sql('postgres', 'CREATE DATABASE "' + database + '"')
            sql(database, 'CREATE EXTENSION IF NOT EXISTS pgcrypto')
            paths = sorted((stage / database).glob('*.up.sql'), key=lambda p: int(p.name.split('_')[0]))
            if sql(database, "SELECT to_regclass('public.schema_migrations') IS NOT NULL") == 't':
                if sql(database, 'SELECT count(*) FROM schema_migrations WHERE dirty=true') != '0':
                    raise RuntimeError('Native database has an unfinished migration: ' + database)
                installed = int(sql(database, 'SELECT coalesce(max(version),0) FROM schema_migrations'))
                if installed > max(int(p.name.split('_')[0]) for p in paths):
                    raise RuntimeError('Native database is newer than selected image: ' + database)
            apply_migrations(database, database, paths)
            sql(database, 'CREATE TABLE IF NOT EXISTS schema_migrations(version bigint PRIMARY KEY,dirty boolean NOT NULL); DELETE FROM schema_migrations; INSERT INTO schema_migrations VALUES(' + str(max(int(p.name.split('_')[0]) for p in paths)) + ',false);')
    print('Community, Broker and native database migrations are ready.')


def pull_images(configured=None):
    config = json.loads(command('docker', 'compose', 'config', '--format', 'json'))
    configured = configured or {}
    local_id = configured.get('TP_THINGSVIS_LOCAL_IMAGE_ID')
    local_image = None
    if local_id:
        local_image = config['services'].get('thingsvis-server', {}).get('image')
        if local_image != configured.get('TP_THINGSVIS_IMAGE') or re.fullmatch(r'sha256:[0-9a-f]{64}', local_id) is None:
            raise RuntimeError('Local ThingsVis image selection does not match the reviewed configuration')
        if command('docker', 'image', 'inspect', '--format', '{{.Id}}', local_image) != local_id:
            raise RuntimeError('Local ThingsVis image identity differs; no registry fallback was attempted')
    # Reviewed local images for the backend and the TDengine profile follow the
    # same rule: an exact image ID is pinned, and a mismatch never falls back to a registry.
    pinned = {}
    for image_key, id_key in (('TP_BACKEND_IMAGE', 'TP_BACKEND_LOCAL_IMAGE_ID'), ('TP_TDENGINE_IMAGE', 'TP_TDENGINE_LOCAL_IMAGE_ID')):
        identity = configured.get(id_key)
        if not identity:
            continue
        reference = configured.get(image_key)
        if not reference or re.fullmatch(r'sha256:[0-9a-f]{64}', identity) is None:
            raise RuntimeError('Local image selection does not match the reviewed configuration: ' + id_key)
        if command('docker', 'image', 'inspect', '--format', '{{.Id}}', reference) != identity:
            raise RuntimeError('Local image identity differs; no registry fallback was attempted')
        pinned[reference] = identity
    images = dict.fromkeys(service['image'] for service in config['services'].values())
    for image in images:
        if image == local_image:
            print('Using the explicitly selected local ThingsVis image identity.', flush=True)
            continue
        if image in pinned:
            print('Using the pinned local image ' + image, flush=True)
            continue
        print('Pulling ' + image, flush=True)
        for attempt in range(3):
            result = subprocess.run(['docker', 'pull', image], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
            if result.returncode == 0: break
            if attempt == 2: raise RuntimeError('Image pull failed after 3 attempts: ' + image)
            print('Registry connection failed; retrying the same image (' + str(attempt + 1) + '/3)', flush=True)
            time.sleep(3)


def apply_migrations(database, component, paths):
    sql(database, 'CREATE TABLE IF NOT EXISTS installer_migrations(component text NOT NULL,version int NOT NULL,checksum text NOT NULL,PRIMARY KEY(component,version))')
    for path in paths:
        number = int(re.match(r'\d+', path.name).group())
        content = path.read_text().replace('\r\n', '\n')
        checksum = hashlib.sha256(content.encode()).hexdigest()
        recorded = sql(database, "SELECT checksum FROM installer_migrations WHERE component='" + component + "' AND version=" + str(number))
        if recorded:
            if recorded != checksum: raise RuntimeError('Migration checksum mismatch: ' + component + '/' + path.name)
            continue
        # A pre-existing native database without this installer's ledger needs its native version respected.
        if component not in ('broker', EXTRA_COMMUNITY_COMPONENT) and sql(database, "SELECT to_regclass('public.schema_migrations') IS NOT NULL") == 't':
            native_version = sql(database, 'SELECT coalesce(max(version),0) FROM schema_migrations WHERE dirty=false')
            if int(native_version) >= number:
                sql(database, "INSERT INTO installer_migrations VALUES('" + component + "'," + str(number) + ",'" + checksum + "')")
                continue
        sql(database, "BEGIN; SET LOCAL lock_timeout='30s';\n" + content + "\nINSERT INTO installer_migrations VALUES('" + component + "'," + str(number) + ",'" + checksum + "'); COMMIT;")


def verify():
    endpoints = [('/api/v1/sys_version', 200), ('/automation-core/v1/health', 200), ('/api/v2/notification-groups', 401)]
    last_error = ''
    for attempt in range(60):
        try:
            for path, expected in endpoints:
                try:
                    with urllib.request.urlopen('http://127.0.0.1:' + os.getenv('HTTP_PORT', '8080') + path, timeout=4) as response:
                        status = response.status
                        if status != expected: raise RuntimeError(path + ': HTTP ' + str(status))
                        json.load(response)
                except urllib.error.HTTPError as error:
                    status = error.code
                    if status != expected: raise RuntimeError(path + ': HTTP ' + str(status))
                    json.loads(error.read())
                if status != expected: raise RuntimeError('Endpoint not ready')
            rows = [json.loads(line) for line in command('docker', 'compose', 'ps', '--all', '--format', 'json').splitlines()]
            if len(rows) == 1 and isinstance(rows[0], list): rows = rows[0]
            if any(row.get('State') != 'running' for row in rows): raise RuntimeError('Container not running')
            for port in (19091, 503):
                with socket.create_connection(('127.0.0.1', port), timeout=2): pass
            command('docker', 'compose', 'exec', '-T', 'automation-scheduler', 'test', '-f', '/tmp/last-tick-ok')
            print('Web API, native API, notification auth and scheduler are ready.')
            return
        except (RuntimeError, ValueError, OSError) as error:
            last_error = str(error)
            time.sleep(2)
    raise RuntimeError('Installation verification failed: ' + last_error + '; inspect docker compose logs')


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--refresh-devices', action='store_true')
    parser.add_argument('--sync-scope', action='store_true', help='refresh the Broker device scope and restart broker-owner, mosquitto only if it changed')
    parser.add_argument('--bootstrap', action='store_true')
    parser.add_argument('--verify', action='store_true')
    parser.add_argument('--pull', action='store_true')
    args = parser.parse_args()
    # A fresh install has no generated secrets yet, so required Compose
    # interpolation cannot run until configure() has created the .env file.
    check_thingsvis_database(uninterpolated=True)
    env = configure()
    check_thingsvis_database()
    if args.pull: pull_images(env)
    if args.bootstrap: bootstrap()
    if args.refresh_devices: refresh(env)
    if args.sync_scope: sync_scope(env)
    if args.verify: verify()
    print('Native service configuration prepared. Credentials remain in local .env and config/native/.')
