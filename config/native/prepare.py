"""Prepare this installer's native service identities; never copy test-host secrets."""
import argparse
import base64
import json
import os
from pathlib import Path
import secrets
import subprocess
import hashlib
import re
import tempfile
import time
import urllib.request
import urllib.error

ROOT = Path(__file__).resolve().parents[2]
CONFIG = ROOT / 'config/native'
CELL = 'community'
INSTANCE = 'community-mosquitto'
CLIENT = 'tp-community-backend'


def command(*args, data=None):
    result = subprocess.run(args, input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=ROOT)
    if result.returncode:
        errors = result.stderr.decode(errors='replace').splitlines()
        detail = next((line for line in errors if 'ERROR:' in line or 'FATAL:' in line), '')
        raise RuntimeError('Native preparation command failed: ' + ' '.join(args[:4]) + ('; ' + detail if detail else ''))
    return result.stdout.decode().strip()


def sql(database, statement):
    return command('docker', 'compose', 'exec', '-T', 'postgres', 'psql', '-X', '-qAt',
                   '-U', 'postgres', '-d', database, '-v', 'ON_ERROR_STOP=1', data=statement.encode())


def save(name, value, broker=False):
    path = CONFIG / name
    path.write_text(json.dumps(value, indent=2) + '\n', encoding='utf-8')
    path.chmod(0o440 if broker else 0o600)
    if broker and os.name == 'posix' and os.geteuid() == 0:
        os.chown(path, 1883, 1883)


def configure():
    CONFIG.mkdir(parents=True, exist_ok=True)
    envfile = ROOT / '.env'
    text = envfile.read_text(encoding='utf-8') if envfile.exists() else ''
    current = dict(line.split('=', 1) for line in text.splitlines() if '=' in line and not line.startswith('#'))
    defaults = {'TP_DATABASE_PASSWORD': 'postgresThingsPanel', 'TP_NATIVE_DB_PASSWORD': current.get('TP_DATABASE_PASSWORD', 'postgresThingsPanel'),
                'TP_BROKER_DATABASE_DSN': 'host=postgres user=postgres password=' + current.get('TP_DATABASE_PASSWORD', 'postgresThingsPanel') + ' dbname=ThingsPanel port=5432 sslmode=disable',
                'TP_JWT_KEY': secrets.token_hex(32), 'TP_AUTOMATION_TOKEN': secrets.token_hex(32),
                'TP_MQTT_USER': 'tp-community-platform', 'TP_MQTT_PASSWORD': secrets.token_hex(32),
                'TP_DEPLOYMENT_ID': 'community', 'TP_BROKER_CELL': CELL, 'TP_BROKER_INSTANCE': INSTANCE, 'TP_MQTT_CLIENT_ID': CLIENT,
                'TP_NOTIFICATION_KEYRING_B64': base64.b64encode(json.dumps({'main': base64.b64encode(secrets.token_bytes(32)).decode()}).encode()).decode()}
    with envfile.open('a', encoding='utf-8') as output:
        if text and not text.endswith('\n'): output.write('\n')
        for key, value in defaults.items():
            if key not in current:
                output.write(key + '=' + value + '\n')
                current[key] = value
    envfile.chmod(0o600)
    tokenfile = CONFIG / 'plugin-token'
    if not tokenfile.exists():
        tokenfile.write_text(secrets.token_hex(32), encoding='ascii')
    tokenfile.chmod(0o440)
    if os.name == 'posix' and os.geteuid() == 0: os.chown(tokenfile, 1883, 1883)
    save('owner-tokens.json', [{'token': tokenfile.read_text().strip(), 'broker_cell': current['TP_BROKER_CELL'], 'broker_instance': current['TP_BROKER_INSTANCE']}], True)
    # An empty initial scope only subscribes; it cannot issue unscoped commands.
    if not (CONFIG / 'services.json').exists():
        save('services.json', [service(current, [])], True)
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


def refresh(env):
    targets = json.loads(sql('ThingsPanel', "SELECT coalesce(json_agg(t),'[]') FROM (SELECT d.id,d.tenant_id FROM devices d LEFT JOIN device_configs c ON c.id=d.device_config_id AND c.tenant_id=d.tenant_id WHERE d.is_enabled='enabled' AND d.activate_flag='active' AND (d.parent_id IS NULL OR d.parent_id='') AND (d.service_access_id IS NULL OR d.service_access_id='') AND coalesce(d.access_way,'A')<>'B' AND coalesce(c.protocol_type,'MQTT') IN ('MQTT','') ORDER BY d.id)t;"))
    if len(targets) > 256: raise RuntimeError('Broker service target scope exceeds 256; provision separate reviewed service identities')
    save('services.json', [service(env, targets)], True)
    print('MQTT command allowlist refreshed for ' + str(len(targets)) + ' devices; restart broker-owner and mosquitto to load it.')


def bootstrap():
    """Apply SQL shipped in the selected backend image; never reinitialize data."""
    command('docker', 'compose', 'up', '-d', '--wait', '--wait-timeout', '120', 'postgres', 'redis')
    with tempfile.TemporaryDirectory(prefix='tp-installer-') as temp:
        stage = Path(temp)
        image = command('docker', 'compose', 'config', '--format', 'json')
        image = json.loads(image)['services']['backend']['image']
        container = command('docker', 'create', '--entrypoint', '/bin/true', image)
        try:
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


def apply_migrations(database, component, paths):
    sql(database, 'CREATE TABLE IF NOT EXISTS installer_migrations(component text NOT NULL,version int NOT NULL,checksum text NOT NULL,PRIMARY KEY(component,version))')
    for path in paths:
        number = int(path.name.split('_')[0])
        content = path.read_text().replace('\r\n', '\n')
        checksum = hashlib.sha256(content.encode()).hexdigest()
        recorded = sql(database, "SELECT checksum FROM installer_migrations WHERE component='" + component + "' AND version=" + str(number))
        if recorded:
            if recorded != checksum: raise RuntimeError('Migration checksum mismatch: ' + component + '/' + path.name)
            continue
        # A pre-existing native database without this installer's ledger needs its native version respected.
        if component != 'broker' and sql(database, "SELECT to_regclass('public.schema_migrations') IS NOT NULL") == 't':
            native_version = sql(database, 'SELECT coalesce(max(version),0) FROM schema_migrations WHERE dirty=false')
            if int(native_version) >= number:
                sql(database, "INSERT INTO installer_migrations VALUES('" + component + "'," + str(number) + ",'" + checksum + "')")
                continue
        sql(database, "BEGIN; SET LOCAL lock_timeout='30s';\n" + content + "\nINSERT INTO installer_migrations VALUES('" + component + "'," + str(number) + ",'" + checksum + "'); COMMIT;")


def verify():
    endpoints = [('/api/v1/sys_version', 200), ('/automation-core/v1/health', 200), ('/api/v2/notification-groups', 401)]
    for attempt in range(60):
        try:
            for path, expected in endpoints:
                try:
                    with urllib.request.urlopen('http://127.0.0.1:' + os.getenv('HTTP_PORT', '8080') + path, timeout=4) as response:
                        status = response.status
                        json.load(response)
                except urllib.error.HTTPError as error:
                    status = error.code
                    json.loads(error.read())
                if status != expected: raise RuntimeError('Endpoint not ready')
            rows = [json.loads(line) for line in command('docker', 'compose', 'ps', '--all', '--format', 'json').splitlines()]
            if len(rows) == 1 and isinstance(rows[0], list): rows = rows[0]
            if any(row.get('State') != 'running' for row in rows): raise RuntimeError('Container not running')
            command('docker', 'compose', 'exec', '-T', 'automation-scheduler', 'test', '-f', '/tmp/last-tick-ok')
            print('Web API, native API, notification auth and scheduler are ready.')
            return
        except (RuntimeError, ValueError, OSError):
            time.sleep(2)
    raise RuntimeError('Installation verification failed; inspect docker compose logs')


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--refresh-devices', action='store_true')
    parser.add_argument('--bootstrap', action='store_true')
    parser.add_argument('--verify', action='store_true')
    args = parser.parse_args()
    env = configure()
    if args.bootstrap: bootstrap()
    if args.refresh_devices: refresh(env)
    if args.verify: verify()
    print('Native service configuration prepared. Credentials remain in local .env and config/native/.')
