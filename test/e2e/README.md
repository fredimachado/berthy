# Berthy end-to-end harness

The harness drives the real `berthy` binary against a real agent and a real single-node Docker Swarm, the way a user
would. Each scenario is a script that deploys, inspects, or changes something through the CLI and asserts what the user
sees. Verification runs in WSL (distro `Ubuntu`) against the isolated daemon at `/var/tmp/berthy-phase2/docker.sock`.

## Run it

From Windows Git Bash:

    MSYS_NO_PATHCONV=1 wsl -d Ubuntu -- bash /mnt/d/code/berthy/test/e2e/run.sh 00-smoke

From inside WSL:

    bash /mnt/d/code/berthy/test/e2e/run.sh 00-smoke

With no scenario names, every `scenarios/*.sh` runs in name order. `--no-build` reuses the binaries already in
`$E2E_ROOT/bin`. `--keep` leaves the agent and the fakes running after the run; stop them with `stop.sh`. The exit code
is 0 when every scenario passes and 1 otherwise. From Git Bash, never inline a `$` expansion in `wsl -- bash -c '...'`:
put the logic in a script file and run that file by path.

## Layout

`$E2E_ROOT` is `~/berthy-e2e/harness` in WSL, unless `E2E_ROOT` is set.

- `src/`: a copy of the repo made by `build.sh`, with CR stripped from `*.go`, `go.mod` and `go.sum`.
- `bin/`: `berthy`, `faketelegram`, `fakeotlp`, `fakewebhook`, `fakes3` and `tuidriver` (a program that runs a command in a pseudo-terminal and drives it with keys; see `test/tuidriver`).
- `run/`: the agent and CLI configs, `env.sh` (the tokens, mode 0600), the state key, the fake-server files, and pid files. `agent.json` is written from `run/environments.d/*.json`, one fragment per hosted environment.
- `manifest/`: `berthy.yml`, the manifest that scenarios deploy from.
- `logs/`: the agent and fake logs of the current scenario. `setup.sh` clears them for each scenario.
- `runs/<scenario>/`: a copy of those logs, kept by `run.sh` after each scenario.
- `docker-config/`: an empty Docker CLI config, so the Windows credential helper is never asked.

Loopback ports: agent `127.0.0.1:19501`, second agent `127.0.0.1:19502`, fake Telegram `127.0.0.1:18901`, fake OTLP
`127.0.0.1:18902`, fake webhook `127.0.0.1:18903`, fake S3 `127.0.0.1:18904`, and the throwaway registry `127.0.0.1:15021`.
A run that shares the daemon with others sets its own: `AGENT_LISTEN=127.0.0.1:1951<SLOT>`,
`AGENT2_LISTEN=127.0.0.1:1954<SLOT>`, `TG_LISTEN=127.0.0.1:1891<SLOT>`, `OTLP_LISTEN=127.0.0.1:1892<SLOT>`,
`HOOK_LISTEN=127.0.0.1:1895<SLOT>`, `S3_LISTEN=127.0.0.1:1897<SLOT>` and `REG=127.0.0.1:1503<SLOT>`.

The fake webhook (`test/fakewebhook`) takes the agent's webhook alerts and heartbeat pings. It records each request's method,
path, headers and body to `run/hook-requests.jsonl`; `hook_requests` and `hook_reset` in `lib.sh` read and clear the record,
`/_fake/fail` makes it answer an error status, and `stop_hook` and `start_hook` take it down and bring it back.

The fake S3 (`test/fakes3`) is the object storage of backups. It starts with the bucket `$S3_BUCKET` (`e2e-backups`), keeps its
data in memory, and accepts any access key, so a scenario that backs up sets the secrets `S3_ACCESS_KEY` and `S3_SECRET_KEY` to
any value and names `http://$S3_LISTEN` as the endpoint of a destination. `s3_objects PREFIX` in `lib.sh` lists what is stored
under PREFIX, as JSON, and a request to `/_fake/fail` makes the next one fail (see `test/fakes3`).

## Scripts

- `build.sh`: copies the repo, strips CR from Go sources, and builds the four binaries. Each is built to `*.new` and moved
  into place, so a failed build keeps the previous binary.
- `setup.sh`: builds a fresh fixture. It purges the previous e2e objects, makes the admin token (the only one in agent.json), a new state key and configs,
  starts an in-memory registry holding `e2e/web:v1` and `:v2` (pushed from the existing `127.0.0.1:15001` tags), validates
  the manifest, and starts the fakes and the agent, then makes the `ci` agent token through the API (`api_token` in lib.sh; `make_ciadmin` makes an agent admin token the same way). Running it twice ends in the same state.
- `stop.sh`: stops the agent and the fakes, and removes the e2e Swarm services, containers, volumes and registry. It leaves
  `$E2E_ROOT` in place.
- `run.sh`: builds (unless `--no-build`), then runs `setup.sh` and one scenario per name, each on a fresh fixture. It stops
  the fixture at the end unless `--keep`, prints a summary, and exits 1 when a scenario failed.
- `lib.sh`: settings, the `adm` and `ci` wrappers for the CLI, process helpers, and the assertions. `write_agent_config` joins the fragments of `run/environments.d` into `agent.json`; `add_environment PROJECT ENV` adds a hosted environment (state, key, admin token, fragment, manifest directory `$MAN-PROJECT-ENV`, CLI entry) and restarts the agent; `env_cmd PROJECT ENV CMD [ARGS]` runs berthy as that environment's admin from its manifest directory. `start_agent` waits on `/healthz`, which answers once every hosted environment has started. `start_agent2` starts a second agent process on the same Docker daemon (`AGENT2_LISTEN`, `$RUN/agent2.json`, `$RUN/state2`) with one hosted environment (`$PROJECT`, `${ENVN}c`) and adds it to `admin.json`; `purge_fixture` stops it and removes its journaled network and workloads.
- `scenarios/`: one script per scenario. `00-smoke.sh` deploys v1, runs `exec`, lists the history, deploys v2 and rolls back.
  `38-two-agents.sh` starts a second agent and shows one project with an environment on each.

## Add a scenario

Copy `scenarios/00-smoke.sh` to `scenarios/NN-name.sh` and keep its shape: source `../lib.sh`, drive `berthy` through
`adm` (admin token) or `ci` (agent token), assert with `expect_eq`, `expect_contains`, `expect_not_contains` or
`expect_code`, and end with `finish`. A Swarm service is named `<project>-<environment>-<app>-<role>`, so the web role is
`$PROJECT-$ENVN-web-main`. `adm` and `ci` name only the environment, so they are right while one project has `$ENVN`; a scenario that adds a second project with the same environment name uses `env_cmd` from then on. To change the manifest, pipe a new one into `write_manifest`; `setup.sh` gives each scenario a fresh one.

## What it never touches

- The service `p2test-api-web`, the containers `berthy-phase2-proxy` and `berthy-phase2-registry`, and the overlay network
  `berthy-phase2-lab`, which the e2e services only attach to.
- Any Swarm object not labelled `berthy.environment=e2e`, and any volume not named `e2e_*` or in a compose project `e2e*`.
- The source images `127.0.0.1:15001/berthy-phase2/web:*`. They are only tagged, and the e2e tags are removed again.
- Anything under `~/berthy-e2e/` outside `$E2E_ROOT`.

The harness never prints a token. Logs hold no tokens, and `grep -rF` over `logs/` is how to check that.
