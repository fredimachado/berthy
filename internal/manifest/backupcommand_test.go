package manifest

import (
	"slices"
	"testing"
)

// A backup has exactly one of volume and command, and a command backup names where it runs as a job does.
func TestCheckBackupSource(t *testing.T) {
	dump := `json:["sh","-c","pg_dump db"]`
	cases := []struct {
		name  string
		edits []any
		want  Problems
	}{
		{"a volume backup", nil, nil},
		{"a command backup of an image app with one role", []any{"backups.data.volume", nil, "backups.data.command", dump}, nil},
		{"a command backup of a Compose app", []any{"backups.data.app", "db", "backups.data.volume", nil, "backups.data.command", dump, "backups.data.service", "postgres", "backups.data.timeout", "30m"}, nil},
		{"a Compose command backup without a service", []any{"backups.data.app", "db", "backups.data.volume", nil, "backups.data.command", dump}, nil},
		{"a volume and a command", []any{"backups.data.command", dump},
			Problems{{Path: "backups.data.command", Message: "backup data: name exactly one of volume and command"}}},
		{"neither a volume nor a command", []any{"backups.data.volume", nil},
			Problems{{Path: "backups.data.volume", Message: "backup data: name exactly one of volume and command"}}},
		{"an empty command", []any{"backups.data.volume", nil, "backups.data.command", "json:[]"},
			Problems{{Path: "backups.data.volume", Message: "backup data: name exactly one of volume and command"}}},
		{"a command that is not an argv", []any{"backups.data.volume", nil, "backups.data.command", `json:[""]`},
			Problems{{Path: "backups.data.command", Message: `invalid backup "data"`}}},
		{"a service for an image app", []any{"backups.data.volume", nil, "backups.data.command", dump, "backups.data.service", "web"},
			Problems{{Path: "backups.data.service", Message: "backup data: use role for an image app"}}},
		{"a role for a Compose app", []any{"backups.data.app", "db", "backups.data.volume", nil, "backups.data.command", dump, "backups.data.role", "web"},
			Problems{{Path: "backups.data.role", Message: "backup data: use service for a Compose app"}}},
		{"a role and a service", []any{"backups.data.volume", nil, "backups.data.command", dump, "backups.data.role", "web", "backups.data.service", "web"},
			Problems{{Path: "backups.data.service", Message: `invalid backup "data"`}}},
		{"an unknown role", []any{"backups.data.volume", nil, "backups.data.command", dump, "backups.data.role", "nope"},
			Problems{{Path: "backups.data.role", Message: "backup data: role is not a role of the app"}}},
		{"an invalid timeout", []any{"backups.data.volume", nil, "backups.data.command", dump, "backups.data.timeout", "soon"},
			Problems{{Path: "backups.data.timeout", Message: "backup data: invalid timeout"}}},
		{"a timeout of a volume backup", []any{"backups.data.timeout", "1h"},
			Problems{{Path: "backups.data.timeout", Message: "backup data: timeout applies to a command backup"}}},
		{"a role of a volume backup", []any{"backups.data.role", "web"},
			Problems{{Path: "backups.data.role", Message: "backup data: role and service apply to a command backup"}}},
		{"a before job of a command backup", []any{"backups.data.volume", nil, "backups.data.command", dump, "backups.data.before", "seed"},
			Problems{{Path: "backups.data.before", Message: "backup data: before applies to a volume backup"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := uncheckedManifest(t, brokenManifest(t, c.edits...)).Check()
			if !slices.Equal(got, c.want) {
				t.Fatalf("got %+v, want %+v", []Problem(got), []Problem(c.want))
			}
		})
	}
}

// A command backup that sets no timeout takes the job's default, which a stored manifest shows.
func TestCommandBackupTimeoutDefaults(t *testing.T) {
	m := uncheckedManifest(t, brokenManifest(t, "backups.data.volume", nil, "backups.data.command", `json:["pg_dump"]`))
	d := m.Defaulted()
	if got := d.Backups["data"].Timeout; got != "1h" {
		t.Fatalf("timeout %q, want 1h", got)
	}
	if got := d.Backups["data"].TimeoutDuration().String(); got != "1h0m0s" {
		t.Fatalf("duration %s", got)
	}
	v := uncheckedManifest(t, brokenManifest(t)).Defaulted()
	if v.Backups["data"].Timeout != "" {
		t.Fatalf("a volume backup got a timeout %q", v.Backups["data"].Timeout)
	}
}
