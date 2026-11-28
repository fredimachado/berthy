#!/bin/bash
# Print the scenarios assigned to one duration-balanced shard, in run order.
set -euo pipefail
if [ "$#" -ne 2 ] || [[ ! $1 =~ ^[1-9][0-9]*$ || ! $2 =~ ^[1-9][0-9]*$ ]] || (( $1 > $2 )); then
  echo "usage: shard.sh SHARD COUNT (1 <= SHARD <= COUNT)" >&2
  exit 2
fi
DIR=$(cd "$(dirname "$0")" && pwd)
python3 - "$DIR" "$1" "$2" <<'PY'
from pathlib import Path
from statistics import median
import sys

directory = Path(sys.argv[1])
shard, count = map(int, sys.argv[2:])
durations = {}
for line in (directory / "durations.txt").read_text().splitlines():
    name, seconds = line.split()
    if name in durations or int(seconds) <= 0:
        raise ValueError(f"invalid duration for {name}")
    durations[name] = int(seconds)
default = median(durations.values())
scenarios = sorted(path.stem for path in (directory / "scenarios").glob("*.sh"))
loads = [0] * count
assignments = [[] for _ in range(count)]
for name in sorted(scenarios, key=lambda n: (-durations.get(n, default), n)):
    target = min(range(count), key=lambda i: (loads[i], i))
    assignments[target].append(name)
    loads[target] += durations.get(name, default)
print("\n".join(sorted(assignments[shard - 1])), end="\n" if assignments[shard - 1] else "")
PY
