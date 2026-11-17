#!/bin/bash
# 05-config-default proves that the CLI finds its configuration without --config. BERTHY_CONFIG names the file a command reads.
# With neither --config nor BERTHY_CONFIG, the command reads berthy/config.json in the user configuration directory, and a
# deploy made through the admin token shows up in the history that the default file reads. With no file there, the command
# fails with exit 1 and its message names the path it looked for. -e stays required: ps without it refuses with exit 2.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"
. "$RUN/digests.env"

# A deploy through the admin token, so that there is a history to read.
out=$(adm deploy web --version v1 --manifest-dir "$MAN" 2>&1); code=$?
expect_code "deploy v1" 0 "$code" "$out"

# BERTHY_CONFIG names the file: no --config is given, and the agent answers.
out=$(BERTHY_CONFIG="$RUN/admin.json" "$BIN" agent-info -e "$ENVN" 2>&1); code=$?
expect_code "BERTHY_CONFIG: agent-info without --config" 0 "$code" "$out"
expect_contains "BERTHY_CONFIG: agent-info names the environment" "$out" "$ENVN"

# The default file: a user configuration directory of its own, holding the file the admin token reads.
default_dir=$(mktemp -d "$RUN/xdg-default.XXXXXX") || exit 1
mkdir -p "$default_dir/berthy"
cp "$RUN/admin.json" "$default_dir/berthy/config.json"
default_run() { env -u BERTHY_CONFIG XDG_CONFIG_HOME="$default_dir" "$BIN" "$@"; }

out=$(default_run ps -e "$ENVN" 2>&1); code=$?
expect_code "default file: ps without --config" 0 "$code" "$out"
expect_contains "default file: ps lists the app" "$out" "web"

out=$(default_run history web -e "$ENVN" 2>&1); code=$?
expect_code "default file: history without --config" 0 "$code" "$out"
expect_contains "default file: history lists the v1 release" "$out" "$WEB_V1_DIGEST"

# No file at all: the command fails and names the file it looked for.
missing_dir=$(mktemp -d "$RUN/xdg-missing.XXXXXX") || exit 1
out=$(env -u BERTHY_CONFIG XDG_CONFIG_HOME="$missing_dir" "$BIN" ps -e "$ENVN" 2>&1); code=$?
expect_code "no file: ps fails with exit 1" 1 "$code" "$out"
expect_contains "no file: the message names the default path" "$out" "$missing_dir/berthy/config.json"

# -e has no default: without it the command refuses before it reads any configuration.
out=$(default_run ps 2>&1); code=$?
expect_code "ps without -e refuses with exit 2" 2 "$code" "$out"

rm -rf "$default_dir" "$missing_dir"
finish
