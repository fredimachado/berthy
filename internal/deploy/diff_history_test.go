package deploy

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"berthy/internal/state"
)

// releaseIDs are the ids of the releases of app web, newest first.
func releaseIDs(t *testing.T, s *state.Store) []int64 {
	t.Helper()
	rows, err := s.History(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

func TestDiffReleasesComparesTwoReleasesOfAnApp(t *testing.T) {
	s := openStore(t)
	seedManifestRelease(t, s, "first", digestOfLetter("a"), webWith(map[string]string{"OLD": "1"}, 1))
	seedManifestRelease(t, s, "second", digestOfLetter("b"), webWith(map[string]string{"NEW": "1"}, 1))
	seedManifestRelease(t, s, "third", digestOfLetter("c"), webWith(map[string]string{"NEW": "1"}, 2))
	ids := releaseIDs(t, s)
	c := &Controller{Store: s, Environment: "test"}
	newest, middle, oldest := ids[0], ids[1], ids[2]
	ctx := context.Background()

	// Without a base the release is compared with the one before it.
	d, err := c.DiffReleases(ctx, "web", newest, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"image: sha256:" + strings.Repeat("b", 12) + "… -> sha256:" + strings.Repeat("c", 12) + "…", "web: replicas 1 -> 2"}
	if got := changesOf(d); !reflect.DeepEqual(got, want) {
		t.Fatalf("changes\n got %q\nwant %q", got, want)
	}
	// With a base, release A is read as what it changed compared with B, whichever is older.
	if d, err = c.DiffReleases(ctx, "web", newest, oldest); err != nil || len(d.Changes) != 4 {
		t.Fatalf("changes %q: %v", changesOf(d), err)
	}
	if d, err = c.DiffReleases(ctx, "web", oldest, newest); err != nil {
		t.Fatal(err)
	}
	if got := changesOf(d)[0]; got != "image: sha256:"+strings.Repeat("c", 12)+"… -> sha256:"+strings.Repeat("a", 12)+"…" {
		t.Fatalf("first change %q", got)
	}
	// The first release of an app is a new app, and a release compared with itself changes nothing.
	if d, err = c.DiffReleases(ctx, "web", oldest, 0); err != nil || !d.New {
		t.Fatalf("diff %+v: %v", d, err)
	}
	if d, err = c.DiffReleases(ctx, "web", middle, middle); err != nil || !d.Same {
		t.Fatalf("diff %+v: %v", d, err)
	}
}

func TestDiffReleasesRefusesAReleaseOfAnotherApp(t *testing.T) {
	s := openStore(t)
	seedManifestRelease(t, s, "first", digestOfLetter("a"), webWith(nil, 1))
	c := &Controller{Store: s, Environment: "test"}
	ctx := context.Background()
	for _, ids := range [][2]int64{{9999, 0}, {releaseIDs(t, s)[0], 9999}} {
		if _, err := c.DiffReleases(ctx, "web", ids[0], ids[1]); err == nil || !strings.Contains(err.Error(), "is not a release of app web") {
			t.Fatalf("ids %v: %v", ids, err)
		}
	}
	if _, err := c.DiffReleases(ctx, "other", releaseIDs(t, s)[0], 0); err == nil {
		t.Fatal("a release of web was compared as a release of another app")
	}
}
