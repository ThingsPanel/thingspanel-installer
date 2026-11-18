"""Reconcile only the Modbus MQTT principal on the Community main test stack."""

import copy
import fcntl
import json
import os
from pathlib import Path
import subprocess
import time


ROOT = Path("/opt/tp-community-main-20261028")
RUNTIME = ROOT / "modbus-runtime"
CONTAINERS = ("tp-community-main-47-broker-owner-1", "tp-community-main-47-broker-mosquitto-1")
TARGETS_SQL = """
WITH live AS (
    SELECT 1 FROM plugin_installations i
    JOIN plugin_runtime_identities r ON r.plugin_instance=i.plugin_id AND r.tenant_id=i.tenant_id
        AND r.generation=i.generation
    WHERE i.plugin_id='modbus' AND i.tenant_id='' AND i.desired='enabled'
        AND i.state IN ('installing','enabled') AND r.service_identifier='MODBUS'
        AND r.revoked_at IS NULL AND r.expires_at>CURRENT_TIMESTAMP
), eligible AS (
    SELECT d.id,d.parent_id,d.tenant_id,c.protocol_type,c.device_type
    FROM devices d JOIN device_configs c ON c.id=d.device_config_id AND c.tenant_id=d.tenant_id
    WHERE c.protocol_type IN ('MODBUS_TCP','MODBUS_RTU')
        AND d.is_enabled='enabled' AND d.activate_flag='active'
        AND d.access_way IS DISTINCT FROM 'B' AND NULLIF(btrim(d.service_access_id), '') IS NULL
        AND EXISTS (SELECT 1 FROM service_plugins p WHERE p.service_identifier=c.protocol_type
                    AND p.service_type=1 AND p.service_config::jsonb->>'managed_plugin_id'='modbus')
        AND NOT EXISTS (
            SELECT 1 FROM device_topic_mappings m
            WHERE CAST(m.device_config_id AS text)=CAST(c.id AS text) AND m.enabled=TRUE)
), targets AS (
    SELECT id,tenant_id FROM eligible WHERE device_type='2'
    UNION
    SELECT child.id,child.tenant_id FROM eligible child JOIN eligible gateway
        ON gateway.id=child.parent_id AND gateway.tenant_id=child.tenant_id
        AND gateway.protocol_type=child.protocol_type AND gateway.device_type='2'
    WHERE child.device_type='3'
) SELECT json_build_object(
    'live', (SELECT count(*) FROM live)=1,
    'targets', (SELECT COALESCE(json_agg(targets ORDER BY id),'[]'::json) FROM targets)
);
"""


def run(*args):
    return subprocess.check_output(args, universal_newlines=True, timeout=60).strip()


def desired_services(current, credentials, live, targets):
    username = credentials["username"]
    if username != "tp-main47-modbus" or not credentials.get("password"):
        raise ValueError("expected dedicated Modbus MQTT credentials")
    services = [item for item in current if item["username"] != username]
    if not live:
        return services
    if len(targets) > 256:
        raise ValueError("Modbus device scope exceeds the Broker Owner limit")
    platform, = (item for item in services if item["username"] == "tp-main47-platform")
    if not platform.get("target_scoped_publisher"):
        raise ValueError("expected the target-scoped platform MQTT publisher")
    plugin = copy.deepcopy(platform)
    for field in ("target_scoped_publisher", "target_tenant_scope"):
        plugin.pop(field, None)
    plugin.update(username=username, password=credentials["password"],
                  allowed_client_ids=["modbus-managed"], allow_any_client_id=False,
                  plugin_socket=True, allowed_device_ids=sorted({item["id"] for item in targets}),
                  allowed_tenant_ids=sorted({item["tenant_id"] for item in targets}))
    plugin["principal"]["device_id"] = "main47-modbus"
    plugin["policy"] = {"publish": [], "subscribe": [], "max_payload": 1048576, "max_qos": 1,
                        "retain_profile": "live-ingress"}
    services.append(plugin)
    return services


def main():
    with open("/opt/tp-community-actions/deployment.lock", "a") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return
        state = json.loads(run("docker", "exec", "timescaledb", "psql", "-X", "-U", "postgres",
                               "-d", "ThingsPanel", "-Atc", TARGETS_SQL))
        path = ROOT / "active-broker-services.private.json"
        current = json.loads(path.read_text())
        credentials = json.loads((RUNTIME / "mqtt.private.json").read_text())
        wanted = desired_services(current, credentials, state["live"], state["targets"])
        if {item["username"]: item for item in current} != {item["username"]: item for item in wanted}:
            marker = RUNTIME / "scope-restart.pending"
            marker.touch()
            # Preserve the inode of the existing bind-mounted file.
            with path.open("r+") as handle:
                json.dump(wanted, handle, indent=2)
                handle.truncate()
                handle.flush()
                os.fsync(handle.fileno())
        marker = RUNTIME / "scope-restart.pending"
        if not marker.exists():
            return
        for container in CONTAINERS:
            run("docker", "restart", container)
            for _ in range(30):
                if run("docker", "inspect", "--format", "{{.State.Health.Status}}", container) == "healthy":
                    break
                time.sleep(1)
            else:
                raise RuntimeError(f"{container} did not become healthy")
        marker.unlink()
        print(f"Modbus MQTT scope reconciled: live={state['live']} targets={len(state['targets'])}")


if __name__ == "__main__":
    main()
