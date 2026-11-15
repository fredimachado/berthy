package state

import (
	"context"
	"testing"
)

func TestImageProvenanceReopenAndOwnership(t *testing.T) {
	s, dir, key := testStore(t)
	ctx := context.Background()
	if err := s.RecordImageAcquired(ctx, "example/app@sha256:abc", "sha256:local"); err == nil {
		t.Fatal("unlocked provenance mutation")
	}
	_, _, err := s.Admit(ctx, Admission{RequestID: "p", ActorID: "a", Kind: "deploy", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordImageAcquired(ctx, "example/app@sha256:abc", "sha256:local"); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordImageAcquired(ctx, "example/app@sha256:abc", "sha256:other"); err == nil {
		t.Fatal("reassigned provenance")
	}
	s.Close()
	s, err = Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	images, err := s.AcquiredImages(ctx)
	if err != nil || len(images) != 1 || images[0].ID != "sha256:local" {
		t.Fatalf("provenance %v %v", images, err)
	}
	if err = s.ForgetImage(ctx, images[0].Ref, "sha256:other"); err == nil {
		t.Fatal("deleted changed image")
	}
	if err = s.ForgetImage(ctx, images[0].Ref, images[0].ID); err != nil {
		t.Fatal(err)
	}
}
func TestUpgradeVersionOne(t *testing.T) {
	s, dir, key := testStore(t)
	if _, err := s.db.Exec(`DROP TABLE acquired_images; PRAGMA user_version=1`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	upgraded, err := Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var version int
	if err = upgraded.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 2 {
		t.Fatalf("migration: %d %v", version, err)
	}
	if _, err = upgraded.AcquiredImages(context.Background()); err != nil {
		t.Fatal(err)
	}
}
