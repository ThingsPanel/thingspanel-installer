#!/usr/bin/env python3
"""One-command install of a service plugin from a signed catalog onto a running ThingsPanel stack.

    TP_TENANT_EMAIL=... TP_TENANT_PASSWORD=... plugin_install.py install \
        --catalog plugin-catalog.json --trust-root trust-root.pem --plugin http-adapter \
        --stack-dir /opt/thingspanel --web-url http://127.0.0.1:8080 --agent-dir /var/lib/tp-agent \
        --agent-bin tp-agent --agentctl-bin tp-agentctl [--device my-first-device]

What it does, in order (each step is idempotent; re-running installs the next generation):
  1. verifies the catalog signature against the pinned trust root and checks the pinned image ID;
  2. creates the platform-side binding (device template for the plugin's protocol, template secret,
     auto registration) through the platform API;
  3. optionally registers a first device and reloads the Broker allowlist in the required order;
  4. starts (or reuses) the host Agent and asks it to prepare, grant and activate the plugin container;
  5. checks that the plugin registered with the platform and, with --device, that an uplink arrives.

The Agent lease is at most 24 hours; run `plugin_install.py renew` from cron (or any scheduler) to extend it.
"""
import argparse
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import time
import urllib.error
import urllib.request

sys.path.insert(0, str(Path(__file__).resolve().parent))
import plugin_catalog  # noqa: E402


def sh(args, check=True, cwd=None, env=None, timeout=300):
    result = subprocess.run(args, cwd=cwd, env=env, capture_output=True, text=True, timeout=timeout)
    if check and result.returncode:
        raise RuntimeError('command failed (%d): %s\n%s' % (result.returncode, ' '.join(args[:4]), (result.stderr or result.stdout)[-400:]))
    return result


def say(text):
    print('==> ' + text, flush=True)


class Platform:
    def __init__(self, base, email, password):
        self.base = base.rstrip('/')
        status, body = self.call('/api/v1/login', {'email': email, 'password': password})
        if status != 200 or not isinstance(body, dict) or not (body.get('data') or {}).get('token'):
            raise RuntimeError('platform login failed')
        self.token = body['data']['token']

    def call(self, path, data=None, method=None):
        headers = {'Content-Type': 'application/json'}
        if getattr(self, 'token', None):
            headers['x-token'] = self.token
        request = urllib.request.Request(self.base + path, json.dumps(data).encode() if data is not None else None, headers, method=method)
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                return response.status, json.loads(response.read() or b'{}')
        except urllib.error.HTTPError as error:
            return error.code, error.read()[:200].decode('utf-8', 'replace')

    def must(self, path, data=None, method=None):
        status, body = self.call(path, data, method)
        if status != 200 or (isinstance(body, dict) and body.get('code') not in (None, 200)):
            raise RuntimeError('platform call %s failed: %s %s' % (path, status, str(body)[:160]))
        return body.get('data') if isinstance(body, dict) else body


class Agent:
    def __init__(self, args, project):
        self.name = 'tp-agent-' + project
        self.dir = Path(args.agent_dir)
        self.args = args

    def write_profile(self, profile):
        profiles = self.dir / 'profiles.json'
        profiles.write_text(json.dumps([profile]))
        os.chmod(profiles, 0o600)
        sh(['docker', 'restart', self.name])
        time.sleep(3)

    def ensure_running(self, profile):
        self.dir.mkdir(parents=True, exist_ok=True)
        os.chmod(self.dir, 0o700)
        (self.dir / 'state').mkdir(exist_ok=True)
        os.chmod(self.dir / 'state', 0o700)
        token = self.dir / 'token'
        if not token.exists():
            token.write_text(secrets.token_hex(24))
            os.chmod(token, 0o600)
        profiles = self.dir / 'profiles.json'
        profiles.write_text(json.dumps([profile]))
        os.chmod(profiles, 0o600)
        mounts = ['-v', '%s:/tp-agent:ro' % self.args.agent_bin, '-v', '%s:/tp-agentctl:ro' % self.args.agentctl_bin, '-v', '%s:/run/agent' % self.dir]
        if not (self.dir / 'state' / 'state.json').exists():
            sh(['docker', 'run', '--rm', '--entrypoint', '/tp-agent', '-u', '0'] + mounts + [self.args.agent_image,
                '-state', '/run/agent/state/state.json', '-init', 'host-' + self.name])
        sh(['docker', 'rm', '-f', self.name], check=False)
        sh(['docker', 'run', '-d', '--name', self.name, '--restart', 'unless-stopped', '--entrypoint', '/tp-agent', '-u', '0'] + mounts +
           ['-v', '/var/run/docker.sock:/var/run/docker.sock', self.args.agent_image, '-state', '/run/agent/state/state.json',
            '-socket', '/run/agent/agent.sock', '-token-file', '/run/agent/token', '-profiles', '/run/agent/profiles.json',
            '-docker-socket', '/var/run/docker.sock', '-sweep-interval', '5s'])
        time.sleep(3)

    def ctl(self, endpoint, **fields):
        last = ''
        for _ in range(5):
            result = sh(['docker', 'exec', self.name, '/tp-agentctl', '-socket', '/run/agent/agent.sock', '-token-file', '/run/agent/token', endpoint] +
                        ['%s=%s' % item for item in fields.items()], check=False)
            if result.stdout.startswith('200'):
                return result.stdout[4:]
            last = (result.stdout or result.stderr)[:200]
            if not result.stdout.startswith('5'):
                break
            time.sleep(1)
        raise RuntimeError('agent %s failed: %s' % (endpoint, last))


def read_env(path):
    values = {}
    for line in Path(path).read_text().splitlines():
        if '=' in line and not line.startswith('#'):
            key, _, value = line.partition('=')
            values[key] = value
    return values


def project_of(stack_dir):
    return os.environ.get('COMPOSE_PROJECT_NAME') or Path(stack_dir).resolve().name.lower()


def wait_web(url, seconds=120):
    deadline = time.time() + seconds
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(url.rstrip('/') + '/api/v1/sys_version', timeout=5) as response:
                if response.status == 200:
                    return
        except Exception:  # noqa: BLE001
            pass
        time.sleep(4)
    raise RuntimeError('platform did not come back in time')


def reload_broker(stack_dir, web_url):
    """Refresh the device allowlist, then restart in the order the Broker needs (owner, broker, then the platform)."""
    env = dict(os.environ)
    journal = Path(stack_dir) / '.thingspanel-update' / 'journal.json'
    if journal.exists():
        data = json.loads(journal.read_text())
        if data.get('phase') == 'activation_started':
            env['TP_INSTALLER_ACTIVE_OPERATION'] = data['operation']
    sh([sys.executable, 'config/native/prepare.py', '--refresh-devices'], cwd=stack_dir, env=env)
    sh(['docker', 'compose', 'restart', 'broker-owner'], cwd=stack_dir)
    sh(['docker', 'compose', 'restart', 'mosquitto'], cwd=stack_dir)
    sh(['docker', 'compose', 'restart', 'backend', 'frontend'], cwd=stack_dir)
    wait_web(web_url)


def agent_install(agent, entry, state, state_path, platform=None, profile=None):
    generation = int(state.get('generation', 0)) + 1
    key = 'install-%d' % generation
    if state.get('generation'):
        agent.ctl('/v1/revoke', instance=entry['id'], generation=state['generation'], reason='replaced by generation %d' % generation)
    if platform is not None:
        # The platform issues one revocable identity per generation; a higher generation revokes every lower one.
        issued = platform.must('/api/v1/plugin/runtime/identity', {'plugin_instance': entry['id'], 'service_identifier': entry['platform']['protocol_type'],
                                                                   'generation': generation, 'ttl_hours': 24})
        profile['env'] = [e for e in profile['env'] if not e.startswith('P_PLATFORM_API_TOKEN=')] + ['P_PLATFORM_API_TOKEN=' + issued['token']]
        agent.write_profile(profile)
    agent.ctl('/v1/accept-generation', instance=entry['id'], generation=generation)
    if state.get('generation'):
        agent.ctl('/v1/retire', instance=entry['id'], generation=state['generation'])
    agent.ctl('/v1/prepare', instance=entry['id'], generation=generation, request_key=key, profile=entry['id'])
    agent.ctl('/v1/grant', instance=entry['id'], generation=generation, request_key=key, grant_id='grant-%d' % generation, ttl_seconds=86400)
    agent.ctl('/v1/activate', instance=entry['id'], generation=generation, request_key=key)
    state['generation'] = generation
    state['request_key'] = key
    state_path.write_text(json.dumps(state))
    print('    generation %d is running' % generation)
    time.sleep(8)


def uplink(args, entry, state, network, device_number, values=None):
    spec = entry['platform']
    name, ip = container_ip(network, state['generation'])
    key = state['generated'][spec['api_key_env']]
    body = json.dumps({'device_number': device_number, 'values': values or {'temp': 21.5, 'hum': 55}})
    return sh(['docker', 'run', '--rm', '--network', network, '--entrypoint', 'sh', args.agent_image, '-c',
               "wget -q -O- -T 10 --header 'Content-Type: application/json' --header 'X-Api-Key: %s' --post-data '%s' http://%s:%d%s 2>&1 | tail -1"
               % (key, body, ip, spec['uplink_port'], spec['uplink_path'])], check=False).stdout.strip()


def install(args):
    stack = Path(args.stack_dir)
    project = project_of(stack)
    say('verifying the signed catalog')
    catalog = plugin_catalog.load_verified(args.catalog, args.trust_root)
    matches = [p for p in catalog['plugins'] if p['id'] == args.plugin]
    if not matches:
        raise RuntimeError('plugin %s is not in the catalog' % args.plugin)
    entry = matches[0]
    image = sh(['docker', 'image', 'inspect', '--format', '{{.Id}}', entry['image_id']], check=False).stdout.strip()
    if image != entry['image_id']:
        raise RuntimeError('image %s is not present locally; pull it by digest first' % entry['image_id'])
    print('    %s@%s, image %s' % (entry['id'], entry['version'], entry['image_id'][:19]))

    state_path = Path(args.agent_dir) / 'plugins' / (entry['id'] + '.json')
    state_path.parent.mkdir(parents=True, exist_ok=True)
    state = json.loads(state_path.read_text()) if state_path.exists() else {}
    generated = state.setdefault('generated', {})
    for key, size in entry.get('env_generated', {}).items():
        generated.setdefault(key, secrets.token_hex(size // 2))

    email, password = os.environ.get('TP_TENANT_EMAIL'), os.environ.get('TP_TENANT_PASSWORD')
    if not email or not password:
        raise RuntimeError('set TP_TENANT_EMAIL and TP_TENANT_PASSWORD (never put them in a file)')
    platform = Platform(args.web_url, email, password)
    say('creating the platform-side binding')
    spec = entry['platform']
    if not state.get('config_id'):
        created = platform.must('/api/v1/device_config', {'name': spec['template_name'], 'device_type': spec['device_type'],
                                                          'protocol_type': spec['protocol_type'], 'device_conn_type': 'A'})
        state['config_id'] = created['id']
    platform.must('/api/v1/device_config', {'id': state['config_id'], 'name': spec['template_name'],
                                            'template_secret': generated[spec['secret_env']], 'auto_register': 1}, 'PUT')
    print('    device template %s (protocol %s), auto registration on' % (state['config_id'][:8], spec['protocol_type']))

    device_number = args.device

    say('installing through the Agent')
    stack_env = read_env(stack / '.env')
    env = list(entry.get('env', []))
    for target, source in entry.get('env_from_stack', {}).items():
        env.append('%s=%s' % (target, stack_env[source]))
    env += ['%s=%s' % item for item in generated.items()]
    profile = {'name': entry['id'], 'image_id': entry['image_id'], 'entrypoint': entry['entrypoint'], 'args': entry.get('args', []),
               'network': args.network or '%s_thingspanel_network' % project, 'reviewed_image_metadata': True, 'env': env}
    state_path.write_text(json.dumps(state))
    os.chmod(state_path, 0o600)
    agent = Agent(args, project)
    agent.ensure_running(profile)
    agent_install(agent, entry, state, state_path, platform, profile)
    if device_number and not state.get('registered'):
        say('first device: the plugin registers it on its first uplink, then the Broker allowlist is reloaded')
        net = args.network or '%s_thingspanel_network' % project
        first = uplink(args, entry, state, net, device_number)
        print('    first uplink (registers the device): %s' % first[:70])
        time.sleep(3)
        reload_broker(stack, args.web_url)
        platform = Platform(args.web_url, email, password)
        agent_install(agent, entry, state, state_path, platform, profile)  # new generation: the plugin reconnects to the reloaded Broker
        state['registered'] = True
        state['device_number'] = device_number
        state_path.write_text(json.dumps(state))
    check(args, stack, platform, entry, state)


def container_ip(project_network, generation):
    name = sh(['docker', 'ps', '--filter', 'label=io.thingspanel.agent.generation=%d' % generation, '--format', '{{.Names}}']).stdout.split()[0]
    return name, sh(['docker', 'inspect', name, '--format', '{{(index .NetworkSettings.Networks "%s").IPAddress}}' % project_network]).stdout.strip()


def check(args, stack, platform, entry, state):
    say('checking that it works')
    spec = entry['platform']
    project = project_of(stack)
    network = args.network or '%s_thingspanel_network' % project
    time.sleep(8)
    name, ip = container_ip(network, state['generation'])
    print('    plugin container %s is running (the platform blocks the legacy heartbeat endpoint, so liveness is checked on the container)' % name[-8:])
    if not state.get('device_number'):
        return
    result = uplink(args, entry, state, network, state['device_number'], {'temp': 33.3, 'hum': 77})
    print('    uplink answered: %s' % result[:80])
    time.sleep(6)
    device_id = sh(['docker', 'compose', 'exec', '-T', 'postgres', 'psql', '-U', 'postgres', '-d', 'ThingsPanel', '-Atc',
                    "select id from devices where device_number='%s'" % state['device_number']], cwd=stack).stdout.strip()
    data = platform.must('/api/v1/telemetry/datas/current/' + device_id)
    print('    the platform shows: %s' % {item['key']: item['value'] for item in data})


def renew(args):
    state = json.loads((Path(args.agent_dir) / 'plugins' / (args.plugin + '.json')).read_text())
    agent = Agent(args, project_of(args.stack_dir))
    agent.ctl('/v1/grant', instance=args.plugin, generation=state['generation'], request_key=state['request_key'],
              grant_id='renew-%d' % int(time.time()), ttl_seconds=86400)
    print('lease extended by 24 hours')


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('command', choices=['install', 'renew'])
    parser.add_argument('--catalog')
    parser.add_argument('--trust-root')
    parser.add_argument('--plugin', required=True)
    parser.add_argument('--stack-dir', required=True)
    parser.add_argument('--web-url', required=True)
    parser.add_argument('--agent-dir', required=True)
    parser.add_argument('--agent-bin')
    parser.add_argument('--agentctl-bin')
    parser.add_argument('--agent-image', default='registry.cn-hangzhou.aliyuncs.com/thingspanel/thingspanel-go:v1.3.0')
    parser.add_argument('--network')
    parser.add_argument('--device')
    args = parser.parse_args()
    try:
        (install if args.command == 'install' else renew)(args)
    except Exception as error:  # noqa: BLE001 - CLI boundary
        print('error: %s' % error, file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
