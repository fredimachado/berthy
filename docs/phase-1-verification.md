# Phase 1 verification

This page records Phase 1 verification results and their scope. Production
ingress and VPN behavior require their own checks.

## Environment and setup

The measurements below used native Ubuntu 20.04.6 LTS in WSL with systemd as
PID 1, Docker Engine and CLI 29.7.2, cgroup v2, Go 1.25.4 and Python 3.8.10.
The isolated daemon socket was `/var/tmp/berthy-phase1/docker.sock`; Docker
Desktop was not used. The measurement fixture had two Swarm services and four
Compose containers across two projects. The measured agent was built from
`5a5aa83`; `file` identified its Linux binary as statically linked. Later
changes should be checked again with their own build.

At `7e7086f`, the smoke script passed against a separate systemd agent bound to
the WSL private address. It exercised both fixture kinds, text and JSON,
filtered logs, a live Compose log follower and a stats watch for more than 20
seconds with increasing timestamps, history, unauthorized requests and invalid
bind configuration. A Windows-built CLI authenticated to that same agent at
`172.21.112.66:17944` and listed both Swarm services and both Compose projects.
This WSL address test does not measure a production VPN.

The final product build, `31933f2`, was also confirmed as a static Linux
binary with `file`. Its full systemd CLI smoke check passed, and the native
Windows CLI reached the WSL private address and returned exactly four apps.
The Go tests and vet, Linux race tests, and ingress classifier and context
checks passed. The changes after this product build affected documentation and
the verification harness.

Build on Linux with `CGO_ENABLED=0 go build -o "$PWD/berthy" ./cmd/berthy` as
the checkout owner; a root build on a mounted checkout may need `-buildvcs=false`.
Provision a dedicated test daemon with its own socket, data and runtime state;
never point a destructive test at a shared daemon. The following example makes
temporary credentials and starts an agent as a transient systemd service. Choose
an unused private or loopback address, and run it as a user with access to the
test socket. The API is HTTP: use an encrypted private network when it leaves
the host.

```sh
umask 077
dir=$(mktemp -d)
bin="$PWD/berthy"
socket=/absolute/path/to/isolated/docker.sock
listen=127.0.0.1:19087
unit="berthy-phase1-$RANDOM-$$"
mapfile -t generated < <("$bin" agent token -e lab)
export BERTHY_TOKEN="${generated[0]#token: }"
digest="${generated[1]#sha256: }"
printf '{"environment":"lab","listen":"%s","docker_socket":"%s","tokens":[{"digest":"%s","scope":"read"}]}\n' "$listen" "$socket" "$digest" > "$dir/agent.json"
printf '{"environments":{"lab":{"url":"http://%s","token_env":"BERTHY_TOKEN"}}}\n' "$listen" > "$dir/cli.json"
sudo systemd-run --unit="$unit" --uid="$(id -u)" --property=SupplementaryGroups=docker "$bin" agent --config "$dir/agent.json"
systemctl status "$unit"
"$bin" ps -e lab --config "$dir/cli.json" --json
```

Use only a test fixture on that daemon. The agent config must remain private;
the CLI config names an environment variable instead of storing the token.
With a Swarm app containing at least one log line, a Compose app, and a role
and full replica ID obtained from `describe`, run the smoke check:

```sh
bash test/phase1/smoke.sh "$bin" "$dir/cli.json" lab "http://$listen" \
  swarm:YOUR_SERVICE compose:YOUR_PROJECT YOUR_ROLE YOUR_REPLICA_ID LOG_REGEX
```

The Swarm log must contain a line matching `LOG_REGEX`, and a container in the
Compose project must emit log lines continuously. It checks text and JSON
output, filtered and followed logs, current and watched
stats, history, rejected credentials and a rejected wildcard bind. It does
not create Docker objects. Inspect the actual bound address with `ss -ltnp`;
an address restriction does not establish that the network is encrypted.
After testing, stop only the transient agent unit and remove the temporary
directory: `sudo systemctl stop "$unit"; rm -r -- "$dir"; unset BERTHY_TOKEN`.

## Metrics and resources

Five paired samples of the same CPU-bound container gave Berthy CPU percentages
49.18, 49.39, 49.18, 49.21 and 49.37 versus Docker 49.33, 49.52, 49.29,
49.52 and 49.45. The predeclared tolerance was five percentage points. Memory
was 364,544 / 67,108,864 bytes versus Docker's 356 KiB / 64 MiB; the tolerance
was the larger of 5% or 1 MiB plus CLI rounding. A separate settled I/O fixture
reported block read/write 8,192 / 8,388,608 bytes versus Docker's 8.19 kB /
8.39 MB, and network receive/transmit 9,978 / 296,528 bytes versus 9.98 kB /
297 kB, within displayed rounding. Root filesystem total and available bytes
matched `df -B1`: 269,427,478,528 and 216,365,600,768. Docker's unique image
layers were 106,721,836 bytes versus `docker system df`'s 106.7 MB.

To repeat, capture `berthy stats -e lab --config "$dir/cli.json" --json` and
`docker --host "$DOCKER_HOST" stats --no-stream --no-trunc --format '{{json .}}'`
at nearby times for the same full container ID. Compare at least five stable
CPU windows with the tolerances above; compare memory and cumulative I/O after
the fixture's writes and network traffic settle. Compare filesystem bytes with
`df -B1` and unique image layers with `docker system df`. Different collection
times and Docker's displayed rounding matter.

For the RSS acceptance run, record the agent's idle `VmRSS`, then sample
`VmRSS` and `VmHWM` from `/proc/$pid/status` during at least 65 minutes of
normal load. Record the oldest and newest retained stats timestamps and their
age, not just the sample count. The target is 30,000,000 bytes.

The `5a5aa83` collector ran for 65 minutes 16 seconds, from 07:48:07 to
08:53:23 AEST on 15 November 2026. Idle `VmRSS` immediately beforehand was
8,512 KiB. Across 390 polls, the first and last `VmRSS` were 8,616 and 14,764
KiB; the maximum `VmRSS` and `VmHWM` were both 14,800 KiB (15,155,200 bytes),
below the 30,000,000-byte target. This peak applies to this WSL fixture and
build. The load included a stats watcher, periodic `ps` and `df` calls, up to
six containers and the real daemon outage described below; it did not include
an hour-long log follower.

The watcher received 392 samples. The final retained 360 had unique timestamps
spanning 59 minutes 50 seconds, from 07:53:24.092 to 08:53:14.091 AEST; 358
were fresh. This measures the collector build above, not a later binary.

## Isolated outage and recovery

Provision a separate test-only systemd Docker service and, if applicable, its
socket unit, with its own data, runtime and socket. Do not stop Docker Desktop,
the host's ordinary Docker service, or a daemon used by another test. Point a
separate test agent at this socket and record `systemctl show -p MainPID` for
the agent. Stop both the **test** socket activation unit and test daemon unit,
verify the socket cannot reactivate it, and check that `agent-info` still works,
`doctor` reports Docker unavailable and `ps` fails. Check that a live log
stream ends on loss. Start the same test daemon again, verify the agent's
MainPID has not changed, then repeat `doctor`, `ps`, stats and a newly connected
log stream. Record commands and results without recording credentials. This
procedure was exercised at approximately 07:58 AEST on 15 November 2026 on the
dedicated Ubuntu WSL test daemon, running Docker Engine 29.7.2 as a Swarm
manager. The agent was built from `7e7086f` and had systemd MainPID 21373.
The test daemon had no socket activation; stopping its `berthy-phase1-docker`
unit removed its socket. During the outage, the same agent served identity and
host disk information. `doctor` exited nonzero with Docker unavailable while
the host and root filesystem checks remained OK; `ps` returned HTTP 503 and a
nonzero CLI exit. A Compose log follower received one NDJSON record before
exiting nonzero with `agent stream failed`. Within one collection period,
stats retained host metrics and reported `containers_fresh: false` with an
explicit Docker error.

The same test daemon was started again with `systemd-run`. Doctor, inventory
and stats recovered without a change to the agent's MainPID. Swarm converged on
its own; the Compose fixtures had no restart policy and were brought back with
`compose up -d` before the CLI reconnected.

The outage and recovery check was repeated with product build `31933f2`.
During the outage, `doctor`, `ps` and an active log stream each exited with
status 1, while host metrics remained available. App inventory, Compose log
tail and stats recovered after the daemon returned, and the agent's systemd
MainPID remained 1478. A separate systemd agent also started with a configured
nonexistent Unix socket: authenticated `agent-info` worked, `doctor` reported
Docker failure with a healthy root check, and host stats remained available
with `containers_fresh: false`. Its listener was bound only to
`127.0.0.1:17945`.

## Swarm ingress

The [isolated harness](../test/phase1/ingress.md) used a pinned nginx 1.31.8
container on an attachable overlay and a service-name VIP, with one backend
replica and no published service port. Start-first update and rollback used one
task at a time; nginx upstream reuse and retries were disabled. Requests had
fresh connections and one-second timeouts. The health check ran every two
seconds with a one-second timeout, two retries and a ten-second start period;
the update monitor was twenty seconds. The delayed candidate listened
immediately and passed health after eight seconds.

Of 768 requests before the control, zero failed, and every successful request
ID had exactly one backend arrival in the trace. The delayed candidate served
224 requests, none before its first healthy event; the event was at
21:48:47.070924 UTC on 2026-11-14 and the first backend arrival was at
21:48:47.125762045. During the unhealthy update, the prior revision served 380
requests and the candidate received zero through ingress. A direct request to
that candidate succeeded at 21:49:12.623538002, and the same container became
unhealthy at 21:49:23.176376. Swarm reported `rollback_completed`. The
same-ingress control with health checks disabled served 250 requests from the
bad revision. There were 1,033 requests in total. These observations support
the native gate for this exact Engine, proxy and route; they do not establish
graceful draining or validate another production ingress configuration.

A separate full rerun after `9e34ddf`, at approximately 11:50 AEST, inherited
an intentionally invalid `DOCKER_CONTEXT` and used the dedicated Docker 29.7.2
socket. Of 769 gated requests, none failed, lacked a unique backend arrival,
or reached the delayed candidate before health. The delayed candidate served
224 requests; the prior revision served 380 during the bad update, and the
bad candidate received zero ingress requests. The bad candidate answered a
direct request and rollback completed. The positive control served 248 bad
revision requests; the run had 1,031 requests in total.
