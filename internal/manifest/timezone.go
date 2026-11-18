package manifest

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"berthy/internal/schedule"
)

// zones holds the zones already loaded, by name, because the schedulers ask for one every second.
var zones sync.Map

// loadZone returns the zone an IANA name stands for. The empty name is UTC. "Local" is refused: it would be the zone of
// whichever machine reads the manifest.
func loadZone(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	if loc, ok := zones.Load(name); ok {
		return loc.(*time.Location), nil
	}
	if name == "Local" || !canonicalZoneName(name) {
		return nil, fmt.Errorf("unknown time zone %s", name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	zones.Store(name, loc)
	return loc, nil
}

// upperSegments are the segments of zone names that the database writes in capitals: US/Pacific, and the links
// Australia/ACT, Australia/LHI and Australia/NSW.
var upperSegments = map[string]bool{"US": true, "ACT": true, "LHI": true, "NSW": true}

// canonicalZoneName reports whether name is written the way the IANA database writes zone names. Some systems look zone files
// up without regard to case, and LoadLocation takes "europe/lisbon" there while Linux refuses it, so the spelling is checked
// here and a manifest means the same on every system. Each "/"-separated segment starts with an uppercase letter. In a name
// of several segments a segment holds a lowercase letter too, so "EUROPE/LISBON" is refused, except below Etc, whose zones
// are written "Etc/GMT+3", and the segments in upperSegments. A name of one segment ("UTC", "EST5EDT", "Japan") may be all
// uppercase.
func canonicalZoneName(name string) bool {
	segments := strings.Split(name, "/")
	for i, seg := range segments {
		if seg == "" || seg[0] < 'A' || seg[0] > 'Z' {
			return false
		}
		if len(segments) > 1 && !(i > 0 && segments[0] == "Etc") && !upperSegments[seg] && !strings.ContainsFunc(seg, func(r rune) bool { return r >= 'a' && r <= 'z' }) {
			return false
		}
	}
	return true
}

// Location is the zone every schedule of the manifest is read in: Timezone, and UTC when it is empty. A manifest that
// passed Check always has a zone; for one that did not, Location gives UTC.
func (m Manifest) Location() *time.Location {
	loc, err := loadZone(m.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// ParseSchedule parses a cron expression of a job, a backup or the state backup, to be read in the manifest's zone.
func (m Manifest) ParseSchedule(expr string) (schedule.Schedule, error) {
	return schedule.ParseIn(expr, m.Location())
}

// checkTimezone refuses a name that is not an IANA zone the binary knows.
func (m *Manifest) checkTimezone(p *Problems) {
	if _, err := loadZone(m.Timezone); err != nil {
		p.add("timezone", "invalid timezone %q: use an IANA name such as Europe/Lisbon", m.Timezone)
	}
}
