#!/usr/bin/env python3
"""Scenario 48's gated progress fixtures; raw output stays in memory."""
import errno
import fcntl
import http.client
import http.server
import json
import os
import pty
import queue
import re
import selectors
import secrets
import shlex
import shutil
import signal
import socketserver
import sqlite3
import struct
import subprocess
import sys
import termios
import threading
import time
import urllib.request


def stop_on_signal(signum, frame):
    raise InterruptedError('fixture stopped')


def driver_error(result):
    message = result.stderr.decode('utf-8', 'replace')
    for name, value in os.environ.items():
        if name.startswith('BERTHY_') and 'TOKEN' in name and name != 'BERTHY_TOKEN_STORE' and value:
            message = message.replace(value, '[redacted]')
    return message.strip()


def retry_wrapper(args):
    binary, environment, project, config, manifest, digest, docker, socket, docker_config, state_db = args
    request_id = 's8-' + secrets.token_hex(16)
    attrs = termios.tcgetattr(0)
    docker_env = dict(os.environ, DOCKER_CONFIG=docker_config)
    docker_cmd = [docker, '--host', 'unix://' + socket]
    events = subprocess.Popen(docker_cmd + ['events', '--since', str(int(time.time())),
        '--filter', 'type=container', '--filter', 'event=start',
        '--filter', 'label=berthy.project=' + project,
        '--filter', 'label=berthy.environment=' + environment,
        '--filter', 'label=berthy.oneoff=pre_hook', '--format', '{{json .Actor}}'],
        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, env=docker_env, start_new_session=True)
    event_queue = queue.Queue()

    def read_events():
        for line in events.stdout:
            try:
                event_queue.put(json.loads(line))
            except ValueError:
                event_queue.put(None)

    threading.Thread(target=read_events, daemon=True).start()
    quiet = termios.tcgetattr(0)
    quiet[3] &= ~termios.ECHO
    try:
        termios.tcsetattr(0, termios.TCSANOW, quiet)
        child = subprocess.Popen([binary, 'deploy', 'web', '-e', environment, '--project', project,
                                  '--config', config, '--manifest-dir', manifest, '--version', digest,
                                  '--request-id', request_id],
                                 stdin=subprocess.DEVNULL)
    except BaseException:
        termios.tcsetattr(0, termios.TCSANOW, attrs)
        if events.poll() is None:
            events.terminate()
        events.wait(timeout=10)
        raise
    operation = None
    containers = []
    released = set()
    old_handlers = {sig: signal.signal(sig, stop_on_signal) for sig in (signal.SIGTERM, signal.SIGINT)}

    def attempt():
        nonlocal operation
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            try:
                actor = event_queue.get(timeout=max(0.01, deadline - time.monotonic()))
            except queue.Empty:
                break
            assert isinstance(actor, dict), 'invalid Docker event'
            labels = actor.get('Attributes', {})
            candidate = labels.get('berthy.operation', '')
            container = actor.get('ID', '')
            if not re.fullmatch(r'[0-9a-f]{32}', candidate) or not re.fullmatch(r'[0-9a-f]{64}', container):
                continue
            if operation is None:
                status = subprocess.run([binary, 'lock', 'status', '-e', environment, '--project', project,
                                         '--config', config, '--json'], stdout=subprocess.PIPE,
                                        stderr=subprocess.DEVNULL, timeout=30)
                assert status.returncode == 0, 'fixture operation lock unavailable'
                if json.loads(status.stdout).get('operation_id') != candidate:
                    continue
                with sqlite3.connect('file:' + state_db + '?mode=ro', uri=True) as database:
                    row = database.execute('SELECT request_id FROM operations WHERE environment=? AND id=?',
                                           (environment, candidate)).fetchone()
                if row != (request_id,):
                    continue
                operation = candidate
            if candidate != operation or container in containers:
                continue
            containers.append(container)
            logs = subprocess.Popen(docker_cmd + ['logs', '--follow', container], stdout=subprocess.PIPE,
                                    stderr=subprocess.DEVNULL, env=docker_env)
            ready = selectors.DefaultSelector()
            ready.register(logs.stdout, selectors.EVENT_READ)
            try:
                assert ready.select(90), 'fixture hook readiness timed out'
                assert logs.stdout.readline().strip() == b'fixture-fifo-ready', 'fixture FIFO not ready'
            finally:
                if logs.poll() is None:
                    logs.terminate()
                logs.wait(timeout=10)
                ready.close()
            return container
        raise RuntimeError('fixture hook start timed out')

    def release(container, value):
        result = subprocess.run(docker_cmd + ['exec', container, 'sh', '-c',
                                'printf "%s\\n" "$1" > /tmp/berthy-progress', 'sh', value],
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                env=docker_env, timeout=30)
        assert result.returncode == 0, 'fixture FIFO release failed'
        released.add(container)

    try:
        first = attempt()
        assert sys.stdin.readline(), 'first release not requested'
        release(first, '1')
        second = attempt()
        assert first != second, 'retry reused hook container'
        assert sys.stdin.readline(), 'second release not requested'
        release(second, '0')
        code = child.wait(timeout=180)
        assert code == 0, 'fixture deploy failed'
        return 0
    finally:
        try:
            termios.tcsetattr(0, termios.TCSANOW, attrs)
        except termios.error:
            pass
        for container in containers:
            if container in released:
                continue
            try:
                subprocess.run(docker_cmd + ['exec', '--detach', container, 'sh', '-c',
                               'printf "1\\n" > /tmp/berthy-progress'], stdout=subprocess.DEVNULL,
                               stderr=subprocess.DEVNULL, env=docker_env, timeout=10)
            except subprocess.SubprocessError:
                pass
        if child.poll() is None:
            child.terminate()
            child.wait(timeout=10)
        if events.poll() is None:
            events.terminate()
        events.wait(timeout=10)
        if operation and child.returncode != 0:
            try:
                subprocess.run([binary, 'cancel', operation, '-e', environment, '--project', project,
                                '--config', config], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                               timeout=30)
            except subprocess.SubprocessError:
                pass
        for sig, handler in old_handlers.items():
            signal.signal(sig, handler)


if len(sys.argv) > 1 and sys.argv[1] == '--retry-wrapper':
    sys.exit(retry_wrapper(sys.argv[2:]))


if len(sys.argv) > 1 and sys.argv[1] == '--retry-screen':
    driver, run, *args = sys.argv[2:]
    screen_dir = os.path.join(run, 'progress-retry-screen')
    os.mkdir(screen_dir, 0o700)
    steps_path = os.path.join(screen_dir, 'steps.json')
    retry = 'pre-deploy command 1 failed; retrying (attempt 2 of 2)'
    with open(steps_path, 'w') as target:
        json.dump([{'wait_text': 'following deploy (', 'timeout': '100s'},
                   {'snapshot': 'pending'}, {'keys': ['enter']},
                   {'wait_text': retry, 'timeout': '100s'}, {'snapshot': 'retry'},
                   {'keys': ['enter']}, {'wait_text': 'state: succeeded', 'timeout': '180s'},
                   {'snapshot': 'complete'}, {'expect_exit': 0, 'timeout': '100s'}], target)
    wrapper = [sys.executable, os.path.abspath(__file__), '--retry-wrapper'] + args
    try:
        driven = subprocess.run([driver, '--command', ' '.join(shlex.quote(x) for x in wrapper),
                                 '--size', '200x60', '--script', steps_path, '--out', screen_dir,
                                 '--timeout', '360s', '--env', 'TERM=xterm', '--env', 'NO_COLOR='],
                                stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, timeout=380)
        assert driven.returncode == 0, 'retry screen driver failed: ' + driver_error(driven)
        with open(os.path.join(screen_dir, 'pending.txt')) as source:
            pending = source.read()
        with open(os.path.join(screen_dir, 'retry.txt')) as source:
            during = source.read()
        with open(os.path.join(screen_dir, 'complete.txt')) as source:
            complete = source.read()
        with open(os.path.join(screen_dir, 'complete.html')) as source:
            html = source.read()
        assert re.search(r'^[|/\\-] following deploy \(', pending, re.M), 'deploy frame missing'
        assert sum(line.strip() == retry for line in during.splitlines()) == 1, 'retry is not one clean line'
        before_retry = during.split(retry, 1)[0]
        assert not re.search(r'^[|/\\-] following deploy \(', before_retry, re.M), 'frame was not cleared before retry'
        assert sum(line.strip() == retry for line in complete.splitlines()) == 1, 'retry duplicated'
        assert 'state: succeeded' in complete, 'deploy result missing'
        assert not re.search(r'^[|/\\-] following deploy \(', complete, re.M), 'stale deploy frame'
        assert '<span style="color:#008000">succeeded</span>' in html, 'green deploy result missing'
        print('combined retry: clean live step, distinct attempts and final screen')
    finally:
        shutil.rmtree(screen_dir)
    sys.exit(0)


if len(sys.argv) > 1 and sys.argv[1] == '--retry-captures':
    args = sys.argv[2:]
    retry = b'  pre-deploy command 1 failed; retrying (attempt 2 of 2)\n'

    def capture(stdout_tty, stderr_tty):
        control, control_slave = pty.openpty()
        selector = selectors.DefaultSelector()
        owned = []
        targets = []
        for tty in (stdout_tty, stderr_tty):
            if tty:
                master, slave = pty.openpty()
                attrs = termios.tcgetattr(slave)
                attrs[1] &= ~termios.ONLCR
                termios.tcsetattr(slave, termios.TCSANOW, attrs)
                fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 40, 120, 0, 0))
                owned.append((master, slave))
                targets.append(slave)
            else:
                targets.append(subprocess.PIPE)
        env = dict(os.environ, TERM='xterm', NO_COLOR='', BERTHY_TOKEN_STORE='none')
        child = subprocess.Popen([sys.executable, os.path.abspath(__file__), '--retry-wrapper'] + args,
                                 stdin=control_slave, stdout=targets[0], stderr=targets[1],
                                 env=env, start_new_session=True)
        os.close(control_slave)
        streams = []
        next_tty = iter(owned)
        for index, tty in enumerate((stdout_tty, stderr_tty)):
            if tty:
                master, slave = next(next_tty)
                os.close(slave)
                streams.append(master)
            else:
                streams.append((child.stdout if index == 0 else child.stderr).fileno())
            selector.register(streams[-1], selectors.EVENT_READ, index)
        captured = [bytearray(), bytearray()]
        first = second = False
        try:
            deadline = time.monotonic() + 360
            while selector.get_map():
                if time.monotonic() >= deadline:
                    raise RuntimeError('retry capture timed out')
                for key, _ in selector.select(1):
                    try:
                        chunk = os.read(key.fd, 65536)
                    except OSError as exc:
                        if exc.errno != errno.EIO:
                            raise
                        chunk = b''
                    if chunk:
                        captured[key.data].extend(chunk)
                    else:
                        selector.unregister(key.fd)
                        if (stdout_tty, stderr_tty)[key.data]:
                            os.close(key.fd)
                if not first and (b'following deploy (' in captured[1] if stderr_tty else b'accepted\n' in captured[0]):
                    os.write(control, b'\n')
                    first = True
                if first and not second and (retry in captured[0] if stdout_tty else b'\nstep\n' in captured[0]):
                    os.write(control, b'\n')
                    second = True
            assert child.wait(timeout=10) == 0 and first and second, 'retry capture did not finish'
            return tuple(bytes(item) for item in captured)
        finally:
            if child.poll() is None:
                child.terminate()
                child.wait(timeout=80)
            for key in list(selector.get_map().values()):
                selector.unregister(key.fd)
                if (stdout_tty, stderr_tty)[key.data]:
                    os.close(key.fd)
            os.close(control)

    out, err = capture(True, True)
    assert out.count(retry) == 1 and b'state: \x1b[32msucceeded\x1b[0m' in out, 'split terminal result missing'
    assert b'following deploy (' not in out and b'\x1b[2K' not in out, 'progress escaped onto stdout'
    assert b'following deploy (' in err and err.endswith(b'\r\x1b[2K') and retry not in err, 'split stderr progress wrong'
    piped_out, piped_err = capture(False, False)
    mixed_out, mixed_err = capture(False, True)
    assert b'\x1b' not in piped_out + piped_err + mixed_out, 'escape on a pipe'
    assert b'following deploy (' in mixed_err and mixed_err.endswith(b'\r\x1b[2K'), 'mixed stderr progress wrong'
    assert b'\nstep\n' in piped_out and b'\nstep\n' in mixed_out, 'legacy step event missing'
    assert b'state: succeeded\n' in piped_out and b'state: succeeded\n' in mixed_out, 'plain deploy result missing'
    def stable_transcript(data):
        return re.sub(rb'(?m)^(id|started): [^\n]*$', rb'\1: <dynamic>', data)
    assert stable_transcript(piped_out) == stable_transcript(mixed_out), 'legacy plain transcript differs'
    print('retry descriptors: split terminals, pipes and mixed streams')
    sys.exit(0)


if len(sys.argv) > 1 and sys.argv[1] == '--wrapper':
    release, *command = sys.argv[2:]
    attributes = termios.tcgetattr(0)
    quiet = termios.tcgetattr(0)
    quiet[3] &= ~termios.ECHO
    termios.tcsetattr(0, termios.TCSANOW, quiet)
    child = subprocess.Popen(command, stdin=subprocess.DEVNULL)
    old_handlers = {sig: signal.signal(sig, stop_on_signal) for sig in (signal.SIGTERM, signal.SIGINT)}
    try:
        sys.stdin.readline()
        with urllib.request.urlopen(release, timeout=10) as response:
            response.read()
        sys.exit(child.wait(timeout=120))
    finally:
        try:
            termios.tcsetattr(0, termios.TCSANOW, attributes)
        except termios.error:
            pass
        if child.poll() is None:
            child.terminate()
            child.wait(timeout=10)
        for sig, handler in old_handlers.items():
            signal.signal(sig, handler)


def check(held, frame, out, err, code):
    assert held, 'successful wait response was not held'
    assert frame, 'waiting for app frame missing while response held'
    assert code == 0, 'wait command failed'
    assert re.search(rb'\x1b\[32mhealthy\x1b\[0m', out), 'green healthy result missing'
    assert re.search(rb'\r\x1b\[2K[|/\\-] waiting for app \(', err), 'raw frame missing'
    assert err.endswith(b'\r\x1b[2K'), 'final clear missing'


binary, run, project, environment, origin, driver = sys.argv[1:]
host, port = origin.rsplit(':', 1)
gate, reached = threading.Event(), threading.Event()
run_ids = []


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        self.forward()

    def do_GET(self):
        if self.path == '/__release':
            gate.set()
            self.send_response(204)
            self.end_headers()
            return
        self.forward()

    def forward(self):
        connection = http.client.HTTPConnection(host, int(port), timeout=120)
        try:
            headers = {k: v for k, v in self.headers.items()
                       if k.lower() not in ('host', 'connection', 'accept-encoding')}
            length = int(self.headers.get('Content-Length', '0'))
            body_in = self.rfile.read(length) if length else None
            connection.request(self.command, self.path, body=body_in, headers=headers)
            response = connection.getresponse()
            body = response.read()
            path = self.path.split('?')[0]
            if self.command == 'POST' and path.endswith('/jobs/s8/run') and response.status == 202:
                run_ids.append(json.loads(body)['run_id'])
            match = self.command == 'GET' and path.endswith('/wait/web') and response.status == 200
            if self.command == 'GET' and path.endswith('/jobs/s8/runs') and response.status == 200 and run_ids:
                match = any(row.get('id') == run_ids[-1] for row in json.loads(body))
            if match and not reached.is_set():
                reached.set()
                gate.wait(120)
            self.send_response(response.status)
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):
            pass
        finally:
            connection.close()


class Server(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True


server = Server(('127.0.0.1', 0), Handler)
thread = threading.Thread(target=server.serve_forever, daemon=True)
thread.start()
config = os.path.join(run, 'progress-read-proxy.json')
with open(os.path.join(run, 'admin.json')) as source:
    data = json.load(source)
data['projects'][project]['environments'][environment]['url'] = 'http://127.0.0.1:%d' % server.server_port
with open(config, 'w') as target:
    os.chmod(config, 0o600)
    json.dump(data, target)
masters, slaves = [], []
for _ in range(2):
    master, slave = pty.openpty()
    attrs = termios.tcgetattr(slave)
    attrs[1] &= ~termios.ONLCR
    termios.tcsetattr(slave, termios.TCSANOW, attrs)
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 40, 120, 0, 0))
    masters.append(master)
    slaves.append(slave)
env = dict(os.environ, TERM='xterm', NO_COLOR='', BERTHY_TOKEN_STORE='none')
proc = subprocess.Popen([binary, 'wait', 'web', '--healthy', '-e', environment, '--config', config],
                        stdin=subprocess.DEVNULL, stdout=slaves[0], stderr=slaves[1], env=env,
                        start_new_session=True)
for slave in slaves:
    os.close(slave)
selector = selectors.DefaultSelector()
for index, master in enumerate(masters):
    selector.register(master, selectors.EVENT_READ, index)
capture = [bytearray(), bytearray()]
frame = False
try:
    deadline = time.monotonic() + 30
    while selector.get_map():
        if time.monotonic() >= deadline:
            raise RuntimeError('wait response or PTY EOF timed out')
        for key, _ in selector.select(0.25):
            try:
                chunk = os.read(key.fd, 65536)
            except OSError as exc:
                if exc.errno != errno.EIO:
                    raise
                chunk = b''
            if not chunk:
                selector.unregister(key.fd)
                os.close(key.fd)
            else:
                capture[key.data].extend(chunk)
        if reached.is_set() and not gate.is_set():
            frame = bool(re.search(rb'\r\x1b\[2K[|/\\-] waiting for app \(', capture[1]))
            if frame or time.monotonic() >= deadline - 10:
                gate.set()
    code = proc.wait(timeout=10)
    if os.environ.get('S8_BASELINE_MODE') == '1':
        print('baseline read: held=%s frame=%s green=%s' % (
            reached.is_set(), frame, bool(re.search(rb'\x1b\[32mhealthy\x1b\[0m', capture[0]))))
        gate.clear()
        reached.clear()
        poll_masters, poll_slaves = [], []
        for _ in range(2):
            master, slave = pty.openpty()
            attrs = termios.tcgetattr(slave)
            attrs[1] &= ~termios.ONLCR
            termios.tcsetattr(slave, termios.TCSANOW, attrs)
            fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 40, 120, 0, 0))
            poll_masters.append(master)
            poll_slaves.append(slave)
        poll_proc = subprocess.Popen([binary, 'job', 'run', 's8', '-e', environment, '--config', config],
                                     stdin=subprocess.DEVNULL, stdout=poll_slaves[0], stderr=poll_slaves[1],
                                     env=env, start_new_session=True)
        for slave in poll_slaves:
            os.close(slave)
        poll_selector = selectors.DefaultSelector()
        for index, master in enumerate(poll_masters):
            poll_selector.register(master, selectors.EVENT_READ, index)
        poll_capture = [bytearray(), bytearray()]
        held_at = None
        try:
            poll_deadline = time.monotonic() + 120
            while poll_selector.get_map():
                if time.monotonic() >= poll_deadline:
                    raise RuntimeError('baseline job PTY EOF timed out')
                for key, _ in poll_selector.select(1):
                    try:
                        chunk = os.read(key.fd, 65536)
                    except OSError as exc:
                        if exc.errno != errno.EIO:
                            raise
                        chunk = b''
                    if chunk:
                        poll_capture[key.data].extend(chunk)
                    else:
                        poll_selector.unregister(key.fd)
                        os.close(key.fd)
                if reached.is_set() and held_at is None:
                    held_at = time.monotonic()
                if held_at is not None and (b'waiting for job (' in poll_capture[1] or time.monotonic() - held_at >= 20):
                    gate.set()
            poll_code = poll_proc.wait(timeout=10)
            assert reached.is_set() and len(run_ids) == 1 and poll_code == 0, 'baseline job fixture failed'
            assert ('run: %s\n' % run_ids[0]).encode() in poll_capture[0], 'baseline job ID mismatch'
            print('baseline job: held=True frame=%s green=%s' % (
                b'waiting for job (' in poll_capture[1],
                bool(re.search(rb'\x1b\[32msucceeded\x1b\[0m', poll_capture[0]))))
        finally:
            gate.set()
            if poll_proc.poll() is None:
                poll_proc.terminate()
                poll_proc.wait(timeout=10)
            for key in list(poll_selector.get_map().values()):
                poll_selector.unregister(key.fd)
                os.close(key.fd)
        sys.exit(1)
    check(reached.is_set(), frame, bytes(capture[0]), bytes(capture[1]), code)
    print('held healthy wait: frame, green result, final clear')

    # The wait response is stable; pin the CLI's pretty JSON including its newline.
    expected = b'{\n  "status": "healthy"\n}\n'
    command = [binary, 'wait', 'web', '--healthy', '-e', environment, '--config', config, '--json']
    piped = subprocess.run(command, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                           stderr=subprocess.PIPE, env=env, timeout=120)
    assert piped.returncode == 0 and piped.stdout == expected, 'piped healthy JSON differs'
    assert b'\x1b' not in piped.stdout + piped.stderr, 'escape in piped JSON'
    json_masters, json_slaves = [], []
    for _ in range(2):
        master, slave = pty.openpty()
        attrs = termios.tcgetattr(slave)
        attrs[1] &= ~termios.ONLCR
        termios.tcsetattr(slave, termios.TCSANOW, attrs)
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 40, 120, 0, 0))
        json_masters.append(master)
        json_slaves.append(slave)
    json_proc = subprocess.Popen(command, stdin=subprocess.DEVNULL, stdout=json_slaves[0],
                                 stderr=json_slaves[1], env=env, start_new_session=True)
    for slave in json_slaves:
        os.close(slave)
    json_selector = selectors.DefaultSelector()
    for index, master in enumerate(json_masters):
        json_selector.register(master, selectors.EVENT_READ, index)
    json_capture = [bytearray(), bytearray()]
    try:
        json_deadline = time.monotonic() + 120
        while json_selector.get_map():
            if time.monotonic() >= json_deadline:
                raise RuntimeError('JSON PTY EOF timed out')
            for key, _ in json_selector.select(1):
                try:
                    chunk = os.read(key.fd, 65536)
                except OSError as exc:
                    if exc.errno != errno.EIO:
                        raise
                    chunk = b''
                if chunk:
                    json_capture[key.data].extend(chunk)
                else:
                    json_selector.unregister(key.fd)
                    os.close(key.fd)
        assert json_proc.wait(timeout=10) == 0, 'terminal JSON command failed'
        assert bytes(json_capture[0]) == expected, 'terminal healthy JSON differs'
        assert b'\x1b' not in json_capture[0] + json_capture[1], 'escape in terminal JSON'
        print('healthy JSON: pinned bytes and no escapes on pipes or terminals')
    finally:
        if json_proc.poll() is None:
            json_proc.terminate()
            json_proc.wait(timeout=10)
        for key in list(json_selector.get_map().values()):
            json_selector.unregister(key.fd)
            os.close(key.fd)

    gate.clear()
    reached.clear()
    screen_dir = os.path.join(run, 'progress-read-screen')
    os.mkdir(screen_dir, 0o700)
    steps_path = os.path.join(screen_dir, 'steps.json')
    steps = [
        {'wait_text': 'waiting for app (', 'timeout': '100s'},
        {'snapshot': 'pending'}, {'keys': ['enter']},
        {'wait_text': 'healthy', 'timeout': '100s'},
        {'snapshot': 'complete'}, {'expect_exit': 0, 'timeout': '100s'},
    ]
    with open(steps_path, 'w') as target:
        json.dump(steps, target)
    wrapper = [sys.executable, os.path.abspath(__file__), '--wrapper',
               'http://127.0.0.1:%d/__release' % server.server_port,
               binary, 'wait', 'web', '--healthy', '-e', environment, '--config', config]
    try:
        driven = subprocess.run([driver, '--command', ' '.join(shlex.quote(x) for x in wrapper),
                                 '--size', '120x40', '--script', steps_path, '--out', screen_dir,
                                 '--timeout', '180s', '--env', 'TERM=xterm', '--env', 'NO_COLOR='],
                                stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, env=env, timeout=200)
        assert driven.returncode == 0 and reached.is_set(), 'combined wait driver failed: ' + driver_error(driven)
        with open(os.path.join(screen_dir, 'pending.txt')) as source:
            pending = source.read()
        with open(os.path.join(screen_dir, 'complete.txt')) as source:
            complete = source.read()
        with open(os.path.join(screen_dir, 'complete.html')) as source:
            html = source.read()
        assert re.search(r'^[|/\\-] waiting for app \(', pending, re.M), 'combined frame missing'
        assert 'healthy' in complete and 'waiting for app (' not in complete, 'stale wait frame'
        assert '<span style="color:#008000">healthy</span>' in html, 'green healthy result missing'
        print('combined wait: pending frame and final screen')
    finally:
        gate.set()
        shutil.rmtree(screen_dir)

    gate.clear()
    reached.clear()
    plain_masters, plain_slaves = [], []
    for _ in range(2):
        master, slave = pty.openpty()
        attrs = termios.tcgetattr(slave)
        attrs[1] &= ~termios.ONLCR
        termios.tcsetattr(slave, termios.TCSANOW, attrs)
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 40, 120, 0, 0))
        plain_masters.append(master)
        plain_slaves.append(slave)
    plain_env = dict(env, NO_COLOR='1')
    plain_proc = subprocess.Popen([binary, 'wait', 'web', '--healthy', '-e', environment, '--config', config],
                                  stdin=subprocess.DEVNULL, stdout=plain_slaves[0], stderr=plain_slaves[1],
                                  env=plain_env, start_new_session=True)
    for slave in plain_slaves:
        os.close(slave)
    plain_selector = selectors.DefaultSelector()
    for index, master in enumerate(plain_masters):
        plain_selector.register(master, selectors.EVENT_READ, index)
    plain_capture = [bytearray(), bytearray()]
    try:
        plain_deadline = time.monotonic() + 120
        while plain_selector.get_map():
            if time.monotonic() >= plain_deadline:
                raise RuntimeError('NO_COLOR PTY EOF timed out')
            for key, _ in plain_selector.select(1):
                try:
                    chunk = os.read(key.fd, 65536)
                except OSError as exc:
                    if exc.errno != errno.EIO:
                        raise
                    chunk = b''
                if chunk:
                    plain_capture[key.data].extend(chunk)
                else:
                    plain_selector.unregister(key.fd)
                    os.close(key.fd)
            if reached.is_set() and re.search(rb'\r\x1b\[2K[|/\\-] waiting for app \(', plain_capture[1]):
                gate.set()
        assert plain_proc.wait(timeout=10) == 0, 'NO_COLOR wait failed'
        assert reached.is_set() and gate.is_set(), 'NO_COLOR held frame missing'
        assert b'healthy' in plain_capture[0], 'NO_COLOR healthy result missing'
        assert not re.search(rb'\x1b\[[0-9;]*m', plain_capture[0] + plain_capture[1]), 'NO_COLOR emitted SGR'
        print('NO_COLOR wait: frame and result without SGR')
    finally:
        gate.set()
        if plain_proc.poll() is None:
            plain_proc.terminate()
            plain_proc.wait(timeout=10)
        for key in list(plain_selector.get_map().values()):
            plain_selector.unregister(key.fd)
            os.close(key.fd)

    gate.clear()
    reached.clear()
    job_dir = os.path.join(run, 'progress-job-screen')
    os.mkdir(job_dir, 0o700)
    job_steps = os.path.join(job_dir, 'steps.json')
    with open(job_steps, 'w') as target:
        json.dump([{'wait_text': 'waiting for job (', 'timeout': '100s'},
                   {'snapshot': 'pending'}, {'keys': ['enter']},
                   {'wait_text': 'status: succeeded', 'timeout': '100s'},
                   {'wait_text': 'exit_code: 0', 'timeout': '100s'},
                   {'snapshot': 'complete'}, {'expect_exit': 0, 'timeout': '100s'}], target)
    wrapper = [sys.executable, os.path.abspath(__file__), '--wrapper',
               'http://127.0.0.1:%d/__release' % server.server_port,
               binary, 'job', 'run', 's8', '-e', environment, '--config', config]
    try:
        driven = subprocess.run([driver, '--command', ' '.join(shlex.quote(x) for x in wrapper),
                                 '--size', '120x40', '--script', job_steps, '--out', job_dir,
                                 '--timeout', '180s', '--env', 'TERM=xterm', '--env', 'NO_COLOR='],
                                stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, env=env, timeout=200)
        assert driven.returncode == 0 and reached.is_set(), 'combined job driver failed: ' + driver_error(driven)
        assert len(run_ids) == 1, 'job admission ID missing or duplicated'
        with open(os.path.join(job_dir, 'pending.txt')) as source:
            pending = source.read()
        with open(os.path.join(job_dir, 'complete.txt')) as source:
            complete = source.read()
        with open(os.path.join(job_dir, 'complete.html')) as source:
            html = source.read()
        assert re.search(r'^[|/\\-] waiting for job \(', pending, re.M), 'combined job frame missing'
        assert ('run: %s' % run_ids[0]) in complete and 'status: succeeded' in complete and 'exit_code: 0' in complete, 'wrong job result'
        assert 'waiting for job (' not in complete, 'stale job frame'
        assert '<span style="color:#008000">succeeded</span>' in html, 'green job result missing'
        print('combined job: admitted ID, pending frame and final screen')
    finally:
        gate.set()
        shutil.rmtree(job_dir)
finally:
    gate.set()
    if proc.poll() is None:
        proc.terminate()
        proc.wait(timeout=10)
    for key in list(selector.get_map().values()):
        selector.unregister(key.fd)
        os.close(key.fd)
    server.shutdown()
    server.server_close()
    thread.join()
    os.unlink(config)
