"""Opt-in Docker filesystem check; no daemon calls during normal unit discovery.

Requires an explicitly supplied, already-local image with /bin/sh and cat.
Creates one private directory and one uniquely named network-isolated container.
This proves bind-mount visibility, not a real Broker reload or platform chain.
"""
import argparse
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import uuid

REPO = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location('tp_update_mount_check', REPO / 'config/native/update.py')
update = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(update)


def command(*arguments):
    return subprocess.check_output(arguments, text=True, stderr=subprocess.PIPE, timeout=45).strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--image', required=True, help='Already-local reviewed image; no pull is performed')
    args = parser.parse_args()
    image_id = command('docker', 'image', 'inspect', '--format', '{{.Id}}', args.image)
    name = 'tp-takeover-b-installer-mount-' + uuid.uuid4().hex[:12]
    with tempfile.TemporaryDirectory(prefix='tp-takeover-b-installer-mount-') as temporary:
        root = Path(temporary)
        broker = root / 'broker'
        broker.mkdir()
        target = broker / 'services.json'
        update.durable_bytes(target, b'{"generation":1}\n', 0o644)
        try:
            command('docker', 'run', '--pull=never', '--detach', '--name', name,
                    '--network', 'none', '--read-only', '--entrypoint', '/bin/sh',
                    '--mount', 'type=bind,src=' + str(broker) + ',dst=/run/broker,readonly',
                    '--mount', 'type=bind,src=' + str(target) + ',dst=/run/control.json,readonly',
                    args.image, '-c', 'while :; do sleep 10; done')
            before = json.loads(command('docker', 'exec', name, 'cat', '/run/broker/services.json'))
            update.durable_bytes(target, b'{"generation":2}\n', 0o644)
            after = json.loads(command('docker', 'exec', name, 'cat', '/run/broker/services.json'))
            control = json.loads(command('docker', 'exec', name, 'cat', '/run/control.json'))
            if before != {'generation': 1} or after != {'generation': 2}:
                raise RuntimeError('Directory-mounted configuration did not observe the atomic replacement')
            print(json.dumps({'passed': True, 'image': args.image, 'localImageID': image_id,
                              'directoryBefore': before, 'directoryAfter': after,
                              'singleFileControlAfter': control,
                              'scope': 'isolated Docker bind-mount visibility only'}, sort_keys=True))
        finally:
            subprocess.run(['docker', 'rm', '--force', name], stdout=subprocess.PIPE,
                           stderr=subprocess.PIPE, timeout=45, check=False)


if __name__ == '__main__':
    main()
