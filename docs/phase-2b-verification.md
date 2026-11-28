# Phase 2b verification and operator notes

This records one run of the managed ingress checks on the isolated Swarm fixture. The run used a fake Cloudflare API
(`test/fakecloudflare`, backed by `internal/cloudflare/cftest`) and a stand-in connector (`test/fakecloudflared`), plus
one run of the real connector image. It is not an acceptance test against Cloudflare. The checklist under "To verify
with real Cloudflare" is still to be done, and the results below do not establish it.

## Run record

- Berthy commit `eaa380d` on `main`. Uncommitted, and outside the production packages: `internal/cloudflare/cftest/fake.go`
  (additions only), `internal/cloudflare/cftest/fake_test.go`, `test/fakecloudflare/` and `test/fakecloudflared/`.
- Binaries (static, linux/amd64, `CGO_ENABLED=0`): `berthy` sha256 `1c37bb83…`, `fakecloudflare` `16749ab3…`,
  connector stand-in `e407c485…`.
- Daemon: Docker Engine 29.7.2 on an isolated socket, one Swarm manager, Ubuntu on WSL2. Overlay network
  `berthy-phase2-lab`. Private registry at `127.0.0.1:15001`, which needs a login.
- Dates: 2026-11-15, 14:08 to 14:25 UTC.
- Images: connector stand-in pinned as `sha256:597a507fd6c3…`; real `cloudflare/cloudflared:2026.11.0` pushed as
  `sha256:bf723075c9a1…`. Test app: `berthy-schema/app` v1 `sha256:f6a22c9b07a4…` and v3 `sha256:332ea9fe0f97…`.
- Checks: `go vet ./...` and `go test -count=1 ./...` pass in WSL. `gofmt` is clean. `go vet` is clean for the new
  packages on linux/amd64, linux/arm64, windows/amd64 and darwin/arm64.
- Fake Cloudflare zones: `example.com` and `example.net`. The account id and token are fixture values.

## Results

| # | Scenario | Result | Evidence |
| --- | --- | --- | --- |
| 0 | Snapshot | Pass | `p2test-api-web` unchanged. The registry and proxy were started with `docker start`, not recreated. |
| 1 | `ingress apply --dry-run`, deploy token | Pass | Plan: create tunnel `p2bing-p2b`, connector plan, no route or record changes. Fake writes: 0. |
| 2 | First apply | Pass | Tunnel created, remotely managed. Config holds only the 404 rule. `p2bing-ingress-connector` 1/1, healthy. Args `tunnel --no-autoupdate --metrics 0.0.0.0:20241 run`; health check `cloudflared tunnel --metrics 127.0.0.1:20241 ready`. Environment variable names: `TUNNEL_TOKEN` only. `logs ingress` has no token text. `ingress status`: configured. |
| 3 | Deploy `api` | Pass | Rule `api.example.com` → `http://p2bing-api-web:8080` ahead of the 404 rule. Proxied CNAME to `<tunnel>.cfargotunnel.com`, comment `managed by berthy (p2b)`. Config PUT and record create at 14:10:10.00, after the api container started at 14:10:04.76. The first healthy check was not captured, because Docker keeps only five health results. |
| 4 | Repeat apply | Pass; F1 fixed | No config PUT or record write. The connector was rolled (Swarm version 928 → 954, new task). The api service version stayed at 937. Before the F1 fix; after it, a repeat apply left the connector at the same version and task (1348 → 1348). |
| 5 | Hostname held by a hand-made A record | Pass | Refused before any change: `ingress hostname conflict`, event `DNS name www.example.com is used by another record`. api version, container count and fake writes unchanged; lock free. Admin `--replace-existing`: the A record was replaced by the managed CNAME and the route was added. Deploy token with `--replace-existing`: HTTP 403. |
| 6 | Remove `www.example.com` from the manifest | Pass | One config PUT and one record delete. The `api` route and CNAME stayed. A foreign TXT record on another name was unchanged, compared before and after. |
| 7 | Publish failure | Pass, adapted | PUT 500 injected on `/accounts/`. Deploy of v3, with `status.example.com` added to the manifest, exited 1 with `ingress publish failed`. The release was recorded and the service ran v3, which answered from the overlay. After the fault was cleared, `ingress apply` wrote the status route and CNAME. The manifest change is needed because a publish writes only when the rules change. |
| 8 | Adopt `p2bad-legacy` in a fresh state | Pass | Seeded remote tunnel with a foreign rule and the 404 rule. Dry run: no changes and no tunnel created. Apply: no new tunnel, no config write. After a deploy, `shop.example.net` came first, the foreign rule was kept and the 404 rule stayed last. |
| 9 | Tunnel not remotely managed | Pass | Dry run: HTTP 400. Apply: failed with `tunnel is not remotely managed`. Fake state and connector version unchanged. |
| 10 | `doctor` with tunnel status | Pass | Healthy: `ingress_tunnel` ok, detail `healthy`. Down: not ok, not critical. No critical check failed in either case. |
| 11 | Remove managed ingress | Pass | Both Berthy rules and both CNAMEs removed. A dashboard rule and the foreign TXT record kept. Tunnel still exists. Connector service deleted. `ps` no longer lists `ingress`. `TUNNEL_TOKEN` deleted (audit `secret_delete`). `CF_API_TOKEN` and `REGISTRY_TOKEN` kept. |
| 12 | Real connector image | Pass as a refusal; readiness not exercised | Rollout failed after 181 s with `rollout failed and was cleaned up`. Tasks exited 255. No config or record write. Connector removed. See the detail below. |
| 13 | Cleanup | Pass | Agents and fake stopped with SIGTERM. Services, containers and networks match snapshot 0. Images match, except the fake connector tag, which this run created before the snapshot and removed. `p2test-api-web` unchanged. Registry and proxy running. |
| extra | Hostname outside the account's zones | Pass | Dry run: `no Cloudflare zone for api.example.org`. Apply: `ingress hostname conflict`. Zero writes. This is the "zone in the account" check from the spec. |

### Scenario 12 detail

The real connector exits at once with `Provided Tunnel token is not valid.` The fake tunnel token is not a Cloudflare
tunnel token, so the connector never reached its readiness check. This run therefore shows the connector failing and
the rollout cleaning up, not a readiness wait.

The readiness command is accepted by the pinned image. `cloudflared tunnel --metrics 127.0.0.1:20241 ready` exists in
the image and exits 1 with `connection refused` when nothing is listening. The fake connector's `/ready` gate (503 for
3 seconds, then 200) passed during scenario 2. Readiness gating of a connector that runs but is not ready is not verified.

## Findings

- **F1. Fixed.** A repeat `ingress apply` with nothing changed leaves the connector running: no rollout, no new release, routes still synced. A new image, manifest, replica count or tunnel token, or a missing service, still rolls it.
- **F2. Fixed.** A deploy with hostnames is refused with "managed ingress is not applied; run ingress apply" until the connector has an applied release, and `ingress status` reports "connector not deployed; run ingress apply" in that state. A deploy that only removes hostnames still publishes.
- **F3. The token is a service environment variable.** Berthy never printed it, and the run printed only variable
  names. Docker returns service environment values from `service inspect`, so anyone with Docker access can read
  the token. This follows from the spec's "passed to the connector in its environment". The value was not checked here.
- **F4. Not a Berthy bug, but seen in this topology.** Two environments on one daemon: each agent's `ps` lists the other
  environment's services, because `appInventory` (`internal/agent/inventory.go:52-53`) discovers every Swarm service and
  does not filter on `berthy.environment`. One agent per server avoids this.

## Not verified here

- Real Cloudflare behaviour: token scopes, zone lookup, tunnel creation, error codes and rate limits. The fake's error
  bodies are its own.
- Traffic through a tunnel. No request reached an app through a connector. Drain and dropped requests during a
  connector replacement were not measured.
- Readiness gating of a running connector that is not ready. The real connector exits on the invalid token.
- Replacing a hand-started connector. Adoption of a route that already matches the target (the exact-match path).
  Only Berthy's own routes were added in the adoption run.
- The Cloudflare token-check failure, outages, and the connector image update path (a new connector digest).
- `deploy --dry-run` with routes. Only `ingress apply --dry-run` and the adoption dry run were run.
- Two concurrent `ingress apply` runs, and the lock during ingress. Agent restart during a publish (recovery).
- More than one connector replica, and two live connectors during a switch.
- Alerts for an unhealthy tunnel (Phase 3). `doctor` shows the status only.
- Health-before-publish ordering from Docker's health timestamps. The evidence is the container start time and the
  write times, plus the code path.

## Done-when status (Phase 2b)

- A new app was published on a new hostname by a manifest change and a deploy: shown for `www.example.com` (scenario 5,
  admin replace) and `shop.example.net` (scenario 8). Not yet shown against real Cloudflare.
- An existing tunnel is adopted with its routes unchanged: shown (scenario 8). "No dropped requests while the
  hand-started connector is replaced": not verified.
- A hostname already used by another record stops the deploy before anything changes: shown (scenario 5 and the
  extra zone check).
- Removing a hostname removes only what Berthy created: shown (scenarios 6 and 11).

## Rerun

The tools are in this repository. The fixture is not: the scenario scripts and the WSL layout are kept in the WSL home
directory, under `~/berthy-e2e/p2b`, with the scripts in `scripts/`. They depend on that layout, the Phase 2 registry
and the isolated daemon.

```sh
# Tools (static, CGO off). The connector stand-in is packaged as an image, with no buildx.
CGO_ENABLED=0 go build -o "$bin/berthy" ./cmd/berthy
CGO_ENABLED=0 go build -o "$bin/fakecloudflare" ./test/fakecloudflare
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$stage/usr/local/bin/cloudflared" ./test/fakecloudflared
tar -C "$stage" -cf - . | docker import \
  --change 'ENTRYPOINT ["cloudflared"]' \
  --change 'ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin' \
  - "$registry/berthy-p2b/cloudflared:fake"
# Push with the registry login, and pin the digest that the push prints.

# Fake Cloudflare, loopback only. The token file is read and never printed.
"$bin/fakecloudflare" -listen 127.0.0.1:18787 -account "$account" \
  -token-file "$run/cf-token" -zones example.com,example.net -state "$logs/fake-state.json"
```

- Agent config: `"cloudflare_api_url": "http://127.0.0.1:18787/client/v4"`. Plain http is accepted only for a loopback
  host.
- Secrets: `berthy secret set CF_API_TOKEN -e <env> --config <admin.json> < "$run/cf-token"`, and `REGISTRY_TOKEN`
  bound to the registry host with `--registry`.
- Control plane (test only, loopback): `GET /_fake/state`; `POST /_fake/tunnels` `{name, remote, config}`;
  `POST /_fake/records` `{zone, record}`; `POST /_fake/config` `{tunnel, config}`; `POST /_fake/status`
  `{tunnel, status, connections}`; `POST /_fake/fail` `{method, prefix, status}`; `DELETE /_fake/fail` clears faults.
- Order: scenarios 0 to 13 as in the table, with a second agent (`p2bad`, its own state and port) for scenario 8. The
  extra zone check uses a separate fake state file.

Unit tests for the tools: `go test ./test/fakecloudflare ./test/fakecloudflared ./internal/cloudflare/cftest`.

## To verify with real Cloudflare

- [ ] API token: Cloudflare Tunnel edit on the account, and DNS edit limited to the zones that hold the hostnames.
  `ingress apply --dry-run` passes the token check.
- [ ] `ingress apply` creates the remotely managed tunnel `<project>-<env>`, and the connector runs. Its first successful
  `tunnel ready` check falls within the connector's 180-second `deploy_timeout` progress window with the real image digest.
- [ ] A deploy with a new hostname: the route appears in the tunnel configuration, the proxied CNAME
  `<tunnel id>.cfargotunnel.com` exists with the managed comment, and `curl https://<hostname>/` answers through Cloudflare.
- [ ] Conflict on a real record: a hand-made A record for a test hostname makes the deploy refuse with
  `ingress hostname conflict`, with nothing changed. `--replace-existing` as admin takes it over.
- [ ] Adopt an existing dashboard tunnel: `ingress apply --dry-run` shows the adoption, its foreign rules and catch-all
  are unchanged after apply, and no new tunnel appears.
- [ ] Replace a hand-started connector without dropped requests: run a loop of requests during `ingress apply`, for
  example `while true; do curl -s -o /dev/null -w '%{http_code}\n' https://<host>/; sleep 0.2; done`. Check for
  non-200 responses. Stop the hand-started connector only after `ingress status` shows Berthy's connector healthy.
- [ ] Remove a hostname and deploy: its route and record are removed, and the foreign records stay.
- [ ] `ingress status` shows Cloudflare's tunnel status, and `doctor`'s `ingress_tunnel` check follows it. Stopping all
  connectors makes it report down.
- [ ] Repeat `ingress apply` with nothing changed and confirm the connector is not restarted (F1, fixed): its Swarm version and task stay the same.
- [ ] The agent host reaches the Cloudflare API over HTTPS only.

FedRAMP: not applicable to this verification.
