#!/bin/bash
# 29-host-checks proves the doctor's host checks on the real host. A container that Berthy does not manage publishes a port on
# every address, so public_ports warns and names that port, and the doctor still exits 0 with the check not critical. The
# port leaves the detail once the container is gone. berthy checks --cluster lists both host checks. reboot_required reports what the
# host's own reboot-required file says, and is left out on a host without the update notifier. Nothing is written on the host.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

name="$PROJECT-public-port"
trap 'd rm -f "$name" >/dev/null 2>&1' EXIT

# port_busy PORT: true when a socket listens on PORT on any address. It reads the kernel's tables, as the agent does.
port_busy() {
  awk -v p="$(printf '%04X' "$1")" '$4 == "0A" { split($2, a, ":"); if (a[2] == p) found = 1 } END { exit !found }' /proc/net/tcp /proc/net/tcp6 2>/dev/null
}

# port_free PORT: true when nothing listens on PORT.
port_free() { ! port_busy "$1"; }

port=""
for _ in $(seq 1 50); do
  candidate=$((20000 + RANDOM % 20000))
  if port_free "$candidate"; then
    port=$candidate
    break
  fi
done
if [ -z "$port" ]; then
  fail "a free host port" "none found in 20000-39999"
  finish
fi

# 1. A container outside Berthy publishes the port on every address. Its image is the e2e web image, already in the registry.
d rm -f "$name" >/dev/null 2>&1
out=$(d run -d --name "$name" -p "0.0.0.0:$port:80" --entrypoint sleep "$REG/e2e/web@$WEB_V1_DIGEST" 3600 2>&1); code=$?
expect_code "start the container that publishes $port on every address" 0 "$code" "$out"
wait_for "port $port listening" port_busy "$port"

# 2. The doctor names the port as a warning, and still exits 0.
out=$(adm doctor 2>&1); code=$?
expect_code "doctor exits 0 with the public port warning" 0 "$code" "$out"
json=$(HOME="$RUN/doctor-no-copy" adm doctor --json 2>"$RUN/doctor-keycopy-stderr"); code=$?
expect_contains "doctor sends the key copy warning to stderr" "$(cat "$RUN/doctor-keycopy-stderr")" "warning: this machine has no saved copy of the current state key"
compact=${json//[[:space:]]/}
expect_code "doctor --json exits 0 with the public port warning" 0 "$code" "$json"
expect_contains "public_ports names the published port" "$json" "$port on 0.0.0.0 (published by a container)"
expect_contains "public_ports is a warning, not critical" "$compact" '"name":"public_ports","ok":false,"critical":false'
expect_contains "public_ports says a listening port is not proof of reachability" "$json" "not proof that it is reachable"

# 3. reboot_required follows the host's own file. Its check is left out where the host has no update notifier.
if [ -d /usr/share/update-notifier ]; then
  if [ -e /var/run/reboot-required ]; then
    want_reboot='"name":"reboot_required","ok":false,"critical":false'
  else
    want_reboot='"name":"reboot_required","ok":true,"critical":false'
  fi
  expect_contains "reboot_required reports the host's own reboot file" "$compact" "$want_reboot"
else
  expect_not_contains "reboot_required is left out on a host without the update notifier" "$compact" '"name":"reboot_required"'
fi

# 4. berthy checks --cluster lists both host checks, since the monitor records them. The monitor lists a check after its first run, which
# follows its rebuild, so the scenario waits for the list as scenario 18 does.
listed() { adm checks --cluster --json | jq -e --arg name "$1" 'any(.[]; .name == $name)' >/dev/null; }

# wait_listed NAME SECONDS: waits until berthy checks lists NAME.
wait_listed() {
  local i
  for i in $(seq 1 $(($2 / 3))); do
    listed "$1" && return 0
    sleep 3
  done
  return 1
}

if wait_listed public_ports 120; then
  pass "checks --cluster lists public_ports"
else
  fail "checks --cluster lists public_ports" "$(adm checks --cluster --json 2>&1)"
fi
if [ -d /usr/share/update-notifier ]; then
  if wait_listed reboot_required 120; then
    pass "checks --cluster lists reboot_required"
  else
    fail "checks --cluster lists reboot_required" "$(adm checks --cluster --json 2>&1)"
  fi
fi

# 5. Once the container is removed, its port is gone from the detail and the doctor still exits 0.
out=$(d rm -f "$name" 2>&1); code=$?
expect_code "remove the container" 0 "$code" "$out"
wait_for "port $port released" port_free "$port"
json=$(HOME="$RUN/doctor-no-copy" adm doctor --json 2>"$RUN/doctor-keycopy-stderr"); code=$?
expect_code "doctor exits 0 once the container is gone" 0 "$code" "$json"
expect_not_contains "public_ports no longer names the port" "$json" "$port on 0.0.0.0"

finish
