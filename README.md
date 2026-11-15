# Berthy

A small deployment and operations agent for Docker servers. It deploys image
and Compose apps, shows live logs and metrics, runs health checks with alerts,
runs scheduled commands in app containers, and backs up Docker volumes to
object storage. People use it through a CLI and a terminal UI. AI agents use
the same CLI and an MCP server.

"Berthy" is a working name.

## Status

Phase 1 provides a read-only agent and CLI. Later deployment, backup and TUI
features remain planned. See [Phase 1 verification](docs/phase-1-verification.md)
for the current evidence and a safe way to reproduce the checks.

## Documents

- [v1 spec](docs/spec-v1.md): scope, architecture, deploy lifecycle, backups,
  guardrails, rollout plan and open questions.
- [Implementation plan](docs/implementation-plan-v1.md): phase boundaries and acceptance checks.
