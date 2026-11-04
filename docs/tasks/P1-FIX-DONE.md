# P1 scope retry and concurrency fix

**Branch:** `wp/p1-fix-retry-lock`, created from installer `develop` at `27840cdc19e4b50a01aeef44deaac27d95e93cef`.

**Worktree:** `/Users/junhong/Downloads/code/thingspanel-installer-wp-p1-fix`.

## Changes

- `config/native/prepare.py --sync-scope` now holds the installer's cross-process update lock from database scope query through file update, ordered container restarts, and completion acknowledgement.
- A durable pending-restart record is written before updating `services.json`. If either `broker-owner` or `mosquitto` restart fails or the process stops, the record remains. The next pass retries both restarts even when the config file already matches the current database scope.
- `tools/scope_sync.py ensure` uses that same lock around PID validation, watcher creation, and atomic PID write, preventing concurrent installers/renewals from starting duplicate watchers. If an install owns the lock, it reports `busy`; a later install or renewal can start the watcher.
- Added regressions for restart failure and retry, overlapping scope runs, and concurrent watcher startup.

## Validation

`PYTHONPATH=tests python3 -m unittest tests.test_update_transaction tests.test_prepare_auth_secret tools.test_plugin_install tests.test_p1_auto_steps`

Result: 91 tests passed.

## Known migration limitation

P1's `bootstrap()` still intentionally skips `33.sql` when the selected backend image does not contain it, to preserve old-image compatibility. The installer does not currently distinguish a supported old image from a newer image that was accidentally built without `33.sql`; therefore a missing required migration can still go unnoticed. This task leaves that policy unchanged as requested.
