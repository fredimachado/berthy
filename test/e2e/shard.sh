#!/bin/bash
# Print scenario names assigned to one shard, in run order.
set -euo pipefail
if [ "$#" -ne 2 ] || [[ ! $1 =~ ^[1-9][0-9]*$ || ! $2 =~ ^[1-9][0-9]*$ ]] || (( $1 > $2 )); then
  echo "usage: shard.sh SHARD COUNT (1 <= SHARD <= COUNT)" >&2
  exit 2
fi
DIR=$(cd "$(dirname "$0")" && pwd)
index=0
for file in "$DIR"/scenarios/*.sh; do
  [ -f "$file" ] || continue
  if (( index % $2 == $1 - 1 )); then basename "$file" .sh; fi
  index=$((index + 1))
done
