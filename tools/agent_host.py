#!/usr/bin/env python3
"""Start (or stop) the host Agent in catalog mode, the mode the platform's one-click install talks to.

    agent_host.py up   --stack-dir DIR --agent-dir DIR --agent-bin tp-agent --catalog plugin-catalog.json --trust-root root.pem
    agent_host.py down --stack-dir DIR
    agent_host.py catalog-sync --catalog BUNDLE/plugins/plugin-catalog.json --trust-root plugin-root.pem --release-source https://...

Layout under --agent-dir (created 0700):
    run/    agent.sock + token      the ONLY part the platform backend container mounts (TP_AGENT_SOCKET/TP_AGENT_TOKEN_FILE)
    state/  state.json, secrets.json   Agent-private: journal and secret store (site secrets, identity tokens)
    system/ system.json, upgrade.json   with --system: the system role's private state; mounted nowhere but into the Agent
    system-api/ agent-system.sock, token   the system role's API: its own socket and token. Only the platform's Encore
                                    container may mount it (read-only); never the Gin backend or a plugin
    backups/, status/               with --system: backup archives; status.json and the maintenance flag

The Agent verifies the signed catalog itself and builds plugin containers from it; nothing here handles a secret.
Installing plugins is done in the platform UI (or `plugin_install.py`, which calls the same backend API).
"""
import argparse
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import sys
import tempfile
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
    sysapi = agent_dir / 'system-api'   # socket + token only: the one directory the platform's Encore container mounts
    private_dir(sysdir)
    private_dir(sysapi)
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
        if args.compose_image:
            provision += ['-compose-image', args.compose_image]
        if args.release_ca:
            provision += ['-release-ca', '/run/agent/release-ca.pem']
            release_mounts += ['-v', '%s:/run/agent/release-ca.pem:ro' % Path(args.release_ca).resolve()]
    sh(['docker', 'run', '--rm', '--entrypoint', '/tp-agent', '-u', '0', '-v', '%s:/tp-agent:ro' % Path(args.agent_bin).resolve(),
        '-v', '%s:/run/agent/system' % sysdir, args.agent_image] + provision)
    sh(['docker', 'run', '--rm', '--entrypoint', '/tp-agent', '-u', '0', '-v', '%s:/tp-agent:ro' % Path(args.agent_bin).resolve(),
        '-v', '%s:/run/agent/system-api' % sysapi, args.agent_image, '-provision-system-api', '/run/agent/system-api'])
    mounts = release_mounts + ['-v', '%s:/run/agent/system' % sysdir, '-v', '%s:/run/agent/system-api' % sysapi, '-v', '%s:/run/agent/stack' % Path(args.stack_dir).resolve(),
              '-v', '%s:/run/agent/backups' % backups, '-v', '%s:/run/agent/status' % status]
    flags = ['-system-socket', '/run/agent/system-api/agent-system.sock', '-system-token-file', '/run/agent/system-api/token',
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


def docker_platform():
    out = sh(['docker', 'info', '--format', '{{.OSType}}/{{.Architecture}}']).stdout.strip().lower()
    os_name, _, arch = out.partition('/')
    return '%s/%s' % (os_name, {'aarch64': 'arm64', 'x86_64': 'amd64'}.get(arch, arch))


def image_present(image_id):
    return sh(['docker', 'image', 'inspect', '--format', '{{.Id}}', image_id], check=False).returncode == 0


def catalog_sync(args):
    """Install a newer signed plugin catalog from the release source and have every entry's image on this host.

        agent_host.py catalog-sync --catalog BUNDLE/plugins/plugin-catalog.json --trust-root PLUGIN_ROOT.pem
                                   --release-source https://... [--release-ca ca.pem] [--image-dir DIR]

    Fetches <source>/plugins/catalog-linux-<arch>.json and .sig, verifies them against the plugin trust root exactly as
    the Agent will (the Agent verifies again on every use; nothing here is trusted by it), and refuses anything that is
    not a drop-in replacement of the running bundle: the Agent binary and the Home Assistant image archive digests must
    be the ones the bundle was verified with (changing them needs a new bundle, which is a different procedure), and
    the catalog version must not go backwards. Images: an entry whose image is not on the host is loaded from
    DIR/<id>-<os>-<arch>.tar (sha256 must equal the entry's image_archive_sha256, and the loaded image id must equal
    image_id). If any image stays missing the catalog is NOT replaced, unless --allow-missing-images.
    """
    import hashlib
    import plugin_bundle
    import plugin_catalog
    catalog_path = Path(args.catalog).resolve()
    bundle = catalog_path.parent.parent
    agent_bin, ha_archive = bundle / 'bin/tp-agent', bundle / 'plugins/homeassistant-image.tar'
    trust_root = Path(args.trust_root).resolve()
    if not args.release_source or not args.release_source.startswith('https://'):
        raise RuntimeError('catalog-sync needs --release-source (https://); none is configured, so there is nothing to fetch')
    if not trust_root.is_file() or not catalog_path.is_file() or not Path(str(catalog_path) + '.sig').is_file():
        raise RuntimeError('the active catalog, its signature and the plugin trust root must exist')
    if args.release_ca:
        os.environ['SSL_CERT_FILE'] = str(Path(args.release_ca).resolve())   # urllib's default context honours it
    platform = docker_platform()
    arch = platform.split('/', 1)[1]
    stage = Path(tempfile.mkdtemp(prefix='.catalog-sync-', dir=str(catalog_path.parent)))
    os.chmod(stage, 0o700)
    try:
        base = args.release_source.rstrip('/') + '/plugins/catalog-linux-%s.json' % arch
        new_json, new_sig = stage / 'plugin-catalog.json', stage / 'plugin-catalog.json.sig'
        plugin_bundle._download(plugin_bundle._download_stream, base, new_json, plugin_bundle.MAX_CATALOG_BYTES)
        plugin_bundle._download(plugin_bundle._download_stream, base + '.sig', new_sig, plugin_bundle.MAX_SIGNATURE_BYTES)
        os.rename(new_json, stage / 'catalog-linux.json')   # load_verified reads <file> and <file>.sig
        os.rename(new_sig, stage / 'catalog-linux.json.sig')
        fresh = plugin_catalog.load_verified(stage / 'catalog-linux.json', trust_root)   # signature, schema, expiry, entries
        try:
            current = plugin_catalog.load_verified(catalog_path, trust_root)
        except Exception:  # noqa: BLE001 - an expired installed catalog is exactly when a sync is needed; digests still gate it
            current = {}
            print('note: the installed catalog does not verify (expired?); only digests are compared')
        if fresh.get('target_platform') != platform:
            raise RuntimeError('the served catalog is for %s, this host is %s' % (fresh.get('target_platform'), platform))
        if current and fresh.get('agent_sha256') != current.get('agent_sha256'):
            raise RuntimeError('the served catalog names a different Agent binary: that needs a new plugin bundle, not a catalog sync')
        ha_new = [e for e in fresh['plugins'] if e['id'] == 'homeassistant']
        if len(ha_new) != 1 or ha_new[0].get('image_archive_sha256') != hashlib.sha256(ha_archive.read_bytes()).hexdigest():
            raise RuntimeError('the served catalog changes the Home Assistant image archive: that needs a new plugin bundle, not a catalog sync')
        if hashlib.sha256(agent_bin.read_bytes()).hexdigest() != fresh.get('agent_sha256'):
            raise RuntimeError('the active Agent binary does not match the served catalog')
        if current and str(fresh['version']) != str(current['version']):
            try:
                if int(fresh['version']) < int(current['version']):
                    raise RuntimeError('the served catalog (version %s) is older than the installed one (%s); not going backwards'
                                       % (fresh['version'], current['version']))
            except ValueError:
                pass   # non-numeric versions are compared by equality only; the signature already vouches for the file
        missing = []
        image_dir = Path(args.image_dir).resolve() if args.image_dir else None
        for entry in fresh['plugins']:
            image_id = entry['image_id']
            if image_present(image_id):
                print('image ok       %s %s' % (entry['id'], entry['version']))
                continue
            archive = image_dir / ('%s-%s-%s.tar' % (entry['id'], platform.split('/')[0], arch)) if image_dir else None
            want = entry.get('image_archive_sha256')
            if archive and archive.is_file() and want:
                digest = hashlib.sha256()
                with archive.open('rb') as stream:
                    for block in iter(lambda: stream.read(1 << 20), b''):
                        digest.update(block)
                if digest.hexdigest() != want:
                    raise RuntimeError('image archive %s does not match the signed catalog digest' % archive.name)
                sh(['docker', 'load', '-i', str(archive)])
                if image_present(image_id):
                    print('image loaded   %s %s (from %s)' % (entry['id'], entry['version'], archive.name))
                    continue
                raise RuntimeError('%s loaded but the image id is not the one the catalog names' % archive.name)
            missing.append(entry)
            print('image MISSING  %s %s: %s is not on this host%s' % (
                entry['id'], entry['version'], image_id[:19],
                ' (no archive %s in --image-dir)' % archive.name if archive else ' (give --image-dir with <id>-%s-%s.tar, or docker load it)' % (platform.split('/')[0], arch)))
        if missing and not args.allow_missing_images:
            raise RuntimeError('%d image(s) missing; the catalog was NOT replaced (the Agent would list updates it cannot apply). '
                               'Provide the images or use --allow-missing-images' % len(missing))
        # Both files are replaced one after the other; the Agent re-verifies per use, so a read between the two renames
        # fails closed for that one call only.
        os.replace(stage / 'catalog-linux.json', catalog_path)
        os.replace(stage / 'catalog-linux.json.sig', Path(str(catalog_path) + '.sig'))
        print('catalog version %s installed: %s' % (fresh['version'], ', '.join('%s@%s' % (e['id'], e['version']) for e in fresh['plugins'])))
    finally:
        shutil.rmtree(stage, ignore_errors=True)


def down(args):
    sh(['docker', 'rm', '-f', container_name(args.stack_dir)], check=False)


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('command', choices=['up', 'down', 'catalog-sync'])
    parser.add_argument('--stack-dir')
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
    parser.add_argument('--compose-image', help='with --release-source: image that runs docker compose during `system upgrade` (default docker:27-cli)')
    parser.add_argument('--release-ca', help='with --release-source: extra CA bundle (PEM) for the release source')
    parser.add_argument('--image-dir', help='catalog-sync: directory with <id>-<os>-<arch>.tar image archives for entries whose image is missing')
    parser.add_argument('--allow-missing-images', action='store_true', help='catalog-sync: install the catalog even if some images are missing')
    parser.add_argument('--defer-scope-watcher', action='store_true',
                        help='installer owns the update lock and will start the watcher after activation')
    args = parser.parse_args()
    if args.command != 'catalog-sync' and not args.stack_dir:
        parser.error('--stack-dir is required')
    if args.command == 'catalog-sync' and not (args.catalog and args.trust_root):
        parser.error('catalog-sync needs --catalog and --trust-root')
    if args.compose_image and not args.release_source:
        parser.error('--compose-image needs --release-source')
    if args.command == 'up' and not all([args.agent_dir, args.agent_bin, args.catalog, args.trust_root]):
        parser.error('up needs --agent-dir, --agent-bin, --catalog and --trust-root')
    try:
        {'up': up, 'down': down, 'catalog-sync': catalog_sync}[args.command](args)
    except Exception as error:  # noqa: BLE001 - CLI boundary
        print('error: %s' % error, file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
