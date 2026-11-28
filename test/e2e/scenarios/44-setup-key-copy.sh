#!/bin/bash
# Exercise setup's key copy through two disposable SSH servers.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
export FS_HOME="$RUN/key-copy-home" FS_DOCKER_HOST="unix://$SOCK" DOCKER_CONFIG="$E2E_ROOT/docker-config"
mkdir -p "$FS_HOME/bin" "$RUN/key-copy-empty"
ln -sf "$DOCKER_BIN" "$FS_HOME/bin/docker"
export PATH="$FS_HOME/bin:/usr/bin:/bin"
export HOME="$FS_HOME"
fixture="$REPO/test/fakeserver/fakeserver.sh"
private=$(mktemp -d /dev/shm/berthy-key-copy.XXXXXXXX) || exit 1
chmod 700 "$private" || exit 1
scope=$(printf '%s' "$PROJECT/$ENVN" | sha256sum | cut -c1-8)
nonce=$(printf '%s' "$private" | sha256sum | cut -c1-16)
export FS_OWNER="$nonce"
server_a="a${scope}-${nonce}"
server_b="b${scope}-${nonce}"
started_a=0 started_b=0 setpriv_hidden=0
cleanup() {
  if [ "$setpriv_hidden" -eq 1 ]; then
    bash "$fixture" exec "$server_a" mv /usr/bin/setpriv.key-copy /usr/bin/setpriv >/dev/null 2>&1 || echo "could not restore fixture setpriv" >&2
  fi
  [ "$started_b" -eq 0 ] || bash "$fixture" rm "$server_b" >/dev/null 2>&1 || echo "could not remove fixture B" >&2
  [ "$started_a" -eq 0 ] || bash "$fixture" rm "$server_a" >/dev/null 2>&1 || echo "could not remove fixture A" >&2
  rm -rf "$private"
}
trap cleanup EXIT
check() { if ! "$@" >/dev/null 2>&1; then echo "setup key copy check failed: $label" >&2; exit 1; fi; }
for server in "$server_a" "$server_b"; do
  [[ $server =~ ^[a-z][a-z0-9-]{0,30}$ ]] || { echo "invalid fixture name" >&2; exit 1; }
  existing=$(d ps -aq --no-trunc --filter "name=^/${server}\$") || exit 1
  [ -z "$existing" ] || { echo "fixture container already exists" >&2; exit 1; }
  volumes=$(d volume ls -q) || exit 1
  for volume in "$server-docker" "$server-containerd"; do
    if d volume inspect "$volume" >/dev/null 2>&1 || printf '%s\n' "$volumes" | grep -Fxq -- "$volume"; then
      echo "fixture volume already exists" >&2; exit 1
    fi
  done
done
started_a=1
bash "$fixture" start "$server_a" >/dev/null 2>&1 || { echo "could not start fixture A" >&2; exit 1; }
ip_a=$(bash "$fixture" ip "$server_a") || exit 1
remote() { bash "$fixture" exec "$server_a" "$@"; }
remote sh -c 'printf "%s\n" "Defaults log_output" "Defaults !compress_io" "Defaults iolog_dir=/var/log/sudo-io" > /etc/sudoers.d/key-copy && chmod 0440 /etc/sudoers.d/key-copy && visudo -cf /etc/sudoers.d/key-copy' >/dev/null 2>&1 || { echo "could not enable sudo recording" >&2; exit 1; }
key="/var/lib/berthy/environments/$PROJECT/$ENVN/key"
cfg="$RUN/key-copy-cli.json"
setup=("$BIN" setup -e "$ENVN" --project "$PROJECT" --host "ops@$server_a" --listen "$ip_a:7710" --network "$NETWORK" --binary "$BIN" --manifest-dir "$RUN/key-copy-empty" --config "$cfg" --json)
mkdir -m 755 "$FS_HOME/.berthy" || exit 1
out=$("${setup[@]}" </dev/null 2>/dev/null); code=$?
label="initial setup failed"
check test "$code" -eq 0
label="initial setup did not report skipped cli_key"
check jq -e '.ok and ([.steps[] | select(.step == "cli_key" and .status == "skipped" and (.detail | contains("not saved")))] | length == 1)' <<<"$out" >/dev/null
label="initial refusal advice"
check jq -e '(.key_copy == null) and (.next_steps[0] | contains("run the same berthy setup command again")) and ([.next_steps[] | select(contains("sudo cat") or contains("berthy setup -e"))] | length == 0)' <<<"$out" >/dev/null
label="initial refusal wrote a key"
check test -z "$(find "$FS_HOME/.berthy" -type f -print -quit)"
token=$(jq -er '.token | select(startswith("bty."))' <<<"$out") || { echo "setup did not return a token" >&2; exit 1; }
chmod 700 "$FS_HOME/.berthy" || exit 1
out=$("${setup[@]}" </dev/null 2>/dev/null); code=$?
label="repaired setup"
check test "$code" -eq 0
check jq -e '[.steps[] | select(.step == "cli_key" and .status == "changed")] | length == 1' <<<"$out" >/dev/null
check jq -e '[.steps[] | select((.step == "binary" or .step == "config" or .step == "service") and .status == "ok")] | length == 3' <<<"$out" >/dev/null
first=$(jq -er '.key_copy | strings' <<<"$out") || { echo "setup did not return a copy path" >&2; exit 1; }
keys="$FS_HOME/.berthy/keys"
record_files() { printf '%s\0' "$@" | sort -z | xargs -0 -r stat -c '%n %i %y'; printf '%s\0' "$@" | sort -z | xargs -0 -r sha256sum; }
record() { local files=(); while IFS= read -r -d '' file; do files+=("$file"); done < <(find "$keys" -maxdepth 1 -type f -print0); record_files "${files[@]}"; }
absent() { local status; grep -Fq -- "$1" "$2" >/dev/null 2>&1; status=$?; [ "$status" -eq 1 ]; }
hex() { od -An -v -tx1 "$@" | tr -d ' \n'; }
key_forms() {
  local b64 url
  raw=$(hex "$1") || return 1
  b64=$(base64 -w0 "$1") || return 1
  url=${b64//+/-}; url=${url//\//_}
  forms=("$raw" "$b64" "$url" "${b64//=/}" "${url//=/}")
}
count() { find "$keys" -maxdepth 1 -type f | wc -l; }
sum() { sha256sum "$1" | cut -d' ' -f1; }
server_sum() { remote sha256sum "$1" | cut -d' ' -f1; }
label="first copy properties"
check test "$(count)" -eq 1
check test "$(stat -c '%a %s' "$first")" = '600 32'
check test "$(sum "$first")" = "$(server_sum "$key")"
record > "$private/one"
out=$("${setup[@]}" </dev/null 2>/dev/null); code=$?
label="repeat setup"
check test "$code" -eq 0
check jq -e --arg path "$first" '.key_copy == $path and ([.steps[] | select(.step == "cli_key" and .status == "ok")] | length == 1)' <<<"$out" >/dev/null
record > "$private/current"
check cmp -s "$private/one" "$private/current"
# Search the recorded bytes without printing the key or matching output.
if ! remote sh -c 'find /var/log/sudo-io -type f -exec cat {} +' 2>/dev/null | od -An -v -tx1 | tr -d ' \n' > "$private/io"; then
  echo "setup key copy check failed: sudo recording capture" >&2; exit 1
fi
label="sudo recording control"
control=$(printf 'berthy-key-wrap/1 ok' | hex) 2>/dev/null || { echo "setup key copy check failed: sudo recording control" >&2; exit 1; }
check grep -Fq -- "$control" "$private/io"
# Raw bytes and textual encodings are checked independently, as hex.
label="sudo recording contains key material"
key_forms "$first" 2>/dev/null || { echo "setup key copy check failed: key encoding" >&2; exit 1; }
if ! absent "$raw" "$private/io"; then echo "setup key copy check failed: sudo recording scan" >&2; exit 1; fi
for form in "${forms[@]}"; do
  encoded=$(printf %s "$form" | hex) 2>/dev/null || { echo "setup key copy check failed: key encoding" >&2; exit 1; }
  if ! absent "$encoded" "$private/io"; then echo "setup key copy check failed: sudo recording scan" >&2; exit 1; fi
done
token_env=$(jq -er --arg p "$PROJECT" --arg e "$ENVN" '.projects[$p].environments[$e].token_env | strings' "$cfg") || exit 1
expected_env=$(printf 'BERTHY_%s_%s_TOKEN' "$PROJECT" "$ENVN" | tr '[:lower:]-' '[:upper:]_')
label="token environment"
check test "$token_env" = "$expected_env"
export "$token_env=$token"
doctor=("$BIN" doctor -e "$ENVN" --config "$cfg" --project "$PROJECT")
"${doctor[@]}" --json >"$private/doctor-before" 2>"$private/doctor-before-err" || { echo "doctor before rotation failed" >&2; exit 1; }
"$BIN" key rotate -e "$ENVN" --config "$cfg" --project "$PROJECT" --json >"$private/rotate" 2>"$private/rotate-err" || { echo "key rotation failed" >&2; exit 1; }
old_id=$(jq -er '.old_key_id | strings' "$private/rotate") || exit 1
new_id=$(jq -er '.new_key_id | strings' "$private/rotate") || exit 1
label="rotation key filenames"
check test "$old_id" != "$new_id"
check test "$first" = "$keys/berthy.$PROJECT.$ENVN.$old_id.key"
check test ! -e "$keys/berthy.$PROJECT.$ENVN.$new_id.key"
"${doctor[@]}" >"$private/doctor-text" 2>"$private/doctor-text-err"; doctor_code=$?
"${doctor[@]}" --json >"$private/doctor-after" 2>"$private/doctor-after-err"; doctor_json_code=$?
warning="warning: this machine has no saved copy of the current state key $new_id. Run remote berthy setup for $PROJECT/$ENVN from this machine without --key-file to save a copy."
label="missing current key copy warning"
check test "$(grep -Fxc -- "$warning" "$private/doctor-text")" -eq 1
check test "$doctor_code" -eq 0
check test "$doctor_json_code" -eq 0
check test "$(grep -Fxc -- "$warning" "$private/doctor-after-err")" -eq 1
label="doctor key IDs"
check jq -e --arg id "$old_id" '.key_id == $id' "$private/doctor-before"
check jq -e --arg id "$new_id" '.key_id == $id' "$private/doctor-after"
out=$("${setup[@]}" </dev/null 2>/dev/null); code=$?
label="rotated key copy"
check test "$code" -eq 0
check jq -e '[.steps[] | select(.step == "cli_key" and .status == "changed")] | length == 1' <<<"$out" >/dev/null
second=$(jq -er '.key_copy | strings' <<<"$out") || exit 1
check test "$second" = "$keys/berthy.$PROJECT.$ENVN.$new_id.key"
check test -f "$first"
check test "$second" != "$first"
check test "$(count)" -eq 2
check test "$(sum "$first")" = "$(server_sum "$key.prev")"
check test "$(sum "$second")" = "$(server_sum "$key")"
"${doctor[@]}" >"$private/doctor-saved" 2>"$private/doctor-saved-err" || { echo "doctor after setup failed" >&2; exit 1; }
"${doctor[@]}" --json >"$private/doctor-saved-json" 2>"$private/doctor-saved-json-err" || { echo "doctor JSON after setup failed" >&2; exit 1; }
label="saved current key copy"
check jq -e --arg id "$new_id" '.key_id == $id' "$private/doctor-saved-json"
check absent "$warning" "$private/doctor-saved"
check absent "$warning" "$private/doctor-saved-json-err"
# The first record must remain byte-identical after the new file appears.
stat -c '%n %i %y' "$first" > "$private/first"
sha256sum "$first" >> "$private/first"
check cmp -s "$private/one" "$private/first"
record > "$private/two"
find "$keys" -mindepth 1 -maxdepth 1 -printf '%f\n' | sort > "$private/listing"
remote sh -c 'mkdir "$1.prev.tmp" && touch "$1.prev.tmp/obstacle" && chown -R berthy:berthy "$1.prev.tmp"' sh "$key" >/dev/null 2>&1 || exit 1
if "$BIN" key rotate -e "$ENVN" --config "$cfg" --project "$PROJECT" >"$private/rotate" 2>&1; then echo "blocked rotation unexpectedly succeeded" >&2; exit 1; fi
label="blocked rotation did not leave a pending key"
check remote test -f "$key.next"
for attempt in 1 2; do
  if [ "$attempt" -eq 1 ]; then
    out=$("${setup[@]}" --key-file "$second" </dev/null 2>/dev/null); code=$?
  else
    out=$("${setup[@]}" </dev/null 2>/dev/null); code=$?
  fi
  label="unsettled rotation refusal"
  check test "$code" -eq 0
  check jq -e '(.key_copy == null) and ([.steps[] | select(.step == "cli_key" and .status == "skipped" and (.detail | contains("key rotation")))] | length == 1) and ([.next_steps[] | select(contains("sudo cat"))] | length == 0)' <<<"$out" >/dev/null
  if [ "$attempt" -eq 1 ]; then
    check jq -e '[.next_steps[] | select(contains("--key-file") and (contains("leave out") or contains("omit")))] | length == 1' <<<"$out"
  fi
  record > "$private/current"
  check cmp -s "$private/two" "$private/current"
  find "$keys" -mindepth 1 -maxdepth 1 -printf '%f\n' | sort > "$private/current-listing"
  check cmp -s "$private/listing" "$private/current-listing"
done
# Both fallbacks must remain safe while the interrupted rotation is still present.
label="interrupted rotation fixture"
check remote test -d "$key.prev.tmp"
check remote test -f "$key.next"
remote mv /usr/bin/setpriv /usr/bin/setpriv.key-copy >/dev/null 2>&1 || { echo "could not hide fixture setpriv" >&2; exit 1; }
setpriv_hidden=1
out=$("${setup[@]}" </dev/null 2>/dev/null); code=$?
remote mv /usr/bin/setpriv.key-copy /usr/bin/setpriv >/dev/null 2>&1 || { echo "could not restore fixture setpriv" >&2; exit 1; }
setpriv_hidden=0
label="missing setpriv fallback"
check test "$code" -eq 0
check jq -e '(.key_copy == null) and ([.steps[] | select(.step == "cli_key" and .status == "skipped")] | length == 1) and ([.next_steps[] | select(contains("sudo cat"))] | length == 0)' <<<"$out"
check remote test -d "$key.prev.tmp"
check remote test -f "$key.next"
# Use the installed build and a separate home and config on the disposable server.
out=$(remote env HOME=/tmp/key-copy-local BERTHY_TOKEN_STORE=none /usr/local/bin/berthy setup -e "$ENVN" --project "$PROJECT" --local --network "$NETWORK" --binary /usr/local/bin/berthy --manifest-dir /tmp --config /tmp/key-copy-local.json --json </dev/null 2>/dev/null); code=$?
label="local handoff during interrupted rotation"
check test "$code" -eq 0
local_step="setup kept no copy of the encryption key of $PROJECT/$ENVN, because it ran on the server itself. To save one, run berthy setup for that same project and environment over SSH from your own machine, with --project $PROJECT and the same build that is installed on this server: the same released version of the CLI, or the same custom Linux binary given with --binary. First resolve any interrupted key rotation on this server. If you are not sure about the build or the rotation, check before you copy; give the same --network and --protected settings as this run"
check jq -e --arg step "$local_step" '(.key_copy == null) and ([.steps[] | select(.step == "cli_key" and .status == "skipped")] | length == 1) and ([.next_steps[] | select(. == $step)] | length == 1) and ([.next_steps[] | select(contains("sudo cat") or contains("berthy setup -e") or contains("--host"))] | length == 0)' <<<"$out"
check remote test -d "$key.prev.tmp"
check remote test -f "$key.next"
record > "$private/current"
check cmp -s "$private/two" "$private/current"
find "$keys" -mindepth 1 -maxdepth 1 -printf '%f\n' | sort > "$private/current-listing"
check cmp -s "$private/listing" "$private/current-listing"
remote rm -rf -- "$key.prev.tmp" || exit 1
remote systemctl restart berthy || { echo "fixture agent restart failed" >&2; exit 1; }
out=$("${setup[@]}" </dev/null 2>/dev/null); code=$?
label="settled rotation copy"
check test "$code" -eq 0
check jq -e '[.steps[] | select(.step == "cli_key" and .status == "changed")] | length == 1' <<<"$out" >/dev/null
third=$(jq -er '.key_copy | strings' <<<"$out") || exit 1
check test "$third" != "$first"
check test "$third" != "$second"
check test "$(count)" -eq 3
check test "$(sum "$third")" = "$(server_sum "$key")"
record_files "$first" "$second" > "$private/previous"
check cmp -s "$private/two" "$private/previous"
record > "$private/three"
started_b=1
bash "$fixture" start "$server_b" >/dev/null 2>&1 || { echo "could not start fixture B" >&2; exit 1; }
ip_b=$(bash "$fixture" ip "$server_b") || exit 1
out=$("$BIN" setup -e "$ENVN" --project "$PROJECT" --host "ops@$server_b" --listen "$ip_b:7710" --network "$NETWORK" --binary "$BIN" --manifest-dir "$RUN/key-copy-empty" --config "$RUN/key-copy-b.json" --key-file "$third" --json </dev/null 2>/dev/null); code=$?
label="rebuilt server"
check test "$code" -eq 0
check jq -e '[.steps[] | select(.step == "cli_key" and .status == "ok")] | length == 1' <<<"$out" >/dev/null
b_sum=$(bash "$fixture" exec "$server_b" sha256sum "$key" | cut -d' ' -f1)
check test "$b_sum" = "$(sum "$third")"
record > "$private/current"
check cmp -s "$private/three" "$private/current"
out=$("${setup[@]}" --verbose </dev/null 2>"$private/verbose"); code=$?
label="verbose setup"
check test "$code" -eq 0
check grep -Fq -- '# cli_key' "$private/verbose"
if ! hex "$private/verbose" > "$private/verbose-hex" 2>/dev/null; then
  echo "setup key copy check failed: verbose output capture" >&2; exit 1
fi
for file in "$first" "$second" "$third"; do
  key_forms "$file" 2>/dev/null || { echo "setup key copy check failed: key encoding" >&2; exit 1; }
  if ! absent "$raw" "$private/verbose-hex"; then echo "setup key copy check failed: verbose output scan" >&2; exit 1; fi
  for form in "${forms[@]}"; do
    if ! absent "$form" "$private/verbose"; then echo "setup key copy check failed: verbose output scan" >&2; exit 1; fi
  done
done
out=$("${setup[@]}" --dry-run </dev/null 2>/dev/null); code=$?
label="dry run"
check test "$code" -eq 0
check jq -e '[.steps[] | select(.step == "cli_key" and .status == "skipped" and (.detail | contains("would keep")))] | length == 1' <<<"$out" >/dev/null
record > "$private/current"
check cmp -s "$private/three" "$private/current"
unset "$token_env" token out
pass "setup key copy"
finish
