# Berthy v1 implementation plan

Deliver v1 on one Linux Docker server per environment. Use the same non-production environment throughout the build so each phase exercises the capabilities shipped in earlier phases. The [v1 spec](spec-v1.md) defines the product behavior and scope.

## Phase 1 · Read-only agent and CLI

- Build the Go binary, run the agent under systemd, and connect the CLI to its private HTTP API. Establish named environments, scoped tokens, text and JSON output, and streaming for logs and metrics.
- Discover existing Swarm services and Compose projects. Ship `ps`, `describe`, `logs`, `stats`, `df` and the first `doctor` checks.
- Prove the riskiest deploy assumption early. A controlled start-first Swarm update and rollback passed through an ordinary nginx ingress on the isolated WSL daemon; see [the experiment and its limits](phase-1-verification.md#swarm-ingress). Phase 2 must confirm its actual production ingress configuration and enforce its own deploy deadline.
- Resolve the spec's open questions about TLS, internal probes, schema rollback and required backups for protected deploys.

**Done when** the CLI can inspect a real server, rejects unauthorized access, and reports a Docker outage. Metrics agree with `docker stats`, and the agent's memory use is measured against the spec's 30 MB target. The traffic test establishes how Phase 2 will gate a rollout.

## Phase 2 · Image deploys from CI

- Load and validate the base manifest and environment overlay. Add SQLite for applied configuration, deploy history and audit records, plus encrypted secrets and private registry credentials.
- Deploy digest-pinned Swarm services from CI. Add the deploy lock, dry-run plans, health-gated rollout and drain, rollback, retention and prune. Support adopting an existing service without recreating it.
- Add one-off command hooks, the generic migration commands, and the CLI operations for waiting, restarting, scaling and running an ad hoc command. Enforce token scopes and reject mutable tags in protected environments.

**Done when** CI has deployed to a non-production environment for two weeks. A deliberately unhealthy release rolls back while the previous version keeps serving. A failed pre-deploy command blocks the rollout, and pruning preserves the volumes and images needed for recovery.

## Phase 2b · Managed ingress

Starts once Phase 2's image deploys work, because the connector is an image app and publishing is a step of an image deploy. It does not change Phase 2's own acceptance check.

- Add the `ingress.cloudflare` block and per-role `hostnames` to the manifest. Find or create the remotely managed tunnel through Cloudflare's API, fetch its token into the secret store, and run the connector as a health-gated Swarm service on the shared network.
- Sync routes and proxied DNS records from the manifest, in `ingress apply` and as the publish step of an image deploy. Keep every rule and record Berthy did not create, and stop on a conflict unless an admin replaces it. Add `ingress status` and the `doctor` check.
- Confirm the connector's readiness command against the pinned connector version before relying on it as a health check.

**Done when** a new app is published on a new hostname by a manifest change and a deploy alone. An existing tunnel is adopted with its routes unchanged and no dropped requests while the hand-started connector is replaced. A hostname already used by another record stops the deploy before anything changes, and removing a hostname removes only what Berthy created.

## Phase 3 · Compose, monitoring and data protection

### 3a · Compose and alerting

- Deploy and adopt Compose projects by name. Reuse the manifest, secret and registry handling. Preview service and volume changes and enforce the stateful recreation guard.
- Add scheduled HTTP and TCP probes, built-in checks, peer checks and Telegram alerts. Include the unhealthy-tunnel check for environments with managed ingress. Wire up optional OpenTelemetry export. Verify that probes against service names can reach their targets on the shared network.

**Done when** Berthy adopts a running Compose project with its existing data, blocks an unauthorized stateful recreation, and sends failure and recovery alerts for a test probe.

### 3b · Scheduled container commands

- Load UTC cron schedules from the last applied manifest. Resolve image roles and Compose services to running containers, run each command once per occurrence, and report exit status, failures and overlapping runs.

**Done when** a job runs once against a replicated image role and another runs in a Compose service. The agent resumes scheduling after a restart, and an intentional job failure raises an alert.

### 3c · Volume backup and recovery

- Back up named volumes from either app type to an S3-compatible destination. Support scheduled and manual runs, integrity checks, retention, failure and overdue alerts, and restore into a new volume.
- Connect optional preparation jobs and the `backup:` deploy hook. A failed preparation must stop the backup, and a failed pre-deploy backup must stop the deploy before migrations.
- Back up the agent's SQLite state and verify recovery with the separately stored encryption key.

**Done when** an image app's file volume restores with its data and permissions intact, and a PostgreSQL dump produced in a Compose service restores into a scratch database. An interrupted upload leaves no successful backup record, and a failing backup hook stops the deploy before migrations.

## Phase 4 · TUI and MCP

- Build the terminal views and confirmation flows on the same agent API the CLI uses. Expose the v1 operations through MCP and check coverage against the CLI.
- Implement pending approvals, expiry and approval execution for agent requests in protected environments. Use the same dry-run plan for CLI, TUI and MCP actions.

**Done when** an AI agent can inspect a protected environment and request a change. The environment stays untouched until a person approves the reviewed plan, and the result appears in the audit log. An expired request cannot run.

## Phase 5 · Server setup

- Add `berthy setup`, over SSH and with `--local`. It checks the server, installs Docker when missing, turns on Swarm mode, creates the shared network, installs or upgrades the agent under systemd and creates the first `admin` token. It supports `--dry-run`, can be run again safely and deploys nothing.
- Until this phase, the agent is installed by hand from the systemd unit in the repository.

**Done when** `berthy setup` alone has brought a fresh server to a passing `doctor`, and a second run changed nothing. A run against a server with running apps added only the agent, and a run with a saved encryption key let the rebuilt agent read its restored state.

## v1 release gate

Run the adoption and recovery checklist on the least critical environment. Hand over automatic deploys app by app, and keep the old backups running until a Berthy backup has been restored and verified. Rebuild a test host from provisioning, `berthy setup` with the saved encryption key, the saved agent state and the volume backups. Verify the recovered apps and confirm deploys, monitoring, alerts, scheduled commands and backups work end to end.

Multi-node operation remains an optional follow-on.
