# Phase 3a verification and operator notes

This records what the Phase 3a checks showed on the isolated Swarm fixture. The Compose, alert and probe-helper results
come from earlier runs on the same WSL fixture. They are recorded as they were observed and were not re-run for this
document. The telemetry run is from the change that added OpenTelemetry export. Every service the checks used was a fake:
a fake Telegram Bot API, a fake OTLP receiver and a fake Cloudflare API. None of it proves behaviour against the real
services, which is listed under "Not verified here".

## Run record

- Berthy commit `35fc4c2` on `main`, with the telemetry change uncommitted: `internal/telemetry/`,
  `internal/agent/telemetry.go`, the doctor and CLI wiring in `internal/agent` and `internal/cli`, `test/fakeotlp/`,
  the `scale --replicas` check, and `go.mod` and `go.sum`.
- Daemon: Docker Engine 29.7.2, single-node Swarm, on the isolated socket `/var/tmp/berthy-phase2/docker.sock`.
  Compose v5.5.1. Ubuntu on WSL2, Go 1.25.4.
- Telemetry libraries: OpenTelemetry Go v1.46.0 (`sdk/metric`, `otlpmetrichttp`) and `go.opentelemetry.io/proto/otlp`
  v1.11.0. v1.47.0 needs Go 1.26, and the module stays on `go 1.25.0`.
- Agent binary: `CGO_ENABLED=0 go build -o berthy ./cmd/berthy`, sha256 `b125b6de…`. The clean tree at `35fc4c2` gave
  `4aab3ad7…`.
- Checks in WSL: `go vet ./...` and `go test -race -count=1 ./...` pass. `gofmt -l` is empty. Cross-vet passes for
  linux/amd64, linux/arm64, windows/amd64 and darwin/arm64. `go test ./...` passes on Windows.
- Telemetry run dates: 2026-11-15, 22:55 to about 23:03 UTC. A first attempt failed on a bug in the test script (a
  condition evaluated once, not on each try). The script was fixed and the whole run repeated from a clean state. Its
  logs are kept on the fixture as `logs/attempt-1-script-bug`.

## Compose (fixture `~/berthy-e2e/p3a`, earlier run)

| Scenario | Result |
| --- | --- |
| Deploy a stateful app with a volume and a port bound to `127.0.0.1` | Pass |
| No-op redeploy keeps the container | Pass |
| Changed service of a stateful app refused without `--allow-recreate` | Pass |
| The same change refused with the flag for a deploy token (403) | Pass |
| Recreated by an admin with the flag, volume data intact | Pass |
| Port on all interfaces refused before any change | Pass |
| Hand-started project adopted with its data and container unchanged | Pass |
| Adoption with a differing file refused, listing the difference | Pass |
| Rollback to the first release with admin and the flag | Pass |
| Agent SIGKILL during `up`: operation interrupted with "compose deploy interrupted", lock released, Compose child gone with the agent (8 ms) | Pass |
| Restart works; `scale` and `exec` refused for Compose apps (`exec is for image apps`) | Pass |
| Host-reaching settings (privileged, host namespaces, `cap_add`, devices, bind mounts, `env_file`, host-file secrets and configs, `container:` sharing) refused for a deploy token, allowed for admin | Pass |

## Alerts (fixture `~/berthy-e2e/p3b`, fake Telegram, earlier run)

| Scenario | Result |
| --- | --- |
| HTTP probe critical after its threshold (10 s) | Pass |
| Reminder at about 22 s with a 20 s reminder interval | Pass |
| No duplicate alert after restarting the agent while the probe fails | Pass |
| Recovery alert | Pass |
| Peer agent stopped: `silent: connection refused`, critical; recovered after it restarted | Pass |
| `alerts test` delivered | Pass |
| Failed delivery (fake 503) retried and delivered after recovery | Pass |
| `/healthz` without a token returns exactly `{"ok":true}` | Pass |
| Restart-loop warning `3 task failures in 10m` on a Swarm service whose container exits 1 | Pass |
| Disk checks limited to `/`, the Docker data mount and mounts of at least 1 GiB | Pass |
| Bot token absent from every log and output (the marker is 0 in the fixture's logs, messages and state) | Pass |

## Probe helper (fixture `~/berthy-e2e/p3c`, earlier run)

| Scenario | Result |
| --- | --- |
| Helper image built locally from the static agent binary (28 MB, one layer) | Pass |
| Helper container read-only, `CapDrop ALL`, `no-new-privileges`, 64 MiB, port bound only to `127.0.0.1` | Pass |
| Helper token-protected: 401 without the token | Pass |
| Service probe critical when the service had 0 tasks, and recovered when it had tasks | Pass |
| Helper recreated after an agent restart | Pass |
| Helper removed when the probe was dropped | Pass |

## Telemetry (fixture `~/berthy-e2e/p3d`, fake OTLP receiver)

The receiver is `test/fakeotlp`. It listens on loopback, decodes each OTLP/HTTP request and writes a summary of counts,
metric and resource names, header names, and whether `Authorization` matched a value read from a file. It never writes
a header value. The manifest names `telemetry.otlp_endpoint: http://127.0.0.1:18800` and
`headers_secret: OTLP_HEADERS`. The secret holds `authorization=Bearer%20<value>`.

| Scenario | Result |
| --- | --- |
| Agent running with no release: the doctor has no telemetry line | Pass |
| Deploy with the agent running, no restart: first export accepted 70 s after the deploy | Pass |
| Agent restart: first export accepted 9 s after the restart | Pass |
| `Authorization` matched the value from the secret, after percent-decoding | Pass |
| Resource attributes `service.name=berthy-agent`, `deployment.environment.name=p3d`, `host.name` | Pass |
| 16 metric names, including `system.*`, `container.*` and `berthy.check.state` | Pass |
| Doctor `telemetry`: ok, `exporting to http://127.0.0.1:18800`; exit code unchanged | Pass |
| Receiver stopped: doctor not ok 53 s later; the detail names scheme and host only, with `connection refused` | Pass |
| Receiver started: doctor ok 9 s later | Pass |
| Secret value: 0 matches in the agent log, the receiver's log and summary, saved CLI output and the state directory; the scan finds a planted value | Pass |
| Cleanup: processes stopped, Compose objects of the run removed, secret-bearing files removed; `p2test-api-web`, `berthy-phase2-registry` and `berthy-phase2-proxy` unchanged | Pass |

Export starts within one minute of a deploy because the exporter reads the last applied manifest once a minute. The
10-second export interval then applies. A restart reads the manifest at start.

### Binary size

| Build | Before (`35fc4c2`) | After | Change |
| --- | --- | --- | --- |
| Documented build, `CGO_ENABLED=0 go build ./cmd/berthy` | 18,078,740 bytes | 29,937,710 bytes | +11,858,970 (+66%) |
| Stripped, `-ldflags='-s -w'` | 12,435,640 bytes | 20,684,984 bytes | +8,249,344 (+66%) |

Most of the growth comes from the OTLP protobuf package that `otlpmetrichttp` imports. That package includes gRPC
service stubs, so gRPC and the protobuf runtime are linked even though no gRPC exporter is used. Stripped sizes of
single-package programs: empty 1.07 MB, OTLP metrics protobuf package 10.62 MB, SDK metrics 4.13 MB, HTTP exporter
11.61 MB. A decision is needed on whether this size is acceptable. The alternative is a small OTLP encoder that does
not use the protobuf package, which departs from the spec's choice of the official exporter.

## Findings

- The telemetry check is also shown when export is off because of an error, such as a missing headers secret. A
  silent check would hide that. The check is not critical.
- Container metrics carry the container ID only. The collector does not keep app or role labels, and looking them up
  per sample was not cheap, so they are left out.
- The manifest checks that `otlp_endpoint` is set, not that it is a URL. An invalid endpoint is reported by the doctor
  and the agent log, not refused at deploy time. Validating it in the manifest would need the URL check duplicated
  there, because the telemetry package depends on the state package, which depends on the manifest package.

## Done-when status (Phase 3a)

- Berthy adopts a running Compose project with its existing data, and blocks an unauthorized stateful recreation: shown
  (Compose table).
- Sends failure and recovery alerts for a test probe: shown with a fake Telegram (alerts table).
- Optional OpenTelemetry export: shown against a fake receiver (telemetry table). Not shown against a real backend.
- Probes against service names reach their targets on the shared network: shown with the overlay probe helper (probe
  helper table).
- The unhealthy-tunnel check for managed ingress: not covered by these runs.

## Not verified here

- Real Telegram delivery. The fake Bot API accepted the messages.
- A real OTLP backend, an `https` endpoint, and a backend's own authorization. The receiver checks one header value.
- Swarm rolling-update exit codes on a live update. The restart-loop warning came from a throwaway service that exits 1.
- The managed-ingress tunnel alert against real Cloudflare. Phase 2b used a fake Cloudflare API.
- Agent memory with telemetry enabled, measured against the 30 MB target. It was not measured in this run.

## Rerun

The telemetry tools are in this repository. The fixture is in the WSL home directory, under `~/berthy-e2e/p3d`, and
depends on the isolated daemon.

```sh
CGO_ENABLED=0 go build -o "$bin/berthy" ./cmd/berthy
go build -o "$bin/fakeotlp" ./test/fakeotlp
"$bin/fakeotlp" -listen 127.0.0.1:18800 -summary "$logs/otlp-summary.json" -expect-authorization-file "$run/otlp-expect"
```

- Store the headers with `berthy secret set OTLP_HEADERS -e <env> --config <admin.json> < "$run/otlp-headers"`. The
  file holds `authorization=Bearer%20<value>` with no line break, and is never printed.
- Add the telemetry block to the manifest: `telemetry: {otlp_endpoint: "http://127.0.0.1:18800", headers_secret: OTLP_HEADERS}`.
- The agent reads the telemetry settings from the last applied manifest, once at start and then once a minute.
- Check `berthy doctor` for the `telemetry` line. The file `otlp-summary.json` shows what the receiver accepted.
