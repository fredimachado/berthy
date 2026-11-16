# The todo example

The manifest and Compose files that the [user guide](../../docs/user-guide.md)
uses. They describe a small todo app on one server per environment:

| App | Kind | What it is |
| --- | --- | --- |
| `api` | `image` | The web service, from the private image `ghcr.io/acme/todo-api`. |
| `postgres` | `compose`, `stateful` | The database, with a dump job and a volume backup. |
| `redis` | `compose` | The cache. |

| File | What it is |
| --- | --- |
| `berthy.yml` | The base manifest: apps, registry, migrations, hooks and jobs. |
| `berthy.dev.yml` | The dev overlay: debug logs. |
| `berthy.prod.yml` | The prod overlay: protected, two replicas, resource limits, backups, state backup, managed ingress, probes, peers, alerts and telemetry. |
| `deploy/postgres.yml` | The Compose file of the `postgres` app. It joins the shared network `todo` as `todo-db`. |
| `deploy/redis.yml` | The Compose file of the `redis` app. It joins the shared network as `todo-redis`. |

Check them without a server:

```bash
berthy config validate -e dev --manifest-dir examples/todo
berthy config validate -e prod --manifest-dir examples/todo
```

Both print `manifest valid`. To deploy them for real, replace `acme`, the
hostname `todo.example.com`, the Cloudflare account id, the storage endpoint
and bucket, the Telegram chat id and the private addresses with your own, and
set the secrets the guide lists. The todo API image is not part of this
repository: the guide only assumes an image that serves `/health` and ships
`/app/bin/migrate`, `/app/bin/warm-cache` and `/app/bin/cleanup`.
