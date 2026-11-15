# Swarm ingress experiment

Run on a Linux host with an isolated Swarm manager, a dedicated Docker socket,
the pinned Alpine and nginx images available locally, Go, Python 3 and Bash.
This creates and removes a uniquely named service, overlay network and proxy on
the specified daemon. It writes raw traces and the fixture binary under
`/var/tmp/berthy-ingress-*`; remove those directories when no longer needed.

```sh
DOCKER_HOST=unix:///var/tmp/berthy-phase1/docker.sock \
DOCKER_CONFIG=/var/tmp/berthy-phase1 \
DOCKER_BIN=/var/tmp/berthy-phase1/bin/docker \
GO_BIN=/var/tmp/berthy-phase1/go/bin/go \
bash test/phase1/ingress.sh
```

The script reports its trace directory and exits nonzero if a required control,
health event, request, or rollback observation is missing or the gate fails.
Requests use fresh connections, one-second timeouts and no proxy upstream retry;
the backend logs arrival timestamps and container IDs. The delayed backend
listens immediately and passes health after eight seconds. Health checks use a
ten-second start period, two-second interval, one-second timeout and two retries;
the update monitor is twenty seconds. The always-unhealthy backend is tested
directly and its health event must match its identity. The final no-healthcheck
update is the positive control. This does not test graceful draining or a
production proxy configuration.

Recalculate a trace with `python3 test/phase1/ingress-report.py TRACE_DIRECTORY`;
check the report classifier with `python3 test/phase1/ingress-report-test.py`.
