#!/usr/bin/env python3
"""Start (or stop) the host Agent in catalog mode, the mode the platform's one-click install talks to.

    agent_host.py up   --stack-dir DIR --agent-dir DIR --agent-bin tp-agent --catalog plugin-catalog.json --trust-root root.pem
    agent_host.py down --stack-dir DIR

Layout under --agent-dir (created 0700):
    run/    agent.sock + token      the ONLY part the platform backend container mounts (TP_AGENT_SOCKET/TP_AGENT_TOKEN_FILE)
    state/  state.json, secrets.json   Agent-private: journal and secret store (site secrets, identity tokens)

The Agent verifies the signed catalog itself and builds plugin containers from it; nothing here handles a secret.
Installing plugins is done in the platform UI (or `plugin_install.py`, which calls the same backend API).
"""
import argparse
import os
from pathlib import Path
import secrets
import subprocess
import sys
import time

DEFAULT_IMAGE = 'registry.cn-hangzhou.aliyuncs.com/thingspanel/thingspanel-go:v1.3.0'


def sh(args, check=True):
    result = subprocess.run(args, capture_output=True, text=True)
    if check and result.returncode:
        raise RuntimeError('command failed (%d): %s\n%s' % (result.returncode, ' '.join(args[:4]), (result.stderr or result.stdout)[-300:]))
    return result


def project_of(stack_dir):
    return os.environ.get('COMPOSE_PROJECT_NAME') or Path(stack_dir).resolve().name.lower()


def container_name(stack_dir):
    return 'tp-agent-' + project_of(stack_dir)


def private_dir(path, mode=0o700):
    path.mkdir(parents=True, exist_ok=True)
    os.chmod(path, mode)


def up(args):
    agent_dir, stack = Path(args.agent_dir).resolve(), Path(args.stack_dir).resolve()
    project = project_of(stack)
    name = container_name(stack)
    private_dir(agent_dir)
    run, state = agent_dir / 'run', agent_dir / 'state'
    private_dir(run)
    private_dir(state)
    token = run / 'token'
    if not token.exists():
        token.write_text(secrets.token_hex(24))
    os.chmod(token, 0o600)
    catalog, trust_root = Path(args.catalog).resolve(), Path(args.trust_root).resolve()
    if not (catalog.is_file() and Path(str(catalog) + '.sig').is_file() and trust_root.is_file()):
        raise RuntimeError('catalog, catalog signature and trust root must exist')
    network = args.network or '%s_thingspanel_network' % project
    mounts = ['-v', '%s:/tp-agent:ro' % Path(args.agent_bin).resolve(), '-v', '%s:/run/agent-ctl' % run, '-v', '%s:/run/agent/state' % state,
              '-v', '%s:/run/agent/catalog:ro' % catalog.parent, '-v', '%s:/run/agent/trust-root.pem:ro' % trust_root,
              '-v', '%s:/run/agent/stack.env:ro' % (stack / '.env')]
    if not (state / 'state.json').exists():
        sh(['docker', 'run', '--rm', '--entrypoint', '/tp-agent', '-u', '0', '-v', '%s:/tp-agent:ro' % Path(args.agent_bin).resolve(),
            '-v', '%s:/run/agent/state' % state, args.agent_image, '-state', '/run/agent/state/state.json', '-init', 'host-' + name])
    sh(['docker', 'rm', '-f', name], check=False)
    sh(['docker', 'run', '-d', '--name', name, '--restart', 'unless-stopped', '--entrypoint', '/tp-agent', '-u', '0'] + mounts +
       ['-v', '/var/run/docker.sock:/var/run/docker.sock', args.agent_image, '-state', '/run/agent/state/state.json',
        '-socket', '/run/agent-ctl/agent.sock', '-token-file', '/run/agent-ctl/token',
        '-catalog', '/run/agent/catalog/' + catalog.name, '-trust-root', '/run/agent/trust-root.pem',
        '-stack-env', '/run/agent/stack.env', '-network', network, '-docker-socket', '/var/run/docker.sock', '-sweep-interval', '5s'])
    for _ in range(20):
        if (run / 'agent.sock').exists():
            break
        time.sleep(0.5)
    else:
        raise RuntimeError('the Agent did not create its socket; see: docker logs ' + name)
    print('Agent %s is up; the platform backend mounts %s as /run/tp-agent' % (name, run))


def down(args):
    sh(['docker', 'rm', '-f', container_name(args.stack_dir)], check=False)


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('command', choices=['up', 'down'])
    parser.add_argument('--stack-dir', required=True)
    parser.add_argument('--agent-dir')
    parser.add_argument('--agent-bin')
    parser.add_argument('--catalog')
    parser.add_argument('--trust-root')
    parser.add_argument('--agent-image', default=DEFAULT_IMAGE)
    parser.add_argument('--network')
    args = parser.parse_args()
    if args.command == 'up' and not all([args.agent_dir, args.agent_bin, args.catalog, args.trust_root]):
        parser.error('up needs --agent-dir, --agent-bin, --catalog and --trust-root')
    try:
        {'up': up, 'down': down}[args.command](args)
    except Exception as error:  # noqa: BLE001 - CLI boundary
        print('error: %s' % error, file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
