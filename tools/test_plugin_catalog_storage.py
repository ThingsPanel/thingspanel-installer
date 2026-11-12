import copy
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import plugin_catalog as pc  # noqa: E402

BASE = {'id': 'matter', 'version': '1.0.0', 'kind': 'service', 'image_id': 'sha256:' + 'b' * 64, 'entrypoint': '/connector',
        'platform': {'binding': 'none', 'service_identifier': 'matter'},
        'volumes': [{'key': 'fabric', 'destination': '/var/lib/thingspanel-matter', 'mode': 'rw'}],
        'tmpfs': [{'destination': '/tmp', 'size_mib': 64}]}


def bad(**changes):
    e = copy.deepcopy(BASE)
    e.update(changes)
    return e


class StorageCatalogTest(unittest.TestCase):
    def test_good_entry(self):
        pc.check_entry(copy.deepcopy(BASE))

    def test_rejections(self):
        cases = {
            'bind-like': bad(volumes=[{'key': 'fabric', 'destination': '/d:/host', 'mode': 'rw'}]),
            'relative': bad(volumes=[{'key': 'fabric', 'destination': 'data', 'mode': 'rw'}]),
            'traversal': bad(volumes=[{'key': 'fabric', 'destination': '/a/../etc', 'mode': 'rw'}]),
            'reserved': bad(volumes=[{'key': 'fabric', 'destination': '/var/run/docker.sock', 'mode': 'rw'}]),
            'mode': bad(volumes=[{'key': 'fabric', 'destination': '/data', 'mode': 'rw,z'}]),
            'host source': bad(volumes=[{'key': 'fabric', 'destination': '/data', 'mode': 'rw', 'source': '/'}]),
            'dup key': bad(volumes=[BASE['volumes'][0], {'key': 'fabric', 'destination': '/srv/x', 'mode': 'rw'}]),
            'nested': bad(volumes=[BASE['volumes'][0], {'key': 'inner', 'destination': '/var/lib/thingspanel-matter/x', 'mode': 'rw'}]),
            'tmpfs size': bad(tmpfs=[{'destination': '/tmp', 'size_mib': 4096}]),
            'tmpfs option': bad(tmpfs=[{'destination': '/tmp', 'size_mib': 8, 'options': 'exec'}]),
        }
        for name, entry in cases.items():
            with self.subTest(name), self.assertRaises(ValueError):
                pc.check_entry(entry)


if __name__ == '__main__':
    unittest.main()
