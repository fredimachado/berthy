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
fixture=$(mktemp -d)
trap 'rm -f "$expected" "$actual"; rm -rf "$fixture"' EXIT
mkdir "$fixture/scenarios"
cp "$DIR/shard.sh" "$fixture/shard.sh"
touch "$fixture/scenarios/"{a,b,c,d}.sh
printf 'a 100\nb 40\nc 10\n' > "$fixture/durations.txt"
test "$(bash "$fixture/shard.sh" 1 2)" = a
test "$(bash "$fixture/shard.sh" 2 2)" = "$(printf 'b\nc\nd')"
echo 'Every scenario appears in exactly one shard for counts 1 through 8.'
