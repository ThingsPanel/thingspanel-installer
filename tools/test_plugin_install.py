import json
import os
from pathlib import Path
import sys
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import plugin_catalog  # noqa: E402
import plugin_install as pi  # noqa: E402

PLUGINS = Path(__file__).resolve().parents[1] / 'plugins'


def entry(name):
    return json.loads((PLUGINS / (name + '.json')).read_text())


class UserEnv(unittest.TestCase):
    def test_set_and_secret_from_environment(self):
        with mock.patch.dict(os.environ, {'HA_ACCESS_TOKEN': 'tok-123'}):
            got = pi.collect_user_env(entry('homeassistant'), ['HA_BASE_URL=http://ha.local:8123'], False)
        self.assertEqual(got, {'HA_BASE_URL': 'http://ha.local:8123', 'HA_ACCESS_TOKEN': 'tok-123'})

    def test_secret_refused_on_command_line(self):
        with self.assertRaisesRegex(RuntimeError, 'secret'):
            pi.collect_user_env(entry('homeassistant'), ['HA_ACCESS_TOKEN=x'], False)

    def test_missing_required_noninteractive_fails_without_echoing_values(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            with self.assertRaisesRegex(RuntimeError, 'HA_BASE_URL is required'):
                pi.collect_user_env(entry('homeassistant'), [], False)

    def test_url_validation(self):
        with mock.patch.dict(os.environ, {'HA_ACCESS_TOKEN': 'x'}):
            with self.assertRaisesRegex(RuntimeError, 'http'):
                pi.collect_user_env(entry('homeassistant'), ['HA_BASE_URL=ftp://nope'], False)

    def test_newline_rejected(self):
        with mock.patch.dict(os.environ, {'HA_ACCESS_TOKEN': 'a\nB=c'}):
            with self.assertRaisesRegex(RuntimeError, 'single line'):
                pi.collect_user_env(entry('homeassistant'), ['HA_BASE_URL=http://h'], False)

    def test_prompt_hides_secret(self):
        with mock.patch.dict(os.environ, {}, clear=True), mock.patch('builtins.input', return_value='http://h:1') as inp, \
                mock.patch('getpass.getpass', return_value='sekret') as gp:
            got = pi.collect_user_env(entry('homeassistant'), [], True)
        self.assertEqual(got['HA_ACCESS_TOKEN'], 'sekret')
        inp.assert_called_once()
        gp.assert_called_once()
        self.assertNotIn('sekret', gp.call_args[0][0])


class Identity(unittest.TestCase):
    def test_with_identity_replaces_and_does_not_mutate(self):
        e = entry('http-adapter')
        profile = {'env': ['A=1', 'P_PLATFORM_API_TOKEN=old', 'TP_PLUGIN_SERVICE_IDENTIFIER=old']}
        out = pi.with_identity(profile, e, 'pri_x.y')
        self.assertEqual(out['env'], ['A=1', 'P_PLATFORM_API_TOKEN=pri_x.y', 'TP_PLUGIN_SERVICE_IDENTIFIER=HTTP'])
        self.assertEqual(profile['env'][1], 'P_PLATFORM_API_TOKEN=old')

    def test_default_token_env_and_service(self):
        e = entry('homeassistant')
        self.assertEqual(pi.token_env_of(e), 'TP_PLUGIN_RUNTIME_TOKEN')
        self.assertEqual(pi.service_identifier(e), 'homeassistant')
        self.assertEqual(pi.binding_of(e), 'none')
        self.assertEqual(pi.binding_of(entry('http-adapter')), 'template')


class CatalogChecks(unittest.TestCase):
    def test_both_entries_valid(self):
        for name in ('homeassistant', 'http-adapter'):
            plugin_catalog.check_entry(entry(name))

    def test_bad_binding_rejected(self):
        e = entry('homeassistant')
        e['platform']['binding'] = 'bogus'
        with self.assertRaises(ValueError):
            plugin_catalog.check_entry(e)

    def test_none_needs_service_identifier(self):
        e = entry('homeassistant')
        del e['platform']['service_identifier']
        with self.assertRaises(ValueError):
            plugin_catalog.check_entry(e)

    def test_template_needs_fields(self):
        e = entry('http-adapter')
        del e['platform']['secret_env']
        with self.assertRaises(ValueError):
            plugin_catalog.check_entry(e)


if __name__ == '__main__':
    unittest.main()
