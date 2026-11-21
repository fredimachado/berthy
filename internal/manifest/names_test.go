package manifest

import (
	"strings"
	"testing"
)

func TestServiceNameCarriesProjectEnvironmentAppAndRole(t *testing.T) {
	m := Manifest{Project: "shop"}
	if got := m.ServiceName("prod", "api", "web"); got != "shop-prod-api-web" {
		t.Fatalf("service name %q", got)
	}
}

func TestVolumeNameCarriesProjectAndEnvironment(t *testing.T) {
	m := Manifest{Project: "shop"}
	if got := m.VolumeName("prod", "data"); got != "shop-prod_data" {
		t.Fatalf("volume name %q", got)
	}
}

func TestServiceNameErrorNamesANameOver63Characters(t *testing.T) {
	m := Manifest{Project: "shop"}
	app := strings.Repeat("a", 63-len("shop-prod--web"))
	if got := m.ServiceNameError("prod", app, "web"); got != "" {
		t.Fatalf("63 characters: %q", got)
	}
	app += "a"
	name := m.ServiceName("prod", app, "web")
	want := "name " + name + " is longer than 63 characters; shorten the project, environment, app or role"
	if got := m.ServiceNameError("prod", app, "web"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
