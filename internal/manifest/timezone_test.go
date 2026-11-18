package manifest

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// timezoneYAML is a manifest with one image app, with a timezone line for zone. An empty zone writes no line. The
// timezone line is the third line of the file.
func timezoneYAML(zone string) string {
	line := ""
	if zone != "" {
		line = "timezone: " + zone + "\n"
	}
	return "project: demo\nnetwork: demo\n" + line + "apps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n"
}

func TestTimezoneIsReadAndGivesTheLocationOfEverySchedule(t *testing.T) {
	m, err := loadYAML(t, timezoneYAML("Europe/Lisbon"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Timezone != "Europe/Lisbon" || m.Location().String() != "Europe/Lisbon" {
		t.Fatalf("Timezone %q, Location %v", m.Timezone, m.Location())
	}
	// 02:00 on 2026-03-30 in Lisbon is 01:00 UTC, because the clocks went forward the day before.
	s, err := m.ParseSchedule("0 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC)
	if got, want := s.Next(from), time.Date(2026, 3, 30, 1, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}
}

func TestTimezoneDefaultsToUTC(t *testing.T) {
	m, err := loadYAML(t, timezoneYAML(""))
	if err != nil {
		t.Fatal(err)
	}
	if m.Location() != time.UTC {
		t.Fatalf("Location %v, want UTC", m.Location())
	}
	s, err := m.ParseSchedule("0 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC)
	if got, want := s.Next(from), time.Date(2026, 3, 30, 2, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "timezone") {
		t.Fatalf("a manifest without timezone marshals with the key: %s", b)
	}
}

func TestAnUnknownTimezoneIsRefusedWithFileAndLine(t *testing.T) {
	for _, zone := range []string{"Mars/Base", "Lisbon", "europe/lisbon", "Local", "../etc/passwd", `"/etc/localtime"`} {
		_, err := loadYAML(t, timezoneYAML(zone))
		var ps Problems
		if !errors.As(err, &ps) || len(ps) != 1 {
			t.Errorf("timezone %s: error %v", zone, err)
			continue
		}
		if got := ps[0].String(); !strings.HasPrefix(got, "berthy.yml:3:") || !strings.Contains(got, "invalid timezone") || !strings.Contains(got, "IANA") {
			t.Errorf("timezone %s: problem %q", zone, got)
		}
	}
	for _, zone := range []string{"UTC", "Europe/Lisbon", "Asia/Tokyo", "America/Argentina/Buenos_Aires", "Australia/Lord_Howe"} {
		if _, err := loadYAML(t, timezoneYAML(zone)); err != nil {
			t.Errorf("timezone %s: %v", zone, err)
		}
	}
}

func TestSettingsChangesNamesTheTimezone(t *testing.T) {
	prev := decodeScoped(t, scopedManifest)
	next := decodeScoped(t, scopedManifest)
	next.Timezone = "Europe/Lisbon"
	if got, want := SettingsChanges(prev, next), []string{"timezone: UTC -> Europe/Lisbon"}; !slices.Equal(got, want) {
		t.Fatalf("changes %v, want %v", got, want)
	}
	if got, want := SettingsChanges(next, prev), []string{"timezone: Europe/Lisbon -> UTC"}; !slices.Equal(got, want) {
		t.Fatalf("reverse changes %v, want %v", got, want)
	}
	prev.Timezone = "UTC"
	next.Timezone = ""
	if got := SettingsChanges(prev, next); len(got) != 0 {
		t.Fatalf("UTC written out and left out reported as %v", got)
	}
}

// The timezone belongs to the environment: an app's scope does not carry it and the environment's does.
func TestTimezoneIsInTheEnvironmentScope(t *testing.T) {
	m := decodeScoped(t, scopedManifest)
	m.Timezone = "Asia/Tokyo"
	if got := m.EnvScope().Timezone; got != "Asia/Tokyo" {
		t.Errorf("EnvScope timezone %q", got)
	}
	if got := m.AppScope("api").Timezone; got != "" {
		t.Errorf("AppScope timezone %q", got)
	}
}

// Some systems look zone files up without regard to case, so LoadLocation accepts "europe/lisbon" there and refuses it on
// Linux. The spelling is checked on its own, so every system gives the same answer.
func TestTimezoneMustBeWrittenInTheCanonicalIANAForm(t *testing.T) {
	for _, name := range []string{"UTC", "GMT", "CET", "EST5EDT", "Japan", "Etc/UTC", "Etc/GMT+3", "Etc/GMT-14", "Europe/Lisbon", "US/Pacific", "Australia/NSW",
		"America/Argentina/Buenos_Aires", "America/Port-au-Prince", "Asia/Ho_Chi_Minh", "America/Indiana/Indianapolis", "Australia/Lord_Howe"} {
		if !canonicalZoneName(name) {
			t.Errorf("%q is refused", name)
		}
	}
	for _, name := range []string{"", "utc", "europe/lisbon", "Europe/lisbon", "europe/Lisbon", "EUROPE/LISBON", "Europe/LISBON", "asia/tokyo",
		"America/argentina/Buenos_Aires", "/Europe/Lisbon", "Europe//Lisbon", "Europe/", "etc/GMT+3", "Etc/gmt+3"} {
		if canonicalZoneName(name) {
			t.Errorf("%q is accepted", name)
		}
	}
	for _, name := range []string{"europe/lisbon", "EUROPE/LISBON", "Europe/lisbon"} {
		if _, err := loadZone(name); err == nil {
			t.Errorf("loadZone accepts %q", name)
		}
	}
}
