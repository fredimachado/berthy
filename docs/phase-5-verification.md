# Phase 5 verification and operator notes

This records what the Phase 5 checks showed on disposable fake servers. The end-to-end checks D1 to D4 ran on 2026-11-16
against commit `27abaf2`, on the fixture described under "Run record". The doctor, fixture and setup results of the earlier
Phase 5 changes come from earlier runs with the same fixture scripts. They are recorded as they were observed and were not
re-run for this document. The fake servers are privileged systemd Debian 12 containers with `sshd`, an `ops` user with
`sudo` and no Docker. Docker, Swarm and every app run for real inside them, and `berthy setup` ran from the WSL host over
SSH, as an operator would run it from a laptop. What the fixture cannot prove is listed under "Not verified here".

## Run record

- Berthy commit `27abaf2` on `main`. The binaries were built from a copy of the working tree without `.git`, with
  `CGO_ENABLED=0` for linux/amd64. The tree had no changes: Windows `git status` was clean. (`git status` in WSL lists
  `examples/ci/deploy.sh` as modified. The `/mnt/d` mount reports a different file mode. The content is the same.)
- Toolchain: Go 1.25.8 in WSL and on Windows (`go version`).
- Binaries: `berthy-A` built with `-X berthy/internal/version.Version=v0.5.0-e2e`, 50281974 bytes, sha256 prefix
  `b38553f549a0`. `berthy-B`, version `v0.5.1-e2e`, the same size, prefix `e76c51789775`. The fake S3 server `fakes3`
  is 9672189 bytes.
- Daemon: Docker Engine 29.7.2, Compose v5.5.1, on `unix:///var/tmp/berthy-phase2/docker.sock`. Only this daemon was
  used. The service `p2test-api-web` and the containers `berthy-phase2-registry` and `berthy-phase2-proxy` were up before,
  during and after the run, and nothing touched them.
- Fake server image: `berthy-fakeserver:debian12`, id `sha256:dee9e8e113a6`, built from `debian:12` (index digest
  `sha256:268fb24c39bc` in `test/fakeserver/README.md`). Inside the servers `get.docker.com` installed Docker Engine
  29.10.0, with Compose v5.7.1.
- Host: Ubuntu 20.04.6 LTS on WSL2, kernel 6.18.58.1-microsoft-standard-WSL2. The servers are on the bridge
  `berthy-fakeservers` (`172.31.250.0/24`, gateway `.1`).
- Server names: `sf1` to `sf4`, not `f1` to `f4`. Docker resolves a name that matches no container as a prefix of a
  container id, and `f1` is the start of the id of a `p2test-api-web` container. `fakeserver.sh start f1` therefore found a
  "running container" that was not its own (see Findings). The run never used the name `f1`, and nothing was removed or
  changed in that container. `sf1` took `172.31.250.10`, `sf2` `.11`. After `sf1` was destroyed, `sf3` took `.10`, as in a
  rebuild on the same private address, and `sf4` took `.12`.
- Checks:
  - `gofmt -l .` prints nothing (WSL and Windows). `go vet ./...` passes on Windows, and in WSL as the first step of
    `p4d-wsltest.sh` (native, and cross vet for windows/amd64 and darwin/arm64).
  - `go test -race -count=1 ./...` in WSL, on a copy of the tree: 32 packages ok, 3 without test files (`cmd/berthy`,
    `deploy/systemd`, `test/phase1`), no race reports. It took 2 min 25 s.
  - Native Windows `go test -p 2 ./...`: 31 packages ok (29 of them from the Go test cache for unchanged inputs), 4 without
    test files (`cmd/berthy`, `deploy/systemd`, `test/phase1`, `test/tuidriver`), exit 0.
- Times, UTC, 2026-11-16: static checks 12:51 to 12:54; build and first server 12:52 to 12:55; D1 12:55 to 12:59; D2 12:59 to
  13:02; D3 13:02 to 13:08; token check 13:08 and again after this document was written; static checks on Windows 13:09;
  cleanup 13:12.

## Earlier results (not re-run)

### Doctor, P5-1 (commit b73e23a)

A real agent on the Phase 2 fixture daemon, with a CLI configuration.

| Check | Result |
| --- | --- |
| `doctor` lists `agent`, `host`, `disk`, `docker`, `docker_api`, `swarm_manager`, `network` (all critical) and `compose`, `lock` | Pass |
| `network` ok when the shared network is an attachable overlay (`berthy-phase2-lab`) | Pass |
| `network` failed when it is missing (`p5-1-missing: missing`) and when it is not an overlay (`bridge: not an attachable overlay`), with exit 1 | Pass |
| `lock` ok when free, and `held by p5-1-admin for 2s: p5-1 real run` while an operator held it | Pass |
| `agent-info` and `doctor` show the agent version. A newer CLI prints `note: the agent runs v0.0.0-test, this CLI is v0.0.1-test; berthy setup upgrades the agent` | Pass |

### Fake servers, P5-2 (commit 6bafc19)

The harness check script passed 11 of 11: systemd running, `sshd` key-only, the Docker install script run for real inside, a
nested Swarm and overlay network, a unit running as `nobody`, the server reachable from WSL on its private address, and
`rm-all` that leaves no container, volume or network.

### `berthy setup`, P5-3a (commit 4a183d1)

Coder runs and the reviewer's run on a fresh server, before the agent steps existed.

| Check | Result |
| --- | --- |
| Dry run on a fresh server: 3 steps `would_change`, nothing changed (also as JSON) | Pass |
| Real run: Docker 29.9.3 installed, Swarm turned on, overlay created. A second run: every step `ok`, ids unchanged | Pass |
| Refused at `docker` with `--no-install-docker`, at `network` for an existing non-overlay network, at `check` for an address the server does not hold and for a server that runs another environment's agent, at `connect` for an unreachable host | Pass |
| `--local` as root and as `ops` with `sudo`; a login as `root` over SSH; usage errors exit 2 | Pass |

### `berthy setup`, P5-3b (commit de99b7a)

Coder runs, and a reviewer run on a fresh server.

| Check | Result |
| --- | --- |
| First run: all 13 steps, the token printed once, `doctor` 9 of 9 ok with it. Second run: every step `ok`, the same `MainPID` | Pass |
| Upgrade with `--binary`: `binary` and `service` changed, the agent reported the new version, a hand-made Swarm service kept its version and update time | Pass |
| A failed upgrade (`--binary /bin/false`): `the new agent did not come up; the previous one was put back`, and a later run repaired it | Pass |
| `--new-token` added a second `admin` token and restarted the service for the changed configuration | Pass |
| `--key-file` on a new server installed the saved key and the agent started. A different key file failed at `service` with the agent left stopped | Pass |
| A server with a running Swarm service: only the agent was added. `--local` as `ops` | Pass |
| Reviewer run: 13 steps, `doctor` 9 of 9, a second run with the same `MainPID`, an upgrade to the next build, owners and modes as in the spec | Pass |
| 0 token hits in the results | Pass |

## Phase 5 "Done when" (this run)

The Phase 5 "Done when" says: `berthy setup` alone has brought a fresh server to a passing `doctor`, and a second run changed
nothing. A run against a server with running apps added only the agent, and a run with a saved encryption key let the rebuilt
agent read its restored state.

Ids are shortened to 12 characters. Every setup run was `berthy setup` from WSL as `ops` over SSH (`sudo`), with the fixture's
`ssh` first on `PATH`. The admin token was captured to a private file with mode 600 and replaced by `<token>` in every copy.

| Scenario | What was done | Observed evidence | Result |
| --- | --- | --- | --- |
| D1a Dry run | Fresh `sf1`: `setup -e p5e --host ops@sf1 --listen 172.31.250.10:7710 --network p5e --config cli.json --dry-run`, then again with `--json` | `connect` and `check` ok, ten steps `would_change` (`docker`, `swarm`, `network`, `user`, `binary`, `key`, `token`, `config`, `service`, `cli_config`), `verify` `skipped`. Exit 0. A snapshot of `sf1` before and after was identical: no `docker`, no `/etc/berthy`, no `/var/lib/berthy`, no `berthy` user, no new file in `/usr/local/bin`, `/tmp` or `/var/tmp`, no new listener. `cli.json` was not written | Pass |
| D1b Setup alone | The real run on the fresh `sf1` with nothing done by hand before or between. Then `berthy doctor -e p5e --config cli.json` with the printed token in `BERTHY_P5E_TOKEN` | Exit 0 in 48 s. Docker 29.10.0 installed, Swarm advertising `172.31.250.10`, network `p5e` created (attachable overlay), `berthy` user, binary uploaded, key generated, one `admin` token created, `agent v0.5.0-e2e, 9 of 9 checks ok`. The token was printed once, after the steps. `doctor` exit 0, checks `agent`, `host`, `disk` (root 74.7% available), `docker`, `docker_api` (1.40 - 1.57), `swarm_manager` (active), `network` (p5e) all critical and ok, `compose` (v5.7.1) and `lock` (free) non-critical and ok. Modes: `/etc/berthy/agent.json` `berthy:berthy` 600, `/var/lib/berthy` and `/state` 700, `key` 32 bytes 600, the unit equal to `deploy/systemd/berthy.service`, `berthy` in group `docker`. The agent listened on `172.31.250.10:7710`. `sf1` had 0 containers and 0 services | Pass |
| D1c Second run | The same command without `--listen`, with and without the token variable, then `--json`. A snapshot of `sf1` before and after each | Every step `ok` (`docker` "Docker 29.10.0, running, enabled", `binary` "this CLI (v0.5.0-e2e) is installed", `token` "the tokens of /etc/berthy/agent.json are kept", `service` "running and enabled", `verify` "9 of 9 checks ok"), exit 0, no token printed. In `--json`, 13 steps all `ok`, no `token` key. Identical before and after: agent `MainPID` 1857 and start time 12:56:21, the mtime, inode and SHA-256 of `/usr/local/bin/berthy`, `/etc/berthy/agent.json`, the unit and `/var/lib/berthy/key`, the file lists (`.prev` and `.new` files absent), the Docker daemon `MainPID` 1160 and start 12:56:17, the Swarm node id `iu7o53dmxpeb`, the id of network `p5e` (`mhrdbyrqumcn`) | Pass |
| D1d The agent is usable | A Compose app `hello` (`nginx:alpine`, joined to the external network `p5e`, project `p5e`): `config validate`, `deploy hello`, `ps`, `doctor` | The deploy was accepted and succeeded. `ps` showed `p5e-hello` kind `compose`, role `web`, 1 running, `state: running`. On `sf1` the container `p5e-hello-web-1` was up on network `p5e`. `history hello` showed release 1. `doctor` exit 0, the same 9 checks ok. `berthy wait hello --healthy` returned `agent returned HTTP 400: operation unavailable or invalid`: `wait` works on image apps only (see Findings) | Pass |
| D1e Upgrade | `setup` with `--binary` set to the `v0.5.1-e2e` build, the CLI of that build | `binary` "uploaded --binary berthy-B, sha256 e76c51789775", `service` "restarted berthy.service (binary changed)", `config` unchanged, `verify` "agent v0.5.1-e2e, 9 of 9 checks ok", exit 0, no token. `agent-info` showed `v0.5.1-e2e`, `doctor` had no critical failure. `/usr/local/bin/berthy.prev` was kept. The agent's `MainPID` went from 1857 to 4007. The `hello` container kept its id (`23903f33d9c3`), its start time (12:58:02) and 0 restarts, and the Docker daemon `MainPID` stayed 1160. The journal shows the old agent stopping and `berthy agent v0.5.1-e2e listening on 172.31.250.10:7710` | Pass |
| D2 Running apps | Fresh `sf2`, prepared by hand: `install-docker`, `swarm init --advertise-addr`, overlay `p5e`, Swarm service `legacy-web` (`nginx:alpine`, 2 replicas, on `p5e`), container `legacy-db` (`redis:alpine`, `unless-stopped`). Records, setup, the same records | `setup` `docker` "Docker 29.10.0, running, enabled", `swarm` "manager", `network` "p5e (attachable overlay)": all `ok` and unchanged. `user` through `verify` `changed`, and `verify` "agent v0.5.0-e2e, 9 of 9 checks ok". The records before and after were identical: Docker daemon `MainPID` 929 and start 13:00:26, the Swarm fields (node id, address, manager, cluster), service `legacy-web` (`2ejs0mth009u`, version 15, updated 13:00:29), the task ids `sgga94kqx3fa` and `qzbzvcqw2kug` (see below), the three containers with ids, start times and 0 restarts, the six networks with ids, the SHA-256 of `docker inspect` of the service, the three containers and the four networks, the images and 0 volumes. New: the `berthy` user (uid 999, group `docker`), `/etc/berthy/agent.json` (600), `/var/lib/berthy/key` and `state` (600, 700), `/usr/local/bin/berthy` and `berthy.service`, active. `doctor` exit 0, 9 checks ok. `ps` listed `legacy-web` with 2 running replicas | Pass |
| D3a Key and state | On `sf1` (D1): a destination on fake S3 and a `state_backup` block in the manifest (`hello` with the env secret `D3_MARKER`), secrets `S3_ACCESS_KEY`, `S3_SECRET_KEY` and `D3_MARKER` (dummy values from files), `deploy hello` | `secret list`: `D3_MARKER`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`. `history hello`: release 2 (`1e8ccd35d133`, 13:03:32) and release 1 (`faa7e539e2aa`, 12:58:03). The audit held 7 entries: the two deploys (accepted, succeeded) and the three `secret_set` rows. The SHA-256 of `$D3_MARKER` inside the `hello` container was `e0b9a0deb1ee…` and the SHA-256 of the dummy value was the same | Pass |
| D3b Save the key, snapshot | `ssh ops@sf1 sudo cat /var/lib/berthy/key > p5e.key` (32 bytes, mode 600, never printed), then `berthy state-backup -e p5e --config cli.json` | `run: 1`, `status: succeeded`, `key: berthy-state/p5e/20261116T130412Z.db`, 188416 bytes, sha256 `7b60e1ade899…`. The bucket held the snapshot and its sidecar (207 bytes) | Pass |
| D3c Server destroyed | `fakeserver.sh rm sf1` | Container and both volumes gone. `p2test-api-web` still up | Pass |
| D3d Rebuild | Fresh `sf3`, in the guide's Step 15 order: `scp` the binary and the key, `state-restore` on `sf3` as root with the S3 credentials in the environment, then `berthy setup -e p5e --host ops@sf3 --listen 172.31.250.10:7710 --network p5e --key-file p5e.key --config cli3.json` | Before the restore, `sf3` had no Docker and no `berthy` user, and `/var/lib/berthy` was empty. `state-restore` exit 0: `environment: p5e`, `schema: 12`, `secrets: 3` with their names, and `/var/lib/berthy/state/state.db` 600. Setup exit 0 in 47 s: `key` "prepared /var/lib/berthy, prepared /var/lib/berthy/state, gave the state to berthy, installed the key from --key-file", `token` "created an admin token named admin", `service` "installed, enabled and started berthy.service", `verify` "agent v0.5.1-e2e, 9 of 9 checks ok". `state.db` now belonged to `berthy` (600). The key on `sf3` equals the saved key by SHA-256. The agent's journal: `state backup: 1 snapshot(s) were interrupted by an agent restart and are recorded as failed`, then `listening on 172.31.250.10:7710`. The new token was shown once | Pass |
| D3e The restored state | With the new token on `sf3`: `secret list`, `history hello`, `audit`; then `deploy hello` and a hash of the env var in the container | `secret list` identical to `sf1`. `history hello` identical (releases 2 and 1). The audit held all 7 entries of `sf1` and one more, `state_backup` started at 13:04:12, which the snapshot recorded while it ran. `checks` showed `state_backup` `unknown`, "not run yet". The deploy on `sf3` (no container before) succeeded, release 3 with the same digest as 2. The SHA-256 of `$D3_MARKER` in the new container was `e0b9a0deb1ee…`, equal to the one set on `sf1`. The agent had opened the secret with the saved key | Pass |
| D3f Refusals | A fresh `sf4` with the same restore. Setup without `--key-file`, then setup with a different 32-byte key file | Without a key: `✖ key /var/lib/berthy/state has state but no key: pass --key-file with the saved key`, `setup … failed at key`, exit 1, no token. After it, `sf4` had `state.db` and `owner.lock` and no key, no `agent.json`, no unit, and no agent. `docker`, `swarm`, `network`, `user` and `binary` had run before the `key` step. With the wrong key: `key` installed it, `token` and `config` ran, then `✖ service the agent did not come up and is left stopped: state key: the key does not open this state | …`, `setup … failed at service`, exit 1. The token was printed (a first install keeps the configuration that holds it). `journalctl -u berthy -o cat` held `state key: the key does not open this state` three times, then `Start request repeated too quickly`. The unit was `failed`, nothing listened on 7710, and the state files were still in place. The order matters: after the second run a key exists, so the first case can no longer be shown | Pass |
| D4 Token check | The 4 tokens printed during the run, the saved key and the wrong key (hex and base64 of each), the two dummy S3 credentials and the `D3_MARKER` value (11 strings), searched in every result file, the scripts, the brief, the logs, the four CLI configurations, the agent journals of `sf2`, `sf3` and `sf4`, and this document | 0 hits in 82 result files, 23 scripts, the logs, the 4 configurations, the 3 journals (38 lines) and the document. A positive control found the 4 tokens in the 4 private raw setup outputs | Pass |

The task ids of `legacy-web` in D2 were meant to come from `docker service ps`, but the record script passed a flag that command
does not have, so that section of the before and after files holds the same error text. The ids are also in the container names
recorded before and after (`legacy-web.1.sgga94kqx3fa…`, `legacy-web.2.qzbzvcqw2kug…`), which are identical. A `service ps`
listing after the setup shows exactly those two tasks and service version 15 with the same update time, so no task was replaced.

### How fake S3 reached the servers

A manifest accepts a plain `http` endpoint only for a loopback address (`manifest.ValidEndpoint`), and `fakes3` listens only on
loopback. Nothing in the product changed. In WSL a small forwarder (`p5-4-fwd.py`) listens on the bridge gateway
`172.31.250.1:19901` and passes to `fakes3` on `127.0.0.1:19900`. On each server that needed it (`sf1`, `sf3`, `sf4`) `socat`
from `apt` ran as a transient systemd unit, `127.0.0.1:9000` to `172.31.250.1:19901`. The manifest's endpoint was
`http://127.0.0.1:9000`, and so was the `--endpoint` of `state-restore`. The forwarders were installed after setup on `sf1`
(after the D1 proofs) and before the restore on `sf3` and `sf4`. A real S3 endpoint is `https`, which needs none of this.

## Findings

- No product bug was found in D1 to D4. Nothing was changed to make a check pass.
- `test/fakeserver/fakeserver.sh` is not safe with names that are valid hexadecimal prefixes. `exists`, `running`, `exec`
  and `rm` all pass the bare name to `docker container inspect`, `exec` and `rm -f -v`, and Docker takes a name that matches no
  container as a prefix of an id. With a `p2test-api-web` container whose id begins `f1e0…`, `fakeserver.sh start f1`
  treated that container as the server, waited for systemd in it for 90 seconds and failed, and `fakeserver.sh rm f1` would
  have removed it. The name rule `[a-z][a-z0-9-]*` allows `f1`, `a1`, `b2` and so on. A fix would look the name up with
  `docker ps -a --filter "name=^NAME$"`, or refuse names made only of `[0-9a-f]`. This is test tooling, not product code, so it
  was not changed here, and this run avoided it with the names `sf1` to `sf4`.
- `berthy wait <app> --healthy` on a Compose app answers `agent returned HTTP 400: operation unavailable or invalid`. The agent
  looks for the app's bound Swarm services, and a Compose app has none. The message does not say that `wait` is for image apps.
  `docs/user-guide.md` shows `wait` for the image app `api` only.
- A failed first install leaves the unit `failed`, not just stopped. The unit restarts the agent, which exits at once on a key
  that does not open the state, until systemd's start limit stops it after five starts. The `service` failure detail carries
  the last 20 journal lines, so the same cause repeats three times in one line.
- On the first start of a restored agent, the log says `state backup: 1 snapshot(s) were interrupted by an agent restart and
  are recorded as failed`. The snapshot was taken while its own run was recorded as running. The `state_backup` check is
  `unknown` until the next run. This is expected from the spec, and worth knowing before reading the log.
- `berthy audit` prints the newest entry first, so the tail of the output is the oldest entries.
- `systemctl list-units 'berthy*'` also matches the fixture's own `berthy-ssh-hostkeys.service`. It is not part of Berthy.

## Not verified here

- A real VM or cloud server, with a real reboot. The fake servers are containers on the host's kernel. A stop and start does not
  run a fresh kernel boot, so the agent's start at boot, Docker's start at boot and a Swarm that returns after a reboot were not
  seen.
- A real Docker install on other distributions. Only `get.docker.com` on Debian 12 ran.
- The release download against a published GitHub release. No release workflow exists yet, so no `berthy-linux-<arch>` and
  `checksums.txt` have been published. Every run here uploaded the CLI itself or used `--binary`.
- arm64 servers. Every server was amd64.
- SSH through jump hosts or an SSH agent. The fixture uses one key, with a config file that the `ssh` wrapper adds.
- A real S3 service. The state snapshot and its restore used `fakes3` behind forwarders. Real endpoints (https, a region, real
  credentials and bucket policies) were not tried.
- A private address on a VPN. The servers used the private bridge.
- A rebuilt server whose volumes come back from volume backups, and the full guide Step 15 after the state: redeploying every
  app in order, `ingress apply` and `berthy restore`. D3 redeployed one app.

## Operator notes

### Running setup

```bash
# from the application's repository, so setup reads `network` and `protected` from the manifest
mkdir -p ~/.config/berthy
berthy setup -e prod --host ops@10.0.0.5 --listen 10.0.0.5:7710 --dry-run
berthy setup -e prod --host ops@10.0.0.5 --listen 10.0.0.5:7710 --config ~/.config/berthy/config.json
```

- Look first with `--dry-run`. It changes nothing on the server and writes no file.
- Setup needs `root`, or an account that can use `sudo` without a password. It runs your own `ssh`.
- The first run prints the `admin` token once, after the steps. Store it, and set it in the variable the CLI configuration names
  (`BERTHY_PROD_TOKEN`). The agent keeps only its digest. Lose it and run setup again with `--new-token`.
- `--listen` is needed on the first run only. A different address later is refused.
- Run it again as often as you like. A second run changes nothing: the agent keeps its `MainPID`, and Docker, Swarm, the
  network and the running apps are not touched.
- Setup on a server that already runs Docker, Swarm and apps adds the `berthy` user, the agent files and the service, and
  nothing else.

### Saving the key

The key is made on the server and never leaves it unless you copy it. After the first run:

```bash
ssh ops@10.0.0.5 sudo cat /var/lib/berthy/key > prod.key
chmod 600 prod.key
```

Keep `prod.key` in a password manager or your provisioning vault. A snapshot of the agent's state is useless without it. Set a
`state_backup` block in the manifest, and take one now with `berthy state-backup -e prod --config $CFG`.

### Rebuilding a server

In this order, as in `docs/user-guide.md` Step 15:

1. Provision the server, with the same private address if you can.
2. Put the binary and the key on the new server (mode 600) and, as root, `install -d -m 0700 /var/lib/berthy`, then run
   `berthy state-restore --endpoint … --bucket … --object berthy-state/prod/<time>.db --out /var/lib/berthy/state
   --key-file ./prod.key` with `BERTHY_S3_ACCESS_KEY` and `BERTHY_S3_SECRET_KEY` in the environment, never as flags. It prints the
   secret names, not their values. A wrong key leaves nothing behind.
3. From your machine: `berthy setup -e prod --host ops@10.0.0.5 --listen 10.0.0.5:7710 --key-file ./prod.key --config $CFG`.
   Do not run setup before step 2: it would start an agent on an empty state.
4. Setup prints a new `admin` token. Tokens live in the agent's configuration, not in the state, so make new ones for CI and for
   AI agents.
5. `berthy doctor`, then redeploy each app, and restore volumes with `berthy restore`. The new server has no containers.

Without `--key-file`, setup refuses a server that has state and no key. With a key that does not open the state, setup
fails at `service` and leaves the agent stopped, with `the key does not open this state` in `journalctl -u berthy`. Nothing is
changed in the state in either case.

### Upgrading the agent

Run setup with a newer CLI, or with `--binary <path>` for a Linux build:

```bash
berthy setup -e prod --host ops@10.0.0.5 --config $CFG
berthy setup -e prod --host ops@10.0.0.5 --config $CFG --binary ./berthy-linux-amd64
```

Only the agent's process restarts, and the apps keep running (D1e: same container id and start time). The previous binary is
kept as `/usr/local/bin/berthy.prev`. If the new agent does not answer `/healthz` within 30 seconds, setup puts the previous one
back and says so. `berthy doctor` prints a note when the agent and the CLI have different versions.

## Rerun

The fixture scripts and results are kept in the session scratchpad and are not part of the repository. The steps are:

1. Build two linux/amd64 binaries from the tree with different `-X berthy/internal/version.Version` values, and `test/fakes3`.
2. `test/fakeserver/fakeserver.sh start sf1`, and for the other scenarios `sf2`, `sf3` and `sf4`. Use names that cannot be the
   start of a container id.
3. Run setup as in D1a to D1e. For D2, prepare `sf2` with `install-docker`, `swarm init`, the network, a service and a
   container before setup.
4. For D3, start `fakes3`, the bridge forwarder and a loopback `socat` on each server that needs one. Set the secrets, deploy,
   save the key, run `state-backup`, then `fakeserver.sh rm sf1`. Rebuild on `sf3` and refuse on `sf4`.
5. Search the results for the tokens, the key and the dummy values. Then `fakeserver.sh rm-all` and stop `fakes3`.
