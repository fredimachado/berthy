#!/bin/bash
# 74-stats-watch proves that berthy stats --watch puts exactly one blank line between successive
# human samples. It reads the live stream until the second top-level time: line, then checks that
# the first time: line and the next containers: line are separated by one empty line.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"

python3 - "$BIN" "$RUN" "$PROJECT" "$ENVN" <<'PY'
import os
import signal
import subprocess
import sys
import time

binary, run, project, environment = sys.argv[1:]
config = os.path.join(run, 'admin.json')
watchdog = 120
out_path = os.path.join(run, 'stats-watch.out')
err_path = os.path.join(run, 'stats-watch.err')

child = None
try:
    with open(out_path, 'wb') as stdout, open(err_path, 'wb') as stderr:
        child = subprocess.Popen(
            [binary, 'stats', '-e', environment, '--project', project, '--config', config, '--watch'],
            stdin=subprocess.DEVNULL, stdout=stdout, stderr=stderr,
            env={**os.environ, 'NO_COLOR': '1', 'NO_PROXY': '127.0.0.1'},
        )
        deadline = time.monotonic() + watchdog
        while time.monotonic() < deadline:
            assert child.poll() is None, 'CLI exited before the second sample'
            with open(out_path, 'rb') as current:
                text = current.read().decode()
            top_time = [line for line in text.splitlines() if line.startswith('time:')]
            if len(top_time) >= 2:
                break
            time.sleep(0.2)
        else:
            raise AssertionError('second top-level time: line did not arrive')
    with open(out_path, 'rb') as current:
        text = current.read().decode()
    lines = text.splitlines()
    first_time = next(i for i, line in enumerate(lines) if line.startswith('time:'))
    next_containers = next(
        i for i, line in enumerate(lines[first_time + 1 :], first_time + 1)
        if line.startswith('containers:')
    )
    between = lines[first_time + 1:next_containers]
    assert between == [''], (
        f'want one blank line between samples, got {between!r} around {lines[first_time:next_containers + 1]!r}'
    )
finally:
    if child is not None and child.poll() is None:
        child.send_signal(signal.SIGTERM)
        try:
            child.wait(timeout=watchdog)
        except subprocess.TimeoutExpired:
            child.kill()
            child.wait()
PY
code=$?
expect_code "stats --watch separates human samples" 0 "$code" "$(cat "$RUN/stats-watch.err" 2>/dev/null; cat "$RUN/stats-watch.out" 2>/dev/null | head -n 40)"

finish
