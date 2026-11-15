# Phase 2 verification and operator notes

These are setup and verification instructions, not a record of a passed Phase 2
acceptance test. Record the binary commit, daemon, dates and observed results
when the checks are run. The two-week non-production CI gate and the target
environment's ingress and VPN checks remain separate requirements.

## Prepare an isolated environment

Use Linux, an agent account with access to the intended Docker socket, a Swarm
manager and an existing **attachable overlay** whose name matches `network` in
the manifest. Phase 2 does not create the network or install the agent; server
setup is planned for Phase 5. For a disposable test daemon, select its socket
explicitly. The local native Ubuntu WSL fixture uses
`/var/tmp/berthy-phase2/docker.sock`; do not run mutation tests on a shared
Docker daemon. For example, after initializing the disposable Swarm, create its
test network with:

```sh
socket=/var/tmp/berthy-phase2/docker.sock
docker --host "unix://$socket" network create --driver overlay --attachable berthy-test
```

Use a dedicated private state directory and a separately stored key. Back up
the 32 raw key bytes independently of the state database; losing the key makes
encrypted state unreadable. Keep the same key across agent restarts. The agent
refuses an unsafe state directory or key.

```sh
umask 077
bin="$PWD/berthy"
CGO_ENABLED=0 go build -o "$bin" ./cmd/berthy
work=$(mktemp -d)
mkdir -m 700 "$work/state"
head -c 32 /dev/urandom > "$work/state.key"
chmod 600 "$work/state.key"
```

Generate distinct admin and deploy tokens. The agent stores only their SHA-256
digests; the CLI reads the bearer token from an environment variable. The
admin token is for secret administration and adoption, and the deploy token
is for CI deployment. `read` is the lower scope for inspection. Keep both
bearer tokens and their generation output private.

```sh
mapfile -t admin_lines < <("$bin" agent token -e lab)
mapfile -t deploy_lines < <("$bin" agent token -e lab)
export BERTHY_ADMIN_TOKEN="${admin_lines[0]#token: }"
export BERTHY_DEPLOY_TOKEN="${deploy_lines[0]#token: }"
admin_hash="${admin_lines[1]#sha256: }"
deploy_hash="${deploy_lines[1]#sha256: }"
listen=127.0.0.1:19087
printf '{"environment":"lab","listen":"%s","docker_socket":"%s","state_dir":"%s/state","key_file":"%s/state.key","tokens":[{"name":"admin","digest":"%s","scope":"admin"},{"name":"ci","digest":"%s","scope":"deploy"}]}\n' \
  "$listen" "$socket" "$work" "$work" "$admin_hash" "$deploy_hash" > "$work/agent.json"
printf '{"environments":{"lab":{"url":"http://%s","token_env":"BERTHY_ADMIN_TOKEN"}}}\n' "$listen" > "$work/admin.json"
printf '{"environments":{"lab":{"url":"http://%s","token_env":"BERTHY_DEPLOY_TOKEN"}}}\n' "$listen" > "$work/ci.json"
chmod 600 "$work/agent.json" "$work/admin.json" "$work/ci.json"
"$bin" agent --config "$work/agent.json" &
agent_pid=$!
```

The example runs the agent in the background of this shell. Confirm it is ready
with `"$bin" ps -e lab --config "$work/admin.json"`. For a persistent
installation, run it as a service supervised by the host.
For a remote client, bind to a specific private or VPN address and use an
encrypted private network: the Phase 2 API uses HTTP. Provision that network
and access to the listener independently. Configure the agent's
`protected: true` for a protected environment; it requires a SHA-256 digest.

## Check and deploy an image

Run these from a checkout containing `berthy.yml`; the selected environment's
optional `berthy.lab.yml` is merged over it. The example presumes the manifest's
`network: berthy-test`, an image app named `api`, and a registry credential
named `REGISTRY_TOKEN` under the same registry host in `registries`. The
registry username is in the manifest; the password is stored as a secret and
is bound to that registry host. Set the secret through standard input without
printing it or putting it in command arguments:

```sh
"$bin" config validate -e lab --manifest-dir "$PWD"
"$bin" config show -e lab --manifest-dir "$PWD" --json
"$bin" secret set REGISTRY_TOKEN -e lab --config "$work/admin.json" \
  --registry registry.example.com < /secure/path/registry-password
"$bin" deploy api -e lab --config "$work/ci.json" --manifest-dir "$PWD" \
  --version "sha256:REPLACE_WITH_64_HEX_DIGEST" --commit "$(git rev-parse HEAD)" --dry-run
"$bin" deploy api -e lab --config "$work/ci.json" --manifest-dir "$PWD" \
  --version "sha256:REPLACE_WITH_64_HEX_DIGEST" --commit "$(git rev-parse HEAD)"
```

Replace the sample digest and registry host with the pushed image's actual
values. A dry run is a preview, not a reservation: conditions are checked
again when an actual deployment is admitted. Supply `--request-id ID` to
reattach a retried request to the same operation. The CLI follows operation
progress and exits successfully only for a successful terminal outcome.

Image roles have no host-published ports. An ordinary ingress service on the
shared overlay can route to them by service name. Managed Cloudflare ingress
is Phase 2b: an image deployment with `hostnames` on a selected role checks
Cloudflare before Docker writes, then publishes its routes and DNS records
once the new version is healthy. A failed publish leaves the new version
running and exits non-zero. An unrelated unpublished image app in the same
manifest can still be selected.

The CLI also exposes `history APP`, `audit`, `lock status`, `lock acquire -m
MESSAGE`, `lock release`, `secret list`, and `secret delete NAME`. Mutating
lock and secret commands accept `--dry-run`. Image operation commands include
`rollback APP [--to ID] [--revert-schema]`, `restart APP`, `scale APP --role
ROLE --replicas N`, `wait APP --healthy [--role ROLE] [--timeout DURATION]`,
`exec APP [--role ROLE] -- COMMAND ARGS...`, and `prune`; these require their
respective server implementation and scope. Adoption uses `deploy APP
--adopt` and an admin token. Add `-e lab --config PATH` to each remote command;
commands that carry a manifest also accept `--manifest-dir PATH`.

## CI and verification

[The CI example](../examples/ci/deploy.sh) runs from the checked-out commit
with an installed Berthy binary, a build-produced digest, a deploy-scoped token
and a private-network path to the agent. The CLI config's `token_env` must be
`BERTHY_TOKEN` for that example. Store the token in the runner's secret store;
do not put it in the repository, build logs or agent config. Build and push the
image separately before invoking the example.

On the isolated daemon, run Go tests and vet, native Linux race tests, then
exercise the CLI and agent together against a private test registry. Record:

- successful authenticated digest resolution and pinned deployment; a moved
  tag must not change the recorded digest;
- a deliberately unhealthy update, the observed old-service health and the
  resulting terminal status; do not infer traffic safety from status alone;
- ingress traffic and an in-flight slow request through the actual target
  proxy, plus a test of the deploy deadline and drain behavior;
- denied credentials, missing secrets, dry-run nonmutation, lock conflicts,
  interruption and recovery, adoption identity and named-volume preservation;
- hook and migration failure behavior, retained rollback and cleanup boundaries,
  including foreign containers and volumes; and
- agent memory use, CI connectivity and two weeks of non-production CI deploys.

These steps are proposed checks until their results are recorded. Stop only
the test agent and remove only test-owned fixtures after verification; retain
the database and key if the test environment is intended for reuse.

## Observed first deployment

An initial native Ubuntu WSL run used a statically linked Linux amd64 development
build including `c4af76b` and `ecc677a`, plus uncommitted managed-update work.
It used the isolated Docker Engine 29.7.2 socket and an authenticated private
registry. The CLI reached the agent over HTTP. A dry run selected the private
`v1` digest beginning `sha256:20f8291a`; an actual first deployment reached
`succeeded`, with Swarm service `p2test-api-web` at 1/1 on that digest and
history release ID 1. A separate earlier invalid-command first deployment
failed, removed its own created service and recorded no release. The update,
rollback, hook, traffic, interruption and longer-term gates above are still
pending; this result does not establish them.



