package deploy

import (
	"context"
	"reflect"
	"testing"

	"berthy/internal/engine"
)

// driftCompose deploys the Compose app db with its container in place and returns the controller and the fake Docker.
func driftCompose(t *testing.T) (*Controller, *composeDocker) {
	t.Helper()
	docker := newComposeDocker()
	c, s := composeController(t, &fakeCompose{config: configOne, hashes: hashOne}, docker)
	op, _, err := admit(t, c, context.Background(), Actor{ID: "ci"}, composeReq("one", composeFileOne, composeApp(false)))
	if err != nil {
		t.Fatal(err)
	}
	if got := waitComposeOperation(t, s, op.ID); got.State != "succeeded" {
		t.Fatalf("deploy %s %s", got.State, got.Err)
	}
	docker.containers = []engine.ComposeContainer{{ID: "c1", Service: "database", ConfigHash: "hash-0000000000000001"}}
	return c, docker
}

func TestDriftOfAComposeAppThatMatchesItsReleaseIsEmpty(t *testing.T) {
	c, _ := driftCompose(t)
	got, err := c.Drift(context.Background(), "db")
	if err != nil || len(got.Lines) != 0 || got.Release != shortDigest(digestOf(composeFileOne)) {
		t.Fatalf("report %+v: %v", got, err)
	}
}

func TestDriftNamesTheComposeServicesAndVolumesADeployWouldChange(t *testing.T) {
	c, docker := driftCompose(t)
	docker.containers = []engine.ComposeContainer{{ID: "c1", Service: "database", ConfigHash: "hash-edited-by-hand"}, {ID: "c2", Service: "extra", ConfigHash: "x"}}
	docker.volumes = map[string]bool{}
	got, err := c.Drift(context.Background(), "db")
	want := []string{"db: service database would be recreated", "db: service extra would be removed", "db: volume shop-test-db_data would be created"}
	if err != nil || !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("report %+v: %v", got, err)
	}
}
