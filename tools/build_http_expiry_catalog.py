"""Build HTTP 1.2.1 metadata from the verified 1.2.0 catalog; signing stays in CI."""
import copy
import hashlib
import json
import os
from pathlib import Path
import time

SOURCE = "247579be25b7125dd2f9a5c7b4f975e56ccdb695"
TOOLKIT = "3f5ac1ec4034a6879867be56e54c7c9d1798aa14"
BASE_SOURCE = "1b3cd6825bfc06b083b3059bca585adb9a3b379a"
VERSION = "1.2.1"


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def build(base, image, arch, agent_sha, ha_sha, archive_sha, installer_commit, now):
    if arch not in ("amd64", "arm64") or base["target_platform"] != "linux/" + arch:
        raise ValueError("catalog platform mismatch")
    if base["version"] != "1.2.0" or base["release_tag"] != "plugin-http-v1.2.0":
        raise ValueError("unexpected base release")
    if len(base["plugins"]) != 2 or {p["id"] for p in base["plugins"]} != {"http-adapter", "homeassistant"}:
        raise ValueError("unexpected base plugins")
    catalog = copy.deepcopy(base)
    ha = next(p for p in catalog["plugins"] if p["id"] == "homeassistant")
    http = next(p for p in catalog["plugins"] if p["id"] == "http-adapter")
    if agent_sha != base["agent_sha256"] or ha_sha != ha["image_archive_sha256"]:
        raise ValueError("preserved Agent/HA archive mismatch")
    if http["source_commit"] != BASE_SOURCE or http["version"] != "1.2.0":
        raise ValueError("unexpected deployed HTTP source")
    if image["Os"] != "linux" or image["Architecture"] != arch:
        raise ValueError("image platform mismatch")
    config = image["Config"]
    if config.get("Labels") or config.get("Volumes") or config.get("OnBuild"):
        raise ValueError("unsupported image metadata")
    if config.get("Entrypoint") != [http["entrypoint"]] + http["args"]:
        raise ValueError("image entrypoint mismatch")
    if not image["Id"].startswith("sha256:") or len(image["Id"]) != 71:
        raise ValueError("invalid image ID")
    http.update(version=VERSION, image_id=image["Id"], image_platform="linux/" + arch,
                image_archive_sha256=archive_sha, source_commit=SOURCE, installer_commit=installer_commit)
    catalog.update(version=VERSION, release_tag="plugin-http-v" + VERSION,
                   published_at=now, expires_at=now + 3650 * 86400)
    return catalog


def main():
    if os.environ["SOURCE_COMMIT"] != SOURCE or os.environ["TOOLKIT_COMMIT"] != TOOLKIT:
        raise ValueError("unreviewed source or toolkit")
    arch = os.environ["ARCH"]
    tmp, out = Path(os.environ["RUNNER_TEMP"]), Path("out")
    base = json.loads((tmp / "base.json").read_text())
    image = json.loads((tmp / "image.json").read_text())[0]
    catalog = build(base, image, arch, digest(out / ("tp-agent-linux-" + arch)),
                    digest(out / ("ha-image-linux-" + arch + ".tar")),
                    digest(out / ("http-image-linux-" + arch + ".tar")),
                    os.environ["GITHUB_SHA"], int(time.time()))
    (out / ("catalog-linux-" + arch + ".json")).write_text(json.dumps(catalog, sort_keys=True, indent=1) + "\n")
    provenance = {"source_commit": SOURCE, "toolkit_commit": TOOLKIT,
                  "workflow_commit": os.environ["GITHUB_SHA"], "workflow_run": os.environ["GITHUB_RUN_ID"],
                  "base_catalog_sha256": digest(tmp / "base.json"), "image_id": image["Id"],
                  "catalog_sha256": digest(out / ("catalog-linux-" + arch + ".json")),
                  "deployment_performed": False}
    (out / ("provenance-linux-" + arch + ".json")).write_text(json.dumps(provenance, indent=2) + "\n")


if __name__ == "__main__":
    main()
