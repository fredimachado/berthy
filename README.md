# Berthy

A small deployment and operations agent for Docker servers. It deploys image
and Compose apps, shows live logs and metrics, runs health checks with alerts,
runs scheduled commands in app containers, and backs up Docker volumes to
object storage. People use it through a CLI and a terminal UI. AI agents use
the same CLI and an MCP server.

"Berthy" is a working name.

## Status

Phases 1 to 4 are built and verified: the read-only agent and CLI, image and
Compose deploys, managed ingress with Cloudflare Tunnel, health checks, alerts,
scheduled commands, volume and state backups, the terminal UI, the MCP server
and approvals for AI agents. Phase 5, `berthy setup`, which prepares a server
and installs the agent, is in progress. See the
[verification records](docs/phase-4-verification.md) for the evidence and what
it does not cover.

## Documents

- [User guide](docs/user-guide.md): every command, flag and manifest key,
  taught step by step by deploying and managing a sample todo app.
- [Todo example](examples/todo/): the manifest and Compose files the guide
  uses, a private image app with PostgreSQL and Redis.
- [v1 spec](docs/spec-v1.md): scope, architecture, deploy lifecycle, backups,
  guardrails, rollout plan and open questions.
- [Implementation plan](docs/implementation-plan-v1.md): phase boundaries and acceptance checks.
