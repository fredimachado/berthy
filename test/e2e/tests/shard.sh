#!/bin/bash
set -euo pipefail
DIR=$(cd "$(dirname "$0")/.." && pwd)
expected=$(mktemp)
actual=$(mktemp)
trap 'rm -f "$expected" "$actual"' EXIT
for file in "$DIR"/scenarios/*.sh; do basename "$file" .sh; done | LC_ALL=C sort > "$expected"
for count in {1..8}; do
  : > "$actual"
  for ((shard=1; shard<=count; shard++)); do
    bash "$DIR/shard.sh" "$shard" "$count" >> "$actual"
  done
  LC_ALL=C sort -o "$actual" "$actual"
  diff -u "$expected" "$actual"
done
echo 'Every scenario appears in exactly one shard for counts 1 through 8.'