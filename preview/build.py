#!/usr/bin/env python3
"""Build an isolated offline preview from exact, clean source commits."""
import argparse
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile


def run(*args, cwd=None, env=None, capture=False):
    result = subprocess.run(args, cwd=cwd, env=env, check=True,
                            stdout=subprocess.PIPE if capture else None, text=capture)
    return result.stdout.strip() if capture else None


def sha(path):
    h = hashlib.sha256()
    with path.open('rb') as f:
        for block in iter(lambda: f.read(1024 * 1024), b''):
            h.update(block)
    return h.hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('backend', 'frontend', 'core', 'smtp'):
        parser.add_argument('--' + name, type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--arch', choices=('arm64', 'amd64'), required=True)
    parser.add_argument('--encore-bin', default='encore', help='Encore CLI binary; validated preview uses 1.58.6')
    args = parser.parse_args()
    installer = Path(__file__).resolve().parents[1]
    lock = json.loads((installer / 'preview/source-lock.json').read_text())
    if any(len(v['commit']) != 40 or any(c not in '0123456789abcdef' for c in v['commit']) for v in lock['repositories'].values()):
        raise SystemExit('Every repository must have an exact reviewed commit SHA')
    roots = {name: getattr(args, name).resolve() for name in lock['repositories']}
    for name, root in {**roots, 'installer': installer}.items():
        if run('git', 'status', '--porcelain', cwd=root, capture=True):
            raise SystemExit(f'{name}: tracked/untracked changes must be excluded before building')
        actual = run('git', 'rev-parse', 'HEAD', cwd=root, capture=True)
        if name != 'installer' and actual != lock['repositories'][name]['commit']:
            raise SystemExit(f'{name}: source commit differs from source-lock.json')
    out = args.output.resolve()
    if out.exists():
        raise SystemExit('Output must be a new directory; never reuse an installed directory')
    out.mkdir(parents=True)
    installer_sha = run('git', 'rev-parse', 'HEAD', cwd=installer, capture=True)
    with tempfile.TemporaryDirectory(prefix='tp-preview-build-') as temp:
        temp = Path(temp)
        archive = temp / 'installer.tar'
        run('git', 'archive', '--format=tar', f'--output={archive}', installer_sha, cwd=installer)
        with tarfile.open(archive) as f:
            f.extractall(out, filter='data')
        archived_roots = {}
        for name, original in roots.items():
            dest = temp / ('source-' + name)
            dest.mkdir()
            source_archive = temp / (name + '.tar')
            run('git', 'archive', '--format=tar', f'--output={source_archive}', lock['repositories'][name]['commit'], cwd=original)
            with tarfile.open(source_archive) as f:
                f.extractall(dest, filter='data')
            archived_roots[name] = dest
        roots = archived_roots
        build_env = dict(os.environ, GOOS='linux', GOARCH=args.arch, CGO_ENABLED='0', GOMAXPROCS='2')
        for name, root, target in (
            ('ThingsPanel-Go', roots['backend'], '.'),
            ('notification-smtp', roots['smtp'], './cmd/plugin'),
            ('notification-migrate', roots['core'] / 'encore-api', './cmd/notification-migrate'),
            ('notification-setup', out / 'scripts/notification-setup', '.'),
        ):
            run('go', 'build', '-trimpath', '-ldflags=-s -w', '-o', str(temp / name), target,
                cwd=root, env=build_env)
        frontend_env = dict(os.environ, VITE_SERVICE_ENV='prod', VITE_NOTIFICATION_API_BASE_URL='')
        run('pnpm', 'install', '--frozen-lockfile', cwd=roots['frontend'])
        run('pnpm', 'exec', 'vite', 'build', '--mode', 'production', cwd=roots['frontend'], env=frontend_env)
        shutil.copytree(roots['frontend'] / 'dist', temp / 'dist')
        shutil.copy(roots['frontend'] / 'nginx.conf', temp / 'nginx.conf')
        shutil.copytree(roots['backend'] / 'configs', temp / 'configs')
        shutil.copytree(roots['backend'] / 'sql', temp / 'sql')
        shutil.copytree(roots['core'] / 'encore-api/l3-core/notification/migrations', temp / 'notification-migrations')
        prefix = f'thingspanel/preview-{args.arch}'
        tags = {name: f'{prefix}-{name}:{lock["previewId"]}' for name in ('backend', 'frontend', 'core', 'smtp', 'thingmodel')}
        native = tags['core'] + '-native'
        for tag, services in ((native, 'notification,authn'), (tags['thingmodel'], 'devicemetadata,authn')):
            run(args.encore_bin, 'build', 'docker', tag, '--arch', args.arch, '--os', 'linux',
                '--skip-config', '--services', services, cwd=roots['core'] / 'encore-api', env=build_env)
        # Native Encore supplies the public trust store used by setup and SMTP.
        container = run('docker', 'create', native, capture=True)
        try:
            run('docker', 'cp', f'{container}:/etc/ssl/certs/ca-certificates.crt', str(temp / 'ca-certificates.crt'))
        finally:
            run('docker', 'rm', container)
        dockerfiles = {
            'core': f'FROM {native}\nCOPY notification-migrate notification-setup /usr/local/bin/\nCOPY notification-migrations /usr/local/share/notification/migrations\nENV NOTIFICATION_MIGRATIONS_DIR=/usr/local/share/notification/migrations\n',
            'backend': 'FROM alpine:3.21\nRUN apk add --no-cache tzdata ca-certificates\nWORKDIR /go/src/app\nCOPY ThingsPanel-Go ./ThingsPanel-Go\nCOPY configs ./configs\nCOPY sql ./sql\nENTRYPOINT ["./ThingsPanel-Go"]\n',
            'frontend': 'FROM nginx:1.27-alpine\nCOPY dist /usr/share/nginx/html\nCOPY nginx.conf /etc/nginx/nginx.conf\n',
            'smtp': 'FROM scratch\nCOPY notification-smtp /notification-smtp\nCOPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt\nENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt\nUSER 65532:65532\nENTRYPOINT ["/notification-smtp"]\n',
        }
        for name, contents in dockerfiles.items():
            dockerfile = temp / ('Dockerfile.' + name)
            dockerfile.write_text(contents)
            run('docker', 'build', '--platform', 'linux/' + args.arch, '-f', str(dockerfile), '-t', tags[name], str(temp))
        # Only fresh preview volumes use the complete fixed-source model schema.
        migration_dir = roots['core'] / 'encore-api/devicemetadata/migrations'
        migrations = sorted(migration_dir.glob('*.up.sql'))
        model_sql = out / 'init-db/preview-thingmodel.sql'
        model_sql.write_bytes(b'\n'.join(p.read_bytes() for p in migrations))
        (out / 'init-db/02-thingmodel-migrations.sh').write_text(
            '#!/bin/sh\nset -eu\npsql -v ON_ERROR_STOP=1 --single-transaction --username "$POSTGRES_USER" '
            '--dbname device_metadata --file /docker-entrypoint-initdb.d/preview-thingmodel.sql\n')
        model_sql.rename(out / 'config/preview-thingmodel.sql')
        # .sql files in init-db would otherwise be applied again to ThingsPanel.
        init_script = out / 'init-db/02-thingmodel-migrations.sh'
        init_script.write_text(init_script.read_text().replace('/docker-entrypoint-initdb.d/preview-thingmodel.sql', '/run/preview-thingmodel.sql'))
        runtime = json.loads((out / 'config/thingmodel-runtime.json').read_text())
        runtime['hosted_services'] = ['authn', 'devicemetadata']
        runtime['hosted_gateways'] = []
        (out / 'config/thingmodel-runtime.json').write_text(json.dumps(runtime, indent=2) + '\n')
        (out / 'preview.overlay.yaml').write_text('services:\n  postgres:\n    volumes:\n      - ./config/preview-thingmodel.sql:/run/preview-thingmodel.sql:ro\n  thingmodel-api:\n    ports:\n      - \"127.0.0.1:29407:8080\"\n')
        network_ids = run('docker', 'network', 'ls', '-q', capture=True).split()
        networks = json.loads(run('docker', 'network', 'inspect', *network_ids, capture=True)) if network_ids else []
        allocated = [ipaddress.ip_network(item['Subnet']) for network in networks for item in ((network.get('IPAM') or {}).get('Config') or []) if item.get('Subnet')]
        free_cidr = next((f'172.30.{octet}.0/28' for octet in range(251, 199, -1) if not any(ipaddress.ip_network(f'172.30.{octet}.0/28').overlaps(old) for old in allocated)), None)
        if free_cidr is None:
            raise SystemExit('No free preview CIDR in the candidate pool')
        env = {
            'COMPOSE_PROJECT_NAME': 'tp-notification-preview-20261010',
            'COMPOSE_FILE': 'docker-compose.yml:preview.overlay.yaml', 'COMPOSE_PATH_SEPARATOR': ':',
            'FRONTEND_IMAGE': tags['frontend'], 'BACKEND_IMAGE': tags['backend'],
            'THINGMODEL_IMAGE': tags['thingmodel'], 'NOTIFICATION_CORE_IMAGE': tags['core'],
            'NOTIFICATION_SMTP_IMAGE': tags['smtp'], 'FRONTEND_HOST_PORT': '127.0.0.1:28080',
            'BACKEND_HOST_PORT': '127.0.0.1:29999', 'POSTGRES_HOST_PORT': '127.0.0.1:25555',
            'REDIS_HOST_PORT': '127.0.0.1:26379', 'MQTT_HOST_PORT': '127.0.0.1:21883',
            'GMQTT_WEB_HOST_PORT': '127.0.0.1:28082', 'GMQTT_WS_HOST_PORT': '127.0.0.1:28083',
            'MODBUS_TCP_HOST_PORT': '127.0.0.1:20502', 'MODBUS_ALT_HOST_PORT': '127.0.0.1:20503',
            'HTTP_ADAPTER_TCP_HOST_PORT': '127.0.0.1:29090', 'HTTP_ADAPTER_HTTP_HOST_PORT': '127.0.0.1:29091',
            'THINGSVIS_SERVER_HOST_PORT': '127.0.0.1:28000', 'THINGSVIS_STUDIO_HOST_PORT': '127.0.0.1:23000',
            'NOTIFICATION_CORE_HOST_PORT': '29406', 'NOTIFICATION_PRIVATE_CIDR': free_cidr,
        }
        (out / '.env').write_text(''.join(f'{k}={v}\n' for k, v in env.items()))
        controlled_env = {k: v for k, v in os.environ.items() if k not in env and not k.startswith('COMPOSE_')}
        controlled_env.update(env)
        config = json.loads(run('docker', 'compose', 'config', '--no-env-resolution', '--format', 'json', cwd=out, env=controlled_env, capture=True))
        images = sorted({s['image'] for s in config['services'].values()})
        image_lock = []
        for image in images:
            if image not in tags.values():
                run('docker', 'pull', '--platform', 'linux/' + args.arch, image)
            inspected = json.loads(run('docker', 'image', 'inspect', image, capture=True))[0]
            image_lock.append({'reference': image, 'imageId': inspected['Id'], 'architecture': inspected['Architecture'], 'repoDigests': inspected.get('RepoDigests', [])})
        archive_dir = out / 'images.d'
        archive_dir.mkdir()
        for index, image in enumerate(image_lock):
            archive_path = archive_dir / f'image-{index:02d}.tar.gz'
            with archive_path.open('wb') as stream:
                save = subprocess.Popen(['docker', 'save', image['reference']], stdout=subprocess.PIPE)
                gzip = subprocess.Popen(['gzip', '-1', '-c'], stdin=save.stdout, stdout=stream)
                save.stdout.close()
                if gzip.wait() or save.wait():
                    raise SystemExit('Offline image export failed')
            image['archive'] = str(archive_path.relative_to(out))
            image['archiveSha256'] = sha(archive_path)
        manifest = dict(lock, installerCommit=installer_sha, architecture=args.arch, images=image_lock,
                        encoreVersion=run(args.encore_bin, 'version', capture=True),
                        composeProject=env['COMPOSE_PROJECT_NAME'], expectedImages=images,
                        thingModelMigrations=[{'file': p.name, 'sha256': sha(p)} for p in migrations])
        (out / 'offline-images.lock.json').write_text(json.dumps(manifest, indent=2) + '\n')
        (out / 'verify-images.py').write_text((out / 'preview/verify-images.py').read_text())
    print(f'Preview bundle ready: {out}. It is not a production release.')


if __name__ == '__main__':
    main()
