package deploy

import (
	"context"
	"strings"
	"testing"
	"time"

	"berthy/internal/engine"
	"berthy/internal/jobs"
	"berthy/internal/scope"
	"berthy/internal/session"
)

// longestID is the longest hosted environment the agent accepts.
func longestID() scope.ID {
	return scope.ID{Project: strings.Repeat("p", 30), Environment: strings.Repeat("e", scope.MaxIDLength-31)}
}

func TestContainerNamesOfTheLongestEnvironmentAreValidDockerNames(t *testing.T) {
	id := longestID()
	if err := id.CheckLength(); err != nil {
		t.Fatalf("longest ID refused: %v", err)
	}
	check := func(t *testing.T, name string) {
		t.Helper()
		if name == "" || !engine.ValidName(name) {
			t.Errorf("container name %q (%d characters) is not a valid Docker identifier", name, len(name))
		}
	}
	t.Run("hook", func(t *testing.T) {
		c, f, opID, p := hookFixture(t)
		c.Project, c.Environment = id.Project, id.Environment
		f.exits = []int{0}
		if _, err := c.runOneOff(context.Background(), opID, p, oneOff{Ordinal: 10000, Kind: "migration_apply", Argv: []string{"true"}, Timeout: time.Minute}); err != nil {
			t.Fatal(err)
		}
		check(t, f.names[0])
	})
	t.Run("command", func(t *testing.T) {
		for _, kind := range []string{"job", "backup", "restore"} {
			c, _ := jobFixture(t)
			c.Project, c.Environment = id.Project, id.Environment
			out, err := c.RunCommand(context.Background(), imageJobManifest(), kind, "report", "9223372036854775807", webJob, jobs.Streams{})
			if err != nil {
				t.Fatal(err)
			}
			check(t, strings.TrimPrefix(out.Container, "ctr-"))
		}
	})
	t.Run("session", func(t *testing.T) {
		c, f := runSessionFixture(t)
		c.Project, c.Environment = id.Project, id.Environment
		if _, err := c.OpenSession(context.Background(), sessionAdmin, session.Request{Kind: "run", App: "web", Command: []string{"sh"}}); err != nil {
			t.Fatal(err)
		}
		check(t, nameOf(f))
	})
}
