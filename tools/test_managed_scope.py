import importlib.util
from pathlib import Path
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location(
    'native_prepare', Path(__file__).resolve().parents[1] / 'config/native/prepare.py')
prepare = importlib.util.module_from_spec(spec)
spec.loader.exec_module(prepare)


class ManagedScopeTests(unittest.TestCase):
    def setUp(self):
        self.env = {
            'TP_MQTT_USER': 'platform', 'TP_MQTT_PASSWORD': 'test', 'TP_MQTT_CLIENT_ID': 'backend',
            'TP_PROTOCOL_MQTT_USER': 'legacy', 'TP_PROTOCOL_MQTT_PASSWORD': 'test',
            'TP_HTTP_MQTT_USER': 'http', 'TP_HTTP_MQTT_PASSWORD': 'test',
            'TP_MODBUS_MQTT_USER': 'modbus', 'TP_MODBUS_MQTT_PASSWORD': 'test',
            'TP_BROKER_CELL': 'cell', 'TP_BROKER_INSTANCE': 'broker',
        }

    def test_managed_credentials_own_only_their_device_scope(self):
        http = {'id': 'http-device', 'tenant_id': 'http-tenant'}
        modbus = {'id': 'modbus-device', 'tenant_id': 'modbus-tenant'}
        legacy = {'id': 'legacy-device', 'tenant_id': 'legacy-tenant'}
        entries = prepare.scope_services(
            self.env, [], [legacy], {'http-adapter': [http], 'modbus': [modbus]})
        by_account = {entry['username']: entry for entry in entries}
        self.assertEqual(by_account['http']['allowed_device_ids'], ['http-device'])
        self.assertEqual(by_account['modbus']['allowed_device_ids'], ['modbus-device'])
        self.assertEqual(by_account['modbus']['allowed_client_ids'], ['modbus-managed'])
        self.assertEqual(by_account['legacy']['allowed_device_ids'], ['legacy-device'])
        self.assertEqual(by_account['platform']['allowed_device_ids'], ['legacy-device', 'http-device', 'modbus-device'])
        for name in ('http', 'modbus'):
            self.assertEqual(by_account[name]['policy']['subscribe'], [])
            self.assertEqual(by_account[name]['policy']['publish'], [])

    def test_empty_live_scope_connects_without_device_permissions(self):
        entries = prepare.scope_services(self.env, [], [], {'modbus': []})
        by_account = {entry['username']: entry for entry in entries}
        self.assertNotIn('http', by_account)
        self.assertEqual(by_account['modbus']['allowed_device_ids'], [])
        self.assertEqual(by_account['modbus']['allowed_tenant_ids'], [])

    def test_older_backend_without_installation_tables_keeps_existing_scope(self):
        with patch.object(prepare, 'sql', return_value='f') as sql:
            self.assertEqual(prepare.query_managed_scopes(), {})
        sql.assert_called_once()
