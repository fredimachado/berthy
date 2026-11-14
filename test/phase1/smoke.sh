#!/usr/bin/env bash
set -euo pipefail
umask 077

if [[ $# != 9 ]]; then
  echo 'usage: smoke.sh BINARY CLI_CONFIG ENV AGENT_URL SWARM_APP COMPOSE_APP ROLE REPLICA LOG_REGEX' >&2
  exit 2
fi
bin=$1 config=$2 env=$3 url=${4%/} swarm=$5 compose=$6 role=$7 replica=$8 marker=$9
dir=$(mktemp -d)
trap 'rm -r -- "$dir"' EXIT
run() { "$bin" "$@" -e "$env" --config "$config"; }
json() { python3 -c 'import json,sys; json.load(sys.stdin)' < "$1"; }
stream() {
  local output=$1 duration=$2; shift 2
  local status=0
  timeout --signal=INT "$duration" "$bin" "$@" -e "$env" --config "$config" > "$output" 2> "$dir/stream.err" || status=$?
  if [[ $status != 0 && $status != 124 ]] || [[ ! -s $output ]]; then
    cat "$dir/stream.err" >&2
    return 1
  fi
}

for cmd in ps df doctor agent-info stats; do
  run "$cmd" > "$dir/text"
  [[ -s $dir/text ]]
  run "$cmd" --json > "$dir/json"
  json "$dir/json"
done
for app in "$swarm" "$compose"; do
  run describe "$app" > "$dir/text"
  [[ -s $dir/text ]]
  run describe "$app" --json > "$dir/json"
  json "$dir/json"
  run logs "$app" --tail 100 > "$dir/text"
  run logs "$app" --tail 100 --json > "$dir/json"
  json "$dir/json"
done
run logs "$swarm" --tail 100 --since 2000-01-01T00:00:00Z --grep "$marker" --role "$role" --replica "$replica" --json > "$dir/filtered"
python3 -c 'import json,sys; assert json.load(sys.stdin)' < "$dir/filtered"
run stats --since 1h --json > "$dir/history"
json "$dir/history"
stream "$dir/watch" 23 stats --watch --json
python3 -c 'import json,sys; t=[json.loads(x)["time"] for x in sys.stdin if x.strip()]; assert len(t)>=2 and len(set(t))==len(t) and t==sorted(t)' < "$dir/watch"
stream "$dir/watch-text" 4 stats --watch
stream "$dir/follow" 7 logs "$compose" --tail 0 --follow --json
python3 -c 'import json,sys; records=[json.loads(x) for x in sys.stdin if x.strip()]; assert len(records)>=2 and len({x["timestamp"] for x in records})>=2' < "$dir/follow"
stream "$dir/follow-text" 4 logs "$swarm" --tail 100 --follow
for auth in missing wrong; do
  if [[ $auth == missing ]]; then
    code=$(curl --noproxy '*' -sS -o /dev/null -w '%{http_code}' "$url/v1/environments/$env/apps")
  else
    code=$(curl --noproxy '*' -sS -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer invalid' "$url/v1/environments/$env/apps")
  fi
  [[ $code == 401 ]]
done
python3 - "$config" "$env" "$url" <<'PY'
import json, os, sys, urllib.error, urllib.request
config, environment, url = sys.argv[1:]
with open(config) as f:
    token_env = json.load(f)['environments'][environment]['token_env']
token = os.environ[token_env]
wrong = 'other' if environment != 'other' else 'lab'
request = urllib.request.Request(url + '/v1/environments/' + wrong + '/apps', headers={'Authorization': 'Bearer ' + token})
try:
    urllib.request.build_opener(urllib.request.ProxyHandler({})).open(request, timeout=5)
except urllib.error.HTTPError as e:
    assert e.code == 401
else:
    raise AssertionError('wrong environment accepted')
PY
printf '{"environment":"lab","listen":"0.0.0.0:19087","docker_socket":"/nonexistent","tokens":[{"digest":"%064d","scope":"read"}]}\n' 0 > "$dir/invalid.json"
if "$bin" agent --config "$dir/invalid.json" > "$dir/invalid.out" 2>&1; then
  echo 'wildcard bind unexpectedly accepted' >&2; exit 1
fi
grep -q 'invalid listen address\|listen address must' "$dir/invalid.out"
echo 'Phase 1 CLI smoke checks passed'
