# Berthy

Berthy deploys and runs apps on your own Docker servers. One Go binary is the agent on the server, the CLI on your laptop and in CI, a terminal UI, and an MCP server for AI agents. You describe your apps in a manifest in your repository, CI builds and pushes the images, and Berthy deploys them: zero-downtime image rollouts, automatic rollback, health checks and alerts, scheduled commands, volume backups, and approvals for changes that AI agents ask for.

It is for a small team that runs a web application on its own servers, one server per environment. It has no web dashboard and builds no images.

Berthy was built to replace a Dokploy setup, and its author is moving all their own projects from Dokploy to Berthy. [How it compares](#how-it-compares) with Dokploy, Kamal, Coolify, Dokku and CapRover is below, including when you should pick one of them instead.

## Berthy is opinionated

Berthy makes choices so that you do not have to. If you disagree with several of these, another tool in the comparison will suit you better.

- **One server per environment.** `prod` and `dev` are different servers, each with its own agent, secrets and tokens. There is no central server.
- **Swarm for stateless apps, Compose for stateful ones.** Image apps run as Docker Swarm services, even on one server, for rolling updates and rollback. Databases and caches are Docker Compose apps, and a `stateful` one is never recreated unless a command names it.
- **CI builds, Berthy deploys.** Berthy never builds an image. It deploys images that already exist in a registry, pinned by digest.
- **Protected environments take digests, not tags.** A tag can move. On a `protected` environment, only a `sha256:` digest is accepted.
- **The manifest lives in the app's repository.** YAML: a base file plus one overlay per environment. Every deploy sends the manifest of the commit it deploys. Nothing is configured by hand on the server.
- **A private agent API.** The agent listens on one private address (a VPN such as WireGuard or Tailscale) and every request carries a scoped token. It is never meant to be on the public internet.
- **Secrets stay in the agent.** They are encrypted on the server, write-only from the CLI, and the manifest names them without holding values.
- **No bundled proxy.** Public traffic enters through a Cloudflare Tunnel that Berthy can manage for you (tunnel, connector, hostnames and DNS records), or through an ingress app you run yourself.
- **AI agents are first-class users.** They get their own tokens. On a protected environment, what an agent asks for is held until a person approves it.

## What you get

- Zero-downtime image deploys: the new version starts beside the old one, must pass its health check, then takes traffic. A failed rollout rolls back to the previous digest by itself.
- `berthy deploy` with no app deploys every app of the manifest in `depends_on` order.
- Release history with the commit and the requested tag, and `berthy rollback` to any recorded release.
- Migration and hook steps around a deploy, and a deploy lock.
- Live logs (with per-replica filtering), CPU, memory and disk metrics, and OpenTelemetry export.
- Health probes and alerts to Telegram.
- Scheduled commands inside app containers.
- Volume backups to S3-compatible storage with restore, and snapshots of the agent's own state.
- `berthy setup` installs Docker, Swarm and the agent on a bare Linux server over SSH.
- A terminal UI (`berthy ui`) for apps, logs, server metrics and approvals, with restart, rollback and approve from the keyboard.
- An MCP server (`berthy mcp`) with the same permissions and the same holds as the CLI.

## Quick start

You need a Linux server with systemd (amd64 or arm64), a private address on it, and an account that is `root` or has passwordless `sudo`. Setup does not create the server, its firewall or your VPN. Read the [user guide](docs/user-guide.md#step-1-prepare-a-server-and-set-it-up) before you point it at a server you care about.

**1. Install the CLI** from the [release](https://github.com/fredimachado/berthy/releases/tag/v0.1.0). The files are named `berthy-<os>-<arch>` for linux and darwin on amd64 and arm64, plus `berthy-windows-amd64.exe` and `checksums.txt`.

```bash
VERSION=v0.1.0 OS=linux ARCH=amd64     # OS: linux or darwin. ARCH: amd64 or arm64.
curl -fsSLO https://github.com/fredimachado/berthy/releases/download/$VERSION/berthy-$OS-$ARCH
curl -fsSLO https://github.com/fredimachado/berthy/releases/download/$VERSION/checksums.txt
sha256sum --ignore-missing -c checksums.txt    # on macOS, compare the hash with shasum -a 256
chmod +x berthy-$OS-$ARCH && sudo mv berthy-$OS-$ARCH /usr/local/bin/berthy
berthy version
```

With the GitHub CLI: `gh release download v0.1.0 -R fredimachado/berthy -p 'berthy-linux-amd64' -p checksums.txt`.

**2. Set up the server.** Look first with `--dry-run`, which changes nothing, then run it for real. It prints the first admin token once, so store it.

```bash
mkdir -p ~/.config/berthy
export CFG=~/.config/berthy/config.json
berthy setup -e prod --host ops@10.0.0.5 --listen 10.0.0.5:7710 --config $CFG --dry-run
berthy setup -e prod --host ops@10.0.0.5 --listen 10.0.0.5:7710 --config $CFG
export BERTHY_PROD_TOKEN=<the admin token setup printed>
```

Also copy the encryption key off the server, as the setup output says. Without it, a lost server means lost secrets.

**3. Write a manifest** in your repository. `berthy.yml`:

```yaml
project: hello
network: hello

registries:                      # only for a private image
  ghcr.io:
    username: acme
    password_secret: GHCR_TOKEN

apps:
  web:
    kind: image
    image: ghcr.io/acme/hello    # the repository only; you pick the version at deploy time
    roles:
      web:
        replicas: 2
        port: 8080
        health:
          test: ["CMD", "curl", "-f", "http://localhost:8080/health"]
          interval: 15s
          start_period: 30s
    env:
      plain:
        LOG_LEVEL: info
      secret:
        - DATABASE_URL
```

`berthy.prod.yml`, the overlay for the `prod` environment:

```yaml
protected: true
```

Check both without a server: `berthy config validate -e prod`.

**4. Set the secrets.** Values are read from standard input, so they stay out of your shell history.

```bash
read -rs PAT && printf '%s' "$PAT" | berthy secret set GHCR_TOKEN --registry ghcr.io -e prod --config $CFG; unset PAT
read -rs DB  && printf '%s' "$DB"  | berthy secret set DATABASE_URL -e prod --config $CFG; unset DB
```

**5. Deploy.** `prod` is protected, so give the image digest that CI printed, not a tag.

```bash
berthy deploy web -e prod --config $CFG --version sha256:<64 hex characters> --dry-run
berthy deploy web -e prod --config $CFG --version sha256:<64 hex characters> --commit "$(git rev-parse HEAD)"
berthy ps -e prod --config $CFG
berthy ui -e prod --config $CFG
```

The [user guide](docs/user-guide.md) goes on from here with a todo app that has a database and a cache, CI deploys, public hostnames, alerts, backups, protected environments and AI agents. The finished files are in [`examples/todo/`](examples/todo/), and a runnable app with its CI is in [berthy-examples](https://github.com/fredimachado/berthy-examples).

## How it compares

Berthy's author wrote this section, so read it with that in mind. Every cell below comes from the project's own documentation or repository, checked on 2026-11-17. "—" means the documentation checked does not say, so treat it as unknown, not as "no". Versions change; follow the links under the tables.

### What they are

| | Berthy | Kamal | Dokploy | Coolify | Dokku | CapRover |
| --- | --- | --- | --- | --- | --- | --- |
| Interface | CLI, terminal UI, MCP | CLI | Web UI, CLI, API, MCP | Web UI, API, CLI, MCP | CLI over SSH. A web UI only in the paid Dokku Pro | Web UI, CLI, API |
| What runs on the server | Berthy's own agent (systemd), plus Docker | Docker and a kamal-proxy container. Kamal itself runs on your machine and works over SSH | A control plane (web app, PostgreSQL, Traefik) | A Coolify instance, which reaches servers over SSH | Dokku on the host | CapRover, nginx and Docker Swarm |
| Where images are built | In your CI. Berthy only deploys | On your machine or a remote builder, pushed to a registry | On the server or a build server (Nixpacks, Railpack, Dockerfile, buildpacks), or an existing image | On the server or a build server (Nixpacks, Railpack, Dockerfile, Compose), or an existing image | On the server (Dockerfile or buildpacks), or from an image | On the server from a `captain-definition`, or an existing image |
| Orchestration | Docker Swarm for image apps, Compose for stateful apps | Plain Docker over SSH | Docker Swarm, Compose or Stack | Docker and Compose. Swarm is deprecated | Plain Docker on one server. An optional k3s scheduler | Docker Swarm |
| Databases | Compose apps marked `stateful`, from your own Compose file | Accessories, managed separately from the app | Managed MySQL, PostgreSQL, MongoDB, MariaDB, libSQL, Redis | One-click databases | Plugins | Deployed as apps or one-click apps |
| Multi-server | One server per environment. A Swarm cluster is not set up by Berthy yet | Yes, a list of hosts | Remote servers, or Swarm nodes | Yes, standalone servers. Swarm is deprecated | One server, or the k3s scheduler | Yes, Swarm nodes |
| License | MIT | MIT | Apache-2.0, except a proprietary directory | Apache-2.0 | MIT | Apache-2.0 with an appendix |
| Written in | Go, one binary | Ruby | TypeScript | PHP | Shell | TypeScript |

### How they run things

| | Berthy | Kamal | Dokploy | Coolify | Dokku | CapRover |
| --- | --- | --- | --- | --- | --- | --- |
| Zero-downtime deploys | New version starts first and must pass its health check | kamal-proxy switches traffic once the new container answers `GET /up` | With a health check you configure, via Swarm | Rolling update. Not for Compose apps, and no guarantee | Health checks, then traffic switches | Documented, with health checks and update settings |
| Rollback | Automatic when a rollout fails. `berthy rollback` to any recorded release | `kamal rollback VERSION`, from old containers kept for 3 days by default | Swarm rollback on failed health checks, and manual to a stored image | Manual, to a retained image | — | Revert in Version History |
| Secrets | Encrypted in the agent, write-only | `.kamal/secrets`, with 1Password, LastPass and Bitwarden helpers | Environment variables, shared or per service, and external secret managers | Environment variables | Environment variables | Environment variables |
| Public ingress and TLS | No proxy. Optional Cloudflare Tunnel managed by Berthy | kamal-proxy, with Let's Encrypt on a single server | Traefik. Cloudflare Tunnel by a manual guide | Traefik (default), Caddy, or your own. Cloudflare Tunnel by a manual guide | nginx by default. Caddy, HAProxy, OpenResty and Traefik also documented | nginx with Let's Encrypt |
| Backups | Volumes to S3-compatible storage on a schedule, restore, agent state snapshots | — | Database backups to S3 destinations, and volume backups | Scheduled database and volume backups to S3 and others | A documented manual procedure | Documented manual procedures |
| Health checks and alerts | Probes and built-in conditions. Alerts to Telegram only | — | Notifications of deploy results (Slack, Discord, Telegram, email) | Monitoring and notifications (email, Discord, Telegram, Slack, Pushover, webhook) | — | NetData monitoring, with optional alerts |
| AI agent access | MCP server. Agent tokens, and changes held for approval on protected environments | — | MCP server, with the permissions of its API key | MCP server, with the permissions of its API token | — | — |

Main documentation and sources:

- Kamal: [kamal-deploy.org](https://kamal-deploy.org/docs/installation/), [repository](https://github.com/basecamp/kamal)
- Dokploy: [docs.dokploy.com](https://docs.dokploy.com/docs/core), [repository](https://github.com/Dokploy/dokploy)
- Coolify: [coolify.io/docs](https://coolify.io/docs), [repository](https://github.com/coollabsio/coolify)
- Dokku: [dokku.com/docs](https://dokku.com/docs/getting-started/installation/), [repository](https://github.com/dokku/dokku)
- CapRover: [caprover.com/docs](https://caprover.com/docs/get-started), [repository](https://github.com/caprover/caprover)

### Which one to choose

- **Choose Berthy when** CI already builds your images, you run one server (or a few, one per environment) behind a VPN, and you want digest-pinned, health-gated deploys, backups, scheduled jobs and AI-agent approvals driven from a CLI and a terminal UI, in one small binary. Berthy is young: this is v0.1.0.
- **Do not choose Berthy when** you want a web dashboard, builds on the server (buildpacks, Nixpacks, `git push`), users and roles for a team beyond token scopes, one-click templates, a multi-server cluster set up and shown for you, or an alert channel other than Telegram. It also needs a private network to reach the agent.
- **Choose Kamal when** you want deploys over plain SSH with nothing but Docker on the servers, builds in your own pipeline, and several hosts in one config file. It runs from your machine over SSH, with no agent of its own on the server.
- **Choose Dokploy when** you want a web UI with builds on the server, managed databases with backups, templates, notifications, and Swarm or remote servers.
- **Choose Coolify when** you want the broadest self-hosted PaaS: web UI, teams, 300+ one-click services, databases with scheduled backups, monitoring and many notification channels.
- **Choose Dokku when** you want the smallest Heroku-style tool on one server: `git push` to deploy, buildpacks, a plugin for each datastore, and nothing else to run.
- **Choose CapRover when** you want a web dashboard on top of Docker Swarm with one-click apps and a simple multi-node cluster.

## Documentation

- [User guide](docs/user-guide.md): every command, flag and manifest key, taught by deploying and managing a todo app.
- [Todo example](examples/todo/): the manifest and Compose files the guide uses.
- [berthy-examples](https://github.com/fredimachado/berthy-examples): a small app with its code, manifests, CI and release pipeline.
- [v1 spec](docs/spec-v1.md): scope, architecture, deploy lifecycle, backups, guardrails and open questions.
- [Implementation plan](docs/implementation-plan-v1.md) and the verification records: [phase 1](docs/phase-1-verification.md), [2](docs/phase-2-verification.md), [2b](docs/phase-2b-verification.md), [3a](docs/phase-3a-verification.md), [3c](docs/phase-3c-verification.md), [4](docs/phase-4-verification.md), [5](docs/phase-5-verification.md). Each says what it did not cover.

## Status

The first release is [v0.1.0](https://github.com/fredimachado/berthy/releases/tag/v0.1.0). Built and released: the agent and CLI, image and Compose deploys, managed ingress, health checks, alerts, scheduled commands, backups, the terminal UI, the MCP server, approvals, and `berthy setup`.

Berthy's tests run against a real Docker Swarm, a real registry and fakes for the rest. On 2026-11-17 it was also run against a real Ubuntu server, real Cloudflare and GHCR. Telegram alerts, OpenTelemetry backends and S3 backups have not yet been run against the real services. The user guide lists the rest under "Not verified against real services". Expect to find edge cases, and try a non-production environment first.

## License

MIT. See [LICENSE](LICENSE).
