#!/bin/bash
# 24-deploy-diff proves that a deploy plan says what it changes compared with the release the app runs, on the real Swarm. A dry
# run of a first deploy says "new app". A dry run of v2 with one more replica and one more variable lists exactly the image, the
# replica count and the variable. A dry run that changes nothing says so. A variable taken from another secret shows by the
# names of the variable and of the secret, and no secret value is in any output, plain or JSON. `history web --diff` compares two
# releases with the same lines and refuses a release that is not the app's. On a protected environment a deploy that an agent
# token asked for is held, `approvals` and the held answer show the diff, and approving it runs the deploy.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

svc="$PROJECT-$ENVN-web-main"
SECRET_1='s3cret-value-aaa111'
SECRET_2='s3cret-value-bbb222'

# manifest REPLICAS EXTRA SECRET [PROTECTED]: the web app with REPLICAS replicas of its main role, the plain variable EXTRA (a
# whole YAML line, or empty), and API_KEY taking the value of the secret SECRET.
manifest() {
  [ "${4:-}" = protected ] && echo "protected: true"
  cat <<YML
project: $PROJECT
network: $NETWORK
apps:
  web:
    kind: image
    image: $REG/e2e/web
    roles:
      main:
        replicas: $1
        health:
          test: ["CMD", "true"]
          interval: 2s
          timeout: 2s
          start_period: 1s
          retries: 3
    env:
      plain:
        LOG_LEVEL: info
$2
      secret:
        - API_KEY: $3
YML
}

# diff_lines TEXT: the lines of the diff under the app name "web" in the output of a dry run.
diff_lines() { printf '%s\n' "$1" | awk '/^web$/ { f = 1; next } f && /^  / { print; next } { f = 0 }'; }

# seen: everything the scenario printed, to look for secret values in it at the end.
seen=""
note() { seen="$seen"$'\n'"$1"; }

out=$(printf '%s' "$SECRET_1" | adm secret set API_KEY_V1 2>&1); code=$?
expect_code "secret API_KEY_V1 is set" 0 "$code" "$out"
out=$(printf '%s' "$SECRET_2" | adm secret set API_KEY_V2 2>&1); code=$?
expect_code "secret API_KEY_V2 is set" 0 "$code" "$out"

# 1. A first deploy has nothing to compare with.
manifest 1 "" API_KEY_V1 | write_manifest
out=$(adm deploy web --version v1 --dry-run --manifest-dir "$MAN" 2>&1); code=$?
expect_code "the dry run of a first deploy" 0 "$code" "$out"
note "$out"
expect_contains "a first deploy is a new app" "$out" $'web\n  new app'

out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1" 0 "$code" "$out"
note "$out"

# 2. The same release again changes nothing.
out=$(adm deploy web --version v1 --dry-run --manifest-dir "$MAN" 2>&1); code=$?
expect_code "the dry run of the same release" 0 "$code" "$out"
note "$out"
expect_contains "the same release says so" "$out" "no changes: the same release would be recorded"

# 3. v2 with one more replica and one more variable: exactly the image, the replicas and the variable.
manifest 2 "        FEATURE_X: \"on\"" API_KEY_V1 | write_manifest
out=$(adm deploy web --version v2 --dry-run --manifest-dir "$MAN" 2>&1); code=$?
expect_code "the dry run of v2" 0 "$code" "$out"
note "$out"
lines=$(diff_lines "$out")
expect_eq "the dry run lists three changes" "3" "$(printf '%s\n' "$lines" | grep -c .)"
expect_contains "it lists the image with the tags" "$lines" "image    sha256:"
expect_contains "it names the tags" "$lines" "(v1 -> v2)"
expect_contains "it lists the replicas" "$lines" "main     replicas 1 -> 2"
expect_contains "it lists the variable" "$lines" "env      + FEATURE_X (plain)"
expect_eq "the dry run created no service change" "1" "$(d service inspect --format '{{.Spec.Mode.Replicated.Replicas}}' "$svc")"

# --json carries the same diff.
out=$(adm deploy web --version v2 --dry-run --json --manifest-dir "$MAN" 2>&1); code=$?
expect_code "the JSON dry run" 0 "$code" "$out"
note "$out"
expect_contains "the JSON plan has a diff" "$out" '"diff"'
expect_contains "the JSON diff has the replicas" "$out" '"area": "main"'

# Send one dry-run request with and without the field query.
api_url="http://$AGENT_LISTEN/v1/projects/$PROJECT/environments/$ENVN"
diff_api() { curl_local --fail --header @- "$@" < <(printf 'Authorization: Bearer %s\n' "$BERTHY_ADMIN_TOKEN"); }
preview_body=$(jq -cn --arg p "$PROJECT" --arg n "$NETWORK" --arg img "$REG/e2e/web" '
  {app:"web",version:"v2",dry_run:true,manifest:{project:$p,network:$n,apps:{web:{
    kind:"image",image:$img,roles:{main:{replicas:2,health:{
      test:["CMD","true"],interval:"2s",timeout:"2s",start_period:"1s",retries:3
    }}},env:{plain:{LOG_LEVEL:"info",FEATURE_X:"on"},secret:[{API_KEY:"API_KEY_V1"}]}
  }}}}')
applied=$(diff_api "$api_url/applied/web"); code=$?
expect_code "read the current applied release" 0 "$code" "$applied"
ordinary=$(diff_api -H 'Content-Type: application/json' --data-binary "$preview_body" "$api_url/deploys"); code=$?
expect_code "request the ordinary HTTP dry run" 0 "$code" "$ordinary"
rich=$(diff_api -H 'Content-Type: application/json' --data-binary "$preview_body" "$api_url/deploys?diff=fields"); code=$?
expect_code "request the HTTP field preview" 0 "$code" "$rich"
expect_eq "ordinary HTTP plan matches the CLI dry run" "$(jq -cS . <<<"$out")" "$(jq -cS . <<<"$ordinary")"
expect_eq "field preview keeps the complete ordinary plan" "$(jq -cS . <<<"$ordinary")" "$(jq -cS 'del(.release_diff)' <<<"$rich")"
if jq -e --argjson a "${applied:-null}" '
  $a.id > 0 and .release_diff.base == $a.id and any(.release_diff.fields[];
    .path == ["roles","main","replicas"] and .before == "1" and .after == "2")
' <<<"$rich" >/dev/null; then
  pass "field preview compares the applied release and lists replica sides"
else
  fail "field preview compares the applied release and lists replica sides" "$rich"
fi
expect_eq "the HTTP previews leave one swarm replica" "1" "$(d service inspect --format '{{.Spec.Mode.Replicated.Replicas}}' "$svc")"
note "$ordinary"
note "$rich"

# 4. A variable taken from another secret shows by names only.
manifest 2 "        FEATURE_X: \"on\"" API_KEY_V2 | write_manifest
out=$(adm deploy web --version v2 --dry-run --manifest-dir "$MAN" 2>&1); code=$?
expect_code "the dry run with another secret" 0 "$code" "$out"
note "$out"
expect_contains "the variable is now from the other secret" "$out" "env      ~ API_KEY now from secret API_KEY_V2"
out=$(adm deploy web --version v2 --dry-run --json --manifest-dir "$MAN" 2>&1); code=$?
note "$out"

# 5. Deploy v2, then compare the two releases.
out=$(adm deploy web --version v2 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v2" 0 "$code" "$out"
note "$out"
out=$(adm history web 2>&1); code=$?
expect_code "history answers" 0 "$code" "$out"
id2=$(printf '%s\n' "$out" | sed -n 's/^id: //p' | sed -n 1p)
id1=$(printf '%s\n' "$out" | sed -n 's/^id: //p' | sed -n 2p)
if [ -n "$id1" ] && [ -n "$id2" ]; then pass "history has two releases ($id2, $id1)"; else fail "history has two releases" "ids '$id2' '$id1'"; fi

out=$(adm history web --diff "$id2" "$id1" 2>&1); code=$?
expect_code "history --diff compares two releases" 0 "$code" "$out"
note "$out"
expect_contains "the diff has the image" "$out" "(v1 -> v2)"
expect_contains "the diff has the replicas" "$out" "main     replicas 1 -> 2"
expect_contains "the diff has the variable" "$out" "env      + FEATURE_X (plain)"
expect_contains "the diff has the secret mapping" "$out" "env      ~ API_KEY now from secret API_KEY_V2"
expect_eq "the diff has four changes" "4" "$(printf '%s\n' "$out" | grep -c .)"

out2=$(adm history web --diff "$id2" 2>&1); code=$?
expect_code "history --diff with one release" 0 "$code" "$out2"
expect_eq "one release is compared with the one before it" "$out" "$out2"

out=$(adm history web --diff "$id1" 2>&1); code=$?
expect_code "history --diff of the first release" 0 "$code" "$out"
expect_eq "the first release is a new app" "new app" "$out"

out=$(adm history web --diff "$id1" "$id2" 2>&1); code=$?
expect_code "history --diff the other way round" 0 "$code" "$out"
expect_contains "the other way round removes the variable" "$out" "env      - FEATURE_X (plain)"

out=$(adm history web --diff 999999 2>&1); code=$?
expect_code "a release of no app is refused" 1 "$code" "$out"
expect_contains "the refusal names the app" "$out" "is not a release of app web"

out=$(adm history web --diff "$id2" --json 2>&1); code=$?
expect_code "history --diff --json" 0 "$code" "$out"
note "$out"
expect_contains "the JSON diff has changes" "$out" '"changes"'

# 6. A rollback dry run compares the release that runs with the one it returns to.
out=$(adm rollback web --dry-run 2>&1); code=$?
expect_code "the rollback dry run" 0 "$code" "$out"
note "$out"
expect_contains "the rollback takes the replicas back" "$out" "main     replicas 2 -> 1"

# 7. On a protected environment an agent token's deploy is held, and the diff is in what the person reads.
manifest 1 "" API_KEY_V2 protected | write_manifest
out=$(adm deploy web --version "$WEB_V2_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "protect the environment with a digest deploy" 0 "$code" "$out"
note "$out"
manifest 2 "        FEATURE_Y: \"1\"" API_KEY_V2 protected | write_manifest
out=$(ci deploy web --version "$WEB_V2_DIGEST" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "an agent's deploy is held for approval" 3 "$code" "$out"
note "$out"
expect_contains "the held answer shows the replicas" "$out" "main     replicas 1 -> 2"
expect_contains "the held answer shows the variable" "$out" "env      + FEATURE_Y (plain)"
id=$(printf '%s\n' "$out" | sed -n 's/^held for approval: \([^ ]*\) .*/\1/p')
expect_eq "the held deploy changed nothing" "1" "$(d service inspect --format '{{.Spec.Mode.Replicated.Replicas}}' "$svc")"

out=$(adm approvals 2>&1); code=$?
expect_code "approvals lists the request" 0 "$code" "$out"
note "$out"
expect_contains "approvals shows what changes" "$out" "$id: changes of web"
expect_contains "approvals shows the replicas" "$out" "main     replicas 1 -> 2"
expect_contains "approvals shows the variable" "$out" "env      + FEATURE_Y (plain)"

# The terminal interface shows the same lines in the detail of the request, not the diff as JSON.
tui_dir=$LOG/tui-diff
rm -rf "$tui_dir"; mkdir -p "$tui_dir"
cat > "$tui_dir/script.json" <<'JSON'
[
  {"wait_text": "Approvals", "timeout": "20s"},
  {"keys": ["4"]},
  {"wait_text": "pending"},
  {"keys": ["enter"]},
  {"wait_text": "What changes", "timeout": "20s"},
  {"snapshot": "detail"},
  {"keys": ["q"]}
]
JSON
out=$("$TUI_BIN" --command "$BIN ui -e $ENVN --config $RUN/admin.json" --size 120x40 --script "$tui_dir/script.json" --out "$tui_dir/out" --timeout 60s --redact-env BERTHY_ADMIN_TOKEN 2>&1); code=$?
expect_code "the terminal interface opens the request" 0 "$code" "$out"
screen=$(cat "$tui_dir"/out/*detail*.txt 2>/dev/null)
expect_contains "the detail shows the replicas" "$screen" "main     replicas 1 -> 2"
expect_contains "the detail shows the variable" "$screen" "env      + FEATURE_Y (plain)"
expect_not_contains "the detail does not show the diff as JSON" "$screen" '"area"'

out=$(adm approve "$id" 2>&1); code=$?
expect_code "approve runs the held deploy" 0 "$code" "$out"
note "$out"
expect_contains "approve shows the diff first" "$out" "main     replicas 1 -> 2"
expect_eq "the approved deploy runs two replicas" "2" "$(d service inspect --format '{{.Spec.Mode.Replicated.Replicas}}' "$svc")"

# 8. No secret value was in anything the scenario printed.
expect_not_contains "no output holds the first secret value" "$seen" "$SECRET_1"
expect_not_contains "no output holds the second secret value" "$seen" "$SECRET_2"
agent_log=$(cat "$LOG"/agent.log 2>/dev/null)
expect_not_contains "the agent log holds no secret value" "$agent_log" "$SECRET_1"

finish
