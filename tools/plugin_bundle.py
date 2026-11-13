#!/usr/bin/env python3
"""Fetch and stage one signed, per-architecture Home Assistant plugin bundle.

The returned directory is private and isolated. This module never extracts an
archive, executes the Agent, loads a Docker image, or changes an active bundle.
"""
from dataclasses import dataclass
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import stat
import tempfile
import time
from urllib.parse import urlsplit
from urllib.request import Request, urlopen

import plugin_catalog


DEFAULT_PAGES_BASE_URL = 'https://install.thingspanel.io'
DEFAULT_RELEASE_BASE_URL = 'https://github.com/ThingsPanel/thingspanel-installer/releases/download'
SUPPORTED_PLATFORMS = frozenset(('linux/amd64', 'linux/arm64'))
MAX_CATALOG_BYTES = 1024 * 1024
MAX_SIGNATURE_BYTES = 16 * 1024
MAX_AGENT_BYTES = 64 * 1024 * 1024
MAX_IMAGE_ARCHIVE_BYTES = 12 * 1024 * 1024 * 1024
CHUNK_SIZE = 1024 * 1024
SHA256_RE = re.compile(r'^[0-9a-f]{64}$')


class PluginBundleError(RuntimeError):
    """The optional plugin bundle is absent, malformed, or untrusted."""


@dataclass(frozen=True)
class StagedPluginBundle:
    """Validated files in a new private directory; caller owns cleanup/promotion."""

    stage_dir: Path
    catalog_path: Path
    signature_path: Path
    agent_path: Path
    image_archive_path: Path
    catalog: dict


def _validate_https_base(value, *, release=False, custom=False):
    parsed = urlsplit(value)
    if parsed.scheme != 'https' or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise PluginBundleError('bundle base URL must be an absolute HTTPS URL without credentials, query, or fragment')
    if custom:
        # Operator-chosen mirror/beta source (R9). Integrity does not depend on the host: the catalog must verify against the
        # pinned plugin trust root and every asset digest comes from that signed catalog.
        return value.rstrip('/')
    if release:
        normalized = value.rstrip('/')
        if normalized != DEFAULT_RELEASE_BASE_URL:
            raise PluginBundleError('release base URL must be the configured ThingsPanel GitHub Releases base')
    elif value.rstrip('/') != DEFAULT_PAGES_BASE_URL:
        raise PluginBundleError('catalog base URL must be the configured ThingsPanel Pages site')
    return value.rstrip('/')


def _safe_stage_parent(path):
    path = Path(path)
    try:
        info = path.lstat()
    except OSError as error:
        raise PluginBundleError('plugin stage parent is unavailable') from error
    if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode):
        raise PluginBundleError('plugin stage parent must be a real directory')
    return path.resolve()


def _download_stream(url, destination, max_bytes, *, timeout=90):
    """Stream a URL to a new mode-0600 file, enforcing the byte limit while reading."""
    destination = Path(destination)
    request = Request(url, headers={'User-Agent': 'ThingsPanel-Plugin-Bundle/1'})
    try:
        with urlopen(request, timeout=timeout) as response:
            final = urlsplit(response.geturl())
            source = urlsplit(url)
            allowed_hosts = {source.hostname}
            if source.hostname == 'github.com':
                allowed_hosts.update(('release-assets.githubusercontent.com', 'objects.githubusercontent.com'))
            if final.scheme != 'https' or final.hostname not in allowed_hosts:
                raise PluginBundleError('bundle asset redirected outside its trusted HTTPS host')
            length = response.headers.get('Content-Length')
            if length is not None:
                try:
                    announced = int(length)
                except ValueError:
                    raise PluginBundleError('bundle asset has an invalid Content-Length') from None
                if announced < 0 or announced > max_bytes:
                    raise PluginBundleError('bundle asset exceeds its configured size limit')
            fd = os.open(str(destination), os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, 'O_NOFOLLOW', 0), 0o600)
            count = 0
            try:
                with os.fdopen(fd, 'wb') as output:
                    while True:
                        block = response.read(CHUNK_SIZE)
                        if not block:
                            break
                        count += len(block)
                        if count > max_bytes:
                            raise PluginBundleError('bundle asset exceeds its configured size limit')
                        output.write(block)
                    output.flush()
                    os.fsync(output.fileno())
            except BaseException:
                destination.unlink(missing_ok=True)
                raise
            if count == 0:
                destination.unlink(missing_ok=True)
                raise PluginBundleError('bundle asset is empty')
    except PluginBundleError:
        raise
    except Exception as error:  # noqa: BLE001 - URL/client errors are an optional bundle failure.
        destination.unlink(missing_ok=True)
        raise PluginBundleError('bundle asset download failed') from error


def _download(fetcher, url, destination, max_bytes):
    """Call a testable downloader and enforce a postcondition on its output."""
    fetcher(url, destination, max_bytes)
    path = Path(destination)
    try:
        info = path.lstat()
    except OSError:
        raise PluginBundleError('bundle downloader did not create an asset') from None
    if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode):
        raise PluginBundleError('bundle downloader returned a non-regular asset')
    if info.st_size <= 0:
        raise PluginBundleError('bundle asset is empty')
    if info.st_size > max_bytes:
        raise PluginBundleError('bundle asset exceeds its configured size limit')
    os.chmod(path, 0o600)


def _sha256(path):
    digest = hashlib.sha256()
    with Path(path).open('rb') as stream:
        for block in iter(lambda: stream.read(CHUNK_SIZE), b''):
            digest.update(block)
    return digest.hexdigest()


def _valid_digest(value):
    return isinstance(value, str) and SHA256_RE.fullmatch(value) is not None


def _validate_catalog(catalog, target_platform):
    if catalog.get('target_platform') != target_platform:
        raise PluginBundleError('plugin catalog target platform does not match Docker')
    try:
        bundle = plugin_catalog.validate_bundle_metadata(
            catalog.get('target_platform'), catalog.get('release_tag'), catalog.get('agent_sha256'))
    except (TypeError, ValueError) as error:
        raise PluginBundleError('plugin catalog bundle metadata is invalid') from error
    if bundle is None or not _valid_digest(catalog.get('agent_sha256')):
        raise PluginBundleError('plugin catalog Agent metadata is incomplete or invalid')
    entries = catalog.get('plugins')
    if not isinstance(entries, list) or not entries:
        raise PluginBundleError('plugin catalog has no plugin entries')
    ids = [entry.get('id') for entry in entries if isinstance(entry, dict)]
    if len(ids) != len(entries) or len(ids) != len(set(ids)):
        raise PluginBundleError('plugin catalog plugin IDs are malformed or duplicated')
    ha = [entry for entry in entries if entry.get('id') == 'homeassistant']
    if len(ha) != 1:
        raise PluginBundleError('plugin catalog must contain exactly one Home Assistant entry')
    entry = ha[0]
    if entry.get('image_platform') != target_platform:
        raise PluginBundleError('Home Assistant image platform does not match Docker')
    if not _valid_digest(entry.get('image_archive_sha256')):
        raise PluginBundleError('Home Assistant image archive digest is invalid')
    image_id = entry.get('image_id')
    if not isinstance(image_id, str) or re.fullmatch(r'sha256:[0-9a-f]{64}', image_id) is None:
        raise PluginBundleError('Home Assistant image ID is invalid')
    return entry


def stage_plugin_bundle(target_platform, *, trust_root, stage_parent,
                        pages_base_url=DEFAULT_PAGES_BASE_URL,
                        release_base_url=DEFAULT_RELEASE_BASE_URL,
                        custom_bases=False,
                        downloader=None, catalog_loader=None,
                        max_agent_bytes=MAX_AGENT_BYTES,
                        max_image_archive_bytes=MAX_IMAGE_ARCHIVE_BYTES):
    """Verify and download a per-platform bundle into a new private stage.

    ``downloader(url, destination, max_bytes)`` and ``catalog_loader(path,
    trust_root)`` are injectable for deterministic tests. The default loader is
    the existing ``plugin_catalog.load_verified`` implementation.
    """
    if target_platform not in SUPPORTED_PLATFORMS:
        raise PluginBundleError('unsupported Docker target platform')
    for limit in (max_agent_bytes, max_image_archive_bytes):
        if type(limit) is not int or limit <= 0:
            raise PluginBundleError('bundle size limits must be positive integers')
    trust_root = Path(trust_root)
    if not trust_root.is_absolute() or not trust_root.is_file():
        raise PluginBundleError('plugin trust root must be an available absolute package-external file')
    pages_base_url = _validate_https_base(pages_base_url, custom=custom_bases)
    release_base_url = _validate_https_base(release_base_url, release=True, custom=custom_bases)
    parent = _safe_stage_parent(stage_parent)
    trust_root = trust_root.resolve()
    try:
        trust_root.relative_to(parent)
    except ValueError:
        pass
    else:
        raise PluginBundleError('plugin trust root must be outside the plugin stage parent')
    fetcher = downloader or _download_stream
    loader = catalog_loader or plugin_catalog.load_verified
    arch = target_platform.split('/', 1)[1]
    stage = Path(tempfile.mkdtemp(prefix='tp-plugin-bundle-', dir=str(parent)))
    os.chmod(stage, 0o700)
    try:
        catalog_path = stage / ('catalog-linux-%s.json' % arch)
        signature_path = Path(str(catalog_path) + '.sig')
        _download(fetcher, pages_base_url + '/plugins/catalog-linux-%s.json' % arch,
                  catalog_path, MAX_CATALOG_BYTES)
        _download(fetcher, pages_base_url + '/plugins/catalog-linux-%s.json.sig' % arch,
                  signature_path, MAX_SIGNATURE_BYTES)
        try:
            catalog = loader(catalog_path, trust_root)
        except Exception as error:  # noqa: BLE001 - verifier errors must remain fail-closed.
            raise PluginBundleError('plugin catalog signature or schema is invalid') from error
        image_entry = _validate_catalog(catalog, target_platform)
        tag = catalog['release_tag']
        agent_url = '%s/%s/tp-agent-linux-%s' % (release_base_url, tag, arch)
        image_url = '%s/%s/ha-image-linux-%s.tar' % (release_base_url, tag, arch)
        agent_path = stage / ('tp-agent-linux-%s' % arch)
        image_path = stage / ('ha-image-linux-%s.tar' % arch)
        _download(fetcher, agent_url, agent_path, max_agent_bytes)
        if _sha256(agent_path) != catalog['agent_sha256']:
            raise PluginBundleError('Agent binary digest does not match the signed catalog')
        _download(fetcher, image_url, image_path, max_image_archive_bytes)
        if _sha256(image_path) != image_entry['image_archive_sha256']:
            raise PluginBundleError('Home Assistant image archive digest does not match the signed catalog')
        return StagedPluginBundle(stage, catalog_path, signature_path, agent_path, image_path, catalog)
    except BaseException:
        shutil.rmtree(stage, ignore_errors=True)
        raise


def arrange_staged_bundle(staged):
    """Arrange a newly verified stage for the existing Agent host helpers."""
    root = Path(staged.stage_dir)
    if root.is_symlink() or not root.is_dir():
        raise PluginBundleError('plugin stage is not a real directory')
    for name in ('bin', 'plugins'):
        (root / name).mkdir(mode=0o700)
    for source, relative in (
        (staged.catalog_path, 'plugins/plugin-catalog.json'),
        (staged.signature_path, 'plugins/plugin-catalog.json.sig'),
        (staged.agent_path, 'bin/tp-agent'),
        (staged.image_archive_path, 'plugins/homeassistant-image.tar'),
    ):
        source = Path(source)
        if source.parent != root or source.is_symlink() or not source.is_file():
            raise PluginBundleError('plugin stage contains an invalid asset')
        os.replace(source, root / relative)
    os.chmod(root / 'bin/tp-agent', 0o700)
    return root


def verify_active_bundle(root, trust_root, target_platform, *, require_unexpired=False):
    """Recheck an already activated bundle, including an expired catalog's signature.

    This is only for restoring an existing Agent. The Agent itself continues to
    reject expired catalogs for new installations and preparations.
    """
    root = Path(root)
    for directory in (root, root / 'bin', root / 'plugins'):
        if directory.is_symlink() or not directory.is_dir():
            raise PluginBundleError('active plugin bundle is not a real directory')
    catalog_path = root / 'plugins/plugin-catalog.json'
    signature_path = root / 'plugins/plugin-catalog.json.sig'
    agent_path = root / 'bin/tp-agent'
    image_path = root / 'plugins/homeassistant-image.tar'
    for path, maximum in ((catalog_path, MAX_CATALOG_BYTES), (signature_path, MAX_SIGNATURE_BYTES),
                          (agent_path, MAX_AGENT_BYTES), (image_path, MAX_IMAGE_ARCHIVE_BYTES)):
        try:
            info = path.lstat()
        except OSError:
            raise PluginBundleError('active plugin bundle is incomplete') from None
        if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode) or info.st_size <= 0 or info.st_size > maximum:
            raise PluginBundleError('active plugin bundle contains an invalid asset')
    try:
        raw = catalog_path.read_bytes()
        verifier = plugin_catalog._verifier()
        verifier.verify_signature(raw, signature_path.read_bytes(), verifier.load_trust_root(trust_root))
        catalog = json.loads(raw)
        if catalog.get('schema') != plugin_catalog.SCHEMA or type(catalog.get('expires_at')) is not int:
            raise ValueError('invalid catalog')
        for entry in catalog['plugins']:
            plugin_catalog.check_entry(entry)
        plugin_catalog.validate_catalog_bundle(catalog)
        image_entry = _validate_catalog(catalog, target_platform)
    except Exception as error:  # noqa: BLE001 - all verifier errors fail closed.
        raise PluginBundleError('active plugin catalog signature or schema is invalid') from error
    if require_unexpired and int(time.time()) >= catalog['expires_at']:
        raise PluginBundleError('pending catalog expired before activation')
    if _sha256(agent_path) != catalog['agent_sha256'] or _sha256(image_path) != image_entry['image_archive_sha256']:
        raise PluginBundleError('active plugin bundle digest mismatch')
    return catalog

