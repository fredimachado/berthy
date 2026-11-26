package cli

import (
	"fmt"
	"io"
	"net/http"
	"os"

	"berthy/internal/agentclient"
)

func printRefusal(errout io.Writer, safeMessage string, human bool) {
	fmt.Fprintln(errout, styleFor(errout, human).paint(toneError, safeMessage))
}

func printAgentRefusal(errout io.Writer, safeMessage string, human, older bool) {
	lead := agentclient.Message(http.StatusNotFound, nil) + ": "
	if older && safeMessage == lead+agentclient.OlderAgentMessage {
		s := styleFor(errout, human)
		fmt.Fprintln(errout, s.paint(toneError, lead)+s.paint(toneInfo, agentclient.OlderAgentMessage))
		return
	}
	printRefusal(errout, safeMessage, human)
}

type cliStyle struct{ color bool }
type cliTone uint8

const (
	tonePlain cliTone = iota
	toneOK
	toneWarning
	toneError
	toneInfo
	toneMuted
	toneAccent
	toneBold
)

func styleFor(w io.Writer, human bool) cliStyle {
	_, err := progressWidth(w)
	return cliStyle{color: human && err == nil && progressVT(w, os.Getenv("TERM")) && os.Getenv("NO_COLOR") == ""}
}

func (s cliStyle) paint(t cliTone, safeText string) string {
	if !s.color || safeText == "" {
		return safeText
	}
	codes := [...]string{"", "32", "33", "31", "36", "2", "35", "1"}
	if int(t) >= len(codes) || codes[t] == "" {
		return safeText
	}
	return "\x1b[" + codes[t] + "m" + safeText + "\x1b[0m"
}

func (s cliStyle) Status(word string) string { return s.paint(statusTone(word), terminalSafe(word)) }

func statusTone(word string) cliTone {
	switch word {
	case "succeeded", "ok", "changed", "healthy", "stored", "approved", "available":
		return toneOK
	case "would_change", "planned", "pending", "running":
		return toneInfo
	case "skipped", "warning", "warn", "starting", "partial", "held":
		return toneWarning
	case "failed", "critical", "rolled_back", "recovery_required", "timed_out", "exited", "unhealthy", "refused", "rejected", "unavailable", "interrupted":
		return toneError
	case "unknown", "expired", "never":
		return toneMuted
	default:
		return tonePlain
	}
}
