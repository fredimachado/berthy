package backup

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"berthy/internal/manifest"
	"berthy/internal/objectstore"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// TestBackupListAndRestoreThroughTheRealObjectClient runs the runner against the S3 client and an in-process S3 server, so
// the layout, the sidecar and the retention work on what a bucket really answers, not only on the in-memory fake.
func TestBackupListAndRestoreThroughTheRealObjectClient(t *testing.T) {
	backend := s3mem.New()
	if err := backend.CreateBucket("bkt"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	defer srv.Close()
	store, err := objectstore.New(objectstore.Config{Endpoint: srv.URL, Bucket: "bkt", AccessKey: "AKIATESTACCESS", SecretKey: "test-secret-key-0123456789"})
	if err != nil {
		t.Fatal(err)
	}
	g := newRig(t)
	g.runner.Objects = func(context.Context, manifest.Destination) (objectstore.Store, error) { return store, nil }
	b := g.m.Backups["uploads"]
	b.Retention = &manifest.Retention{Keep: float64(2)}
	g.m.Backups["uploads"] = b
	var keys []string
	for _, body := range []string{"first-archive", "second-archive", "third-archive"} {
		g.archive(body)
		res := g.run("uploads")
		if res.Run.Status != "succeeded" {
			t.Fatalf("run: %+v", res.Run)
		}
		keys = append(keys, res.Key)
	}
	list, err := g.runner.List(context.Background(), g.m, "")
	if err != nil || len(list) != 2 || list[0].Key != keys[2] || list[1].Key != keys[1] {
		t.Fatalf("list after retention: %+v %v", list, err)
	}
	if _, err = store.Stat(context.Background(), keys[0]); err == nil {
		t.Fatal("the oldest archive was kept")
	}
	g.docker = fresh(g, "")
	if err = g.runner.Restore(context.Background(), g.m, keys[2], "restored-1", "op-1"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := g.docker.stdinSeen.String(); got != "third-archive" || !strings.Contains(keys[2], "files/uploads/uploads/") {
		t.Fatalf("restored %q from %s", got, keys[2])
	}
}
