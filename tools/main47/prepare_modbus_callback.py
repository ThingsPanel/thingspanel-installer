"""Provide loopback callbacks for the existing main47 host backend."""
import argparse
import fcntl
import hashlib
import json
from pathlib import Path
import subprocess
import os

ROOT = Path("/opt/tp-community-main-20261028")
BASE = ROOT / "modbus-runtime"


def run(*args):
    return subprocess.check_output(args, universal_newlines=True, timeout=60).strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--sha256", required=True)
    args = parser.parse_args()
    binary = BASE / "modbus-callback"
    if hashlib.sha256(binary.read_bytes()).hexdigest() != args.sha256:
        raise RuntimeError("callback binary checksum mismatch")
    binary.chmod(0o755)
    with open("/opt/tp-community-actions/deployment.lock", "a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        # Reuse the same verified runtime image as the existing HTTP callback bridge.
        existing = json.loads(run("docker", "inspect", "tp-main47-http-ingress"))[0]
        network, = existing["NetworkSettings"]["Networks"]
        service = {
            "image": existing["Image"], "container_name": "tp-main47-modbus-callback",
            "entrypoint": ["/modbus-callback"], "user": "65534:65534", "read_only": True,
            "restart": "unless-stopped", "cap_drop": ["ALL"],
            "security_opt": ["no-new-privileges:true"], "mem_limit": "64m",
            "cpus": 0.5, "pids_limit": 64,
            "volumes": [str(binary) + ":/modbus-callback:ro"],
            "ports": ["127.0.0.1:1503:1503"], "networks": ["existing"],
            "logging": {"driver": "json-file", "options": {"max-size": "5m", "max-file": "2"}},
        }
        config = ROOT / "compose.modbus-callback.private.json"
        config.write_text(json.dumps({"services": {"modbus-callback": service},
                                     "networks": {"existing": {"external": True, "name": network}}}, indent=2))
        config.chmod(0o600)
        run("docker", "compose", "-p", "tp-main47-modbus-runtime", "-f", str(config),
            "up", "-d", "--no-deps", "modbus-callback")
        hosts = Path("/etc/hosts")
        for line in hosts.read_text().splitlines():
            parts = line.split("#")[0].split()
            if "tp-plugin-modbus" in parts:
                if not parts or parts[0] != "127.0.0.1":
                    raise RuntimeError("unexpected existing Modbus hostname mapping")
                break
        else:
            with hosts.open("a") as file:
                file.write("\n127.0.0.1 tp-plugin-modbus # main47 managed Modbus callback\n")
        print("Modbus host callback bridge ready at 127.0.0.1:1503")


if __name__ == "__main__":
    os.umask(0o077)
    main()
