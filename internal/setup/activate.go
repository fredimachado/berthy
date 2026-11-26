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

const guardUnit = "berthy-activation-guard"
const guardPath = "/run/berthy-activation-guard.sh"
const disarmScript = "systemctl stop " + guardUnit + ".timer >/dev/null 2>&1 || true\n"

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
const getFunc = `get() { if command -v curl >/dev/null 2>&1; then curl -fsS --max-time 3 -o /dev/null "$1" >/dev/null 2>&1; else wget -q -T 3 -O /dev/null "$1" >/dev/null 2>&1; fi; }
`

// healthURL is where the agent answers whether it is up.
func healthURL(listen string) string { return "http://" + listen + "/healthz" }

// waitScript succeeds as soon as url answers, and fails when secs seconds pass, or earlier when systemd has given the service
// up.
func waitScript(url string, secs int) string {
	return getFunc +
		"i=0\n" +
		fmt.Sprintf("while [ \"$i\" -lt %d ]; do\n", secs) +
		"  if get " + shellQuote(url) + "; then exit 0; fi\n" +
		"  if systemctl is-failed --quiet berthy; then exit 1; fi\n" +
		"  sleep 1\n" +
		"  i=$((i+1))\n" +
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
	var out string
	secs := wholeSeconds(timeout)
	armed := false
	prevCfg := false
	prevBin := false
	outcome := ConfigPrevious
	guarded := func(err error) error {
		if armed {
			if prevCfg && outcome != ConfigPrevious {
				outcome = ConfigUnconfirmed
			}
			err = fmt.Errorf("%w; %s", err, guardText(prevBin || prevCfg, true, secs))
		}
		return activationFailure(err, outcome)
	}
	if swap := swapScript(staged, secs); swap != "" {
		if err := ctx.Err(); err != nil {
			return activationFailure(fmt.Errorf("putting the new agent in place: %w", err), outcome)
		}
		res, err := r.Run(ctx, swap, strings.NewReader(guardScript(binPath, configPath, staged, listen, secs)))
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

	up, err := restartAndWait(ctx, r, listen, secs)
	if err != nil {
		return guarded(err)
	}
	if up {
		if armed {
			disarm(ctx, r)
		}
		return nil
	}

	journal, err := journalTail(ctx, r)
	if err != nil {
		return guarded(err)
	}
	if !prevBin && !prevCfg {
		res, err := r.Run(ctx, "systemctl stop berthy", nil)
		if err != nil {
			return guarded(fmt.Errorf("stopping the agent: %w", err))
		}
		if res.Code != 0 {
			return guarded(fmt.Errorf("stopping the agent: %s", failureText(res)))
		}
		if armed {
			disarm(ctx, r)
		}
		return &ActivateError{Message: withLog("the agent did not come up and is left stopped", journal), ConfigOutcome: outcome}
	}
	var back strings.Builder
	back.WriteString("set -e\n")
	if prevBin {
		back.WriteString("mv -f " + binPath + ".prev " + binPath + "\n")
	}
	if prevCfg {
		back.WriteString("mv -f " + configPath + ".prev " + configPath + "\n")
	}
	res, err := r.Run(ctx, back.String(), nil)
	if err != nil {
		if prevCfg {
			outcome = ConfigUnconfirmed
		}
		return guarded(fmt.Errorf("putting the previous agent back: %w", err))
	}
	if res.Code != 0 {
		if prevCfg {
			outcome = ConfigUnconfirmed
		}
		return guarded(fmt.Errorf("%s", withLog("the new agent did not come up and the previous one could not be put back: "+failureText(res), journal)))
	}
	if prevCfg {
		outcome = ConfigPrevious
	}
	msg := "the new agent did not come up; the previous one was put back"
	again, err := restartAndWait(ctx, r, listen, secs)
	if err != nil {
		return guarded(err)
	}
	if armed {
		disarm(ctx, r)
	}
	if !again {
		msg += ", but it does not answer either"
	}
	return &ActivateError{Message: withLog(msg, journal), ConfigOutcome: outcome}
}

// swapScript puts the staged binary and configuration in place, keeping the previous ones as .prev and printing which it kept. It
// is "" when nothing is staged.
func guardText(kept, certain bool, secs int) string {
	action := "stops it"
	if kept {
		action = "puts the previous agent back"
	}
	prefix := "the server"
	if !certain {
		prefix = "the swap may have armed a guard: the server"
	}
	return fmt.Sprintf("%s restarts the agent %d seconds after the swap and %s if it does not answer", prefix, guardDelay(secs), action)
}
func disarm(ctx context.Context, r Runner) { _, _ = r.Run(ctx, disarmScript, nil) }
func armScript(secs int) string {
	return "(umask 077; cat > " + guardPath + ")\n" +
		"systemctl stop " + guardUnit + ".timer " + guardUnit + ".service >/dev/null 2>&1 || true\n" +
		"systemctl reset-failed " + guardUnit + ".timer " + guardUnit + ".service >/dev/null 2>&1 || true\n" +
		fmt.Sprintf("systemd-run --quiet --unit=%s --on-active=%ds --timer-property=AccuracySec=1s /bin/sh %s \"$prev_bin\" \"$prev_cfg\"\n", guardUnit, guardDelay(secs), guardPath)
}
func guardScript(bin, cfg string, staged Staged, listen string, secs int) string {
	var s strings.Builder
	s.WriteString("up() { (\n" + restartScript(listen, secs) + "); }\nif up; then exit 0; fi\nif [ \"$1\" != yes ] && [ \"$2\" != yes ]; then systemctl stop berthy; exit 0; fi\n")
	if staged.Binary {
		s.WriteString("if [ \"$1\" = yes ] && [ -e " + shellQuote(bin+".prev") + " ]; then mv -f " + shellQuote(bin+".prev") + " " + shellQuote(bin) + "; fi\n")
	}
	if staged.Config {
		s.WriteString("if [ \"$2\" = yes ] && [ -e " + shellQuote(cfg+".prev") + " ]; then mv -f " + shellQuote(cfg+".prev") + " " + shellQuote(cfg) + "; fi\n")
	}
	s.WriteString("up\n")
	return s.String()
}
func swapScript(staged Staged, secs int) string {
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
	s.WriteString(armScript(secs))
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
	// reset-failed clears the start limit a crash loop used up, or the restart would be refused as too quick.
	return "systemctl reset-failed berthy >/dev/null 2>&1 || true\n" +
		"systemctl restart berthy 2>&1 || exit 1\n" + waitScript(healthURL(listen), secs)
}

// activateScripts are the scripts Activate sends when the new agent comes up: the swap, when something is staged, and the restart.
// A dry run lists them. The scripts that put the previous agent back, and the journal, run only when the new agent does not come
// up, so a dry run does not list them.
func activateScripts(staged Staged, listen string, timeout time.Duration) []string {
	var out []string
	if swap := swapScript(staged, wholeSeconds(timeout)); swap != "" {
		out = append(out, swap, restartScript(listen, wholeSeconds(timeout)), disarmScript)
		return out
	}
	return append(out, restartScript(listen, wholeSeconds(timeout)))
}

// restartAndWait restarts the service and reports whether the agent answered within the given seconds.
func restartAndWait(ctx context.Context, r Runner, listen string, secs int) (bool, error) {
	res, err := r.Run(ctx, restartScript(listen, secs), nil)
	if err != nil {
		return false, fmt.Errorf("restarting the agent: %w", err)
	}
	return res.Code == 0, nil
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
