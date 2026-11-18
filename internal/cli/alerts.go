package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// printAlertsTest prints what alerts test sent: one line for each channel, "ok" or "failed" with the reason. It returns 1
// when any channel failed. With asJSON it prints the agent's answer as it is, and the exit code is the same.
func printAlertsTest(out, errout io.Writer, data []byte, asJSON bool) int {
	var answer struct {
		Channels []struct {
			Channel string `json:"channel"`
			OK      bool   `json:"ok"`
			Error   string `json:"error"`
		} `json:"channels"`
	}
	if json.Unmarshal(data, &answer) != nil || len(answer.Channels) == 0 {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	code := 0
	for _, ch := range answer.Channels {
		if !ch.OK {
			code = 1
		}
	}
	if asJSON {
		var pretty bytes.Buffer
		if json.Indent(&pretty, data, "", "  ") != nil {
			fmt.Fprintln(errout, "invalid agent response")
			return 1
		}
		fmt.Fprintln(out, pretty.String())
		return code
	}
	for _, ch := range answer.Channels {
		if ch.OK {
			fmt.Fprintf(out, "%s: ok\n", terminalSafe(ch.Channel))
			continue
		}
		fmt.Fprintf(out, "%s: failed: %s\n", terminalSafe(ch.Channel), terminalSafe(ch.Error))
	}
	return code
}
