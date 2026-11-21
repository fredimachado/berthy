#!/bin/bash
# setup.sh builds a fresh fixture: no e2e services, a new agent with new tokens, a throwaway registry holding the two
# test images, the manifest, and the fakes. It starts from nothing every time, so running it twice ends in the same state.
set -uo pipefail
. "$(dirname "$0")/lib.sh"
umask 077

die() {
  echo "setup: $*" >&2
  exit 1
}

# setup wipes these directories. Refuse unless they lie under $E2E_ROOT, so a wrong E2E_ROOT deletes nothing else.
for dir in "$RUN" "$LOG" "$MAN"; do
  case $dir in
    "$E2E_ROOT"/?*) ;;
    *) die "refusing to wipe $dir, which is outside $E2E_ROOT" ;;
  esac
done

purge_fixture || die "previous fixture cleanup is incomplete"
rm -rf "$RUN" "$LOG" "$MAN" "$MAN"-*
mkdir -p "$RUN/state" "$LOG" "$MAN" || die "cannot create the fixture directories"

# Tokens. Only the admin token is in the agent's configuration, as berthy setup writes it. "agent token" prints "token: ...",
# "sha256: ..." and the JSON entry for the agent's configuration. The token is captured into variables and written only to
# env.sh; it is never echoed. The ci token is made through the API once the agent runs (below).
admin_out=$("$BIN" agent token -e "$ENVN" --name admin --scope admin) || die "could not make the admin token"
admin_token=$(printf '%s\n' "$admin_out" | sed -n 's/^token: //p')
admin_entry=$(printf '%s\n' "$admin_out" | sed -n '3p')
case $admin_token in bty.$ENVN.*) ;; *) die "the admin token has an unexpected shape" ;; esac
case $admin_entry in \{*) ;; *) die "the admin entry has an unexpected shape" ;; esac
printf "export BERTHY_ADMIN_TOKEN='%s'\n" "$admin_token" > "$RUN/env.sh" || die "cannot write env.sh"
. "$RUN/env.sh"
unset admin_out admin_token

# Secrets and fake-server credentials. The state key must be exactly 32 bytes; the agent refuses anything else.
head -c 32 /dev/urandom > "$RUN/state.key" && chmod 600 "$RUN/state.key" || die "cannot write the state key"
openssl rand -hex 16 > "$RUN/tg-token" || die "cannot write the Telegram token"
printf 'Bearer e2e-otlp' > "$RUN/otlp-auth" || die "cannot write the OTLP authorization"

# Agent configuration: agent.json lists the hosted environments, and each is a fragment in $RUN/environments.d (see
# write_agent_config in lib.sh). This is the first one, with the state and the key of the fixture. The admin entry is kept for
# scenario 07, which writes the configuration of an older agent. Keys match agent.File; the agent rejects unknown keys. The
# Compose CLI is the standalone Compose v2 binary of the fixture: the docker CLI is not on the harness PATH.
printf '%s\n' "$admin_entry" > "$RUN/admin-entry.json" || die "cannot write the admin entry"
write_fragment "$PROJECT" "$ENVN" "$RUN/state" "$RUN/state.key" "$admin_entry" || die "cannot write the environment's fragment"
write_agent_config || die "cannot write agent.json"

# CLI configurations. Each names its token by environment variable, so the token never sits in a file the CLI reads.
printf '{"projects":{"%s":{"environments":{"%s":{"url":"http://%s","token_env":"BERTHY_ADMIN_TOKEN"}}}}}\n' "$PROJECT" "$ENVN" "$AGENT_LISTEN" > "$RUN/admin.json" || die "cannot write admin.json"
printf '{"projects":{"%s":{"environments":{"%s":{"url":"http://%s","token_env":"BERTHY_CI_TOKEN"}}}}}\n' "$PROJECT" "$ENVN" "$AGENT_LISTEN" > "$RUN/ci.json" || die "cannot write ci.json"

# Registry: throwaway and unauthenticated, in memory, so nothing it holds outlives the run.
d run -d --name "$REG_NAME" --label berthy.e2e=1 -p "$REG:5000" --tmpfs /var/lib/registry registry:2 >/dev/null \
  || die "the registry container did not start (is port ${REG##*:} free?)"
wait_for "registry" curl_local -f -o /dev/null "http://$REG/v2/" || die "the registry does not answer"

# Push the two test images under the e2e name and record the digests the registry will serve. The source tags stay;
# only the e2e tags are removed afterwards.
: > "$RUN/digests.env"
for v in v1 v2; do
  src="127.0.0.1:15001/berthy-phase2/web:$v"
  dst="$REG/e2e/web:$v"
  d image inspect "$src" >/dev/null 2>&1 || die "the source image $src is missing on the daemon"
  d tag "$src" "$dst" || die "tagging $dst failed"
  d push "$dst" >/dev/null || die "pushing $dst failed"
  digest=$(d image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$dst" | sed -n "s#^$REG/e2e/web@##p" | head -n 1)
  case $digest in sha256:*) ;; *) die "no registry digest for $dst" ;; esac
  d rmi "$dst" >/dev/null || die "removing the tag $dst failed"
  printf 'WEB_%s_DIGEST=%s\n' "${v^^}" "$digest" >> "$RUN/digests.env"
done

base_manifest | write_manifest || die "cannot write the manifest"
"$BIN" config validate -e "$ENVN" --manifest-dir "$MAN" >/dev/null || die "the manifest does not validate"

start_fakes || die "the fakes did not start"
start_agent || die "the agent did not start"

# The ci token, an agent token that may deploy, is made the way a person makes one: through the API, with no shell on the
# server and no restart.
ci_token=$(api_token ci --scope deploy --agent) || die "could not make the ci token"
case $ci_token in bty.$ENVN.*) ;; *) die "the ci token has an unexpected shape" ;; esac
printf "export BERTHY_CI_TOKEN='%s'\n" "$ci_token" >> "$RUN/env.sh" || die "cannot write env.sh"
. "$RUN/env.sh"
unset ci_token

echo "fixture ready: $("$BIN" version); registry $REG; fakes on $TG_LISTEN, $OTLP_LISTEN, $HOOK_LISTEN and $S3_LISTEN; agent on $AGENT_LISTEN"
