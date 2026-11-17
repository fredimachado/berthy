package setup

import (
	"bytes"
	"context"
	"errors"
	"strings"

	"berthy/deploy/systemd"
)

// unitContent is the unit file setup installs: exactly deploy/systemd/berthy.service, with Unix line endings.
func unitContent() []byte { return bytes.ReplaceAll(systemd.Unit, []byte("\r\n"), []byte("\n")) }

// unitScript installs the unit that arrives on the script's standard input, and reloads systemd.
func unitScript() string {
	return "set -e\numask 022\n" +
		"tmp=$(mktemp " + unitPath + ".XXXXXX)\n" +
		"trap 'rm -f \"$tmp\"' EXIT\n" +
		"cat > \"$tmp\"\n" +
		"chmod 0644 \"$tmp\"\n" +
		"mv -f \"$tmp\" " + unitPath + "\n" +
		"systemctl daemon-reload\n"
}

// enableScript makes berthy.service start at boot.
const enableScript = "systemctl enable berthy 2>&1"

// serviceStep installs the unit, enables the service and starts it, or restarts it when the binary, the configuration or the
// unit changed. It waits for the agent to answer and puts the previous agent back when it does not. Running apps are not
// touched: only the agent's process restarts.
func serviceStep(ctx context.Context, s *Session) (string, string, error) {
	f, err := s.facts(ctx)
	if err != nil {
		return "", "", err
	}
	unit := unitContent()
	s.unitChanged = f.UnitSHA != sha256Hex(unit)
	var reasons []string
	if s.staged.Binary {
		reasons = append(reasons, "binary")
	}
	if s.staged.Config {
		reasons = append(reasons, "configuration")
	}
	if s.unitChanged {
		reasons = append(reasons, "unit")
	}
	enable := !f.ServiceEnabled || s.unitChanged
	if len(reasons) == 0 && f.ServiceActive && f.ServiceEnabled {
		return StatusOK, "berthy.service is running and enabled", nil
	}
	first := f.UnitSHA == "" && !f.ServiceActive
	restart := len(reasons) > 0 || !f.ServiceActive
	var text string
	switch {
	case first:
		text = "install, enable and start berthy.service"
	case len(reasons) > 0:
		text = "restart berthy.service (" + strings.Join(reasons, ", ") + " changed)"
	case !f.ServiceActive:
		text = "start berthy.service"
	default:
		text = "enable berthy.service"
	}
	if s.DryRun {
		if s.unitChanged {
			s.Log.Would(unitScript())
		}
		if enable {
			s.Log.Would(enableScript)
		}
		if restart {
			for _, sc := range activateScripts(s.staged, s.Opts.listen(), s.Opts.healthTimeout()) {
				s.Log.Would(sc)
			}
		}
		return StatusWouldChange, "would " + text, nil
	}
	if s.unitChanged {
		res, err := s.R.Run(ctx, unitScript(), bytes.NewReader(unit))
		if err != nil {
			return "", "", errors.New("installing berthy.service: " + err.Error())
		}
		if res.Code != 0 {
			return "", "", errors.New("installing berthy.service: " + failureText(res))
		}
	}
	if enable {
		if _, err := s.change(ctx, "enabling berthy.service", enableScript); err != nil {
			return "", "", err
		}
	}
	if restart {
		err := Activate(ctx, s.R, s.Opts.listen(), s.staged, s.Opts.healthTimeout())
		var ae *ActivateError
		switch {
		case err == nil:
			s.tokenLive()
		case errors.As(err, &ae):
			if ae.ConfigInPlace {
				s.tokenLive()
			}
			return "", "", err
		default:
			return "", "", err
		}
	}
	return StatusChanged, past(text), nil
}

// tokenLive records that the configuration holding the token's digest is the server's live one.
func (s *Session) tokenLive() {
	if s.Token != "" && s.staged.Config {
		s.TokenLive = true
	}
}

// past turns the service step's plan into what happened.
func past(text string) string {
	switch {
	case strings.HasPrefix(text, "install, enable and start"):
		return "installed, enabled and started berthy.service"
	case strings.HasPrefix(text, "restart"):
		return "restarted" + strings.TrimPrefix(text, "restart")
	case strings.HasPrefix(text, "start"):
		return "started" + strings.TrimPrefix(text, "start")
	}
	return "enabled" + strings.TrimPrefix(text, "enable")
}
