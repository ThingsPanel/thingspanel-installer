"""Append a Modbus image to an independently verified HA/HTTP catalog."""

import argparse
import hashlib
import json
from pathlib import Path
import re
import shutil
import time


def sha256(path):
    with path.open("rb") as file:
        return hashlib.file_digest(file, "sha256").hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("baseline", "image-inspect", "image-archive", "output"):
        parser.add_argument(f"--{name}", required=True, type=Path)
    parser.add_argument("--source-commit", required=True)
    parser.add_argument("--installer-commit", required=True)
    args = parser.parse_args()

    baseline = json.loads(args.baseline.read_text(encoding="utf-8"))
    target = baseline["target_platform"]
    if target not in ("linux/amd64", "linux/arm64"):
        parser.error("unsupported baseline target")
    ids = [entry["id"] for entry in baseline["plugins"]]
    if sorted(ids) not in (["homeassistant", "http-adapter"], ["homeassistant", "http-adapter", "modbus"]):
        parser.error("expected a signed HA/HTTP catalog with optional Modbus")
    image, = json.loads(args.image_inspect.read_text(encoding="utf-8"))
    if f"{image['Os']}/{image['Architecture']}" != target:
        parser.error("image and baseline architectures differ")
    if image["Config"]["User"] != "65534:65534" or any(
        image["Config"].get(field) for field in ("Labels", "Volumes", "OnBuild")
    ):
        parser.error("unexpected managed image configuration")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", image["Id"]) or not all(
        re.fullmatch(r"[0-9a-f]{40}", commit)
        for commit in (args.source_commit, args.installer_commit)
    ):
        parser.error("expected fixed image and source digests")
    template = Path(__file__).resolve().parents[1] / "packaging" / "modbus-managed.json"
    modbus = json.loads(template.read_text(encoding="utf-8"))
    modbus.update(image_id=image["Id"], image_platform=target, image_archive_sha256=sha256(args.image_archive),
                  source_commit=args.source_commit, installer_commit=args.installer_commit)
    now = int(time.time())
    if baseline["expires_at"] <= now:
        parser.error("baseline catalog has expired")
    arch = target.split("/")[1]
    entries = {entry["id"]: entry for entry in baseline["plugins"]}
    retained = {
        f"tp-agent-linux-{arch}": baseline["agent_sha256"],
        f"ha-image-linux-{arch}.tar": entries["homeassistant"]["image_archive_sha256"],
        f"http-image-linux-{arch}.tar": entries["http-adapter"]["image_archive_sha256"],
    }
    for name, digest in retained.items():
        if sha256(args.baseline.parent / name) != digest:
            parser.error(f"signed baseline asset digest does not match: {name}")
    for name in retained:
        shutil.copyfile(args.baseline.parent / name, args.output.parent / name)
    baseline["plugins"] = [entry for entry in baseline["plugins"] if entry["id"] != "modbus"] + [modbus]
    baseline.update(version="1.3.0", release_tag=f"plugin-modbus-v{modbus['version']}", published_at=now)
    args.output.write_text(json.dumps(baseline, sort_keys=True, indent=2) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
