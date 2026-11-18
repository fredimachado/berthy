package manifest

import (
	"fmt"
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
	if name == "Local" {
		return nil, fmt.Errorf("unknown time zone %s", name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	zones.Store(name, loc)
	return loc, nil
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
