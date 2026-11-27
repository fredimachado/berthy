package setup

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Where the agent lives on the server.
const (
	binPath        = "/usr/local/bin/berthy"
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

// guardDelay allows time for activation and its own rollback.
func guardDelay(secs int) int { return 2*secs + 60 }

const defaultHealthTimeout = 30 * time.Second

// Staged says what a run left next to the live files for Activate to put in place: the binary as /usr/local/bin/berthy.new
// and the configuration as /etc/berthy/agent.json.new.
type Staged struct {
	Binary, Config bool
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

// getFunc is a shell function that asks a URL and succeeds when it answers with a success status. It uses curl, or wget.
const getFunc = `get() { if command -v curl >/dev/null 2>&1; then curl -fsS --max-time "$2" -o /dev/null "$1" >/dev/null 2>&1; else timeout "$2" wget --tries=1 -q -T "$2" -O /dev/null "$1" >/dev/null 2>&1; fi; }
`

// healthURL is where the agent answers whether it is up.
func healthURL(listen string) string { return "http://" + listen + "/healthz" }

// waitScript succeeds as soon as url answers, and fails when secs seconds pass, or earlier when systemd has given the service
// up.
func waitScript(url string, secs int) string { return waitScriptOnSuccess(url, secs, "exit 0") }

func waitScriptOnSuccess(url string, secs int, success string) string {
	return getFunc +
		fmt.Sprintf("end=$(( $(date +%%s) + %d ))\n", secs) +
		"while :; do\n" +
		"  left=$((end - $(date +%s)))\n" +
		"  [ \"$left\" -gt 0 ] || break\n" +
		"  probe=$left; [ \"$probe\" -le 3 ] || probe=3\n" +
		"  if get " + shellQuote(url) + " \"$probe\"; then " + success + "; fi\n" +
		"  if systemctl is-failed --quiet berthy; then exit 1; fi\n" +
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
	res, err := r.Run(ctx, restartScriptWithGuard(listen, secs, armed), nil)
	if err != nil {
		return guarded(fmt.Errorf("restarting the agent: %w", err))
	}
	if armed && res.Code == guardDecided {
		return guardOutcome(parseGuardState(res.Stdout), prevCfg, outcome, "")
	}
	if res.Code == 0 {
		return nil
	}
	journal, err := journalTail(ctx, r)
	if err != nil {
		return guarded(err)
	}
	if !armed {
		res, err := r.Run(ctx, "systemctl stop berthy", nil)
		if err != nil {
			return activationFailure(fmt.Errorf("stopping the agent: %w", err), outcome)
		}
		if res.Code != 0 {
			return activationFailure(fmt.Errorf("stopping the agent: %s", failureText(res)), outcome)
		}
		return &ActivateError{Message: withLog("the agent did not come up and is left stopped", journal), ConfigOutcome: outcome}
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
		return activationFailure(fmt.Errorf("setup lost the server while it put the previous agent back or stopped it; the server finishes it: %w", err), outcome)
	}
	result := parseGuardState(res.Stdout)
	if result == guardNoWord {
		if prevCfg {
			outcome = ConfigUnconfirmed
		}
		return &ActivateError{Message: withLog("setup may have started recovery; the server finishes it if it started, and the timer decides otherwise", journal), ConfigOutcome: outcome}
	}
	return guardOutcome(result, prevCfg, outcome, journal)
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
		if prevCfg {
			outcome = ConfigUnconfirmed
		}
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

func stateCheck(guard bool) string {
	s := "state=$(cat " + guardState + " 2>/dev/null) || state=running\n" +
		"if [ \"$state\" != pending ]; then printf 'guard=%s\\n' \"${state:-running}\"; "
	if guard {
		s += "[ \"$state\" = confirmed ] && exit 0; "
	}
	return s + fmt.Sprintf("exit %d; fi\n", guardDecided)
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
		"exec 9>" + guardLock + "\nflock 9\n" + stateHelpers() + "set_state pending\n" +
		fmt.Sprintf("systemd-run --quiet --unit=%s --on-active=%ds --timer-property=AccuracySec=1s /bin/sh %s \"$prev_bin\" \"$prev_cfg\"\n", guardUnit, guardDelay(secs), guardPath)
}

// guardScript serializes a health decision and any recovery with setup.
func guardScript(bin, cfg string, staged Staged, listen string, secs int) string {
	var s strings.Builder
	s.WriteString("exec 9>" + guardLock + "\nflock 9 || exit 1\n" + stateHelpers() + stateCheck(true))
	s.WriteString("if [ \"${3:-}\" = now ]; then systemctl stop " + guardUnit + ".timer >/dev/null 2>&1 || true; fi\n")
	s.WriteString("up() { (\n" + restartScriptWithoutGuard(listen, secs) + "); }\n")
	s.WriteString("result() { set_state \"$1\" || exit 1; printf 'guard=%s\\n' \"$1\"; }\n")
	s.WriteString("if [ \"${3:-}\" != now ] && up >/dev/null 2>&1; then result kept; exit 0; fi\n")
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
	return restartScriptWithGuard(listen, secs, true)
}

func restartScriptWithoutGuard(listen string, secs int) string {
	return restartScriptWithGuard(listen, secs, false)
}

func restartScriptWithGuard(listen string, secs int, guarded bool) string {
	success := "exit 0"
	prefix := ""
	if guarded {
		prefix = "exec 9>" + guardLock + "\n" + fmt.Sprintf("flock -w %d 9 || { echo guard=running; exit %d; }\n", secs, guardDecided) + stateHelpers() + stateCheck(false)
		success = "set_state confirmed || exit 1; systemctl stop " + guardUnit + ".timer >/dev/null 2>&1 || true; exit 0"
	}
	return prefix + "systemctl reset-failed berthy >/dev/null 2>&1 || true\n" +
		"systemctl restart berthy >/dev/null 2>&1 || exit 1\n" + waitScriptOnSuccess(healthURL(listen), secs, success)
}

// activateScripts lists the swap when staged and the restart for a successful activation.
// A dry run lists them. The scripts that put the previous agent back, and the journal, run only when the new agent does not come
// up, so a dry run does not list them.
func activateScripts(staged Staged, listen string, timeout time.Duration) []string {
	var out []string
	secs := wholeSeconds(timeout)
	guard := guardScript(binPath, configPath, staged, listen, secs)
	if swap := swapScript(staged, secs, len(guard)); swap != "" {
		out = append(out, swap, restartScript(listen, secs))
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
