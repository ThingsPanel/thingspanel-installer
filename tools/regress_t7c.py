#!/usr/bin/env python3
"""T7c regression on a minimal stack (no real Broker data path): service plugin auto-registration, stable address
across a reinstall, and periodic device discovery by the Home Assistant plugin.

    TP_TENANT_EMAIL=... TP_TENANT_PASSWORD=... HA_ACCESS_TOKEN=... regress_t7c.py --catalog C --trust-root R \
        --stack-dir S --web-url URL --agent-dir D --agent-bin B --agentctl-bin CTL [--ha-url http://ha.invalid:8123]
"""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import uuid

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import plugin_install as pi  # noqa: E402

RESULTS = []


def step(name, ok, detail=''):
    RESULTS.append(ok)
    print('[%s] %s %s' % ('PASS' if ok else 'FAIL', name, detail), flush=True)


def plugin_rows(platform):
    return [r for r in pi.list_service_plugins(platform, 2) if r['service_identifier'] == 'homeassistant']


def in_network(args, network, command):
    return pi.sh(['docker', 'run', '--rm', '--name', 't7c-probe-%d' % int(time.time() * 1000), '--network', network, '--entrypoint', 'sh', args.agent_image, '-c', command], check=False).stdout.strip()


def sync_status(args, network):
    raw = in_network(args, network, 'wget -q -O- -T 5 http://tp-plugin-homeassistant:9002/sync-status')
    return json.loads(raw) if raw.startswith('{') else {}


def main():
    parser = argparse.ArgumentParser()
    for name in ('catalog', 'trust-root', 'stack-dir', 'web-url', 'agent-dir', 'agent-bin', 'agentctl-bin'):
        parser.add_argument('--' + name, required=True)
    parser.add_argument('--ha-url', default='http://ha.invalid:8123')
    parser.add_argument('--agent-image', default='registry.cn-hangzhou.aliyuncs.com/thingspanel/thingspanel-go:v1.3.0')
    args = parser.parse_args()
    project = pi.project_of(args.stack_dir)
    network = '%s_thingspanel_network' % project
    install = [sys.executable, str(HERE / 'plugin_install.py'), 'install', '--catalog', args.catalog, '--trust-root', args.trust_root, '--plugin', 'homeassistant',
               '--stack-dir', args.stack_dir, '--web-url', args.web_url, '--agent-dir', args.agent_dir, '--agent-bin', args.agent_bin,
               '--agentctl-bin', args.agentctl_bin, '--non-interactive', '--set', 'HA_BASE_URL=' + args.ha_url]
    platform = pi.Platform(args.web_url, os.environ['TP_TENANT_EMAIL'], os.environ['TP_TENANT_PASSWORD'])
    step('0 no homeassistant row before install', len(plugin_rows(platform)) == 0)

    run = subprocess.run(install, capture_output=True, text=True)
    step('1 one-command install', run.returncode == 0, 'exit=%d' % run.returncode)
    if run.returncode:
        print((run.stdout + run.stderr)[-1200:])
        return 1
    rows = plugin_rows(platform)
    config = json.loads(rows[0]['service_config']) if rows else {}
    step('2 service plugin row auto-registered', len(rows) == 1 and config.get('sub_topic_prefix') == 'plugin/homeassistant/' and config.get('http_address') == 'tp-plugin-homeassistant:9001', str(config))
    row_id = rows[0]['id'] if rows else None
    form = platform.call('/api/v1/service/access/voucher/form?service_plugin_id=%s' % row_id)
    step('3 platform reaches the plugin through the registered address', form[0] == 200 and isinstance(form[1], dict) and form[1].get('code') == 200, 'HTTP %s code %s' % (form[0], form[1].get('code') if isinstance(form[1], dict) else form[1]))

    access = platform.must('/api/v1/service/access', {'name': 't7c-access', 'service_plugin_id': row_id, 'voucher': json.dumps({'base_url': args.ha_url, 'access_token': 'x'}, separators=(',', ':'))})
    access_id = access['id'] if isinstance(access, dict) else access
    time.sleep(5)
    st = sync_status(args, network)
    step('4 plugin sync running, no devices yet', st.get('state') == 'running' and st.get('devices') == 0, str(st))

    def add_device(tag):
        number = 't7c-%s-%s' % (tag, uuid.uuid4().hex[:6])
        platform.must('/api/v1/device/service/access/batch', {'service_access_id': access_id, 'device_list': [{
            'device_name': number, 'device_number': number, 'protocol_config': json.dumps({'entity_id': 'light.' + tag}, separators=(',', ':'))}]})
        return time.time()

    def wait_for(count, limit=75):
        start = time.time()
        while time.time() - start < limit:
            st = sync_status(args, network)
            if st.get('devices', 0) >= count:
                return time.time() - start, st
            time.sleep(3)
        return None, sync_status(args, network)

    add_device('one')
    took, st = wait_for(1)
    step('5 new platform device is noticed within 60s', took is not None and took <= 62, 'after %s s, status %s' % (round(took, 1) if took else None, st))

    run = subprocess.run(install, capture_output=True, text=True)
    step('6 reinstall', run.returncode == 0, 'exit=%d' % run.returncode)
    if run.returncode:
        print((run.stdout + run.stderr)[-1200:])
        return 1
    rows2 = plugin_rows(platform)
    step('7 still exactly one service plugin row, same id', len(rows2) == 1 and rows2[0]['id'] == row_id, '%d row(s)' % len(rows2))
    form = platform.call('/api/v1/service/access/voucher/form?service_plugin_id=%s' % row_id)
    step('8 address still works after the generation changed', form[0] == 200 and isinstance(form[1], dict) and form[1].get('code') == 200, 'HTTP %s' % form[0])
    names = pi.sh(['docker', 'ps', '--filter', 'name=tp-agent-prepared', '--format', '{{.Names}}']).stdout.split()
    step('9 only one plugin container', len(names) == 1, str(names))

    add_device('two')
    took, st = wait_for(2)
    step('10 device created after the reinstall is noticed within 60s', took is not None and took <= 62, 'after %s s, status %s' % (round(took, 1) if took else None, st))
    return 0 if all(RESULTS) else 1


if __name__ == '__main__':
    sys.exit(main())
