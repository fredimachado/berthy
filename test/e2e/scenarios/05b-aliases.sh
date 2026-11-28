#!/bin/bash
# 05b-aliases proves manifest aliases on the real agent. The CLI expands an alias before it checks flags, so the agent gets an
# ordinary request: an exec alias runs in the container, a history alias lists the release, and extra arguments reach the
# command. The agent accepts the aliases key of the manifest it is sent. An alias that sets -e or takes a built-in name fails
# config validate by name, and an unknown name is an unknown command. The alias lookup reads berthy.yml from the working
# directory, so the scenario runs from the manifest directory, as a person in a checkout does.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

{
  base_manifest
  cat <<'YML'
aliases:
  osinfo: "exec web -- cat /etc/os-release"
  hist: "history web"
  run1: "exec web -- echo"
YML
} | write_manifest
cd "$MAN" || exit 1

out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1 with aliases in the manifest" 0 "$code" "$out"

out=$("$BIN" osinfo -e "$ENVN" --config "$RUN/admin.json" 2>&1); code=$?
expect_code "alias osinfo runs exec" 0 "$code" "$out"
expect_contains "osinfo output comes from the container" "$out" "Alpine"

out=$("$BIN" hist -e "$ENVN" --config "$RUN/admin.json" 2>&1); code=$?
expect_code "alias hist runs history" 0 "$code" "$out"
expect_contains "hist lists the v1 release" "$out" "$WEB_V1_DIGEST"

out=$("$BIN" run1 -e "$ENVN" --config "$RUN/admin.json" hello world 2>&1); code=$?
expect_code "alias run1 takes extra arguments" 0 "$code" "$out"
expect_contains "run1 prints the extra arguments as a line of the output" "$out" $'\nhello world\n'

# From another working directory, --manifest-dir names the manifest for the lookup. exec does not take the flag, so it is not
# passed on, and the command still runs.
out=$(cd /tmp && "$BIN" osinfo -e "$ENVN" --config "$RUN/admin.json" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "alias osinfo runs from another directory with --manifest-dir" 0 "$code" "$out"
expect_contains "osinfo from another directory prints the release" "$out" "Alpine"

# An alias that sets the environment is refused by config validate, and the message names it.
{
  base_manifest
  printf 'aliases:\n  bad: "exec web -e prod -- echo"\n'
} | write_manifest
out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "config validate refuses an alias that sets -e" 1 "$code" "$out"
expect_contains "config validate names the alias that sets -e" "$out" "alias bad"

# An alias that takes a built-in name is refused the same way.
{
  base_manifest
  printf 'aliases:\n  deploy: "exec web -- echo"\n'
} | write_manifest
out=$("$BIN" config validate -e "$ENVN" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "config validate refuses an alias that shadows deploy" 1 "$code" "$out"
expect_contains "config validate names the alias that shadows deploy" "$out" "alias deploy"

# Without -e the CLI cannot tell a typo from an alias, so the message says both. The manifest is in the working directory.
{
  base_manifest
  cat <<'YML'
aliases:
  osinfo: "exec web -- cat /etc/os-release"
YML
} | write_manifest
out=$("$BIN" osinfoo 2>&1); code=$?
expect_code "a typo of an alias without -e is an unknown command" 2 "$code" "$out"
expect_contains "the message says an alias of the manifest needs -e" "$out" "unknown command osinfoo (an alias of the manifest needs -e)"

# A name that no alias has is an unknown command.
out=$("$BIN" nosuch -e "$ENVN" --manifest-dir "$MAN" 2>&1); code=$?
expect_code "an unknown name is an unknown command" 2 "$code" "$out"
expect_contains "the message names the unknown command" "$out" "unknown command nosuch"

# Bare berthy lists every public command and omits the internal helpers.
BERTHY_CONFIG="$RUN/admin.json" "$BIN" >"$RUN/bare.out" 2>"$RUN/bare.err"
code=$?
expect_code "bare berthy exits 2" 2 "$code" "$(cat "$RUN/bare.err")"
if [ -s "$RUN/bare.out" ]; then fail "bare berthy stdout is empty" "$(cat "$RUN/bare.out")"; else pass "bare berthy stdout is empty"; fi
bare=$(cat "$RUN/bare.err")
for cmd in deploy config ps backup; do
  expect_contains "bare usage lists $cmd" "$bare" "$cmd"
done
for helper in probe-helper volume-archive volume-extract; do
  expect_not_contains "bare usage omits $helper" "$bare" "$helper"
done

finish
