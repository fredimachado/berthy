package cli

import "testing"

func TestStatusVocabulary(t *testing.T) {
	for _, tc := range []struct {
		words []string
		tone  cliTone
		code  string
	}{
		{[]string{"succeeded", "ok", "changed", "healthy", "stored", "approved", "available"}, toneOK, "32"},
		{[]string{"would_change", "planned", "pending", "running"}, toneInfo, "36"},
		{[]string{"skipped", "warning", "warn", "starting", "partial", "held"}, toneWarning, "33"},
		{[]string{"failed", "critical", "rolled_back", "recovery_required", "timed_out", "exited", "unhealthy", "refused", "rejected", "unavailable", "interrupted"}, toneError, "31"},
		{[]string{"unknown", "expired", "never"}, toneMuted, "2"},
		{[]string{"custom", "OK", "bad\x1b"}, tonePlain, ""},
	} {
		for _, word := range tc.words {
			if got := statusTone(word); got != tc.tone {
				t.Errorf("%q tone = %v", word, got)
			}
			safe := terminalSafe(word)
			want := safe
			if tc.code != "" {
				want = "\x1b[" + tc.code + "m" + safe + "\x1b[0m"
			}
			if got := (cliStyle{color: true}).Status(word); got != want {
				t.Errorf("%q = %q, want %q", word, got, want)
			}
			if got := (cliStyle{}).Status(word); got != safe {
				t.Errorf("plain %q = %q", word, got)
			}
		}
	}
	for _, tc := range []struct {
		tone cliTone
		want string
	}{{toneAccent, "\x1b[35mx\x1b[0m"}, {toneBold, "\x1b[1mx\x1b[0m"}} {
		if got := (cliStyle{color: true}).paint(tc.tone, "x"); got != tc.want {
			t.Errorf("paint = %q", got)
		}
		if got := (cliStyle{color: true}).paint(tc.tone, ""); got != "" {
			t.Errorf("empty paint = %q", got)
		}
	}
}
