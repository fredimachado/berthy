package deploy

import (
	"context"
	"reflect"
	"testing"
)

// TestComposeLogUnlimited is the rule for a Compose service's logs. Only json-file can be unlimited. Its max-size is the
// service's own, or else the daemon's log-opts max-size when the service's driver is the daemon's default driver, as Docker
// merges them. Every other driver is left alone, as Berthy does not judge it.
func TestComposeLogUnlimited(t *testing.T) {
	cases := []struct {
		name      string
		logging   *composeLogging
		daemon    string
		daemonMax string
		want      bool
	}{
		{"no logging, json-file, no daemon limit", nil, "json-file", "", true},
		{"no logging, json-file, daemon sets max-size", nil, "json-file", "20m", false},
		{"no logging, local daemon", nil, "local", "", false},
		{"no logging, journald daemon", nil, "journald", "", false},
		{"no logging, daemon driver unknown", nil, "", "", false},
		{"empty logging inherits as no logging", &composeLogging{}, "json-file", "", true},
		{"service sets local", &composeLogging{Driver: "local"}, "json-file", "", false},
		{"service sets journald", &composeLogging{Driver: "journald"}, "json-file", "", false},
		{"service sets json-file with max-size", &composeLogging{Driver: "json-file", Options: map[string]string{"max-size": "5m"}}, "json-file", "", false},
		{"service sets max-size and takes the daemon driver", &composeLogging{Options: map[string]string{"max-size": "5m"}}, "json-file", "", false},
		{"service sets json-file with no options, daemon max-size", &composeLogging{Driver: "json-file"}, "json-file", "20m", false},
		{"service sets json-file with no options, no daemon limit", &composeLogging{Driver: "json-file"}, "json-file", "", true},
		{"service sets an empty max-size, daemon max-size", &composeLogging{Driver: "json-file", Options: map[string]string{"max-size": ""}}, "json-file", "20m", false},
		{"service sets no driver and no options, daemon local", &composeLogging{}, "local", "", false},
		// Docker merges the daemon's log-opts when the service's driver is the daemon's default driver, key by key, so
		// max-file alone does not make a json-file service unlimited under a json-file daemon with a max-size.
		{"service sets max-file only, daemon json-file with max-size", &composeLogging{Driver: "json-file", Options: map[string]string{"max-file": "3"}}, "json-file", "20m", false},
		{"service sets no driver, max-file only, daemon json-file with max-size", &composeLogging{Options: map[string]string{"max-file": "3"}}, "json-file", "20m", false},
		// A local daemon with a max-size does not reach a json-file service: the drivers differ, so nothing is merged.
		{"service sets max-file only, daemon local with max-size", &composeLogging{Driver: "json-file", Options: map[string]string{"max-file": "3"}}, "local", "20m", true},
		{"service sets json-file with no options, daemon local with max-size", &composeLogging{Driver: "json-file"}, "local", "20m", true},
		// With the daemon's driver unknown, nothing is merged.
		{"service sets json-file with no options, daemon driver unknown, daemon max-size", &composeLogging{Driver: "json-file"}, "", "20m", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := composeLogUnlimited(tc.logging, tc.daemon, tc.daemonMax); got != tc.want {
				t.Fatalf("unlimited %v, want %v", got, tc.want)
			}
		})
	}
}

// logLimitConfig is `compose config --format json` of a project with one service that sets no logging and one that caps
// its logs at 5 MB.
const logLimitConfig = `{"services":{"bare":{"image":"alpine:3.20"},"capped":{"image":"alpine:3.20","logging":{"driver":"json-file","options":{"max-size":"5m"}}}}}`

func TestComposeServicesWithoutLogLimitNamesOnlyUnlimitedServices(t *testing.T) {
	runner := &fakeCompose{config: logLimitConfig}
	c := Controller{Compose: runner, StateDir: t.TempDir()}
	got, err := c.ComposeServicesWithoutLogLimit(context.Background(), "shop", "json-file", "")
	if err != nil || !reflect.DeepEqual(got, []string{"bare"}) {
		t.Fatalf("services %v: %v", got, err)
	}
	// The read changes nothing: one config call, no up or other run.
	if len(runner.runs) != 1 || runner.runs[0].Args[0] != "config" {
		t.Fatalf("runs %+v", runner.runs)
	}
	// With the daemon's max-size, the service that sets no options is limited too.
	got, err = c.ComposeServicesWithoutLogLimit(context.Background(), "shop", "json-file", "20m")
	if err != nil || len(got) != 0 {
		t.Fatalf("services with a daemon limit %v: %v", got, err)
	}
}

func TestComposeServicesWithoutLogLimitRefusesWhatItCannotRead(t *testing.T) {
	c := Controller{Compose: &fakeCompose{config: "not json"}, StateDir: t.TempDir()}
	if _, err := c.ComposeServicesWithoutLogLimit(context.Background(), "shop", "json-file", ""); err == nil {
		t.Fatal("an unreadable configuration was accepted")
	}
	none := Controller{Compose: &fakeCompose{config: logLimitConfig}}
	if _, err := none.ComposeServicesWithoutLogLimit(context.Background(), "shop", "json-file", ""); err == nil {
		t.Fatal("a controller without a state directory was accepted")
	}
}
