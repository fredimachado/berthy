#!/bin/bash
# 74-stats-watch proves that berthy stats --watch puts exactly one blank line between successive
# human samples. It reads the live stream until the second top-level time: line, then checks that
# the first time: line and the next containers: line are separated by one empty line.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"

python3 - "$BIN" "$RUN" "$PROJECT" "$ENVN" <<'PY'
import os
import select
import signal
import subprocess
import sys
import time

binary, run, project, environment = sys.argv[1:]
config = os.path.join(run, 'admin.json')
watchdog = 120
err_path = os.path.join(run, 'stats-watch.err')

child = None
try:
    with open(err_path, 'wb') as stderr:
        child = subprocess.Popen(
            [binary, 'stats', '-e', environment, '--project', project, '--config', config, '--watch'],
            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=stderr,
            env={**os.environ, 'NO_COLOR': '1', 'NO_PROXY': '127.0.0.1'},
        )
        deadline = time.monotonic() + watchdog
        buf = b''
        lines = []
        while True:
            assert child.poll() is None, 'CLI exited before the second sample'
            remaining = max(0, deadline - time.monotonic())
            if remaining == 0:
                raise AssertionError('second top-level time: line did not arrive')
            ready, _, _ = select.select([child.stdout], [], [], remaining)
            if not ready:
                continue
            chunk = os.read(child.stdout.fileno(), 65536)
            assert chunk, 'stats stream ended before the second sample'
            buf += chunk
            while b'\n' in buf:
                line, buf = buf.split(b'\n', 1)
                lines.append(line.decode())
            if sum(1 for line in lines if line.startswith('time:')) >= 2:
                break
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
    if child is not None and child.stdout is not None:
        child.stdout.close()
PY
code=$?
expect_code "stats --watch separates human samples" 0 "$code" "$(cat "$RUN/stats-watch.err" 2>/dev/null | head -n 40)"

finish
