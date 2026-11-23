# The todo example

The manifest and Compose files that the [user guide](../../docs/user-guide.md)
uses. They describe a small todo app as one project with two environments.
Both can live on the same agent. Each environment has its own overlay network
(`todo-dev`, `todo-prod`).

These setup instructions use Berthy v0.4.0 or later.

| App | Kind | What it is |
| --- | --- | --- |
| `api` | `image` | The web service, from the private image `ghcr.io/acme/todo-api`. Swarm service `todo-<environment>-api-web`, network alias `api-web`. |
| `postgres` | `compose`, `stateful` | The database, with a dump job and a volume backup. Compose project `todo-<environment>-postgres`, alias `todo-db`. |
| `redis` | `compose` | The cache. Compose project `todo-<environment>-redis`, alias `todo-redis`. |

| File | What it is |
| --- | --- |
| `berthy.yml` | The base manifest: apps, registry, migrations, hooks and jobs. |
| `berthy.dev.yml` | The dev overlay: debug logs. |
| `berthy.prod.yml` | The prod overlay: protected, two replicas, resource limits, backups, state backup, managed ingress, probes, peers, alerts and telemetry. |
| `deploy/postgres.yml` | The Compose file of the `postgres` app. It joins `${BERTHY_NETWORK}` as `todo-db`. |
| `deploy/redis.yml` | The Compose file of the `redis` app. It joins `${BERTHY_NETWORK}` as `todo-redis`. |

Check them without a server:

```bash
berthy config validate -e dev --manifest-dir examples/todo
berthy config validate -e prod --manifest-dir examples/todo
```

Both print `manifest valid`.

## Setup and deploy

Work from `examples/todo` so the manifest supplies the project. Setup writes
the CLI configuration and saves the admin token in the local credential store. If it cannot confirm a save, it prints the token; keep it privately and set it in `BERTHY_TODO_DEV_TOKEN` (or `BERTHY_TODO_PROD_TOKEN` for prod). Use `--show-token` to print it as well. The key copy
setup asks for is named `todo.dev.key` (or `todo.prod.key`).

```bash
cd examples/todo
mkdir -p ~/.config/berthy
berthy setup -e dev --host ops@10.0.0.5 --listen 10.0.0.5:7710 \
  --config ~/.config/berthy/config.json
export CFG=~/.config/berthy/config.json
```

Add prod on the same server with another setup run. It keeps the agent's
listen address and creates a second hosted environment with its own network:

```bash
berthy setup -e prod --host ops@10.0.0.5 --config $CFG
```

Set the secrets the guide lists, then deploy. Without an app name, deploy
orders by `depends_on` (postgres and redis, then api):

```bash
berthy deploy -e dev --config $CFG --version api=v1.0.1
```

Destroy confirms with the hosted environment name:

```bash
berthy destroy --project todo -e prod --confirm todo/prod --allow-recreate --dry-run
```

To deploy them for real, replace `acme`, the hostname `todo.example.com`, the
Cloudflare account id, the storage endpoint and bucket, the Telegram chat id
and the private addresses with your own, and set the secrets the guide lists.
The todo API image is not part of this repository: the guide only assumes an
image that serves `/health` and ships `/app/bin/migrate`, `/app/bin/warm-cache`
and `/app/bin/cleanup`.
