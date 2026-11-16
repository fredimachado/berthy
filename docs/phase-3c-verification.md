# Phase 3c verification and operator notes

This records what the Phase 3c checks showed on the isolated Swarm fixture. The job, volume backup and volume restore results
come from earlier runs on the same fixture. They are recorded as they were observed and were not re-run for this document.
The agent state results come from the run of this change, on the final build. Every service the checks used was a fake:
`test/fakes3` as the S3 server, and a fake Telegram Bot API for alerts. None of it proves behaviour against the real
services, which is listed under "Not verified here".

## Run record

- Berthy commit `c763b64` on `main`, with the agent state backup uncommitted: `internal/statebackup/`, `internal/state`
  (the `state_backups` table, `Snapshot`, `SchemaVersion`, `ErrWrongKey`), the `state_backup` block in `internal/manifest`,
  the wiring and routes in `internal/agent`, the `state-backup` and `state-restore` commands in `internal/cli`, the
  `StateBackupRun` type in `internal/api`, and `docs/spec-v1.md`.
- Daemon: Docker Engine 29.7.2, single-node Swarm, on the isolated socket `/var/tmp/berthy-phase2/docker.sock`. Compose
  v5.5.1. Ubuntu on WSL2, Go 1.25.4.
- Agent binary for the agent state run: `CGO_ENABLED=0 go build -o berthy ./cmd/berthy`, sha256 `673f518d…`.
- Checks: `go vet ./...` passes. `go test -race -count=1 ./...` passes in WSL. `gofmt -l` is empty. Cross-vet passes for
  linux/amd64, linux/arm64, windows/amd64 and darwin/arm64. `go test ./...` passes on Windows.
- Agent state run dates: 2026-11-16. The first pass ran from about 05:09 UTC. The final run, from a clean fixture,
  ran between 05:18 and 05:26 UTC.

## Phase 3b jobs (earlier run)

| Scenario | Result |
| --- | --- |
| A 2-replica image role with a shared volume and a Compose service each logged exactly one line per minute across a graceful restart and an update redeploy | Pass |
| A failing job alerted through the fake Telegram, with critical alert text ending in `job:fail: exit status 3` | Pass |
| A slow job's next occurrence was skipped and recorded | Pass |
| A graceful stop recorded the run as interrupted, with no alert | Pass |
| `job run` exit codes and scopes | Pass |

## Volume archive and restore (earlier run)

| Scenario | Result |
| --- | --- |
| A 50 MiB tree with owners 1000:1000, 0:0 and 70:70, modes 0600, 0640, 4755, 2775 and 1777, absolute, relative and dangling symlinks, and a hard link, restored identically | Pass |
| Corrupted, truncated and checksum-mismatched archives, and a restore into an existing volume, each left no new volume | Pass |
| Helper containers ran read-only, with no network and `CapDrop ALL` | Pass |

## Backups (earlier run)

| Scenario | Result |
| --- | --- |
| Image and Compose volume backups, the Compose one with a `before` job writing a dump | Pass |
| `backup list` read the storage sidecars | Pass |
| Admin restore identical to the source | Pass |
| Deploy and read tokens refused with `403` | Pass |
| An injected `UploadPart` failure left no listed backup and no upload in progress | Pass |
| Retention `keep 2` | Pass |
| `kill -9` during a backup and during a restore: the leftover helper was removed at the next start, the partial restored volume was removed, and the operation was recorded as interrupted | Pass |

## Scheduled backups (earlier run)

| Scenario | Result |
| --- | --- |
| A backup scheduled every minute ran once per minute for three occurrences, with no duplicate after a restart | Pass |
| A `backup:` pre-deploy hook ran before migrations. A failing hook stopped the deploy before migrations, the old version kept serving, and the reason was `pre-deploy backup uploads failed: upload failed` | Pass |
| Alerts for failure, recovery, a size drop, and overdue after the agent was down | Pass |
| Ages shown in `describe`, `ps` and `doctor` | Pass |

## Agent state (fixture `~/berthy-e2e/p3h`, final run)

The fixture had one Compose app, `files`, with a `state_backup` block: destination `offsite` (the fake S3 on
`127.0.0.1:19900`), schedule `* * * * *` and `keep: 2`. The storage credentials and one app secret were stored through the
admin token.

| Scenario | Result |
| --- | --- |
| Scheduled snapshots once per minute: runs at 05:20, 05:21 and 05:22 succeeded, each for its own occurrence | Pass |
| Retention `keep 2`: after three runs the fake S3 held two snapshots and two sidecars under `berthy-state/p3h/`, and the older run was removed | Pass |
| Sidecar `{"format":"berthy-state-sqlite-1","environment":"p3h","schema_version":11,"size":176128,"sha256":…}` matched its snapshot | Pass |
| `berthy state-backup -e p3h` with the admin token exited 0 and printed run, status, key, size and sha256 | Pass |
| Deploy and read tokens refused with `403` | Pass |
| The `state_backup` check in `checks`: `ok`, `last snapshot 58s ago, 172.0 KiB` | Pass |
| Agent stopped and its state directory deleted (the server lost) | Done |
| `state-restore` with the wrong key: exit 1, `the key does not open this state`, and no output directory left | Pass |
| `state-restore` with a credential flag (`--access-key`): exit 2, the flag refused | Pass |
| `state-restore` with the saved key into a new directory: environment `p3h`, schema 11, the three secret names printed, no values. Directory mode 0700, `state.db` mode 0600 | Pass |
| A new agent on the rebuilt state: the same secret names, the same deploy history, `ps` showing `p3hsvc-files`, and the audit log readable | Pass |
| The snapshot's own run, which was `running` inside the snapshot, recorded as `failed` with `agent restarted during the state backup`; no run left running; the check stayed `ok` | Pass |
| The restored agent wrote a new snapshot at the next minute | Pass |
| Storage down: the scheduled run failed with `state backup storage unavailable`, and the check went `critical` with that detail | Pass |
| Storage back (an empty bucket): the next snapshot succeeded and the check returned to `ok`, `last snapshot 15s ago` | Pass |

The state at the end of the run was about 172 KiB. The fixture was destroyed and its sealed state removed after the run.

## Findings

- **Schema 11.** The state backup adds a `state_backups` table with a unique index on each scheduled occurrence, like the
  volume backups. A metadata row could not give one record per occurrence across restarts without a row per minute.
- **Run listing.** `GET /v1/environments/{env}/state-backup/runs` (read scope) was added beyond the first sketch. The
  on-demand command polls it for the outcome, as `backup run` does for volumes.
- **Interrupted snapshot at restore.** A snapshot holds the run that took it, marked running. Its first start after a
  restore marks it failed. The agent was first built without that step; the final fixture run confirmed it.
- **Wrong key message.** A wrong key at agent start now reads `state key: the key does not open this state`, instead of
  the cipher's own error.
- **Race in a test, not the product.** The on-demand API test read the audit log before the outcome was written. The
  test now polls for both entries. The package passes under `-race` in WSL, since native Windows has no cgo.

## Done-when status (Phase 3c)

- An image app's file volume restores with its data and permissions intact: verified in the earlier volume restore run.
- A PostgreSQL dump produced in a Compose service restores into a scratch database: **not verified**. The earlier run
  verified a Compose `before` job writing a dump, but no restore into a scratch database was among its results.
- An interrupted upload leaves no successful backup record: verified in the earlier backup run.
- A failing backup hook stops the deploy before migrations: verified in the earlier scheduled backup run.
- The agent state is backed up and recovers with the saved key: verified against the fake S3 in the final run. The
  `berthy setup --key-file` path belongs to Phase 5 and is not built.

## Not verified here

- A real S3 provider. `test/fakes3` keeps its objects in memory, so the restart in the storage-down scenario emptied it.
- A PostgreSQL restore into a scratch database (see Done-when above).
- A `kill -9` during a state snapshot. The abandon-on-start path is covered by a store test, not by a kill in the fixture.
- A state large enough to measure snapshot time and memory. The fixture state was about 172 KiB.
- Rebuilding a second host from provisioning. That needs `berthy setup --key-file` (Phase 5).
- Multi-node operation, and real Telegram delivery.

## Rerun

The agent and the fake S3 are built from the repository. The fixture scripts are kept in the session scratchpad and are not
part of the repository.

```sh
CGO_ENABLED=0 go build -o "$bin/berthy" ./cmd/berthy
go build -o "$bin/fakes3" ./test/fakes3
"$bin/fakes3" -listen 127.0.0.1:19900 -bucket berthy-p3h
"$bin/berthy" agent --config "$run/agent.json"
```

- The agent config sets `state_dir` and `key_file` (a 32-byte key, mode 0600) and the three tokens. Create the tokens with
  `berthy agent token -e p3h`.
- Store the credentials with `berthy secret set S3_ACCESS_KEY` and `S3_SECRET_KEY`, through the admin token.
- Deploy a manifest that has the `state_backup` block shown above. Then `berthy state-backup -e p3h --config admin.json`
  takes a snapshot on demand, and `berthy checks -e p3h` shows the `state_backup` check.
- To rebuild: stop the agent, delete its `state_dir`, run `berthy state-restore --endpoint http://127.0.0.1:19900 --bucket
  berthy-p3h --object <newest .db key> --out <new dir> --key-file <key>` with `BERTHY_S3_ACCESS_KEY` and
  `BERTHY_S3_SECRET_KEY` set, then start an agent with `state_dir` set to the new directory.
