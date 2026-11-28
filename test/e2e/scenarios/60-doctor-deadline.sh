#!/bin/bash
# 60-doctor-deadline proves berthy doctor waits for a slow tunnel check instead of printing
# "agent connection failed". A loopback Cloudflare stand-in withholds the tunnel GET until the
# agent closes that connection (its own TunnelHealth timeout), while answering credential
# verification promptly. The doctor then returns a noncritical ingress_tunnel warning and a
# successful compose check, and exits 0.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"

tunnel_name="$PROJECT-$ENVN-tunnel"
secret_name=CF_TOKEN
account_id=acct-doctor-deadline
tunnel_id=tunnel-doctor-deadline
cf_seen="$RUN/cf-tunnel-seen"
cf_pid=""

stop_cf() {
  if [ -n "$cf_pid" ]; then
    kill "$cf_pid" >/dev/null 2>&1 || true
    wait "$cf_pid" >/dev/null 2>&1 || true
    cf_pid=""
  fi
}
trap 'stop_cf' EXIT

# 1. Store a dummy Cloudflare secret the seeded ingress will name.
out=$(printf 'dummy-cloudflare-token' | adm secret set "$secret_name" 2>&1); code=$?
expect_code "store the dummy Cloudflare secret" 0 "$code" "$out"

# 2. Stop the agent and seed one configured tunnel in its existing ingress table.
stop_agent
if python3 - "$RUN/state/state.db" "$ENVN" "$account_id" "$secret_name" "$tunnel_id" "$tunnel_name" <<'PY'
import sqlite3, sys, time
path, env, account, secret, tunnel, name = sys.argv[1:]
updated = int(time.time() * 1000)
with sqlite3.connect('file:' + path + '?mode=rw', uri=True) as db:
    assert db.execute(
        'INSERT INTO ingress(environment, account_id, api_token_secret, tunnel_id, tunnel_name, dns, updated) VALUES(?,?,?,?,?,?,?)',
        (env, account, secret, tunnel, name, 0, updated),
    ).rowcount == 1
    row = db.execute('SELECT tunnel_name FROM ingress WHERE environment=?', (env,)).fetchone()
    assert row and row[0] == name
PY
then pass "seed a configured ingress tunnel"; else fail "seed a configured ingress tunnel" "fixture transaction failed"; start_agent; finish; fi

# 3. Point cloudflare_api_url at a loopback server that withholds the tunnel GET and answers credentials.
rm -f "$cf_seen"
coproc CF_FAKE { exec python3 - "$cf_seen" "$account_id" "$tunnel_id" 2>"$RUN/cf-fake.log" <<'PY'
import http.server, select, sys
seen_path, account, tunnel = sys.argv[1:]
prefix = "/client/v4"
tunnel_path = f"{prefix}/accounts/{account}/cfd_tunnel/{tunnel}"
verify_paths = {f"{prefix}/user/tokens/verify", f"{prefix}/accounts/{account}/tokens/verify"}

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        path = self.path.split("?", 1)[0]
        if path == tunnel_path:
            open(seen_path, "w").close()
            try:
                while True:
                    ready, _, _ = select.select([self.connection], [], [], 0.5)
                    if not ready:
                        continue
                    if not self.connection.recv(1):
                        break
            except OSError:
                pass
            return
        if path in verify_paths:
            body = b'{"success":true,"errors":[],"result":{"status":"active"}}'
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_response(404)
        self.end_headers()

server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
print(server.server_port, flush=True)
server.serve_forever()
PY
}
cf_pid=$CF_FAKE_PID
IFS= read -r cf_port <&"${CF_FAKE[0]}" || { fail "start the Cloudflare stand-in" "no port"; finish; }
jq --arg url "http://127.0.0.1:$cf_port/client/v4" '.cloudflare_api_url = $url' "$RUN/agent.json" > "$RUN/agent.json.new" \
  && chmod 600 "$RUN/agent.json.new" && mv -f "$RUN/agent.json.new" "$RUN/agent.json" \
  || { fail "point the agent at the Cloudflare stand-in" "jq failed"; finish; }
start_agent || { fail "restart the agent with the Cloudflare stand-in" "the agent did not answer"; finish; }

# 4. Doctor returns the complete report: a noncritical tunnel warning and a successful compose check.
adm doctor --json >"$RUN/doctor.out" 2>"$RUN/doctor.err"
code=$?
out=$(cat "$RUN/doctor.out")
err=$(cat "$RUN/doctor.err")
compact=${out//[[:space:]]/}
expect_code "doctor --json exits 0 after a slow tunnel check" 0 "$code" "$err$out"
if [ -f "$cf_seen" ]; then pass "the stand-in saw the tunnel GET"; else fail "the stand-in saw the tunnel GET" "marker missing"; fi
expect_contains "ingress_tunnel reports unavailable" "$compact" '"name":"ingress_tunnel","ok":false,"critical":false,"detail":"unavailable"'
expect_contains "compose check follows and succeeds" "$compact" '"name":"compose","ok":true'
expect_not_contains "doctor does not report a connection failure" "$err" "agent connection failed"

finish
