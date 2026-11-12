#!/usr/bin/env python3
"""Start (or stop) the host Agent in catalog mode, the mode the platform's one-click install talks to.

    agent_host.py up   --stack-dir DIR --agent-dir DIR --agent-bin tp-agent --catalog plugin-catalog.json --trust-root root.pem
    agent_host.py down --stack-dir DIR

Layout under --agent-dir (created 0700):
    run/    agent.sock + token      the ONLY part the platform backend container mounts (TP_AGENT_SOCKET/TP_AGENT_TOKEN_FILE)
    state/  state.json, secrets.json   Agent-private: journal and secret store (site secrets, identity tokens)
    system/ agent-system.sock, token, system.json   with --system: the system role (restart, backup, restore, upgrade);
                                    its own socket and token, NEVER mounted into the platform backend container
    backups/, status/               with --system: backup archives; status.json and the maintenance flag

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

sys.path.insert(0, str(Path(__file__).resolve().parent))
import scope_sync  # noqa: E402

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


def ensure_scope_watcher(stack, agent_dir, *, deferred=False, sleep=time.sleep):
    """Start the scope watcher, or let an installer start it after releasing its lock."""
    if deferred:
        print('Broker scope watcher startup deferred until installer activation releases its lock.')
        return 'deferred'
    status = None
    for _ in range(60):
        status = scope_sync.ensure(stack, agent_dir)
        if status != 'busy':
            break
        sleep(5)
    print('Broker scope watcher: %s (log %s)' % (status, Path(agent_dir) / 'scope-sync.log'))
    return status


def provision_system(args, agent_dir, project):
    """Write the reviewed system-role config and token (done by the Agent binary itself) and return docker flags."""
    sysdir, backups, status = agent_dir / 'system', agent_dir / 'backups', agent_dir / 'status'
    private_dir(sysdir)
    private_dir(backups)
    private_dir(status, 0o755)
    provision = ['-provision-system', '/run/agent/system', '-project', project]
    release_mounts = []
    if args.release_source:
        # `system upgrade` needs a signed release source and the trust root for it, both supplied out of band.
        if not args.release_trust_root:
            raise RuntimeError('--release-source needs --release-trust-root: unsigned upgrades are not allowed')
        provision += ['-release-source', args.release_source, '-release-trust-root', '/run/agent/release-root.pem',
                      '-host-root', str(Path(args.stack_dir).resolve())]
        release_mounts += ['-v', '%s:/run/agent/release-root.pem:ro' % Path(args.release_trust_root).resolve()]
        if args.release_ca:
            provision += ['-release-ca', '/run/agent/release-ca.pem']
            release_mounts += ['-v', '%s:/run/agent/release-ca.pem:ro' % Path(args.release_ca).resolve()]
    sh(['docker', 'run', '--rm', '--entrypoint', '/tp-agent', '-u', '0', '-v', '%s:/tp-agent:ro' % Path(args.agent_bin).resolve(),
        '-v', '%s:/run/agent/system' % sysdir, args.agent_image] + provision)
    mounts = release_mounts + ['-v', '%s:/run/agent/system' % sysdir, '-v', '%s:/run/agent/stack' % Path(args.stack_dir).resolve(),
              '-v', '%s:/run/agent/backups' % backups, '-v', '%s:/run/agent/status' % status]
    flags = ['-system-socket', '/run/agent/system/agent-system.sock', '-system-token-file', '/run/agent/system/token',
             '-system-config', '/run/agent/system/system.json']
    return mounts, flags


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
    system_mounts, system_flags = [], []
    if args.system:
        system_mounts, system_flags = provision_system(args, agent_dir, project)
        # The system role probes the stack's services by name, so the Agent joins the stack network.
        system_mounts += ['--network', network]
    mounts = ['-v', '%s:/tp-agent:ro' % Path(args.agent_bin).resolve(), '-v', '%s:/run/agent-ctl' % run, '-v', '%s:/run/agent/state' % state,
              '-v', '%s:/run/agent/catalog:ro' % catalog.parent, '-v', '%s:/run/agent/trust-root.pem:ro' % trust_root,
              '-v', '%s:/run/agent/stack.env:ro' % (stack / '.env')]
    if not (state / 'state.json').exists():
        sh(['docker', 'run', '--rm', '--entrypoint', '/tp-agent', '-u', '0', '-v', '%s:/tp-agent:ro' % Path(args.agent_bin).resolve(),
            '-v', '%s:/run/agent/state' % state, args.agent_image, '-state', '/run/agent/state/state.json', '-init', 'host-' + name])
    sh(['docker', 'rm', '-f', name], check=False)
    sh(['docker', 'run', '-d', '--name', name, '--restart', 'unless-stopped', '--entrypoint', '/tp-agent', '-u', '0'] + mounts +
       system_mounts + ['-v', '/var/run/docker.sock:/var/run/docker.sock', args.agent_image, '-state', '/run/agent/state/state.json',
        '-socket', '/run/agent-ctl/agent.sock', '-token-file', '/run/agent-ctl/token',
        '-catalog', '/run/agent/catalog/' + catalog.name, '-trust-root', '/run/agent/trust-root.pem',
        '-stack-env', '/run/agent/stack.env', '-network', network, '-docker-socket', '/var/run/docker.sock', '-sweep-interval', '5s'] + system_flags)
    for _ in range(20):
        if (run / 'agent.sock').exists():
            break
        time.sleep(0.5)
    else:
        raise RuntimeError('the Agent did not create its socket; see: docker logs ' + name)
    print('Agent %s is up; the platform backend mounts %s as /run/tp-agent' % (name, run))
    # Installer activation owns the update lock while starting this process;
    # direct/manual starts retain the existing wait behavior.
    if args.system:
        # The Agent keeps the Broker scope in step itself; a second (Python) watcher would only race it.
        scope_sync.stop(agent_dir)
        print('Broker scope sync: handled by the Agent (system role)')
        return
    ensure_scope_watcher(stack, agent_dir, deferred=args.defer_scope_watcher)


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
    parser.add_argument('--system', action='store_true',
                        help='also serve the system role (restart/backup/restore/upgrade) and let the Agent do the Broker scope sync')
    parser.add_argument('--release-source', help='with --system: HTTPS base URL of the signed release; enables `system upgrade`')
    parser.add_argument('--release-trust-root', help='with --release-source: PEM public key that must have signed the release')
    parser.add_argument('--release-ca', help='with --release-source: extra CA bundle (PEM) for the release source')
    parser.add_argument('--defer-scope-watcher', action='store_true',
                        help='installer owns the update lock and will start the watcher after activation')
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
