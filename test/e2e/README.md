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
- `bin/`: `berthy`, `faketelegram`, `fakeotlp` and `tuidriver` (a program that runs a command in a pseudo-terminal and drives it with keys; see `test/tuidriver`).
- `run/`: the agent and CLI configs, `env.sh` (the two tokens, mode 0600), the state key, the fake-server files, and pid files.
- `manifest/`: `berthy.yml`, the manifest that scenarios deploy from.
- `logs/`: the agent and fake logs of the current scenario. `setup.sh` clears them for each scenario.
- `runs/<scenario>/`: a copy of those logs, kept by `run.sh` after each scenario.
- `docker-config/`: an empty Docker CLI config, so the Windows credential helper is never asked.

Loopback ports: agent `127.0.0.1:19501`, fake Telegram `127.0.0.1:18901`, fake OTLP `127.0.0.1:18902`, and the
throwaway registry `127.0.0.1:15021`.

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
- `lib.sh`: settings, the `adm` and `ci` wrappers for the CLI, process helpers, and the assertions.
- `scenarios/`: one script per scenario. `00-smoke.sh` deploys v1, runs `exec`, lists the history, deploys v2 and rolls back.

## Add a scenario

Copy `scenarios/00-smoke.sh` to `scenarios/NN-name.sh` and keep its shape: source `../lib.sh`, drive `berthy` through
`adm` (admin token) or `ci` (agent token), assert with `expect_eq`, `expect_contains`, `expect_not_contains` or
`expect_code`, and end with `finish`. A Swarm service is named `<project>-<app>-<role>`, so the web role is
`e2e-web-main`. To change the manifest, pipe a new one into `write_manifest`; `setup.sh` gives each scenario a fresh one.

## What it never touches

- The service `p2test-api-web`, the containers `berthy-phase2-proxy` and `berthy-phase2-registry`, and the overlay network
  `berthy-phase2-lab`, which the e2e services only attach to.
- Any Swarm object not labelled `berthy.environment=e2e`, and any volume not named `e2e_*` or in a compose project `e2e*`.
- The source images `127.0.0.1:15001/berthy-phase2/web:*`. They are only tagged, and the e2e tags are removed again.
- Anything under `~/berthy-e2e/` outside `$E2E_ROOT`.

The harness never prints a token. Logs hold no tokens, and `grep -rF` over `logs/` is how to check that.
