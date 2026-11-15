// Package schedule parses five-field cron expressions and finds their occurrences in UTC.
package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed five-field cron expression evaluated in UTC: minute hour day-of-month month day-of-week.
type Schedule struct {
	minute, hour, dom, month, dow uint64 // bit i set => value i allowed (dow: 0-6, Sunday = 0)
	domAny, dowAny                bool   // the field starts with "*", which Vixie cron treats as unrestricted
	expr                          string
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

// dayMatches applies the Vixie cron rule to a UTC day: when both day fields are restricted a day matches if either
// does; otherwise both must match.
func (s Schedule) dayMatches(day time.Time) bool {
	dom := has(s.dom, day.Day())
	dow := has(s.dow, int(day.Weekday()))
	if !s.domAny && !s.dowAny {
		return dom || dow
	}
	return dom && dow
}

// Matches reports whether the minute containing t (UTC) is an occurrence.
func (s Schedule) Matches(t time.Time) bool {
	u := t.UTC()
	return has(s.minute, u.Minute()) && has(s.hour, u.Hour()) && has(s.month, int(u.Month())) && s.dayMatches(u)
}

// Next returns the first occurrence strictly after t, truncated to the minute, in UTC. It returns the zero time when
// there is none within 9 years, which covers a Feb 29 after a non-leap century year. It skips whole months, days and
// hours that cannot match, so it does not step by minute.
func (s Schedule) Next(t time.Time) time.Time {
	u := t.UTC()
	cur := time.Date(u.Year(), u.Month(), u.Day(), u.Hour(), u.Minute(), 0, 0, time.UTC).Add(time.Minute)
	limit := cur.AddDate(9, 0, 0)
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
