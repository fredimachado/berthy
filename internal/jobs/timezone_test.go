package jobs

import (
	"context"
	"testing"
	"time"

	"berthy/internal/manifest"
)

// A job at 02:00 with a timezone runs at 02:00 local time, so its occurrence is a different UTC instant on each side of a
// clock change, and it is recorded by that instant.
func TestAJobRunsAtItsLocalTimeOnBothSidesOfAClockChange(t *testing.T) {
	ctx := context.Background()
	daily := tickJob()
	daily.Schedule = "0 2 * * *"
	r := newRig(t, map[string]manifest.Job{"nightly": daily})
	r.zone = "Europe/Lisbon"
	for _, tc := range []struct {
		now  time.Time
		runs int
	}{
		{time.Date(2026, 3, 28, 1, 59, 30, 0, time.UTC), 0}, // 01:59 WET
		{time.Date(2026, 3, 28, 2, 0, 30, 0, time.UTC), 1},  // 02:00 WET, the day before the clocks go forward
		{time.Date(2026, 3, 30, 1, 0, 30, 0, time.UTC), 2},  // 02:00 WEST, the day after
		{time.Date(2026, 3, 30, 2, 0, 30, 0, time.UTC), 2},  // 03:00 WEST
	} {
		r.clock.t = tc.now
		r.settle(ctx)
		if n := r.exec.count(); n != tc.runs {
			t.Fatalf("at %v: %d runs, want %d", tc.now, n, tc.runs)
		}
	}
	runs := r.runs(t, "nightly")
	if len(runs) != 2 || !runs[0].Scheduled.Equal(time.Date(2026, 3, 30, 1, 0, 0, 0, time.UTC)) || !runs[1].Scheduled.Equal(time.Date(2026, 3, 28, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("occurrences %+v", runs)
	}
	if runs[0].Scheduled.Location() != time.UTC {
		t.Fatalf("the occurrence is recorded in %v, want UTC", runs[0].Scheduled.Location())
	}
}

// A time the clocks skip does not run that day, and a time that happens twice runs once.
func TestAJobAtASkippedOrRepeatedLocalTimeRunsOnceAtMost(t *testing.T) {
	ctx := context.Background()
	half := tickJob()
	half.Schedule = "30 1 * * *"
	r := newRig(t, map[string]manifest.Job{"half": half})
	r.zone = "Europe/Lisbon"
	// 2026-03-29 01:30 does not exist in Lisbon; 2026-10-25 01:30 happens at 00:30Z and again at 01:30Z.
	for _, now := range []time.Time{time.Date(2026, 3, 29, 0, 30, 30, 0, time.UTC), time.Date(2026, 3, 29, 1, 30, 30, 0, time.UTC)} {
		r.clock.t = now
		r.settle(ctx)
	}
	if n := r.exec.count(); n != 0 {
		t.Fatalf("a time that does not exist ran %d times", n)
	}
	for _, now := range []time.Time{time.Date(2026, 10, 25, 0, 30, 30, 0, time.UTC), time.Date(2026, 10, 25, 1, 30, 30, 0, time.UTC)} {
		r.clock.t = now
		r.settle(ctx)
	}
	if runs := r.runs(t, "half"); len(runs) != 1 || !runs[0].Scheduled.Equal(time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC)) {
		t.Fatalf("a repeated time: %+v", runs)
	}
}

func TestStatusesShowTheNextOccurrenceInTheManifestsZone(t *testing.T) {
	daily := tickJob()
	daily.Schedule = "0 2 * * *"
	m := testManifest(map[string]manifest.Job{"nightly": daily, "manual": {App: "api", Role: "web", Command: []string{"true"}}})
	store, _, _ := openTestStore(t)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	sts, err := Statuses(context.Background(), store, m, "", now)
	if err != nil || len(sts) != 2 {
		t.Fatalf("%+v %v", sts, err)
	}
	if got := sts[1]; got.Timezone != "UTC" || got.Next == nil || !got.Next.Equal(time.Date(2026, 11, 20, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("without a timezone: %+v", got)
	}

	m.Timezone = "Asia/Tokyo"
	sts, err = Statuses(context.Background(), store, m, "", now)
	if err != nil || len(sts) != 2 {
		t.Fatalf("%+v %v", sts, err)
	}
	got := sts[1]
	if want := time.Date(2026, 11, 19, 17, 0, 0, 0, time.UTC); got.Next == nil || !got.Next.Equal(want) { // 02:00 JST on the 20th
		t.Fatalf("Next %v, want %v", got.Next, want)
	}
	if got.Next.Location() != time.UTC {
		t.Fatalf("Next is in %v, want UTC", got.Next.Location())
	}
	if got.Timezone != "Asia/Tokyo" || got.NextLocal == nil || got.NextLocal.Format(time.RFC3339) != "2026-11-20T02:00:00+09:00" {
		t.Fatalf("Timezone %q NextLocal %v", got.Timezone, got.NextLocal)
	}
	if manual := sts[0]; manual.Name != "manual" || manual.Timezone != "" || manual.Next != nil || manual.NextLocal != nil {
		t.Fatalf("an on-demand job shows no zone or next: %+v", manual)
	}
}
