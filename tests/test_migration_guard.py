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


ID, INSTALL = 'internal/pluginidentity', 'internal/plugininstall'


class RequiredExtraSqlTests(unittest.TestCase):
    def community(self, *files):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name)
        for name in files: (root / name).write_text('SELECT 1;')
        return root

    def names(self, paths):
        return [p.name for p in paths]

    def test_identity_only_image_requires_and_applies_only_33(self):
        # Regression: an image with plugin identity but no plugin-install feature must not be rejected for 34/35.
        root = self.community('33.sql')
        out = io.StringIO()
        with redirect_stdout(out):
            self.assertEqual(self.names(prepare.required_extra_sql(root, {ID}, IMAGE)), ['33.sql'])
        self.assertIn('predates internal/plugininstall; skipping sql/34.sql', out.getvalue())

    def test_image_with_both_markers_requires_and_applies_33_to_35(self):
        root = self.community('33.sql', '34.sql', '35.sql')
        self.assertEqual(self.names(prepare.required_extra_sql(root, {ID, INSTALL}, IMAGE)), ['33.sql', '34.sql', '35.sql'])

    def test_install_marker_with_missing_34_fails_naming_file_and_image(self):
        root = self.community('33.sql', '35.sql')
        with self.assertRaises(RuntimeError) as ctx:
            prepare.required_extra_sql(root, {ID, INSTALL}, IMAGE)
        self.assertIn('sql/34.sql', str(ctx.exception))
        self.assertNotIn('sql/35.sql', str(ctx.exception))
        self.assertIn(IMAGE, str(ctx.exception))

    def test_identity_marker_with_missing_33_fails(self):
        with self.assertRaises(RuntimeError) as ctx:
            prepare.required_extra_sql(self.community(), {ID}, IMAGE)
        self.assertIn('sql/33.sql', str(ctx.exception))

    def test_old_image_without_files_is_skipped_with_log(self):
        out = io.StringIO()
        with redirect_stdout(out):
            self.assertEqual(prepare.required_extra_sql(self.community(), set(), IMAGE), [])
        self.assertIn('skipping sql/33.sql', out.getvalue())
        self.assertIn('skipping sql/35.sql', out.getvalue())

    def test_image_with_files_but_without_markers_still_applies_them(self):
        root = self.community('33.sql')
        self.assertEqual(self.names(prepare.required_extra_sql(root, set(), IMAGE)), ['33.sql'])

    def test_every_extra_file_belongs_to_exactly_one_marker(self):
        self.assertEqual(sorted(prepare.EXTRA_COMMUNITY_SQL), ['33.sql', '34.sql', '35.sql'])


class MarkerProbeTests(unittest.TestCase):
    def probe(self, code, err=b''):
        result = subprocess.CompletedProcess([], code, b'', err)
        with mock.patch.object(prepare.subprocess, 'run', return_value=result):
            return prepare.image_has_marker('c1', ID)

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
