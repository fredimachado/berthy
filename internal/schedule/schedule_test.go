package schedule

import (
	"strings"
	"testing"
	"time"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad test time %q: %v", s, err)
	}
	return ts
}

func mustParse(t *testing.T, expr string) Schedule {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	return s
}

func TestParseAcceptsAndRejects(t *testing.T) {
	for _, expr := range []string{"* * * * *", "*/15 9-17 * * MON-FRI", "0 0 1 JAN,jul *", "30 2 * * 7", "@daily", "@hourly", "5/15 * * * *", "0 2/6 * * *", "0 0 */2 * MON"} {
		if _, err := Parse(expr); err != nil {
			t.Errorf("Parse(%q) refused: %v", expr, err)
		}
	}
	// Each rejected expression must name the problem in its error.
	for expr, want := range map[string]string{
		"60 * * * *":   "invalid minute field",
		"* * * *":      "expected five fields",
		"*/0 * * * *":  "invalid minute field",
		"5-1 * * * *":  "invalid minute field",
		"* * * * 8":    "invalid day-of-week field",
		"@reboot":      `unknown macro "@reboot"`,
		"":             "expected five fields",
		"a b c d e":    "invalid minute field",
		"* * 0 * *":    "invalid day-of-month field",
		"* * * 13 *":   "invalid month field",
		"* * * * MON-": "invalid day-of-week field",
	} {
		_, err := Parse(expr)
		if err == nil {
			t.Errorf("Parse(%q) accepted", expr)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) error %q does not contain %q", expr, err, want)
		}
	}
}

func TestParseRejectsMalformedInputWithoutPanicking(t *testing.T) {
	for _, expr := range []string{
		"1-", "-", "*/", "/5 * * * *", "1,,2 * * * *", ",1 * * * *", "*-5 * * * *", "1-2-3 * * * *",
		"99999999999999999999 * * * *", "+5 * * * *", "-1 * * * *", "*/99999999999999999999 * * * *",
		"MON * * * *", "* * * * MONDAY", "@", "@daily *", "* * * * * *", "\t\n", "*/-1 * * * *",
	} {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q) accepted", expr)
		}
	}
}

func TestParseAcceptsNamesCaseInsensitively(t *testing.T) {
	lower := mustParse(t, "0 0 1 jan,JUL *")
	upper := mustParse(t, "0 0 1 JAN,JUL *")
	if lower.month != upper.month || lower.month != (1<<1|1<<7) {
		t.Fatalf("month bits %b and %b", lower.month, upper.month)
	}
}

func TestStringIsTheOriginalExpression(t *testing.T) {
	for _, expr := range []string{"*/15 9-17 * * MON-FRI", "@daily"} {
		if got := mustParse(t, expr).String(); got != expr {
			t.Errorf("String() = %q, want %q", got, expr)
		}
	}
}

func TestMatches(t *testing.T) {
	for _, tc := range []struct {
		expr string
		at   string
		want bool
	}{
		{"30 2 * * 7", "2026-11-15T02:30:00Z", true},             // 7 is Sunday; 2026-11-15 is a Sunday
		{"30 2 * * 7", "2026-11-16T02:30:00Z", false},            // Monday
		{"*/15 9-17 * * MON-FRI", "2026-11-16T09:15:00Z", true},  // Monday
		{"*/15 9-17 * * MON-FRI", "2026-11-21T09:15:00Z", false}, // Saturday
		{"0 0 1 JAN,jul *", "2027-07-01T00:00:00Z", true},        // named month list
		{"0 0 13 * FRI", "2026-11-20T00:00:00Z", true},           // Friday, not the 13th
		{"0 0 13 * FRI", "2026-12-13T00:00:00Z", true},           // the 13th, a Sunday
		{"0 0 13 * FRI", "2026-12-12T00:00:00Z", false},          // neither
		{"0 0 13 * *", "2026-12-13T00:00:00Z", true},             // day-of-week is "*": both must match
		{"0 0 13 * *", "2026-11-20T00:00:00Z", false},
		{"5/15 * * * *", "2026-11-16T10:05:00Z", true},
		{"5/15 * * * *", "2026-11-16T10:20:00Z", true},
		{"5/15 * * * *", "2026-11-16T10:25:00Z", false},
		{"0 2/6 * * *", "2026-11-16T14:00:00Z", true},
		{"0 2/6 * * *", "2026-11-16T09:00:00Z", false},
		{"0 0 */2 * MON", "2026-11-23T00:00:00Z", true},  // odd day of the month and a Monday
		{"0 0 */2 * MON", "2026-11-17T00:00:00Z", false}, // odd day but a Tuesday: both must match
		{"0 0 */2 * MON", "2026-11-16T00:00:00Z", false}, // a Monday but an even day
		{"0 0 * * 5-7", "2026-11-15T00:00:00Z", true},    // 7 folds into Sunday inside a range
		{"0 0 * * 5-7", "2026-11-16T00:00:00Z", false},
		{"0 0 * * MON", "2026-11-16T00:00:00Z", true}, // day-of-month is "*"
		{"0 0 * * MON", "2026-11-17T00:00:00Z", false},
		{"0 0 * * *", "2026-11-16T00:00:30Z", true},        // the minute containing t counts
		{"30 2 * * *", "2026-11-16T04:30:00+02:00", true},  // matched in UTC: 02:30Z
		{"30 2 * * *", "2026-11-16T02:30:00+02:00", false}, // that is 00:30Z
	} {
		if got := mustParse(t, tc.expr).Matches(at(t, tc.at)); got != tc.want {
			t.Errorf("%q Matches(%s) = %v, want %v", tc.expr, tc.at, got, tc.want)
		}
	}
}

func TestNext(t *testing.T) {
	for _, tc := range []struct {
		name string
		expr string
		from string
		want string // empty means no occurrence
	}{
		{"every 15 minutes", "*/15 * * * *", "2026-11-16T10:07:30Z", "2026-11-16T10:15:00Z"},
		{"daily at 02:30", "30 2 * * *", "2026-11-16T10:07:30Z", "2026-11-17T02:30:00Z"},
		{"minute N/step runs N, N+step, ... to 59", "5/15 * * * *", "2026-11-16T10:07:30Z", "2026-11-16T10:20:00Z"},
		{"hour N/step runs N, N+step, ... to 23", "0 2/6 * * *", "2026-11-16T10:00:00Z", "2026-11-16T14:00:00Z"},
		{"star-prefixed day field is unrestricted, so both fields must match", "0 0 */2 * MON", "2026-11-16T00:00:00Z", "2026-11-23T00:00:00Z"},
		{"every tenth day", "0 0 */10 * *", "2026-11-16T10:07:30Z", "2026-11-21T00:00:00Z"},
		{"leap day", "0 0 29 2 *", "2026-11-16T10:07:30Z", "2028-02-29T00:00:00Z"},
		{"leap day past a non-leap century is found within 9 years", "0 0 29 2 *", "2097-03-01T00:00:00Z", "2104-02-29T00:00:00Z"},
		{"no February 30", "0 0 30 2 *", "2026-11-16T10:07:30Z", ""},
		{"month rollover", "0 0 1 * *", "2026-11-16T10:07:30Z", "2026-12-01T00:00:00Z"},
		{"year rollover", "0 0 1 1 *", "2026-11-16T10:07:30Z", "2027-01-01T00:00:00Z"},
		{"13th or Friday, Friday first", "0 0 13 * FRI", "2026-11-16T00:00:00Z", "2026-11-20T00:00:00Z"},
		{"13th or Friday, then the 13th", "0 0 13 * FRI", "2026-12-11T00:00:00Z", "2026-12-13T00:00:00Z"},
		{"strictly after an occurrence", "*/15 * * * *", "2026-11-16T10:15:00Z", "2026-11-16T10:30:00Z"},
		{"seconds are dropped", "*/15 * * * *", "2026-11-16T10:15:30Z", "2026-11-16T10:30:00Z"},
		{"time in another zone", "30 2 * * *", "2026-11-16T12:00:00+02:00", "2026-11-17T02:30:00Z"},
		{"zone converted before stepping", "*/15 * * * *", "2026-11-16T05:30:00-05:00", "2026-11-16T10:45:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mustParse(t, tc.expr).Next(at(t, tc.from))
			if tc.want == "" {
				if !got.IsZero() {
					t.Fatalf("Next = %v, want the zero time", got)
				}
				return
			}
			if want := at(t, tc.want); !got.Equal(want) {
				t.Fatalf("Next = %v, want %v", got, want)
			}
			if got.Location() != time.UTC {
				t.Fatalf("Next location %v, want UTC", got.Location())
			}
		})
	}
}

// Matches and Next must agree: a minute matches exactly when the first occurrence after the minute before it is that minute.
func TestMatchesAgreesWithNext(t *testing.T) {
	start := at(t, "2026-12-07T00:00:00Z")
	for _, expr := range []string{"*/15 9-17 * * MON-FRI", "0 0 13 * FRI", "30 2 * * 7", "0 */6 1,13 * *", "@weekly"} {
		s := mustParse(t, expr)
		matched := 0
		for m := start; m.Before(start.Add(7 * 24 * time.Hour)); m = m.Add(time.Minute) {
			want := s.Matches(m)
			if got := s.Next(m.Add(-time.Minute)).Equal(m); got != want {
				t.Fatalf("%q at %v: Matches=%v but Next agrees=%v", expr, m, want, got)
			}
			if want {
				matched++
			}
		}
		if matched == 0 {
			t.Errorf("%q matched nothing in the sample week, so the check proved nothing", expr)
		}
	}
}
