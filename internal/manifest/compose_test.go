package manifest

import (
	"reflect"
	"strings"
	"testing"
)

func TestComposeProjectNameDefaultsAndOverride(t *testing.T) {
	m := Manifest{Project: "shop", Apps: map[string]App{
		"db":    {Kind: "compose", File: "compose.yaml"},
		"cache": {Kind: "compose", File: "cache.yaml", ProjectName: "legacy-cache"},
	}}
	if got := m.ComposeProject("prod", "db"); got != "shop-prod-db" {
		t.Fatalf("default project %q", got)
	}
	if got := m.ComposeProject("prod", "cache"); got != "legacy-cache" {
		t.Fatalf("override project %q", got)
	}
}

func TestCheckComposeDeployNeedsComposeApp(t *testing.T) {
	m := Manifest{Project: "shop", Apps: map[string]App{
		"db":  {Kind: "compose", File: "compose.yaml"},
		"web": {Kind: "image", Image: "example/web", Roles: map[string]Role{"web": {}}},
	}}
	if err := m.CheckComposeDeploy("db"); err != nil {
		t.Fatalf("compose app refused: %v", err)
	}
	if err := m.CheckComposeDeploy("web"); err == nil {
		t.Fatal("image app accepted as compose")
	}
	if err := m.CheckComposeDeploy("missing"); err == nil {
		t.Fatal("missing app accepted")
	}
}

func TestCheckComposeFileLimits(t *testing.T) {
	exact := []byte(strings.Repeat("x", MaxComposeFile))
	if err := CheckComposeFile(exact); err != nil {
		t.Fatalf("file at the limit refused: %v", err)
	}
	for name, b := range map[string][]byte{
		"empty":      {},
		"too large":  []byte(strings.Repeat("x", MaxComposeFile+1)),
		"not UTF-8":  []byte("services: \xff\n"),
		"NUL byte":   []byte("services:\x00\n"),
		"nul at end": []byte("x\x00"),
	} {
		if err := CheckComposeFile(b); err != ErrComposeFile {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func TestComposeFixedVolumes(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		want       []string
		bad        bool
	}{
		{"fixed names", "volumes:\n  plain:\n  pgdata: {external: true, name: shared-pg}\n  data: {name: pg-main}\n  ext: {external: true}\n", []string{"ext", "pg-main", "shared-pg"}, false},
		{"invalid yaml", "volumes: [", nil, true},
		{"include", "include: [other.yml]\n", nil, true},
		{"multiple documents", "services: {}\n---\nvolumes: {data: {name: hidden}}\n", nil, true},
		{"merged root", "defaults: &d {volumes: {data: {name: hidden}}}\n<<: *d\n", nil, true},
		{"merged volume", "defaults: &d {name: hidden}\nvolumes: {data: {<<: *d}}\n", nil, true},
		{"bad volumes", "volumes: [data]\n", nil, true},
		{"bad entry", "volumes: {data: 3}\n", nil, true},
		{"interpolated braced", "volumes: {data: {name: '${STACK}_data'}}\n", nil, true},
		{"interpolated bare", "volumes: {data: {name: '$STACK'}}\n", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ComposeFixedVolumes([]byte(tc.file))
			if (err != nil) != tc.bad || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, %v; want %v, bad=%v", got, err, tc.want, tc.bad)
			}
		})
	}
}
