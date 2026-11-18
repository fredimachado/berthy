// Package schedule parses five-field cron expressions and finds their occurrences, in UTC or in the zone a schedule is given.
package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Schedule is a parsed five-field cron expression: minute hour day-of-month month day-of-week. It reads the wall clock of
// its zone, UTC unless In sets another one.
type Schedule struct {
	minute, hour, dom, month, dow uint64 // bit i set => value i allowed (dow: 0-6, Sunday = 0)
	domAny, dowAny                bool   // the field starts with "*", which Vixie cron treats as unrestricted
	expr                          string
	loc                           *time.Location // nil means UTC
}

// fieldSpec describes one of the five fields: its name for errors, its range, and its names if it has any.
type fieldSpec struct {
	name     string
	min, max int
	names    map[string]int
}

var (
	minuteSpec = fieldSpec{name: "minute", min: 0, max: 59}
	hourSpec   = fieldSpec{name: "hour", min: 0, max: 23}
	domSpec    = fieldSpec{name: "day-of-month", min: 1, max: 31}
	monthSpec  = fieldSpec{name: "month", min: 1, max: 12, names: map[string]int{
		"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
		"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
	}}
	// dowSpec accepts 7 as well as 0 for Sunday; Parse folds 7 into 0.
	dowSpec = fieldSpec{name: "day-of-week", min: 0, max: 7, names: map[string]int{
		"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6,
	}}
)

// macros are the @-names, each written as the five-field expression it stands for.
var macros = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

// Parse accepts "*", numbers, ranges "a-b", steps "*/n", "a/n" (a through the field's maximum) and "a-b/n", lists "a,b-c,*/n", month names JAN-DEC and day
// names SUN-SAT (case-insensitive), 7 as Sunday, and the macros @yearly/@annually, @monthly, @weekly, @daily/@midnight
// and @hourly. Any other input must have exactly five fields. Errors name the field and never panic.
func Parse(expr string) (Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) == 1 && strings.HasPrefix(fields[0], "@") {
		macro, ok := macros[strings.ToLower(fields[0])]
		if !ok {
			return Schedule{}, fmt.Errorf("unknown macro %q", fields[0])
		}
		fields = strings.Fields(macro)
	}
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("expected five fields, found %d", len(fields))
	}
	var s Schedule
	var err error
	if s.minute, err = parseField(minuteSpec, fields[0]); err != nil {
		return Schedule{}, err
	}
	if s.hour, err = parseField(hourSpec, fields[1]); err != nil {
		return Schedule{}, err
	}
	if s.dom, err = parseField(domSpec, fields[2]); err != nil {
		return Schedule{}, err
	}
	if s.month, err = parseField(monthSpec, fields[3]); err != nil {
		return Schedule{}, err
	}
	if s.dow, err = parseField(dowSpec, fields[4]); err != nil {
		return Schedule{}, err
	}
	if s.dow&(1<<7) != 0 {
		s.dow = s.dow&^(1<<7) | 1
	}
	// As in Vixie cron, a day field that starts with "*" is unrestricted, so "*/2" makes the day rule an AND.
	s.domAny = strings.HasPrefix(fields[2], "*")
	s.dowAny = strings.HasPrefix(fields[4], "*")
	s.expr = expr
	return s, nil
}

// parseField returns the set of values a comma-separated field allows.
func parseField(spec fieldSpec, text string) (uint64, error) {
	var bits uint64
	for _, item := range strings.Split(text, ",") {
		lo, hi, step, err := parseItem(spec, item)
		if err != nil {
			return 0, fmt.Errorf("invalid %s field: %w", spec.name, err)
		}
		for v := lo; v <= hi; v += step {
			bits |= uint64(1) << uint(v)
		}
	}
	return bits, nil
}

// parseItem reads one list item: "*", "n", "a-b", "*/step", "n/step" or "a-b/step".
func parseItem(spec fieldSpec, item string) (lo, hi, step int, err error) {
	rangeText, stepText, hasStep := strings.Cut(item, "/")
	step = 1
	if hasStep {
		n, ok := number(stepText)
		if !ok || n < 1 {
			return 0, 0, 0, fmt.Errorf("step %q must be a positive whole number", stepText)
		}
		step = n
	}
	switch {
	case rangeText == "*":
		return spec.min, spec.max, step, nil
	case strings.Contains(rangeText, "-"):
		a, b, _ := strings.Cut(rangeText, "-")
		if lo, err = parseValue(spec, a); err != nil {
			return 0, 0, 0, err
		}
		if hi, err = parseValue(spec, b); err != nil {
			return 0, 0, 0, err
		}
		if lo > hi {
			return 0, 0, 0, fmt.Errorf("range %q runs backwards", rangeText)
		}
		return lo, hi, step, nil
	case hasStep:
		// "n/step" runs from n through the field's maximum.
		if lo, err = parseValue(spec, rangeText); err != nil {
			return 0, 0, 0, err
		}
		return lo, spec.max, step, nil
	}
	v, err := parseValue(spec, rangeText)
	return v, v, 1, err
}

// parseValue reads one number or, for fields with names, one name.
func parseValue(spec fieldSpec, text string) (int, error) {
	if text == "" {
		return 0, fmt.Errorf("empty value")
	}
	if spec.names != nil && !isDigit(text[0]) {
		v, ok := spec.names[strings.ToUpper(text)]
		if !ok {
			return 0, fmt.Errorf("%q is not a name in this field", text)
		}
		return v, nil
	}
	n, ok := number(text)
	if !ok {
		return 0, fmt.Errorf("%q is not a number", text)
	}
	if n < spec.min || n > spec.max {
		return 0, fmt.Errorf("%q is out of range %d-%d", text, spec.min, spec.max)
	}
	return n, nil
}

// number parses one to four decimal digits. It rejects signs, spaces and anything longer, so no input can overflow.
func number(s string) (int, bool) {
	if s == "" || len(s) > 4 {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// has reports whether value v is in the set.
func has(set uint64, v int) bool { return set&(uint64(1)<<uint(v)) != 0 }

// ParseIn is Parse for a schedule that reads the wall clock of loc.
func ParseIn(expr string, loc *time.Location) (Schedule, error) {
	s, err := Parse(expr)
	return s.In(loc), err
}

// In returns the schedule reading the wall clock of loc. A nil loc is UTC.
func (s Schedule) In(loc *time.Location) Schedule {
	s.loc = loc
	return s
}

func (s Schedule) zone() *time.Location {
	if s.loc == nil {
		return time.UTC
	}
	return s.loc
}

// dayMatches applies the Vixie cron rule to a wall-clock day: when both day fields are restricted a day matches if either
// does; otherwise both must match.
func (s Schedule) dayMatches(day time.Time) bool {
	dom := has(s.dom, day.Day())
	dow := has(s.dow, int(day.Weekday()))
	if !s.domAny && !s.dowAny {
		return dom || dow
	}
	return dom && dow
}

// wallMatches reports whether a wall-clock minute, written as a UTC time, is one the fields allow.
func (s Schedule) wallMatches(w time.Time) bool {
	return has(s.minute, w.Minute()) && has(s.hour, w.Hour()) && has(s.month, int(w.Month())) && s.dayMatches(w)
}

// wall returns the wall-clock minute of t in the schedule's zone, written as a UTC time so it can be stepped by days.
func (s Schedule) wall(t time.Time) time.Time {
	l := t.In(s.zone())
	return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), 0, 0, time.UTC)
}

// firstInstant returns the first instant at which the clock of the zone reads w, and the zero time when it never does
// because the clocks jump over it. A reading that happens twice because the clocks go back is returned at its first
// instant. The offsets on either side of w are the only ones that can apply, a day on each side being past any change.
func (s Schedule) firstInstant(w time.Time) time.Time {
	loc := s.zone()
	var first time.Time
	for _, probe := range []time.Time{w.Add(-24 * time.Hour), w.Add(24 * time.Hour)} {
		_, offset := probe.In(loc).Zone()
		u := w.Add(-time.Duration(offset) * time.Second)
		if s.wall(u).Equal(w) && (first.IsZero() || u.Before(first)) {
			first = u
		}
	}
	return first
}

// Matches reports whether the minute containing t is an occurrence: its wall clock in the schedule's zone fits the fields,
// and it is the first time the clock read that. A reading the clocks skip never occurs; one they repeat occurs at its first
// instant only.
func (s Schedule) Matches(t time.Time) bool {
	w := s.wall(t)
	if !s.wallMatches(w) {
		return false
	}
	u := s.firstInstant(w)
	return !u.IsZero() && !t.Before(u) && t.Before(u.Add(time.Minute))
}

// Next returns the first occurrence strictly after t, truncated to the minute, in UTC. It returns the zero time when
// there is none within 9 years, which covers a Feb 29 after a non-leap century year. It skips whole months, days and
// hours that cannot match on the wall clock, so it does not step by minute.
func (s Schedule) Next(t time.Time) time.Time {
	// Every occurrence after t reads a later wall clock than t does: the clocks only go back over readings that already
	// happened, which are not occurrences the second time.
	w := s.wall(t).Add(time.Minute)
	limit := w.AddDate(9, 0, 0)
	for {
		if w = s.nextWall(w, limit); w.IsZero() {
			return time.Time{}
		}
		if u := s.firstInstant(w); u.After(t) {
			return u
		}
		w = w.Add(time.Minute)
	}
}

// nextWall returns the first wall-clock minute at or after w that fits the fields, before limit; the zero time if none.
func (s Schedule) nextWall(w, limit time.Time) time.Time {
	cur := w
	for cur.Before(limit) {
		switch {
		case !has(s.month, int(cur.Month())):
			cur = time.Date(cur.Year(), cur.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		case !s.dayMatches(cur):
			cur = time.Date(cur.Year(), cur.Month(), cur.Day()+1, 0, 0, 0, 0, time.UTC)
		case !has(s.hour, cur.Hour()):
			cur = time.Date(cur.Year(), cur.Month(), cur.Day(), cur.Hour()+1, 0, 0, 0, time.UTC)
		case !has(s.minute, cur.Minute()):
			cur = cur.Add(time.Minute)
		default:
			return cur
		}
	}
	return time.Time{}
}

// String returns the expression as it was parsed.
func (s Schedule) String() string { return s.expr }

// Minutes hands out each UTC minute once, for a scheduler that looks at the clock every second. Only the current minute counts:
// a sleep or a clock jump makes up nothing. The zero value is ready to use.
type Minutes struct {
	mu   sync.Mutex
	last time.Time
}

// Claim returns the UTC minute that now falls in, and true, when that minute has not been handed out and is later than the
// last one that was.
func (m *Minutes) Claim(now time.Time) (time.Time, bool) {
	cur := now.UTC().Truncate(time.Minute)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !cur.After(m.last) {
		return time.Time{}, false
	}
	m.last = cur
	return cur, true
}
