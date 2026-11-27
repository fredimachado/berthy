package setup

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"berthy/internal/scope"
	"berthy/internal/state"
)

const startupResult = "/run/berthy-startup/result"
const guardReason = "/run/berthy-activation-guard.reason"

var startupReason = regexp.MustCompile("^(" + regexp.QuoteMeta(state.ErrWrongKey.Error()) +
	"|permission denied opening state|cannot open state|state belongs to another environment" +
	"|cannot read state claims|cannot reserve configured network|deployment recovery failed)$")

// The callers hold the activation lock. Replacing the reason atomically keeps it usable after interrupted recovery.
func reasonHelpers(target scope.ID) string {
	return "reason_file=" + guardReason + "\n" +
		"reason_target=" + shellQuote(target.String()) + "\n" +
		"public_reason=" + shellQuote(startupReason.String()) + "\n" + `
record_reason() (
  umask 077
  tmp=$(mktemp "$reason_file.XXXXXX") || exit 1
  trap 'rm -f "$tmp"' EXIT
  printf '%s\t%s\t%s\n' "$1" "$2" "$3" > "$tmp" && mv -f "$tmp" "$reason_file"
)
validated_reason() {
  [ -f "$reason_file" ] && [ ! -L "$reason_file" ] || return 1
  [ "$(stat -c '%u:%a:%h' "$reason_file" 2>/dev/null)" = "0:600:1" ] || return 1
  [ "$(tail -c 1 "$reason_file" | wc -l)" -eq 1 ] || return 1
  LC_ALL=C awk -F '\t' -v target="$reason_target" -v reason="$public_reason" '
    NR == 1 && NF == 3 && length($1) == 32 && $1 ~ /^[0-9a-f]+$/ && $1 !~ /^0+$/ &&
        $2 == target && $3 ~ reason { row=$0 }
    END { if (NR != 1 || row == "") exit 1; print row }
  ' "$reason_file"
}
report_reason() {
  if rejected=$(validated_reason); then printf 'guard-reason\t%s\n' "$rejected"; fi
}
`
}

func parseGuardReason(out []byte, added *scope.ID) string {
	if added == nil {
		return ""
	}
	var detail string
	for _, line := range strings.Split(string(out), "\n") {
		raw, ok := strings.CutPrefix(line, "guard-reason\t")
		if !ok {
			continue
		}
		fields := strings.Split(raw, "\t")
		if detail != "" || len(fields) != 3 || fields[1] != added.String() {
			return ""
		}
		inv, reason := fields[0], fields[2]
		if _, err := hex.DecodeString(inv); err != nil || len(inv) != 32 || strings.Trim(inv, "0") == "" || inv != strings.ToLower(inv) ||
			!startupReason.MatchString(reason) {
			return ""
		}
		detail = fields[1] + ": " + reason
	}
	return detail
}

// candidateProbe binds the result and the listening socket to the service invocation that answers the health request.
func candidateProbe(listen string, target scope.ID) string {
	ipv4, ipv6 := procListenAddresses(listen)
	return "target=" + shellQuote(target.String()) + "\n" +
		"proc_root=/proc\n" +
		"addr4=" + shellQuote(ipv4) + "\n" +
		"addr6=" + shellQuote(ipv6) + "\n" +
		"startup_result=" + startupResult + "\n" + `
identity() {
  systemctl show --property=InvocationID --property=MainPID berthy 2>/dev/null |
    LC_ALL=C awk -F= '
      $1 == "InvocationID" { inv=$2; ni++ }
      $1 == "MainPID" { pid=$2; np++ }
      END {
        if (ni != 1 || np != 1 || length(inv) != 32 || inv !~ /^[0-9a-f]+$/ ||
            inv ~ /^0+$/ || pid !~ /^[1-9][0-9]*$/) exit 1
        printf "%s %s\n", inv, pid
      }'
}
target_result() {
  [ -f "$startup_result" ] && [ ! -L "$startup_result" ] || return 1
  group=$(id -g berthy) || return 1
  [ "$(stat -c '%u:%g:%a:%h' "$startup_result")" = "0:$group:620:1" ] || return 1
  [ "$(tail -c 1 "$startup_result" | wc -l)" -eq 1 ] || return 1
  LC_ALL=C awk -F '\t' -v invocation="$1" -v target="$target" -v reason="$public_reason" '
    NR == 1 {
      if (NF != 3 || $1 != "berthy-startup-v1" || ($2 "") != (invocation "") || $3 !~ /^[1-9][0-9]*$/) bad=1
      count=$3
      next
    }
    $0 == "complete" {
      if (complete) bad=1
      complete=1
      next
    }
    {
      if (complete || NF != 3 || length($1) > 59 || $1 !~ /^[a-z0-9][a-z0-9_-]*\/[a-z0-9][a-z0-9-]*$/ ||
          seen[$1]++ || ($2 != "started" && $2 != "failed") ||
          ($2 == "started" && $3 != "") ||
          ($2 == "failed" && $3 !~ reason)) bad=1
      rows++
      if ($1 == target) row=($2 == "failed" ? $2 " " $3 : $2)
    }
    END { if (bad || !complete || rows != count || row == "") exit 1; print row }' "$startup_result"
}
owns_listener() (
  pid_fd="$proc_root/$1/fd"
  set -- "$proc_root/net/tcp"
  if [ -e "$proc_root/net/tcp6" ]; then set -- "$@" "$proc_root/net/tcp6"; fi
  for fd in "$pid_fd/"*; do
    link=$(readlink "$fd" 2>/dev/null) || continue
    case "$link" in
      'socket:['*']')
        inode=${link#'socket:['}
        inode=${inode%']'}
        case "$inode" in ''|*[!0-9]*) ;; *) printf '%s\n' "$inode";; esac ;;
    esac
  done | LC_ALL=C awk -v addr4="$addr4" -v addr6="$addr6" '
    FILENAME == "-" { owned["inode:" $1]=1; next }
    $4 == "0A" && ($2 == addr4 || $2 == addr6) {
      found=1
      if (!owned["inode:" $10]) bad=1
    }
    END { exit (!found || bad) }
  ' - "$@"
)
candidate_health() {
  current=$(identity) || return 1
  inv=${current% *}
  pid=${current#* }
  [ "$inv" != "$old_invocation" ] || return 1
  row=$(target_result "$inv") || return 1
  case "$row" in
    'failed '*)
      [ "$(identity)" = "$current" ] || return 1
      record_reason "$inv" "$target" "${row#failed }" || :
      set_state recovering || :
      report_reason
      exit 1 ;;
  esac
  owns_listener "$pid" && ` + healthProbeCommand(listen, `"${1}s"`) + ` &&
    [ "$(identity)" = "$current" ] && owns_listener "$pid"
}
`
}

// Linux prints each address as native-endian 32-bit words. The provisioned
// Linux agents run on little-endian amd64 or arm64.
func procListenAddresses(listen string) (ipv4, ipv6 string) {
	addr, err := netip.ParseAddrPort(listen)
	if err != nil {
		return "", ""
	}
	ip := addr.Addr().Unmap()
	if ip.Is4() {
		v4 := ip.As4()
		ipv4 = fmt.Sprintf("%08X:%04X", binary.LittleEndian.Uint32(v4[:]), addr.Port())
	}
	v6 := ip.As16()
	ipv6 = fmt.Sprintf("%08X%08X%08X%08X:%04X", binary.LittleEndian.Uint32(v6[:4]),
		binary.LittleEndian.Uint32(v6[4:8]), binary.LittleEndian.Uint32(v6[8:12]),
		binary.LittleEndian.Uint32(v6[12:]), addr.Port())
	return ipv4, ipv6
}
