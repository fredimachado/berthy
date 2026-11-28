#!/bin/bash
# Idle keep-alives expire while active log and interactive streams remain usable.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
base_manifest | write_manifest
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy web" 0 "$code" "$out"
if [ "$code" -ne 0 ]; then finish; fi
container=$(d ps -q --filter status=running --filter "label=com.docker.swarm.service.name=$PROJECT-$ENVN-web-main" | head -n 1)
if [ -z "$container" ]; then fail "find web container" "no running task"; finish; fi

if python3 - "$BIN" "$ENVN" "$RUN/admin.json" "$AGENT_LISTEN" "$DOCKER_BIN" "$SOCK" "$E2E_ROOT/docker-config" "$container" <<'PY'
import http.client
import os
import selectors
import signal
import socket
import subprocess
import sys
import time

binary, env, config, address, docker, sock, docker_config, container = sys.argv[1:]
common = ['-e', env, '--config', config]
processes = []
selector = selectors.DefaultSelector()
buffers = {}

def start(args, stdin=None):
    p = subprocess.Popen(args, stdin=stdin, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, bufsize=0)
    processes.append(p)
    selector.register(p.stdout, selectors.EVENT_READ, p)
    buffers[p] = b''
    return p

def observe(p, marker, timeout=60):
    end = time.monotonic() + timeout
    while marker not in buffers[p]:
        remaining = end - time.monotonic()
        if remaining <= 0:
            raise AssertionError(f'missing {marker!r}: {buffers[p]!r}')
        events = selector.select(remaining)
        if not events:
            raise AssertionError(f'missing {marker!r}: {buffers[p]!r}')
        for key, _ in events:
            data = os.read(key.fd, 65536)
            if not data:
                raise AssertionError(f'stream ended: {buffers[key.data]!r}')
            buffers[key.data] += data

def log(marker):
    environ = dict(os.environ, DOCKER_CONFIG=docker_config)
    subprocess.run([docker, '--host', 'unix://' + sock, 'exec', '-i', container,
                    'sh', '-c', 'cat > /proc/1/fd/1'], input=marker + b'\n', env=environ, check=True)

def interrupted(signum, frame):
    raise SystemExit(f'interrupted by signal {signum}')

signal.signal(signal.SIGTERM, interrupted)
signal.signal(signal.SIGHUP, interrupted)

try:
    session = start([binary, 'exec', *common, 'web', '-i', '--', 'sh', '-c',
                     'echo session-ready; read line; echo "session-$line"; exit 7'], subprocess.PIPE)
    observe(session, b'session-ready')
    follow = start([binary, 'logs', *common, 'web', '--follow', '--tail', '0'])
    # A marker on the response proves the original follow request is established.
    # Repeat the write until the subscription has reached Docker.
    end = time.monotonic() + 60
    while b'first-marker-76' not in buffers[follow]:
        log(b'first-marker-76')
        try:
            observe(follow, b'first-marker-76', timeout=1)
        except AssertionError:
            if follow.poll() is not None or time.monotonic() >= end:
                raise
    host, port = address.rsplit(':', 1)
    conn = http.client.HTTPConnection(host, int(port), timeout=180)
    conn.request('GET', '/healthz')
    response = conn.getresponse()
    assert response.status == 200, response.status
    response.read()
    started = time.monotonic()
    try:
        data = conn.sock.recv(1)
    except socket.timeout as exc:
        raise AssertionError('idle connection did not receive EOF within 180 seconds') from exc
    elapsed = time.monotonic() - started
    assert data == b'', f'unexpected idle bytes: {data!r}'
    assert elapsed >= 88, f'connection closed early after {elapsed:.3f}s'
    conn.close()
    session.stdin.write(b'after-idle\n')
    session.stdin.flush()
    observe(session, b'session-after-idle')
    session.stdin.close()
    selector.unregister(session.stdout)
    assert session.wait(timeout=60) == 7, 'session exit status changed'
    log(b'second-marker-76')
    observe(follow, b'second-marker-76')
    print(f'idle EOF after {elapsed:.3f}s; original session and log stream survived')
finally:
    selector.close()
    for p in processes:
        if p.stdin and not p.stdin.closed:
            p.stdin.close()
        if p.poll() is None:
            p.terminate()
    for p in processes:
        try:
            p.wait(timeout=10)
        except subprocess.TimeoutExpired:
            p.kill()
            p.wait()
        p.stdout.close()
PY
then pass "idle expiry preserves active streams"; else fail "idle expiry preserves active streams" "see scenario output"; fi
finish
