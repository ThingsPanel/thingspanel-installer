#!/usr/bin/env python3
"""Keep the Broker device scope in step with the platform's devices, with no manual command.

A plugin (service access) device that the user creates or deletes in the platform UI only works once the Broker
knows about it: the owner's `services.json` must list it, and broker-owner, then mosquitto, must be restarted to
load it. This tool does that:

    scope_sync.py once   --stack-dir DIR              one pass: refresh, restart only if the scope changed
    scope_sync.py watch  --stack-dir DIR [--interval 20]   loop of `once` (foreground)
    scope_sync.py ensure --stack-dir DIR --state-dir DIR   start a detached `watch` unless one is already running

`plugin_install.py install` and `renew` call `ensure`, so installing a service plugin starts the watcher and the
renew cron job revives it after a reboot. The scope itself is computed by `config/native/prepare.py --sync-scope`
(the same code `--refresh-devices` uses at install/upgrade), so there is exactly one definition of the scope.
No credentials are read, written or logged here.
"""
import argparse
import os
from pathlib import Path
import signal
import subprocess
import sys
import time

DEFAULT_INTERVAL = 20


def run_once(stack_dir, runner=subprocess.run, python=sys.executable):
    """One refresh pass. Returns (ok, output). Never raises: a pass that cannot run (an install holds the update
    lock, Docker is restarting) is simply tried again at the next interval."""
    try:
        result = runner([python, 'config/native/prepare.py', '--sync-scope'], cwd=str(stack_dir),
                        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=300)
    except (OSError, subprocess.SubprocessError) as error:
        return False, 'could not run prepare.py: %s' % type(error).__name__
    output = (result.stdout or b'').decode(errors='replace').strip()
    return result.returncode == 0, output[-400:]


def watch(stack_dir, interval=DEFAULT_INTERVAL, runner=subprocess.run, sleep=time.sleep, rounds=None, log=print):
    """Loop of run_once. Logs only on a change or a change of health, so the log does not grow with time."""
    last = None
    count = 0
    while rounds is None or count < rounds:
        ok, output = run_once(stack_dir, runner)
        if (ok, output) != last:
            log('%s %s %s' % (time.strftime('%Y-%m-%dT%H:%M:%S'), 'ok' if ok else 'FAILED', output.replace('\n', ' | ')))
        last = (ok, output)
        count += 1
        if rounds is None or count < rounds:
            sleep(interval)


def pid_file(state_dir):
    return Path(state_dir) / 'scope-sync.pid'


def is_watcher(pid, ps=None):
    """True when `pid` is a live scope_sync watcher (not a recycled pid)."""
    ps = ps or (lambda p: subprocess.run(['ps', '-p', str(p), '-o', 'command='], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL).stdout.decode())
    try:
        command = ps(pid)
    except OSError:
        return False
    return 'scope_sync.py' in command and ' watch' in command


def ensure(stack_dir, state_dir, interval=DEFAULT_INTERVAL, spawn=subprocess.Popen, ps=None):
    """Start a detached watcher unless one is already running for this state dir. Returns 'running' or 'started'."""
    state_dir = Path(state_dir)
    state_dir.mkdir(parents=True, exist_ok=True)
    pidfile = pid_file(state_dir)
    try:
        pid = int(pidfile.read_text().strip())
    except (OSError, ValueError):
        pid = None
    if pid and is_watcher(pid, ps):
        return 'running'
    log = open(state_dir / 'scope-sync.log', 'ab')
    process = spawn([sys.executable, str(Path(__file__).resolve()), 'watch', '--stack-dir', str(stack_dir), '--interval', str(interval)],
                    stdin=subprocess.DEVNULL, stdout=log, stderr=log, start_new_session=True, close_fds=True)
    pidfile.write_text(str(process.pid))
    return 'started'


def stop(state_dir):
    try:
        pid = int(pid_file(state_dir).read_text().strip())
    except (OSError, ValueError):
        return False
    if is_watcher(pid):
        os.kill(pid, signal.SIGTERM)
        return True
    return False


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('command', choices=['once', 'watch', 'ensure', 'stop'])
    parser.add_argument('--stack-dir')
    parser.add_argument('--state-dir', help='where the pid file and log live (ensure/stop)')
    parser.add_argument('--interval', type=int, default=DEFAULT_INTERVAL)
    args = parser.parse_args()
    if args.interval < 5:
        parser.error('--interval must be at least 5 seconds')
    if args.command == 'stop':
        print('stopped' if stop(args.state_dir) else 'not running')
        return 0
    if not args.stack_dir:
        parser.error('--stack-dir is required')
    if args.command == 'once':
        ok, output = run_once(args.stack_dir)
        print(output)
        return 0 if ok else 1
    if args.command == 'watch':
        watch(args.stack_dir, args.interval, log=lambda line: print(line, flush=True))
        return 0
    if not args.state_dir:
        parser.error('--state-dir is required for ensure')
    print(ensure(args.stack_dir, args.state_dir, args.interval))
    return 0


if __name__ == '__main__':
    sys.exit(main())
