# Phase 4 verification and operator notes

This records what the Phase 4 checks showed on the isolated Swarm fixture. The end-to-end checks E1 to E7 ran on 2026-11-16
against commit `8649d6e`, on the fixture described under "Run record". The MCP, TUI and TUI action results come from earlier
runs on the same isolated daemon, with the same fixture scripts. They are recorded as they were observed and were not re-run for
this document. The fixture used a real Docker Swarm service, the fixture registry and the agent. Nothing was faked. What it does
not prove is listed under "Not verified here".

## Run record

- Berthy commit `8649d6e` on `main`. The working tree was clean when the fixture binaries were built, from a copy of the tree
  without `.git`, with `CGO_ENABLED=0`.
- Toolchain: Go 1.25.8, the version in `go.mod`. The Go on the WSL path is 1.25.4, which refuses this module with
  `GOTOOLCHAIN=local`. With the default `GOTOOLCHAIN=auto` the 1.25.8 toolchain from the module cache built and tested it.
  `go version -m` reads go1.25.8 in the `berthy`, `mcpdriver` and `tuidriver` binaries.
- Binary: `berthy`, 50022895 bytes, sha256 prefix `9a527e6a15dc`. The MCP driver is 11227891 bytes.
- Daemon: Docker Engine 29.7.2, Compose v5.5.1, single-node Swarm (active, control available) on
  `unix:///var/tmp/berthy-phase2/docker.sock`. The fixture registry runs on `127.0.0.1:15001`.
- Host: Ubuntu 20.04.6 LTS on WSL2, kernel 6.18.58.1-microsoft-standard-WSL2.
- Checks:
  - `go vet ./...` passes. Cross vet passes for windows/amd64 and darwin/arm64 (`internal/tui`, `internal/cli`,
    `test/tuidriver`, `internal/agentclient`).
  - `gofmt -l .` prints nothing.
  - `go test -race -count=1 ./...` in WSL, on a copy of the tree, passes: 30 packages ok, 2 without test files (`cmd/berthy`,
    `test/phase1`), no race reports. It took 2 min 30 s.
  - Native Windows `go test ./...` with default parallelism failed: the Go linker ran out of commit memory while it built the
    test binaries of `internal/agent`, `internal/cli`, `internal/engine` and `internal/tui` (exit status 0xc000012d). Run again
    as `go test -p 2 ./...`, it passes: every package is ok, some from the Go test cache for unchanged inputs.
- Times, UTC, 2026-11-16: static checks from 10:20; fixture setup 10:24 to 10:28; E1 to E6 10:29 to 10:35; token check 10:35;
  cleanup 10:37 to 10:38.

## MCP server (earlier run, P4-2, commit 71884dd)

| Check | Result |
| --- | --- |
| The agent token's MCP deploy of v4 was held, the service was unchanged, and one approval was pending | Pass |
| A restart dry run through MCP returned the plan | Pass |
| A deploy without a version gave a tool error that names the `version` argument, not the CLI flag | Pass |
| Admin `berthy approve` deployed v4, and `list_approvals` showed it approved | Pass |
| 0 token occurrences in the results | Pass |

## TUI views (earlier run, P4-3, commit cc5916e)

| Check | Result |
| --- | --- |
| At 100x30 on the protected environment, the status line showed "1 pending" | Pass |
| The app detail showed a rolled-back deploy with its reason | Pass |
| Logs followed live, and the logs tab opened the selected app | Pass |
| The server tab updated live | Pass |
| The approval detail showed the wrapped digest and a countdown | Pass |
| The full help opened, and `q` exited with 0 | Pass |
| 0 token hits in 33 checks | Pass |

## TUI actions (earlier run, P4-4, commit 8649d6e)

| Check | Result |
| --- | --- |
| Restart from Apps: plan, confirm, follow, "Restarted api in 10s", tasks replaced | Pass |
| Roll back from the detail: picker, from and to, a wrong name refused, typed `p4c`, done, history row | Pass |
| Approve an agent's held deploy: plan, countdown, typed environment name, "Deployed api", audit "claude (approved by admin)" | Pass |
| Reject with a reason: recorded as "change freeze until monday" | Pass |
| Esc at the confirmation changed nothing: version, update time, task ids and history count were identical | Pass |
| A read token got "this token needs the deploy scope", and nothing changed | Pass |
| An agent token in the TUI got "Held for approval <id>" with the approve command | Pass |
| 80x24 confirmation and picker | Pass |
| Reviewer's own 80x24 run: wrong-case `P4C` refused, `p4c` accepted, esc during follow left "operation <8 chars> continues on the agent" with the lock and pending still shown, and the deploy completed | Pass |
| 0 token hits in 70 files | Pass |

## Phase 4 "Done when" (this run)

The Phase 4 "Done when" says: an AI agent can inspect a protected environment and request a change; the environment stays
untouched until a person approves the reviewed plan; the result appears in the audit log; an expired request cannot run.

The ids below are shortened to 8 characters and the digests to 12. The service ran `sha256:332ea9fe0f97` (v3) before E2.
`sha256:dd3efc15a599` is v4. `sha256:f6a22c9b07a4` is v1. All times are UTC.

| Scenario | What was done | Observed evidence | Result |
| --- | --- | --- | --- |
| E1 Inspect | The agent token, through MCP: `list_resources`, `describe_resource` (api), `list_console_logs` (api, tail 20), `get_stats`, `list_deploys` (api), `list_approvals`, `doctor` | 7 calls, no tool error, structured content on each. `list_resources` showed `api` and `ticker` as images, and `p2test-api-web` as kind `swarm`. `doctor`: 0 of 6 checks failing. Pending 0. The service state, the history (3 rows) and the image were identical before and after | Pass |
| E2 Request | The agent token, through MCP: `deploy` api to v4, then a `restart` with `dry_run` | The deploy returned `held for approval`, approval `adc9cf07`, expires 10:30:49, plan image `sha256:dd3efc15a599`. The dry run returned its plan and held nothing. Pending 1. The service state, history, image and lock (`locked: false`) were identical before and after | Pass |
| E3 Approve in the TUI | The admin token, TUI at 120x40: Approvals, `enter`, `a`, typed `p4c`, `enter`, `y` | The detail showed the plan with the full v4 digest (wrapped over two lines). The confirmation asked for the environment name. Done: "Deployed api in 11s". The service ran v4. History 3 to 4. Pending 0 | Pass |
| E4 Audit log | `berthy audit` with the admin token; then `list_approvals` and `list_deploys` through MCP with the agent token | Audit, oldest first: `10:29:49 approval_request:deploy api sha256:dd3efc15a599` by `claude`; `10:30:00 approval_approve` by `admin`; `10:30:00 deploy` by `claude (approved by admin)`, accepted; `10:30:11` succeeded. MCP: the request is `approved`, decided by `admin`. `list_deploys` shows the v4 digest | Pass |
| E5 Expired request | The agent asked for v1 (approval `b50b0705`, expires 10:33:23). The wait polled the listing until the expiry plus 10 s. TUI at 120x40: Approvals, `enter`, `a` | The list showed `expired` and the status line showed 0 pending. The detail showed "expired by berthy". `a` opened nothing. `berthy approve b50b0705` was refused with `agent returned HTTP 409: approval expired`, exit 1. The service state was identical before and after the refusal and the image stayed v4. `approvals --all`: `expired`, by `berthy`. Audit: `approval_expired` by `berthy` at 10:33:30. MCP `list_approvals`: `expired` | Pass |
| E6 Reject from the CLI | The agent asked for v1 (approval `1802a7a7`). Admin: `berthy reject 1802a7a7 -m "not during the demo"` | Reject exit 0, "rejected". MCP `list_approvals`: `rejected`, decided by `admin`, result "not during the demo". The service state and image were identical before and after | Pass |
| E7 Token check | Every value and secret part of the four fixture tokens, searched in the results (64 files), the driver logs and the agent log (11 files), then again in the copy in the scratchpad (74 files) | 0 hits. A positive control with the same search found each planted value in its file | Pass |

Findings from this run:

- No product bug was found in E1 to E7. Nothing was changed to make a check pass.
- `berthy approve` prints the plan's list items without indentation: `roles:` is followed by `web` at column 0. This is cosmetic.
- `list_resources` lists `p2test-api-web` as kind `swarm`. It is an unmanaged Swarm service of another fixture on the same daemon.
  The agent reports it as it reports other Swarm services. The fixture did not touch it.
- `doctor` reports host, disk, docker, docker_api, swarm_manager and compose. The spec's CLI table also names the shared network,
  the lock and peers, which it does not report yet. This is recorded for Phase 5, whose acceptance check is `doctor`.

## MCP coverage against the CLI

The MCP server registers the 11 tools in the spec's MCP table. Its `execute_resource_command` takes the commands `deploy`,
`rollback`, `restart`, `backup_run` and `restore` (`internal/mcp/server.go`). The CLI list is `cmd/berthy` and `internal/cli`
(`cli.go`, `deploy.go`, `approvals.go`, `backups.go`, `jobs.go`, `statebackup.go`).

| CLI command | MCP tool, or why it is not exposed |
| --- | --- |
| `ps` | `list_resources` |
| `describe <app>` | `describe_resource` |
| `logs <app>` | `list_console_logs`. It has no `--replica` filter. `--follow` is not exposed, since MCP answers are snapshots |
| `stats` | `get_stats`. `--watch` is not exposed (snapshot only) |
| `df` | `get_stats` (the `df` part of its result) |
| `history <app>` | `list_deploys` |
| `diagnostics <operation id>` | `get_deploy_diagnostics` |
| `wait <app> --healthy` | `wait_for_resources` |
| `doctor` | `doctor` |
| `approvals [--all]` | `list_approvals` (it lists every status, like `--all`) |
| `deploy <app> --version` | `execute_resource_command` `deploy` |
| `rollback <app>` | `execute_resource_command` `rollback` |
| `restart <app>` | `execute_resource_command` `restart` |
| `backup list` | `list_backups` |
| `backup run <name>` | `execute_resource_command` `backup_run` |
| `restore <key> --into <volume>` | `execute_resource_command` `restore` (admin scope) |
| `audit` | Not in the v1 tool set |
| `checks` | Not in the v1 tool set |
| `jobs` | Not in the v1 tool set |
| `ingress status` | Not in the v1 tool set |
| `lock status` | Not in the v1 tool set |
| `agent-info` | Not in the v1 tool set |
| `config show`, `config validate` | Not exposed. They read the local manifest, not the agent API |
| `scale <app>` | Not exposed: a change outside the v1 tool set. An AI agent can deploy, roll back, restart, back up and restore; the rest stays with people and the CLI |
| `exec <app> -- cmd` | Not exposed: a change outside the v1 tool set (admin scope). An AI agent can deploy, roll back, restart, back up and restore; the rest stays with people and the CLI |
| `prune` | Not exposed: a change outside the v1 tool set. An AI agent can deploy, roll back, restart, back up and restore; the rest stays with people and the CLI |
| `remove <app>`, `destroy` | Not exposed: a change outside the v1 tool set (admin scope). An AI agent can deploy, roll back, restart, back up and restore; the rest stays with people and the CLI |
| `lock acquire`, `lock release` | Not exposed: a change outside the v1 tool set. An AI agent can deploy, roll back, restart, back up and restore; the rest stays with people and the CLI |
| `ingress apply` | Not exposed: a change outside the v1 tool set. An AI agent can deploy, roll back, restart, back up and restore; the rest stays with people and the CLI |
| `job run <name>` | Not exposed: a change outside the v1 tool set. An AI agent can deploy, roll back, restart, back up and restore; the rest stays with people and the CLI |
| `alerts test` | Not exposed: a change outside the v1 tool set. An AI agent can deploy, roll back, restart, back up and restore; the rest stays with people and the CLI |
| `state-backup` | Not exposed: a change outside the v1 tool set (admin scope). An AI agent can deploy, roll back, restart, back up and restore; the rest stays with people and the CLI |
| `secret set`, `secret list`, `secret rm` | Not exposed: a change outside the v1 tool set (admin scope). An AI agent can deploy, roll back, restart, back up and restore; the rest stays with people and the CLI |
| `recovery resolve` | Not exposed: a change outside the v1 tool set (admin scope, run by a person). An AI agent can deploy, roll back, restart, back up and restore; the rest stays with people and the CLI |
| `approve <id>`, `reject <id>` | Not exposed. People only: they need an admin token that is not an agent token |
| `agent token` | Not exposed. It makes a token locally and prints it once, for a person |
| `state-restore` | Not exposed. It runs locally, with no agent, from object storage |
| `ui` | Not exposed. The interactive terminal interface, built on the same agent API |
| `mcp` | This is the server itself |
| `agent` (run the server) | Not exposed. It is the agent process |
| `setup` | Not in this build. It is planned for Phase 5 (`docs/implementation-plan-v1.md`) |
| `probe-helper`, `volume-archive`, `volume-extract` | Not user commands. They run only inside the containers Berthy starts |

The tool set is the spec's. Every change it offers is held on a protected environment. The read-only commands without a tool are
candidates for a later version.

## Not verified here

- A real MCP client (for example Claude Desktop or Claude Code) connected over stdio. The driver speaks the same protocol through
  the official Go SDK client.
- The TUI on real terminals other than the VT emulator (Windows Terminal, macOS Terminal, iTerm2), light backgrounds and
  `NO_COLOR`, by eye.
- MCP `backup_run` and `restore` against real S3.
- The default approval expiry of 15 minutes. This run set `approval_ttl` to `1m` so the expiry check was short.
- An agent restart while an approval is pending.

## Operator notes

Point an MCP client at the server with the CLI configuration and the environment variable that holds the token. The token is
never written to the client's file. The placeholder paths below need to be replaced:

```json
{
  "mcpServers": {
    "berthy": {
      "command": "berthy",
      "args": ["mcp", "-e", "prod", "--config", "/path/to/berthy-cli.json", "--manifest-dir", "/path/to/manifests"],
      "env": { "BERTHY_PROD_TOKEN": "${BERTHY_PROD_TOKEN}" }
    }
  }
}
```

- The agent token needs the `deploy` scope to request a change, and it must be marked as an agent token. Make it with
  `berthy agent token -e prod --agent --name claude --scope deploy` and put the printed object in the agent's `tokens` list. On a
  protected environment, the agent's changes are then held for a person. Use `--scope read` for an agent that only looks.
- `approval_ttl` is set in the agent's configuration as a Go duration, for example `"approval_ttl": "15m"`. 15 minutes is the
  default when it is not set, and the minimum is 1 minute.
- A person approves in one of three places, each with an admin token that is not an agent token:
  - `berthy approvals` lists the pending requests, and `berthy approvals --all` lists every status. The plan is shown only to a
    token with the `deploy` scope or higher.
  - `berthy approve <id>` shows the plan, carries it out as the agent and follows it. `berthy reject <id> -m "reason"` refuses it.
  - The TUI Approvals tab: `enter` opens the plan, `a` approves and `x` rejects. An expired request does not offer `a`.

## Rerun

The fixture scripts are kept in the session scratchpad and are not part of the repository. The steps are: build the CLI and the
MCP and TUI drivers from the tree; create the fixture with its tokens, agent configuration (with `approval_ttl` `1m`) and manifest;
start the agent; store the registry secret; deploy the fixture apps; then run E1 to E7 in order. E2 and E3 must run within the
approval's lifetime, which is one minute in this fixture.
