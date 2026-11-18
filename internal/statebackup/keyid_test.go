package statebackup

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"berthy/internal/objectstore"
	"berthy/internal/state"
)

var rotator = state.Actor{ID: "admin", Name: "alice"}

func TestTheSidecarNamesTheKeyTheSnapshotNeeds(t *testing.T) {
	g := newRig(t, 14)
	run := g.run("manual")
	if run.Status != "succeeded" {
		t.Fatalf("run: %+v", run)
	}
	var sc Sidecar
	if err := json.Unmarshal(g.read(strings.TrimSuffix(run.Key, ".db")+".json"), &sc); err != nil {
		t.Fatal(err)
	}
	if sc.Format != "berthy-state-sqlite-2" || sc.KeyID != g.store.KeyID() || len(sc.KeyID) != 16 {
		t.Fatalf("sidecar: %+v (store key %s)", sc, g.store.KeyID())
	}
	// A snapshot after a rotation names the new key.
	if _, err := g.store.RotateKey(context.Background(), rotator, false); err != nil {
		t.Fatal(err)
	}
	after := g.run("manual")
	if err := json.Unmarshal(g.read(strings.TrimSuffix(after.Key, ".db")+".json"), &sc); err != nil || sc.KeyID != g.store.KeyID() {
		t.Fatalf("sidecar after a rotation: %+v %v", sc, err)
	}
}

// requests wraps handler and records the method and path of each request.
type requests struct {
	mu   sync.Mutex
	seen []string
}

func (r *requests) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.seen = append(r.seen, req.Method+" "+req.URL.Path)
		r.mu.Unlock()
		h.ServeHTTP(w, req)
	})
}

func (r *requests) has(entry string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.seen {
		if s == entry {
			return true
		}
	}
	return false
}

func TestRestoreWithAnotherKeyNamesBothKeysBeforeItDownloadsAnything(t *testing.T) {
	g := newRig(t, 14)
	run := g.run("manual")
	other := filepath.Join(t.TempDir(), "other.key")
	if err := os.WriteFile(other, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	seen := &requests{}
	proxy := httptest.NewServer(seen.wrap(g.srv.Config.Handler))
	t.Cleanup(proxy.Close)
	opts := g.restoreOptions(run.Key, filepath.Join(t.TempDir(), "restored"), other)
	opts.Endpoint = proxy.URL

	_, err := Restore(context.Background(), opts)
	var mismatch *KeyMismatchError
	if !errors.As(err, &mismatch) || !errors.Is(err, state.ErrWrongKey) {
		t.Fatalf("restore with another key: %v", err)
	}
	wantNeed, _ := state.KeyIDOfFile(g.keyPath)
	wantHave, _ := state.KeyIDOfFile(other)
	if mismatch.Need != wantNeed || mismatch.Have != wantHave || wantNeed == wantHave {
		t.Fatalf("mismatch %+v, want need %s have %s", mismatch, wantNeed, wantHave)
	}
	if want := "the snapshot needs key " + wantNeed + "; " + other + " is key " + wantHave; err.Error() != want {
		t.Fatalf("message %q, want %q", err, want)
	}
	if !seen.has("GET /bkt/" + strings.TrimSuffix(run.Key, ".db") + ".json") {
		t.Fatalf("the sidecar was not read: %v", seen.seen)
	}
	if seen.has("GET /bkt/" + run.Key) {
		t.Fatalf("the snapshot was downloaded before the key was checked: %v", seen.seen)
	}
}

func TestRestoreOfASnapshotFromBeforeARotationNeedsTheOldKey(t *testing.T) {
	g := newRig(t, 14)
	ctx := context.Background()
	old := g.run("manual")
	oldKey, err := os.ReadFile(g.keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = g.store.RotateKey(ctx, rotator, false); err != nil {
		t.Fatal(err)
	}
	// The current key does not open the old snapshot, and says which key does.
	_, err = Restore(ctx, g.restoreOptions(old.Key, filepath.Join(t.TempDir(), "a"), g.keyPath))
	var mismatch *KeyMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("restore of an old snapshot with the new key: %v", err)
	}
	saved := filepath.Join(t.TempDir(), "saved.key")
	if err = os.WriteFile(saved, oldKey, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Restore(ctx, g.restoreOptions(old.Key, filepath.Join(t.TempDir(), "b"), saved)); err != nil {
		t.Fatalf("restore with the saved old key: %v", err)
	}
}

func TestASuccessfulSnapshotReleasesThePreviousKey(t *testing.T) {
	g := newRig(t, 14)
	ctx := context.Background()
	if _, err := g.store.RotateKey(ctx, rotator, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(g.keyPath + ".prev"); err != nil {
		t.Fatalf("no previous key after a rotation: %v", err)
	}
	// A snapshot that fails keeps it.
	g.runner = g.newRunner(g.store, func(s objectstore.Store) objectstore.Store { return faulty{Store: s, suffix: ".db"} })
	if run := g.run("manual"); run.Status != "failed" {
		t.Fatalf("run: %+v", run)
	}
	if !g.store.HasPreviousKey() {
		t.Fatal("a failed snapshot released the previous key")
	}
	g.runner = g.newRunner(g.store)
	if run := g.run("manual"); run.Status != "succeeded" {
		t.Fatalf("run: %+v", run)
	}
	if g.store.HasPreviousKey() {
		t.Fatal("a successful snapshot kept the previous key")
	}
	if _, err := os.Stat(g.keyPath + ".prev"); !os.IsNotExist(err) {
		t.Fatalf("the previous key file is kept: %v", err)
	}
}
