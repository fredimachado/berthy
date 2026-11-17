package tui

import (
	"strings"
	"testing"
	"time"
)

// TestHistoryRowsSayWhenADeployCameFromLocalChanges: the commit of a release or a failed deploy from a checkout with local
// changes is marked, in the list of the history and in its text.
func TestHistoryRowsSayWhenADeployCameFromLocalChanges(t *testing.T) {
	clean := historyRow{ID: 2, Digest: "sha256:0123456789abcdef", Commit: "abc1234def5678", Created: testNow.Add(-time.Hour)}
	local := clean
	local.Modified = true
	if got := clean.commitText(12); got != "abc1234def5…" {
		t.Fatalf("clean commit text %q", got)
	}
	if got := local.commitText(12); got != "abc1234def5… (modified)" {
		t.Fatalf("modified commit text %q", got)
	}
	st := newStyles(palette{})
	if text := strings.Join(deployLines(st, testNow, local, 80, 20), "\n"); !strings.Contains(text, "commit abc1234def5… (modified)") {
		t.Fatalf("release line:\n%s", text)
	}
	if text := strings.Join(deployLines(st, testNow, clean, 80, 20), "\n"); strings.Contains(text, "modified") {
		t.Fatalf("clean release line:\n%s", text)
	}
	failed := historyRow{OperationID: "op1", Kind: "deploy", State: "failed", Commit: "abc1234def5678", Modified: true, Created: testNow.Add(-time.Hour)}
	if text := strings.Join(deployLines(st, testNow, failed, 80, 20), "\n"); !strings.Contains(text, "commit abc1234def5… (modified)") {
		t.Fatalf("failed deploy line:\n%s", text)
	}
}
