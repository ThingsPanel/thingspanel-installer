"""PH1-S2: bootstrap tells an old image (no 33.sql expected) from a new image that forgot to ship it. No Docker."""
import importlib.util
import io
import subprocess
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path
from unittest import mock

REPO = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('tp_prepare_guard', REPO / 'config/native/prepare.py')
prepare = importlib.util.module_from_spec(spec)
spec.loader.exec_module(prepare)
IMAGE = 'registry.example/thingspanel-go:test'


class RequiredExtraSqlTests(unittest.TestCase):
    def community(self, with_file):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name)
        if with_file: (root / '33.sql').write_text('SELECT 1;')
        return root

    def test_new_image_with_file_is_applied(self):
        root = self.community(True)
        self.assertEqual(prepare.required_extra_sql(root, True, IMAGE), [root / '33.sql'])

    def test_new_image_missing_file_fails_naming_file_and_image(self):
        with self.assertRaises(RuntimeError) as ctx:
            prepare.required_extra_sql(self.community(False), True, IMAGE)
        self.assertIn('sql/33.sql', str(ctx.exception))
        self.assertIn(IMAGE, str(ctx.exception))

    def test_old_image_without_file_is_skipped_with_log(self):
        out = io.StringIO()
        with redirect_stdout(out):
            self.assertEqual(prepare.required_extra_sql(self.community(False), False, IMAGE), [])
        self.assertIn('skipping sql/33.sql', out.getvalue())

    def test_old_image_that_has_the_file_still_applies_it(self):
        root = self.community(True)
        self.assertEqual(prepare.required_extra_sql(root, False, IMAGE), [root / '33.sql'])


class MarkerProbeTests(unittest.TestCase):
    def probe(self, code, err=b''):
        result = subprocess.CompletedProcess([], code, b'', err)
        with mock.patch.object(prepare.subprocess, 'run', return_value=result):
            return prepare.image_has_marker('c1')

    def test_marker_present(self):
        self.assertTrue(self.probe(0))

    def test_marker_cleanly_absent_means_old_image(self):
        self.assertFalse(self.probe(1, b'Error: Could not find the file /go/src/app/x in container c1'))

    def test_undeterminable_fails_closed(self):
        # Safe default: an unexpected docker error stops the install instead of silently skipping the migration.
        with self.assertRaises(RuntimeError):
            self.probe(1, b'Cannot connect to the Docker daemon')


if __name__ == '__main__':
    unittest.main()
