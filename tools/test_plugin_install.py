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


class FakePlatform:
    """In-memory stand-in for the service plugin API (list / create / update)."""
    def __init__(self):
        self.rows = []
        self.calls = []

    def must(self, path, data=None, method=None):
        self.calls.append((method or ('POST' if data else 'GET'), path))
        if path.startswith('/api/v1/service/list'):
            return {'list': [dict(r) for r in self.rows], 'total': len(self.rows)}
        if method == 'PUT':
            row = next(r for r in self.rows if r['id'] == data['id'])
            row.update(data)
            return None
        row = dict(data, id='id-%d' % (len(self.rows) + 1))
        self.rows.append(row)
        return {'id': row['id']}


class ServicePlugin(unittest.TestCase):
    def test_alias_is_stable_and_config_filled(self):
        e = entry('homeassistant')
        self.assertEqual(pi.network_alias(e), 'tp-plugin-homeassistant')
        self.assertEqual(pi.service_plugin_config(e), {'sub_topic_prefix': 'plugin/homeassistant/', 'http_address': 'tp-plugin-homeassistant:9001'})

    def test_repeat_install_keeps_exactly_one_row(self):
        e, platform = entry('homeassistant'), FakePlatform()
        first = pi.ensure_service_plugin(platform, e)
        second = pi.ensure_service_plugin(platform, e)
        third = pi.ensure_service_plugin(platform, e)
        self.assertEqual((first[1], second[1], third[1]), ('created', 'updated', 'updated'))
        self.assertEqual(len(platform.rows), 1)
        self.assertEqual(first[0], second[0])
        config = json.loads(platform.rows[0]['service_config'])
        self.assertEqual(config['http_address'], 'tp-plugin-homeassistant:9001')
        self.assertEqual(config['sub_topic_prefix'], 'plugin/homeassistant/')

    def test_update_repairs_stale_address_and_keeps_foreign_keys(self):
        e, platform = entry('homeassistant'), FakePlatform()
        platform.rows.append({'id': 'x', 'service_identifier': 'homeassistant', 'remark': 'mine',
                              'service_config': json.dumps({'http_address': 'tp-agent-prepared-abc:9001', 'extra': 1})})
        pi.ensure_service_plugin(platform, e)
        self.assertEqual(len(platform.rows), 1)
        config = json.loads(platform.rows[0]['service_config'])
        self.assertEqual(config['http_address'], 'tp-plugin-homeassistant:9001')
        self.assertEqual(config['extra'], 1)
        self.assertEqual(platform.rows[0]['remark'], 'mine')

    def test_other_identifiers_are_not_touched(self):
        e, platform = entry('homeassistant'), FakePlatform()
        platform.rows.append({'id': 'o', 'service_identifier': 'other', 'service_config': '{}'})
        pi.ensure_service_plugin(platform, e)
        self.assertEqual(len(platform.rows), 2)
        self.assertEqual(platform.rows[0]['service_config'], '{}')

    def test_template_plugin_registers_nothing(self):
        platform = FakePlatform()
        self.assertEqual(pi.ensure_service_plugin(platform, entry('http-adapter'))[1], 'skipped')
        self.assertEqual(platform.calls, [])

    def test_catalog_rejects_bad_alias_and_template_service_plugin(self):
        e = entry('homeassistant')
        e['platform']['network_alias'] = 'Bad_Alias'
        with self.assertRaises(ValueError):
            plugin_catalog.check_entry(e)
        h = entry('http-adapter')
        h['platform']['service_plugin'] = {'name': 'x', 'config': {}}
        with self.assertRaises(ValueError):
            plugin_catalog.check_entry(h)


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
