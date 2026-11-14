# Berthy

A small deployment and operations agent for Docker servers. It deploys image
and Compose apps, shows live logs and metrics, runs health checks with alerts,
and backs up Postgres to object storage. People use it through a CLI and a
terminal UI; AI agents use the same CLI and an MCP server.

"Berthy" is a working name.

## Status

Design phase. There is no code yet.

## Documents

- [v1 spec](docs/spec-v1.md): scope, architecture, deploy lifecycle, backups,
  guardrails, rollout plan and open questions.
