"""The reverse proxy must survive the services an upgrade recreates (R6)."""
from pathlib import Path
import re
import unittest

REPO = Path(__file__).resolve().parents[1]
RECREATED = ('backend', 'encore-api', 'notification-core')


class ProxyConfig(unittest.TestCase):
    def test_recreated_upstreams_are_resolved_per_request(self):
        conf = (REPO / 'niginx.conf').read_text(encoding='utf-8')
        self.assertIn('resolver 127.0.0.11', conf)
        for name in RECREATED:
            self.assertEqual(re.findall(r'proxy_pass\s+http://%s[:/]' % name, conf), [],
                             'proxy_pass to %s must use a variable so a recreated container is found' % name)

    def test_frontend_is_not_restarted_or_blocked_by_its_dependencies_failing(self):
        compose = (REPO / 'docker-compose.yml').read_text(encoding='utf-8')
        frontend = compose[compose.index('  frontend:'):compose.index('\n  postgres:')]
        self.assertNotIn('restart: true', frontend)

    def test_maintenance_is_served_without_the_backend(self):
        conf = (REPO / 'niginx.conf').read_text(encoding='utf-8')
        self.assertIn('/usr/share/nginx/status/active', conf)
        self.assertIn('location = /system-status/status.json', conf)
        self.assertIn('error_page 502 503 504 @tp_unavailable', conf)
        # nothing secret is exposed: only the status file is aliased
        self.assertNotIn('autoindex on', conf)


if __name__ == '__main__':
    unittest.main()
