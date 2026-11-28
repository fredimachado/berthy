#!/bin/bash
# 77-disk-inodes checks live and sampled mount inode counts against a stable ext4 filesystem.
set -uo pipefail
. "$(dirname "$0")/../lib.sh"

for tool in mkfs.ext4 mount umount mountpoint findmnt losetup truncate stat python3 sudo; do
  command -v "$tool" >/dev/null || { echo "prerequisite missing: $tool" >&2; exit 1; }
done
sudo -n true || { echo 'prerequisite: noninteractive sudo is required' >&2; exit 1; }
out=$(printf 'Authorization=Bearer e2e-otlp' | adm secret set OTLP_HEADERS 2>&1); code=$?
expect_code 'configure OTLP headers' 0 "$code" "$out"
[ "$code" -eq 0 ] || finish
{
  base_manifest
  cat <<YML
telemetry:
  otlp_endpoint: http://$OTLP_LISTEN
  headers_secret: OTLP_HEADERS
YML
} | write_manifest || exit 1
out=$(adm apply --manifest-dir "$MAN" 2>&1); code=$?
expect_code 'apply telemetry settings' 0 "$code" "$out"
[ "$code" -eq 0 ] || finish
stop_agent || { fail 'stop agent' 'could not stop agent'; finish; }

fixture=$(mktemp -d "$RUN/${PROJECT}-${ENVN}-inodes.XXXXXXXX") || exit 1
image="$fixture/disk.img"
mountdir="$fixture/mount"
cleanup() {
  local source backing
  if mountpoint -q "$mountdir"; then
    source=$(findmnt -n -o SOURCE --target "$mountdir") || return 1
    backing=$(sudo -n losetup -n -O BACK-FILE "$source") || return 1
    [ "$backing" = "$image" ] || { echo "refusing to unmount foreign source: $source ($backing)" >&2; return 1; }
    sudo -n umount "$mountdir" || return 1
  fi
  rm -f "$image" || return 1
  [ ! -d "$mountdir" ] || rmdir "$mountdir" || return 1
  rmdir "$fixture"
}
trap 'cleanup || echo "fixture cleanup failed: $fixture" >&2' EXIT
mkdir "$mountdir" || exit 1
truncate -s 2G "$image" || exit 1
mkfs.ext4 -q -F -N 1024 "$image" || exit 1
sudo -n mount -o loop "$image" "$mountdir" || exit 1
source=$(findmnt -n -o SOURCE --target "$mountdir") || exit 1
backing=$(sudo -n losetup -n -O BACK-FILE "$source") || exit 1
[ "$backing" = "$image" ] || { echo "mounted source is not the owned image: $backing" >&2; exit 1; }
sudo -n mkdir "$mountdir/files" || exit 1
sudo -n chown "$(id -u):$(id -g)" "$mountdir/files" || exit 1
python3 - "$mountdir/files" <<'PY'
import os, sys
path = sys.argv[1]
st = os.statvfs(path)
target = max(1, st.f_files * 3 // 100)
for i in range(st.f_ffree - target):
    with open(os.path.join(path, str(i)), 'x'):
        pass
PY
[ "$?" -eq 0 ] || exit 1
read -r total free blocks available blocksize < <(stat -f -c '%c %d %b %a %S' "$mountdir")
echo "  measured: inodes $free/$total free; blocks $available/$blocks available; block size $blocksize"
if ! (( total > 0 && free > 0 && free * 100 <= total * 5 && available * 100 > blocks * 20 && blocks * blocksize >= 1073741824 )); then
  fail 'prepare inode pressure' 'filesystem did not meet inode, byte, or capacity conditions'
  finish
fi
start_agent || { fail 'start agent' 'agent did not start'; finish; }
ready() {
  adm stats --json 2>/dev/null | jq -e --arg p "$mountdir" '.host.mounts[] | select(.path == $p)' >/dev/null || return 1
  adm checks --json 2>/dev/null | jq -e --arg n "disk:$mountdir" '.[] | select(.name == $n and .state != "unknown")' >/dev/null
}
# Check discovery can take a minute, followed by a check interval.
for i in $(seq 1 180); do ready && break; sleep 1; done
ready || fail 'sample and completed disk check' 'fixture mount was not observed'
df_json=$(adm df --json 2>&1); df_code=$?
stats_json=$(adm stats --json 2>&1); stats_code=$?
checks_json=$(adm checks --json 2>&1); checks_code=$?
echo "  df: $(jq -c --arg p "$mountdir" '.host[]? | select(.path == $p)' <<< "$df_json")"
echo "  stats: $(jq -c --arg p "$mountdir" '.host.mounts[]? | select(.path == $p)' <<< "$stats_json")"
echo "  check: $(jq -c --arg n "disk:$mountdir" '.[]? | select(.name == $n)' <<< "$checks_json")"
expect_code 'read df' 0 "$df_code" "$df_json"
expect_code 'read stats' 0 "$stats_code" "$stats_json"
expect_code 'read checks' 0 "$checks_code" "$checks_json"
inode_percent=$(awk -v total="$total" -v free="$free" 'BEGIN { printf "%.1f", 100 * (total - free) / total }')
check=$(jq -c --arg n "disk:$mountdir" '.[]? | select(.name == $n)' <<< "$checks_json")
expect_eq 'inode pressure reports warning' warning "$(jq -r '.state // "missing"' <<< "$check")"
expect_eq 'disk check reports inode use' "$inode_percent% inodes used" "$(jq -r '.detail // "missing"' <<< "$check")"
df_row=$(jq -c --arg p "$mountdir" '.host[]? | select(.path == $p)' <<< "$df_json")
if jq -e '(.total > 0) and (.available / .total > 0.2)' <<< "$df_row" >/dev/null; then
  pass 'df reports byte use below warning'
else
  fail 'df reports byte use below warning' "$df_row"
fi
for kind in df stats; do
  if [ "$kind" = df ]; then row=$(jq -c --arg p "$mountdir" '.host[]? | select(.path == $p)' <<< "$df_json"); else row=$(jq -c --arg p "$mountdir" '.host.mounts[]? | select(.path == $p)' <<< "$stats_json"); fi
  expect_eq "$kind total inodes" "$total" "$(jq -r '.inodes_total // "missing"' <<< "$row")"
  expect_eq "$kind free inodes" "$free" "$(jq -r '.inodes_free // "missing"' <<< "$row")"
  human=$(adm "$kind" 2>&1); code=$?
  expect_code "read human $kind" 0 "$code" "$human"
  block=$(awk -v p="$mountdir" 'BEGIN { RS="" } index($0, "path: " p) { print }' <<< "$human")
  expect_contains "human $kind total inodes" "$block" "inodes_total: $total"
  expect_contains "human $kind free inodes" "$block" "inodes_free: $free"
done
if cleanup; then trap - EXIT; else fail 'clean up inode fixture' "$fixture remains"; fi
finish
