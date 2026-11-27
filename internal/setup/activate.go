package setup

import (
	"context"
	"fmt"
	"strings"
	"time"

	"berthy/internal/scope"
)

// Where the agent lives on the server.
const (
	binPath        = "/usr/local/bin/berthy"
	guardProbePath = binPath + ".setup-probe"
	agentConfigDir = "/etc/berthy"
	configPath     = "/etc/berthy/agent.json"
	unitPath       = "/etc/systemd/system/berthy.service"
	dataDir        = "/var/lib/berthy"
)

// guardUnit names the transient rollback unit.
const guardUnit = "berthy-activation-guard"

// guardPath holds the root-only guard script on tmpfs.
const guardPath = "/run/berthy-activation-guard.sh"

// guardState and guardLock are shared by setup and the server timer.
const guardState = "/run/berthy-activation-guard.state"
const guardLock = "/run/berthy-activation-guard.lock"

const guardDecided = 3
const guardRecordFailed = 4

const recoveryUncertain = "setup may have started recovery; the server finishes it if it started, and the timer decides otherwise"

// guardDelay allows time for activation and its own rollback.
func guardDelay(secs int) int { return 2*secs + 60 }

// Staged says what a run left next to the live files for Activate to put in place: the binary as /usr/local/bin/berthy.new
// and the configuration as /etc/berthy/agent.json.new.
type Staged struct {
	Binary, Config bool
	Added          *scope.ID
}

// ActivateError is the failure of Activate: the new agent did not come up. Message already says what was put back and carries
// the agent's last log lines.
type ActivateError struct {
	Message       string
	ConfigOutcome ConfigOutcome
	Cause         error
}

func (e *ActivateError) Error() string { return e.Message }
func (e *ActivateError) Unwrap() error { return e.Cause }

// ConfigOutcome records what activation confirmed about the staged configuration.
type ConfigOutcome string

const (
	ConfigPrevious    ConfigOutcome = "previous"
	ConfigRetained    ConfigOutcome = "retained"
	ConfigUnconfirmed ConfigOutcome = "unconfirmed"
)

func activationFailure(err error, outcome ConfigOutcome) error {
	return &ActivateError{Message: err.Error(), Cause: err, ConfigOutcome: outcome}
}

const probeAsAgent = "setpriv --reuid=berthy --regid=berthy --clear-groups "

func selectProbeScript(staged Staged) string {
	candidates := shellQuote(binPath) + " " + shellQuote(guardProbePath)
	if staged.Binary {
		candidates = shellQuote(binPath+".new") + " " + candidates
	}
	return `can_probe() (
  marker=$(mktemp) || exit 1
  trap 'rm -f "$marker"' EXIT
  ulimit -f 1 || exit 1
  timeout --kill-after=1s 3s ` + probeAsAgent + `"$1" agent setup-probe version </dev/null >"$marker" || exit 1
  printf 'berthy-setup-probe/1\n' | cmp -s - "$marker"
)
for probe_bin in ` + candidates + `; do
  if [ -x "$probe_bin" ] && can_probe "$probe_bin" 2>/dev/null; then break; fi
  probe_bin=
done
if [ -z "$probe_bin" ]; then echo 'a probe-capable Berthy build is needed' >&2; exit 1; fi
`
}

// prepareProbeScript retains a capable executable across the live and previous binary renames.
func prepareProbeScript(staged Staged) string {
	return "# prepare setup probe\n" + selectProbeScript(staged) + "retained=" + shellQuote(guardProbePath) + "\n" + `
[ "$probe_bin" != "$retained" ] || exit 0
publish_failed() { echo 'cannot publish the setup probe' >&2; exit 1; }
tmp=$(mktemp "$retained.XXXXXX") || publish_failed
trap 'rm -f "$tmp"' EXIT
install -o root -g root -m 0755 -- "$probe_bin" "$tmp" && mv -fT -- "$tmp" "$retained" || publish_failed
`
}

// waitScript selects a client for a standalone health check without replacing the retained copy.
func waitScript(listen string, secs int) string {
	return selectProbeScript(Staged{}) + waitForProbe(listen, secs, "exit 0")
}

func waitScriptOnSuccess(probeBin, listen string, secs int, success string) string {
	return "probe_bin=" + shellQuote(probeBin) + "\n" + waitForProbe(listen, secs, success)
}

// waitForProbe uses the selected probe_bin for the whole wait.
func waitForProbe(listen string, secs int, success string) string {
	return waitScriptWithProbe(healthProbeCommand(listen, `"${probe}s"`), secs, success)
}

func healthProbeCommand(listen, timeout string) string {
	return probeAsAgent + "\"$probe_bin\" agent setup-probe health --listen " + shellQuote(listen) +
		" --timeout " + timeout + " </dev/null >/dev/null 2>&1"
}

func waitScriptWithProbe(probe string, secs int, success string) string {
	return fmt.Sprintf("end=$(( $(date +%%s) + %d ))\n", secs) +
		"while :; do\n" +
		"  active_state=$(systemctl show --property=ActiveState --value berthy) || exit 1\n" +
		"  case \"$active_state\" in active|activating|reloading) ;; *) exit 1 ;; esac\n" +
		"  left=$((end - $(date +%s)))\n" +
		"  [ \"$left\" -gt 0 ] || break\n" +
		"  probe=$left; [ \"$probe\" -le 3 ] || probe=3\n" +
		"  if " + probe + "; then " + success + "; fi\n" +
		"  [ \"$(date +%s)\" -lt \"$end\" ] || break\n" +
		"  sleep 1\n" +
		"done\n" +
		"exit 1\n"
}

func wholeSeconds(d time.Duration) int {
	n := int((d + time.Second - 1) / time.Second)
	if n < 1 {
		n = 1
	}
	return n
}

// Activate puts a staged agent in place and restarts the service, then waits until the agent answers /healthz on its listen
// address. staged says which of the binary and the configuration the caller staged; a service with nothing staged is only
// restarted. The previous binary and configuration are kept as berthy.prev and agent.json.prev for one generation.
// The caller must prepare the retained probe before invoking Activate.
//
// If the agent does not answer within timeout, Activate puts the previous binary and configuration back, restarts the
// service, and returns an *ActivateError with the agent's last log lines. When there was nothing to put back, which is the
// case on a first install, it stops the service and leaves it stopped.
//
// Only the agent's process restarts: no app is touched.
func Activate(ctx context.Context, r Runner, listen string, staged Staged, timeout time.Duration) error {
	secs := wholeSeconds(timeout)
	outcome := ConfigPrevious
	var out string
	armed := false
	prevBin, prevCfg := false, false
	guarded := func(err error) error {
		if armed {
			if prevCfg && outcome != ConfigPrevious {
				outcome = ConfigUnconfirmed
			}
			err = fmt.Errorf("%w; %s", err, guardText(prevBin || prevCfg, true, secs))
		}
		return activationFailure(err, outcome)
	}
	guard := guardScript(binPath, configPath, staged, listen, secs)
	if swap := swapScript(staged, secs, len(guard)); swap != "" {
		if err := ctx.Err(); err != nil {
			return activationFailure(fmt.Errorf("putting the new agent in place: %w", err), outcome)
		}
		res, err := r.Run(ctx, swap, strings.NewReader(guard))
		if err != nil {
			if staged.Config {
				outcome = ConfigUnconfirmed
			}
			return activationFailure(fmt.Errorf("putting the new agent in place: %w; %s", err, guardText(true, false, secs)), outcome)
		}
		if res.Code != 0 {
			if staged.Config {
				outcome = ConfigUnconfirmed
			}
			return activationFailure(fmt.Errorf("putting the new agent in place: %s; %s", failureText(res), guardText(true, false, secs)), outcome)
		}
		if staged.Config {
			outcome = ConfigRetained
		}
		armed = true
		out = string(res.Stdout)
	}
	prevBin = staged.Binary && strings.Contains(out, "prev_bin=yes")
	prevCfg = staged.Config && strings.Contains(out, "prev_cfg=yes")
	res, err := r.Run(ctx, restartScriptWithGuard(listen, secs, armed, staged.Added), nil)
	if err != nil {
		return guarded(fmt.Errorf("restarting the agent: %w", err))
	}
	detail := parseGuardReason(res.Stdout, staged.Added)
	if armed && res.Code == guardDecided {
		result := parseGuardState(res.Stdout)
		switch result {
		case guardRestored, guardRestoredDown, guardStopped, guardStopFailed, guardRestoreFailed:
			if detail == "" {
				detail, _ = journalTail(ctx, r)
			}
		}
		return guardOutcome(result, prevCfg, outcome, detail)
	}
	if armed && res.Code == guardRecordFailed {
		if prevCfg {
			outcome = ConfigUnconfirmed
		}
		return &ActivateError{Message: "the agent answered but the server could not record it; the server's timer checks it", ConfigOutcome: outcome}
	}
	if res.Code == 0 {
		return nil
	}
	if detail == "" {
		detail, err = journalTail(ctx, r)
		if err != nil && staged.Added == nil {
			return guarded(err)
		}
	}
	if !armed {
		res, err := r.Run(ctx, "systemctl stop berthy", nil)
		if err != nil {
			return activationFailure(fmt.Errorf("stopping the agent: %w", err), outcome)
		}
		if res.Code != 0 {
			return activationFailure(fmt.Errorf("stopping the agent: %s", failureText(res)), outcome)
		}
		return &ActivateError{Message: withLog("the agent did not come up and is left stopped", detail), ConfigOutcome: outcome}
	}
	binArg, cfgArg := "no", "no"
	if prevBin {
		binArg = "yes"
	}
	if prevCfg {
		cfgArg = "yes"
	}
	res, err = r.Run(ctx, "/bin/sh "+guardPath+" "+binArg+" "+cfgArg+" now", nil)
	if err != nil {
		if prevCfg {
			outcome = ConfigUnconfirmed
		}
		return activationFailure(fmt.Errorf("%s: %w", withLog(recoveryUncertain, detail), err), outcome)
	}
	if reason := parseGuardReason(res.Stdout, staged.Added); reason != "" {
		detail = reason
	}
	result := parseGuardState(res.Stdout)
	if result == guardNoWord {
		if prevCfg {
			outcome = ConfigUnconfirmed
		}
		return &ActivateError{Message: withLog(recoveryUncertain, detail), ConfigOutcome: outcome}
	}
	return guardOutcome(result, prevCfg, outcome, detail)
}

type guardResult uint8

const (
	guardNoWord guardResult = iota
	guardRunning
	guardConfirmed
	guardKept
	guardRestored
	guardRestoredDown
	guardStopped
	guardStopFailed
	guardRestoreFailed
)

func parseGuardState(out []byte) guardResult {
	unknown := false
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch line {
		case "guard=running":
			return guardRunning
		case "guard=confirmed":
			return guardConfirmed
		case "guard=kept":
			return guardKept
		case "guard=restored":
			return guardRestored
		case "guard=restored-down":
			return guardRestoredDown
		case "guard=stopped":
			return guardStopped
		case "guard=stop-failed":
			return guardStopFailed
		case "guard=restore-failed":
			return guardRestoreFailed
		default:
			if strings.HasPrefix(line, "guard=") && line != "guard=" {
				unknown = true
			}
		}
	}
	if unknown {
		return guardRunning
	}
	return guardNoWord
}

func guardOutcome(result guardResult, prevCfg bool, outcome ConfigOutcome, journal string) error {
	switch result {
	case guardConfirmed, guardKept:
		return nil
	case guardRestored, guardRestoredDown:
		if prevCfg {
			outcome = ConfigPrevious
		}
		msg := "the new agent did not come up; the previous one was put back"
		if result == guardRestoredDown {
			msg += ", but it does not answer either"
		}
		return &ActivateError{Message: withLog(msg, journal), ConfigOutcome: outcome}
	case guardStopped:
		return &ActivateError{Message: withLog("the agent did not come up and is left stopped", journal), ConfigOutcome: outcome}
	case guardStopFailed, guardRestoreFailed:
		msg := "the agent did not come up and could not be stopped"
		if result == guardRestoreFailed {
			msg = "the new agent did not come up and the previous one could not be put back"
		}
		return &ActivateError{Message: withLog(msg, journal), ConfigOutcome: outcome}
	default:
		if prevCfg {
			outcome = ConfigUnconfirmed
		}
		return &ActivateError{Message: withLog("the server guard took over and its result is not known yet", journal), ConfigOutcome: outcome}
	}
}

// guardText describes the recovery that may still run on the server.
func guardText(kept, certain bool, secs int) string {
	action := "stops it"
	if kept {
		action = "puts the previous agent back"
	}
	prefix := "the server"
	if !certain {
		prefix = "the swap may have armed a guard: the server"
		action = "puts the previous agent back or stops it when there is none"
	}
	return fmt.Sprintf("%s restarts the agent %d seconds after the swap and %s if it does not answer", prefix, guardDelay(secs), action)
}

// stateHelpers writes a complete word atomically while the caller holds the lock.
func stateHelpers() string {
	return "set_state() { tmp=$(mktemp " + guardState + ".XXXXXX) || return 1; " +
		"printf '%s\\n' \"$1\" > \"$tmp\" && mv -f \"$tmp\" " + guardState + "; }\n"
}

func stateCheck(guard bool, report string) string {
	s := "state=$(cat " + guardState + " 2>/dev/null) || state=running\n"
	if guard {
		return s + "case \"$state\" in\n" +
			"  pending|recovering) ;;\n" +
			"  confirmed|kept|restored|restored-down|stopped|stop-failed|restore-failed) printf 'guard=%s\\n' \"$state\"; " + report + "exit 0 ;;\n" +
			"  *) printf 'guard=%s\\n' \"${state:-running}\"; exit 1 ;;\n" +
			"esac\n"
	}
	return s + "if [ \"$state\" = recovering ]; then exit 1; fi\n" +
		fmt.Sprintf("if [ \"$state\" != pending ]; then printf 'guard=%%s\\n' \"${state:-running}\"; %sexit %d; fi\n", report, guardDecided)
}

// armScript writes the guard from stdin and starts its timer.
func armScript(secs, size int) string {
	return fmt.Sprintf(`umask 077
guard_tmp=$(mktemp %s.XXXXXX)
trap 'rm -f "$guard_tmp"' EXIT
cat > "$guard_tmp"
[ "$(wc -c < "$guard_tmp")" -eq %d ]
mv -f "$guard_tmp" %s
trap - EXIT
`, guardPath, size, guardPath) +
		"systemctl stop " + guardUnit + ".timer " + guardUnit + ".service >/dev/null 2>&1 || true\n" +
		"systemctl reset-failed " + guardUnit + ".timer " + guardUnit + ".service >/dev/null 2>&1 || true\n" +
		"exec 9>" + guardLock + "\nflock 9\nrm -f " + guardReason + "\n" + stateHelpers() + "set_state pending\n" +
		fmt.Sprintf("systemd-run --quiet --unit=%s --on-active=%ds --timer-property=AccuracySec=1s /bin/sh %s \"$prev_bin\" \"$prev_cfg\"\n", guardUnit, guardDelay(secs), guardPath)
}

// guardScript serializes a health decision and any recovery with setup.
func guardScript(bin, cfg string, staged Staged, listen string, secs int) string {
	var s strings.Builder
	reason, report, pending := "", "", ""
	if staged.Added != nil {
		reason, report = reasonHelpers(*staged.Added), "report_reason; "
		pending = " && ! validated_reason >/dev/null 2>&1"
	}
	s.WriteString("exec 9>" + guardLock + "\nflock 9 || exit 1\n" + stateHelpers() + reason + stateCheck(true, report))
	s.WriteString("up() { (\n" + restartScriptWithoutGuard(listen, secs) + "); }\n")
	candidate := "up"
	if staged.Added != nil {
		s.WriteString("candidate_up() { (\n" + restartScriptWithGuard(listen, secs, false, staged.Added) + "); }\n")
		candidate = "candidate_up"
	}
	s.WriteString("result() { set_state \"$1\" || exit 1; systemctl stop " + guardUnit + ".timer >/dev/null 2>&1 || true; printf 'guard=%s\\n' \"$1\"; " + report + "}\n")
	s.WriteString("if [ \"$state\" = pending ] && [ \"${3:-}\" != now ]" + pending + " && " + candidate + " >/dev/null 2>&1; then result kept; exit 0; fi\n")
	s.WriteString("set_state recovering || exit 1\n")
	s.WriteString("if [ \"$1\" != yes ] && [ \"$2\" != yes ]; then if systemctl stop berthy >/dev/null 2>&1; then result stopped; exit 0; else result stop-failed; exit 1; fi; fi\n")
	if staged.Binary {
		s.WriteString("if [ \"$1\" = yes ] && [ -e " + shellQuote(bin+".prev") + " ]; then mv -f " + shellQuote(bin+".prev") + " " + shellQuote(bin) + " >/dev/null 2>&1 || { result restore-failed; exit 1; }; fi\n")
	}
	if staged.Config {
		s.WriteString("if [ \"$2\" = yes ] && [ -e " + shellQuote(cfg+".prev") + " ]; then mv -f " + shellQuote(cfg+".prev") + " " + shellQuote(cfg) + " >/dev/null 2>&1 || { result restore-failed; exit 1; }; fi\n")
	}
	s.WriteString("if up >/dev/null 2>&1; then result restored; exit 0; else result restored-down; exit 1; fi\n")
	return s.String()
}

// swapScript arms the guard and puts staged files in place, keeping previous ones as .prev and printing which it kept.
// It is empty when nothing is staged.
func swapScript(staged Staged, secs, guardSize int) string {
	if !staged.Binary && !staged.Config {
		return ""
	}
	var s strings.Builder
	s.WriteString("set -e\nprev_bin=no; prev_cfg=no\n")
	for _, f := range []struct {
		enabled    bool
		path, name string
	}{{staged.Binary, binPath, "prev_bin"}, {staged.Config, configPath, "prev_cfg"}} {
		if f.enabled {
			s.WriteString("if [ -e " + f.path + " ]; then cp -p " + f.path + " " + f.path + ".prev; echo " + f.name + "=yes; " + f.name + "=yes; else rm -f " + f.path + ".prev; fi\n")
		}
	}
	s.WriteString(armScript(secs, guardSize))
	if staged.Binary {
		s.WriteString("mv -f " + binPath + ".new " + binPath + "\n")
	}
	if staged.Config {
		s.WriteString("mv -f " + configPath + ".new " + configPath + "\n")
	}
	return s.String()
}

// restartScript restarts the service and waits until the agent answers on listen, for secs seconds at most.
func restartScript(listen string, secs int) string {
	return restartScriptWithGuard(listen, secs, true, nil)
}

func restartScriptWithoutGuard(listen string, secs int) string {
	return restartScriptWithGuard(listen, secs, false, nil)
}

func restartScriptWithGuard(listen string, secs int, guarded bool, added *scope.ID) string {
	success := "exit 0"
	prefix := ""
	reason, report := "", ""
	if added != nil {
		reason, report = reasonHelpers(*added), "report_reason; "
	}
	if guarded {
		prefix = "exec 9>" + guardLock + "\n" + fmt.Sprintf("flock -w %d 9 || { echo guard=running; exit %d; }\n", secs, guardDecided) + stateHelpers() + reason + stateCheck(false, report)
		success = fmt.Sprintf("set_state confirmed || exit %d; systemctl stop %s.timer >/dev/null 2>&1 || true; exit 0", guardRecordFailed, guardUnit)
	} else {
		prefix = reason
	}
	wait := waitScriptOnSuccess(guardProbePath, listen, secs, success)
	if added != nil {
		prefix += candidateProbe(listen, *added)
		prefix += "old_invocation=$(systemctl show --property=InvocationID --value berthy 2>/dev/null) || exit 1\n"
		wait = "probe_bin=" + shellQuote(guardProbePath) + "\n" + waitScriptWithProbe("candidate_health \"$probe\"", secs, success)
	}
	return prefix + "systemctl reset-failed berthy >/dev/null 2>&1 || true\n" +
		"systemctl restart berthy >/dev/null 2>&1 || exit 1\n" + wait
}

// activateScripts lists the swap when staged and the restart for a successful activation.
// A dry run lists them. The scripts that put the previous agent back, and the journal, run only when the new agent does not come
// up, so a dry run does not list them.
func activateScripts(staged Staged, listen string, timeout time.Duration) []string {
	var out []string
	secs := wholeSeconds(timeout)
	guard := guardScript(binPath, configPath, staged, listen, secs)
	if swap := swapScript(staged, secs, len(guard)); swap != "" {
		out = append(out, swap, restartScriptWithGuard(listen, secs, true, staged.Added))
		return out
	}
	return append(out, restartScriptWithoutGuard(listen, secs))
}

// journalTail is the last lines the agent logged, for the failure message. -o cat leaves out the timestamp and host, so the
// lines are the agent's messages.
func journalTail(ctx context.Context, r Runner) (string, error) {
	res, err := r.Run(ctx, "journalctl -u berthy -n 20 --no-pager -o cat 2>&1", nil)
	if err != nil {
		return "", fmt.Errorf("reading the agent's log: %w", err)
	}
	return lastLines(res, 20), nil
}

func withLog(msg, journal string) string {
	if journal == "" {
		return msg
	}
	return msg + ": " + journal
}
