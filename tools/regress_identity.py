#!/usr/bin/env python3
"""Regression of the managed runtime identity with the http-adapter plugin (needs a running platform stack).

    TP_TENANT_EMAIL=... TP_TENANT_PASSWORD=... regress_identity.py --catalog C --trust-root R --stack-dir S \
        --web-url http://127.0.0.1:PORT --agent-dir D --agent-bin B --agentctl-bin CTL

Steps (one log line each, tokens are never printed): install -> read the container's identity -> device/config answers 200
-> uninstall -> the old identity is refused (503) -> reinstall issues a higher generation: old token still refused, new one works.
The token is read from the container environment with `docker inspect`: this is a test-only shortcut.
"""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import urllib.error
import urllib.request

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import plugin_install as pi  # noqa: E402


def post(base, path, body, headers):
    request = urllib.request.Request(base + path, json.dumps(body).encode(), dict({'Content-Type': 'application/json'}, **headers))
    try:
        with urllib.request.urlopen(request, timeout=20) as response:
            return response.status, json.loads(response.read() or b'{}')
    except urllib.error.HTTPError as error:
        raw = error.read()
        try:
            return error.code, json.loads(raw)
        except ValueError:
            return error.code, {}


def code_of(body):
    data = body.get('data') if isinstance(body, dict) else None
    return (data or {}).get('errorCode') if isinstance(data, dict) else None


def container_token(plugin, generation, token_env):
    name = pi.sh(['docker', 'ps', '--filter', 'label=io.thingspanel.agent.generation=%d' % generation,
                  '--filter', 'label=io.thingspanel.agent.instance=%s' % plugin, '--format', '{{.Names}}']).stdout.split()
    if not name:
        name = pi.sh(['docker', 'ps', '--filter', 'label=io.thingspanel.agent.generation=%d' % generation, '--format', '{{.Names}}']).stdout.split()
    env = pi.sh(['docker', 'inspect', name[0], '--format', '{{range .Config.Env}}{{println .}}{{end}}']).stdout.splitlines()
    return next(e.partition('=')[2] for e in env if e.startswith(token_env + '='))


def main():
    parser = argparse.ArgumentParser()
    for name in ('catalog', 'trust-root', 'stack-dir', 'web-url', 'agent-dir', 'agent-bin', 'agentctl-bin'):
        parser.add_argument('--' + name, required=True)
    parser.add_argument('--plugin', default='http-adapter')
    args = parser.parse_args()
    py = [sys.executable, str(HERE / 'plugin_install.py')]
    common = ['--plugin', args.plugin, '--stack-dir', args.stack_dir, '--web-url', args.web_url, '--agent-dir', args.agent_dir]
    install_args = py[:1] + py[1:] + ['install', '--catalog', args.catalog, '--trust-root', args.trust_root, '--agent-bin', args.agent_bin,
                                      '--agentctl-bin', args.agentctl_bin, '--non-interactive'] + common
    results = []

    def step(name, ok, detail):
        results.append(ok)
        print('[%s] %s: %s' % ('PASS' if ok else 'FAIL', name, detail), flush=True)

    entry = json.loads((HERE.parent / 'plugins' / (args.plugin + '.json')).read_text())
    token_env = pi.token_env_of(entry)
    state_path = Path(args.agent_dir) / 'plugins' / (args.plugin + '.json')
    platform = pi.Platform(args.web_url, os.environ['TP_TENANT_EMAIL'], os.environ['TP_TENANT_PASSWORD'])

    run = subprocess.run(install_args, capture_output=True, text=True)
    state = json.loads(state_path.read_text()) if state_path.exists() else {}
    step('1 install', run.returncode == 0, 'exit=%d generation=%s' % (run.returncode, state.get('generation')))
    if run.returncode:
        print((run.stdout + run.stderr)[-800:])
        return 1
    gen1 = state['generation']
    token1 = container_token(args.plugin, gen1, token_env)
    step('2 identity injected into the container env', token1.startswith('pri_') and '.' in token1, 'format pri_<id>.<secret>, length %d' % len(token1))

    device = platform.must('/api/v1/device', {'name': 't5-regress-device', 'device_number': 't5-regress-%d-%d' % (gen1, int(time.time())), 'device_config_id': state['config_id']})
    device_id = device['id']
    auth1 = {'Authorization': 'Bearer ' + token1}
    status, body = post(args.web_url, '/api/v1/plugin/device/config', {'device_id': device_id}, auth1)
    step('3 device/config with the new identity', status == 200, 'HTTP %d' % status)
    status, body = post(args.web_url, '/api/v1/plugin/device/config', {'device_id': device_id}, {})
    step('3b device/config without identity', status == 503 and code_of(body) == 'PLUGIN_RUNTIME_IDENTITY_REQUIRED', 'HTTP %d %s' % (status, code_of(body)))

    run = subprocess.run(py + ['uninstall'] + common, capture_output=True, text=True, env=dict(os.environ))
    step('4 uninstall', run.returncode == 0, 'exit=%d %s' % (run.returncode, run.stdout.strip().splitlines()[-1] if run.stdout.strip() else run.stderr[-200:]))
    status, body = post(args.web_url, '/api/v1/plugin/device/config', {'device_id': device_id}, auth1)
    step('5 old identity after uninstall', status == 503 and code_of(body) == 'PLUGIN_RUNTIME_IDENTITY_REQUIRED', 'HTTP %d %s' % (status, code_of(body)))
    left = pi.sh(['docker', 'ps', '-q', '--filter', 'label=io.thingspanel.agent.generation=%d' % gen1]).stdout.split()
    step('5b container stopped', not left, 'running containers of generation %d: %d' % (gen1, len(left)))

    run = subprocess.run(install_args, capture_output=True, text=True)
    state = json.loads(state_path.read_text())
    step('6 reinstall', run.returncode == 0 and state['generation'] > gen1, 'exit=%d generation %d -> %s' % (run.returncode, gen1, state.get('generation')))
    if run.returncode:
        print((run.stdout + run.stderr)[-800:])
        return 1
    token2 = container_token(args.plugin, state['generation'], token_env)
    status_old, _ = post(args.web_url, '/api/v1/plugin/device/config', {'device_id': device_id}, auth1)
    status_new, _ = post(args.web_url, '/api/v1/plugin/device/config', {'device_id': device_id}, {'Authorization': 'Bearer ' + token2})
    step('7 reinstall: old generation refused, new accepted', status_old == 503 and status_new == 200, 'old HTTP %d, new HTTP %d' % (status_old, status_new))
    return 0 if all(results) else 1


if __name__ == '__main__':
    sys.exit(main())
