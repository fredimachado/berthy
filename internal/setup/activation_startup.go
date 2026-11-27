package setup

import "berthy/internal/scope"

const startupResult = "/run/berthy-startup/result"

// candidateProbe binds the result and the listening socket to the service invocation that answers the health request.
func candidateProbe(listen string, target scope.ID) string {
	return "target=" + shellQuote(target.String()) + "\n" +
		"endpoint=" + shellQuote(listen) + "\n" +
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
  LC_ALL=C awk -F '\t' -v invocation="$1" -v target="$target" '
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
          ($2 == "failed" && ($3 == "" || length($3) > 160 || $3 ~ /[^ -~]/))) bad=1
      rows++
      if ($1 == target) row=($2 == "failed" ? $2 " " $3 : $2)
    }
    END { if (bad || !complete || rows != count || row == "") exit 1; print row }' "$startup_result"
}
owns_listener() {
  ss -H -ltnp "src $endpoint" 2>/dev/null |
    awk -v owner="\",pid=$1,fd=" '{ if (index($0, owner) == 0) bad=1 } END { exit NR == 0 || bad }'
}
candidate_health() {
  current=$(identity) || return 1
  inv=${current% *}
  pid=${current#* }
  [ "$inv" != "$old_invocation" ] || return 1
  row=$(target_result "$inv") || return 1
  case "$row" in
    'failed '*)
      [ "$(identity)" = "$current" ] || return 1
      exit 1 ;;
  esac
  owns_listener "$pid" && ` + healthProbeCommand(listen, `"${1}s"`) + ` &&
    [ "$(identity)" = "$current" ] && owns_listener "$pid"
}
`
}
