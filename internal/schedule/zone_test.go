package schedule

import (
	"testing"
	"time"
)

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// "0 2 * * *" in Europe/Lisbon is 02:00 local time on both sides of a clock change, so its UTC instant moves by an hour.
func TestScheduleInAZoneFollowsTheLocalClockAcrossAClockChange(t *testing.T) {
	s := mustParse(t, "0 2 * * *").In(zone(t, "Europe/Lisbon"))
	for _, tc := range []struct{ from, want string }{
		{"2026-03-27T12:00:00Z", "2026-03-28T02:00:00Z"}, // WET, UTC+0
		{"2026-03-28T12:00:00Z", "2026-03-29T01:00:00Z"}, // the day clocks go forward: 02:00 is WEST, UTC+1
		{"2026-03-29T12:00:00Z", "2026-03-30T01:00:00Z"},
		{"2026-10-24T12:00:00Z", "2026-10-25T02:00:00Z"}, // the day clocks go back: 02:00 is WET again, UTC+0
		{"2026-10-25T12:00:00Z", "2026-10-26T02:00:00Z"},
	} {
		got := s.Next(at(t, tc.from))
		if want := at(t, tc.want); !got.Equal(want) {
			t.Errorf("Next(%s) = %v, want %v", tc.from, got, want)
		}
		if got.Location() != time.UTC {
			t.Errorf("Next(%s) location %v, want UTC", tc.from, got.Location())
		}
		if !s.Matches(got) || s.Matches(got.Add(-time.Hour)) {
			t.Errorf("Matches disagrees with Next at %v", got)
		}
	}
}

// A wall-clock time that does not exist on a day (clocks go forward) is skipped that day.
func TestScheduleSkipsATimeThatDoesNotExist(t *testing.T) {
	// Lisbon, 2026-03-29: 01:00 WET is followed by 02:00 WEST, so 01:30 does not exist.
	s := mustParse(t, "30 1 * * *").In(zone(t, "Europe/Lisbon"))
	if got, want := s.Next(at(t, "2026-03-28T12:00:00Z")), at(t, "2026-03-30T00:30:00Z"); !got.Equal(want) {
		t.Errorf("Next = %v, want %v (the 29th has no 01:30)", got, want)
	}
	for m := at(t, "2026-03-29T00:00:00Z"); m.Before(at(t, "2026-03-29T23:59:00Z")); m = m.Add(time.Minute) {
		if s.Matches(m) {
			t.Fatalf("matched %v on the day 01:30 does not exist", m)
		}
	}
	// Lord Howe moves its clocks forward by 30 minutes: on 2026-10-04 02:00 becomes 02:30, so 02:15 does not exist.
	lh := mustParse(t, "15 2 * * *").In(zone(t, "Australia/Lord_Howe"))
	if got, want := lh.Next(at(t, "2026-10-03T00:00:00Z")), at(t, "2026-10-04T15:15:00Z"); !got.Equal(want) { // 2026-10-05 02:15 at +11
		t.Errorf("Lord Howe Next = %v, want %v", got, want)
	}
}

// A wall-clock time that happens twice (clocks go back) runs once, at the first of its two instants.
func TestScheduleRunsARepeatedTimeOnce(t *testing.T) {
	// Lisbon, 2026-10-25: 02:00 WEST (01:00Z) is followed by 01:00 WET, so 01:30 happens at 00:30Z and again at 01:30Z.
	s := mustParse(t, "30 1 * * *").In(zone(t, "Europe/Lisbon"))
	first, second := at(t, "2026-10-25T00:30:00Z"), at(t, "2026-10-25T01:30:00Z")
	if !s.Matches(first) || s.Matches(second) {
		t.Errorf("Matches first=%v second=%v, want true and false", s.Matches(first), s.Matches(second))
	}
	if got := s.Next(at(t, "2026-10-24T12:00:00Z")); !got.Equal(first) {
		t.Errorf("Next = %v, want %v", got, first)
	}
	if got, want := s.Next(first), at(t, "2026-10-26T01:30:00Z"); !got.Equal(want) {
		t.Errorf("Next after the first = %v, want %v", got, want)
	}
	// Lord Howe moves its clocks back by 30 minutes: 2026-04-05 02:00 LHDT (+11) becomes 01:30 LHST (+10:30).
	lh := mustParse(t, "45 1 * * *").In(zone(t, "Australia/Lord_Howe"))
	f, sec := at(t, "2026-04-04T14:45:00Z"), at(t, "2026-04-04T15:15:00Z")
	if !lh.Matches(f) || lh.Matches(sec) {
		t.Errorf("Lord Howe Matches first=%v second=%v, want true and false", lh.Matches(f), lh.Matches(sec))
	}
	if got, want := lh.Next(f), at(t, "2026-04-05T15:15:00Z"); !got.Equal(want) {
		t.Errorf("Lord Howe Next after the first = %v, want %v", got, want)
	}
}

// Over the days a clock changes, Matches and Next agree minute by minute, and a repeated minute counts once.
func TestMatchesAgreesWithNextAcrossClockChanges(t *testing.T) {
	for _, tc := range []struct {
		zone, from string
		repeated   int // minutes whose local reading already happened once
	}{
		{"Europe/Lisbon", "2026-03-28T00:00:00Z", 0},
		{"Europe/Lisbon", "2026-10-24T00:00:00Z", 60},
		{"Australia/Lord_Howe", "2026-04-03T12:00:00Z", 30},
		{"Australia/Lord_Howe", "2026-10-03T00:00:00Z", 0},
	} {
		loc := zone(t, tc.zone)
		for _, expr := range []string{"* * * * *", "*/20 * * * *", "30 1 * * *", "15 2 * * *", "0 * * * *"} {
			s := mustParse(t, expr).In(loc)
			start := at(t, tc.from)
			matched := 0
			for m := start; m.Before(start.Add(72 * time.Hour)); m = m.Add(time.Minute) {
				want := s.Matches(m)
				if got := s.Next(m.Add(-time.Minute)).Equal(m); got != want {
					t.Fatalf("%s %q at %v: Matches=%v but Next agrees=%v", tc.zone, expr, m, want, got)
				}
				if want {
					matched++
				}
			}
			if want := 72*60 - tc.repeated; expr == "* * * * *" && matched != want {
				t.Errorf("%s from %s: an every-minute schedule matched %d minutes, want %d", tc.zone, tc.from, matched, want)
			}
		}
	}
}

// Without a zone, or with UTC, a schedule reads the UTC clock as before.
func TestScheduleWithoutAZoneIsUTC(t *testing.T) {
	plain, utc := mustParse(t, "0 2 * * *"), mustParse(t, "0 2 * * *").In(time.UTC)
	from := at(t, "2026-10-24T12:00:00Z")
	if a, b := plain.Next(from), utc.Next(from); !a.Equal(b) || !a.Equal(at(t, "2026-10-25T02:00:00Z")) {
		t.Errorf("plain %v, utc %v", a, b)
	}
}
