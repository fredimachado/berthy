package tui

import (
	"strings"
	"testing"
	"time"

	"berthy/internal/api"
)

// A job of a manifest with a timezone shows its next occurrence on that clock; a UTC job shows only how long is left.
func TestJobLinesShowTheNextOccurrenceInTheManifestsZone(t *testing.T) {
	st := newStyles(palette{})
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	next := time.Date(2026, 11, 19, 17, 0, 0, 0, time.UTC)
	job := api.JobStatus{Name: "cleanup", Schedule: "0 2 * * *", Timezone: "Asia/Tokyo", Next: &next}
	head := stripANSIForTest(jobLines(st, now, job, 80)[0])
	if !strings.Contains(head, "next in 5h") || !strings.Contains(head, "2026-11-20 02:00 JST") {
		t.Errorf("a Tokyo job: %q", head)
	}
	job.Timezone = "UTC"
	if head = stripANSIForTest(jobLines(st, now, job, 80)[0]); strings.Contains(head, "UTC") || !strings.Contains(head, "next in 5h") {
		t.Errorf("a UTC job: %q", head)
	}
}
