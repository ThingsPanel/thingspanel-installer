"""Prepare this installer's native service identities; never copy test-host secrets."""
import argparse
import base64
import json
import os
from pathlib import Path
import secrets
import subprocess

ROOT = Path(__file__).resolve().parents[2]
CONFIG = ROOT / 'config/native'
CELL = 'community'
INSTANCE = 'community-mosquitto'
CLIENT = 'tp-community-backend'


def command(*args, data=None):
    result = subprocess.run(args, input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=ROOT)
    if result.returncode:
        raise RuntimeError('Native preparation command failed: ' + ' '.join(args[:4]))
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


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--refresh-devices', action='store_true')
    args = parser.parse_args()
    env = configure()
    if args.refresh_devices: refresh(env)
    print('Native service configuration prepared. Credentials remain in local .env and config/native/.')
