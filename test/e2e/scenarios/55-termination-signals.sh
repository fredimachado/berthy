#!/bin/bash
set -uo pipefail
umask 077
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"
unset BERTHY_CONFIG
export HOME="$RUN/signal-home"
mkdir -p "$HOME" || exit 1
record_deploy "$PROJECT" "$ENVN" web --version "$WEB_V1_DIGEST" --manifest-dir "$MAN" || exit 1

python3 - "$BIN" "$RUN" "$PROJECT" "$ENVN" "$AGENT_LISTEN" "$MAN" "$WEB_V1_DIGEST" "$(dirname "$0")/48-progress.py" <<'PY'
import contextlib
import fcntl
import http.client
import http.server
import json
import os
from pathlib import Path
import pty
import re
import select
import shlex
import signal
import struct
import subprocess
import sys
import tempfile
import termios
import threading
import time
import uuid
from urllib.parse import urlsplit

binary, run, project, environment, origin, manifest, digest, progress_driver = sys.argv[1:]
run = Path(run)
prefix = f'/v1/projects/{project}/environments/{environment}/'
token = os.environ['BERTHY_ADMIN_TOKEN']
watchdog = 120


def agent_request(method, path):
    with contextlib.closing(http.client.HTTPConnection(origin, timeout=watchdog)) as conn:
        conn.request(method, path, headers={'Authorization': 'Bearer ' + token})
        response = conn.getresponse()
        return response.status, response.read()


class QuietHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass


@contextlib.contextmanager
def proxy_config(handler, filename):
    with http.server.ThreadingHTTPServer(('127.0.0.1', 0), handler) as server:
        server.daemon_threads = False
        proxy = threading.Thread(target=server.serve_forever)
        proxy.start()
        try:
            config = run / filename
            data = json.loads((run / 'admin.json').read_text())
            data['projects'][project]['environments'][environment]['url'] = f'http://127.0.0.1:{server.server_port}'
            config.write_text(json.dumps(data))
            yield config
        finally:
            server.shutdown()
            proxy.join()


@contextlib.contextmanager
def terminal_child(args, config, use_stdin=False):
    master, slave = pty.openpty()
    try:
        initial = termios.tcgetattr(slave)
        initial[3] |= termios.ECHO
        termios.tcsetattr(slave, termios.TCSANOW, initial)
        initial = termios.tcgetattr(slave)
        with tempfile.TemporaryFile(dir=run) as stdout, subprocess.Popen(
            [binary, *args, '-e', environment, '--project', project, '--config', str(config),
             '--manifest-dir', manifest],
            stdin=slave if use_stdin else subprocess.DEVNULL, stdout=stdout, stderr=slave,
            env={**os.environ, 'TERM': 'xterm', 'NO_COLOR': '1', 'NO_PROXY': '127.0.0.1'},
        ) as child:
            try:
                yield child, master, slave, initial, stdout
            finally:
                if child.poll() is None:
                    child.kill()
                child.wait()
                termios.tcsetattr(slave, termios.TCSANOW, initial)
    finally:
        os.close(slave)
        os.close(master)


def capture_output(master, stdout, stderr=b''):
    # With the child reaped, any PTY output is already queued. Keep the slave
    # open for the termios assertion, so the master will not report EOF.
    while select.select([master], [], [], 0)[0]:
        stderr += os.read(master, 65536)
    stdout.seek(0)
    return stdout.read(), stderr


def hidden_prompt():
    name = re.sub('[^A-Z0-9_]', '_', f'SIGNAL_{project}_{environment}'.upper())

    def secret_absent():
        status, data = agent_request('GET', prefix + 'secrets')
        assert status == 200, f'secret inventory returned HTTP {status}'
        rows = json.loads(data)
        assert isinstance(rows, list) and all(isinstance(row.get('name'), str) for row in rows), 'invalid secret inventory'
        return all(row['name'] != name for row in rows)

    assert secret_absent(), 'fixture secret already exists'
    with terminal_child(['secret', 'set', name], run / 'admin.json', use_stdin=True) as (
        child, master, slave, initial, stdout,
    ):
        prompt = f'Value of {name} (hidden): '.encode()
        stderr = b''
        deadline = time.monotonic() + watchdog
        while prompt not in stderr:
            assert child.poll() is None, 'CLI exited before the hidden prompt'
            if select.select([master], [], [], max(0, deadline - time.monotonic()))[0]:
                stderr += os.read(master, 65536)
            assert time.monotonic() < deadline, 'hidden prompt did not appear'
        # The prompt is written before the reader clears ECHO.
        while termios.tcgetattr(slave)[3] & termios.ECHO:
            assert child.poll() is None, 'CLI exited before turning echo off'
            assert time.monotonic() < deadline, 'hidden prompt did not turn echo off'
            os.sched_yield()
        child.send_signal(signal.SIGTERM)
        try:
            code = child.wait(timeout=watchdog)
        except subprocess.TimeoutExpired:
            code = None
        final = termios.tcgetattr(slave)
        if code is not None:
            captured_stdout, captured_stderr = capture_output(master, stdout, stderr)
    absent = secret_absent()
    restored = initial == final
    echo = bool(final[3] & termios.ECHO)
    assert code is not None, f'CLI stayed blocked after SIGTERM, echo enabled={echo}, secret absent={absent}'
    assert code == 143 and restored and absent, (
        f'native exit={code}, echo enabled={echo}, full PTY mode restored={restored}, secret absent={absent}'
    )


def watched_deploy():
    request_id = f'{project}-{environment}-signal-{uuid.uuid4().hex}'
    arrived, release = threading.Event(), threading.Event()
    operation, events = None, None
    cancel_posts = []

    class Handler(QuietHandler):
        def do_GET(self):
            if urlsplit(self.path).path == events:
                arrived.set()
                release.wait()
                return
            self.forward()

        def do_POST(self):
            if urlsplit(self.path).path.endswith('/cancel'):
                cancel_posts.append(self.path)
            self.forward()

        def forward(self):
            nonlocal operation, events
            headers = {k: v for k, v in self.headers.items()
                       if k.lower() not in ('host', 'connection', 'accept-encoding')}
            length = int(self.headers.get('Content-Length', '0'))
            body = self.rfile.read(length) if length else None
            with contextlib.closing(http.client.HTTPConnection(origin, timeout=watchdog)) as conn:
                conn.request(self.command, self.path, body=body, headers=headers)
                response = conn.getresponse()
                data = response.read()
                status = response.status
            if self.command == 'POST' and status == 202 and body and json.loads(body).get('request_id') == request_id:
                accepted = json.loads(data)
                candidate = accepted['operation_id']
                assert re.fullmatch('[0-9a-f]{32}', candidate), 'invalid admitted operation ID'
                assert accepted['events_url'] == prefix + f'operations/{candidate}/events', 'invalid events path'
                operation, events = candidate, accepted['events_url']
            try:
                self.send_response(status)
                self.send_header('Content-Length', str(len(data)))
                self.end_headers()
                self.wfile.write(data)
            except (BrokenPipeError, ConnectionResetError):
                pass

    def running_owned_operation():
        return subprocess.check_output(
            ['python3', progress_driver, '--owned-operation', str(run / 'state/state.db'), environment, request_id],
            text=True,
        ).strip()

    try:
        with proxy_config(Handler, 'signal proxy.json') as config:
            try:
                with terminal_child(['deploy', 'web', '--version', digest, '--request-id', request_id], config) as (
                    child, master, slave, initial, stdout,
                ):
                    assert arrived.wait(watchdog), 'follower never requested the admitted events URL'
                    child.send_signal(signal.SIGTERM)
                    code = child.wait(timeout=watchdog)
                    captured_stdout, stderr = capture_output(master, stdout)
            finally:
                release.set()
    finally:
        owned = operation or running_owned_operation()
        if owned:
            try:
                status, stream = agent_request('GET', prefix + f'operations/{owned}/events')
                assert status == 200 and b'\nevent: terminal\n' in stream, 'owned deploy did not reach a terminal event'
                status, body = agent_request('GET', prefix + f'operations/{owned}')
                assert status == 200 and json.loads(body)['state'] == 'succeeded', 'owned deploy did not succeed'
                print('  owned deploy reached succeeded through the real agent', flush=True)
            finally:
                running = running_owned_operation()
                if running:
                    status, _ = agent_request('POST', prefix + f'operations/{running}/cancel')
                    assert status == 200, f'cleanup cancellation returned HTTP {status}'

    suffix = f'{operation} -e {environment} --project {project} --config {shlex.quote(str(config))}'
    expected = [
        f'watch interrupted; operation {operation} may continue',
        f'inspect: berthy diagnostics {suffix}',
        f'cancel: berthy cancel {suffix}',
    ]
    lines = re.sub(rb'\x1b\[[0-9;]*[A-Za-z]', b'', stderr).decode(errors='replace').splitlines()
    ordered_hints = any(lines[i:i + len(expected)] == expected for i in range(len(lines)))
    assert code == 143 and ordered_hints and not cancel_posts, (
        f'native exit={code}, ordered stderr hints={ordered_hints}, cancel POSTs={len(cancel_posts)}'
    )


def piped_secret(command, sig):
    name = re.sub('[^A-Z0-9_]', '_', f'PIPE_{project}_{environment}'.upper())
    value = b'fixture-pipe-value'
    args = ['secret', command]
    data = value + b'\n'
    if command == 'set':
        args.append(name)
    else:
        data = name.encode() + b'=' + data
    posts = []

    class Handler(QuietHandler):
        def do_POST(self):
            posts.append(self.path)
            self.send_error(500)

    def queued(read_end):
        return struct.unpack('i', fcntl.ioctl(read_end, termios.FIONREAD, bytes(4)))[0]

    with proxy_config(Handler, 'signal pipe.json') as config:
        read_end, writer = os.pipe()
        child = None
        try:
            assert os.write(writer, data) == len(data) and queued(read_end) == len(data), 'pipe was not preloaded'
            child = subprocess.Popen(
                [binary, *args, '-e', environment, '--project', project, '--config', str(config)],
                cwd=manifest, stdin=read_end, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                env={**os.environ, 'NO_PROXY': '127.0.0.1'},
            )
            # This descriptor duplicates the child's stdin. The open writer
            # keeps the CLI waiting for EOF after it drains the queued bytes.
            deadline = time.monotonic() + watchdog
            while queued(read_end):
                assert child.poll() is None, 'CLI exited before draining the pipe'
                assert time.monotonic() < deadline, 'CLI did not read the pipe'
                os.sched_yield()
            child.send_signal(sig)
            out, err = child.communicate(timeout=watchdog)
            code = child.returncode
        finally:
            if child is not None and child.poll() is None:
                child.kill()
            os.close(writer)
            os.close(read_end)
            if child is not None:
                child.communicate()
    printed = value in out or value in err
    assert code == 128 + sig and not posts and not printed, (
        f'native exit={code}, secret POSTs={len(posts)}, input printed={printed}'
    )


failed = False
for label, case in [
    ('hidden prompt', hidden_prompt),
    ('deploy watcher', watched_deploy),
    ('piped secret set (SIGTERM)', lambda: piped_secret('set', signal.SIGTERM)),
    ('piped secret import (SIGHUP)', lambda: piped_secret('import', signal.SIGHUP)),
]:
    try:
        case()
    except AssertionError as error:
        print(f'  FAIL  {label}: {error}', flush=True)
        failed = True
    except Exception as error:
        print(f'  FAIL  {label}: fixture error ({type(error).__name__})', flush=True)
        failed = True
    else:
        print(f'  PASS  {label}', flush=True)
sys.exit(int(failed))
PY
