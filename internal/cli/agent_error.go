package cli

import (
	"berthy/internal/agentclient"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func printAgentRequestError(out, errout io.Writer, target agentclient.Target, err error, asJSON bool, extra ...string) {
	if errors.Is(err, agentclient.ErrInvalidResponse) {
		fmt.Fprintln(errout, "invalid agent response")
		return
	}
	redact := target.Redact
	if len(extra) > 0 {
		redact = func(s string) string { return redactSecretText(s, append([]string{target.Token}, extra...)...) }
	}
	reason := agentclient.ConnectionMessage(target.Base, err, redact)
	if asJSON {
		json.NewEncoder(out).Encode(struct {
			Code   string `json:"code"`
			Reason string `json:"reason"`
		}{"agent_connection_failed", reason}) //nolint:errcheck // Output errors are handled consistently with the other CLI renderers.
	} else {
		fmt.Fprintln(errout, reason)
	}
}
