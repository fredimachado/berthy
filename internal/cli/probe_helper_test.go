package cli

import (
	"bytes"
	"strings"
	"testing"

	"berthy/internal/monitor"
)

// The overlay helper's command is hidden: it is not in the usage, and without a valid token it refuses to serve without
// printing the token it was given.
func TestProbeHelperIsHiddenAndNeedsItsToken(t *testing.T) {
	const given = "token-that-is-not-valid-but-must-not-be-echoed"
	t.Setenv(monitor.HelperTokenEnv, given)
	var out, errout bytes.Buffer
	if code := Run([]string{"probe-helper"}, &out, &errout); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errout.String(), "token missing or invalid") || strings.Contains(errout.String(), given) {
		t.Fatalf("stderr %q", errout.String())
	}
	out.Reset()
	errout.Reset()
	if code := Run(nil, &out, &errout); code != 2 || strings.Contains(errout.String(), "probe-helper") {
		t.Fatalf("usage %d %q", code, errout.String())
	}
}
