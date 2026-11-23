package credstore

import (
	"errors"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

// fakeRing is a keyringBackend for tests. It never touches the OS store.
type fakeRing struct {
	vals map[string]string
	// setNoStore makes Set return nil without storing.
	setNoStore bool
	// deleteNoDelete makes Delete return nil without removing.
	deleteNoDelete bool
	// getErr, setErr, deleteErr are returned as written (may hold a token).
	getErr, setErr, deleteErr error
}

func (f *fakeRing) id(service, user string) string { return service + "\x00" + user }

func (f *fakeRing) Get(service, user string) (string, error) {
	if f.getErr != nil {
		return "", f.getErr
	}
	v, ok := f.vals[f.id(service, user)]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return v, nil
}

func (f *fakeRing) Set(service, user, password string) error {
	if f.setErr != nil {
		return f.setErr
	}
	if f.setNoStore {
		return nil
	}
	if f.vals == nil {
		f.vals = map[string]string{}
	}
	f.vals[f.id(service, user)] = password
	return nil
}

func (f *fakeRing) Delete(service, user string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if f.deleteNoDelete {
		return nil
	}
	id := f.id(service, user)
	if _, ok := f.vals[id]; !ok {
		return keyring.ErrNotFound
	}
	delete(f.vals, id)
	return nil
}

func allowFakeSessionBus(t *testing.T) {
	t.Helper()
	// sessionBus only checks the env is set; the fake ring never opens a real bus.
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/berthy-test-no-real-bus")
}

func TestOSStoreSetUnavailableWhenBackendDoesNotStore(t *testing.T) {
	allowFakeSessionBus(t)
	ring := &fakeRing{setNoStore: true}
	s := &osStore{service: "berthy-test-fake", ring: ring}
	key := Key{URL: "http://10.0.0.5:7710", Project: "todo", Environment: "dev", Variable: "BERTHY_TODO_DEV_TOKEN"}
	err := s.Set(key, "bty.test.secret-token-value")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Set: %v", err)
	}
	if got := err.Error(); !strings.Contains(got, "save failed") {
		t.Fatalf("Set error = %q, want save failed", got)
	}
}

func TestOSStoreDeleteUnavailableWhenBackendDoesNotDelete(t *testing.T) {
	allowFakeSessionBus(t)
	key := Key{URL: "http://10.0.0.5:7710", Project: "todo", Environment: "dev", Variable: "BERTHY_TODO_DEV_TOKEN"}
	ring := &fakeRing{vals: map[string]string{"berthy-test-fake\x00" + key.Account(): "kept"}, deleteNoDelete: true}
	s := &osStore{service: "berthy-test-fake", ring: ring}
	_, err := s.Delete(key)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Delete: %v", err)
	}
	if got := err.Error(); !strings.Contains(got, "delete failed") {
		t.Fatalf("Delete error = %q, want delete failed", got)
	}
}

func TestOSStoreErrorsDoNotHoldTheToken(t *testing.T) {
	allowFakeSessionBus(t)
	const token = "bty.test.leaked-token-value"
	key := Key{URL: "http://10.0.0.5:7710", Project: "todo", Environment: "dev", Variable: "BERTHY_TODO_DEV_TOKEN"}
	ring := &fakeRing{getErr: errors.New("backend said " + token)}
	s := &osStore{service: "berthy-test-fake", ring: ring}
	_, err := s.Get(key)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Get: %v", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("Get error holds the token: %v", err)
	}
	ring = &fakeRing{setErr: errors.New("backend said " + token)}
	s = &osStore{service: "berthy-test-fake", ring: ring}
	err = s.Set(key, token)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Set: %v", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("Set error holds the token: %v", err)
	}
	ring = &fakeRing{
		vals:      map[string]string{"berthy-test-fake\x00" + key.Account(): token},
		deleteErr: errors.New("backend said " + token),
	}
	s = &osStore{service: "berthy-test-fake", ring: ring}
	_, err = s.Delete(key)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Delete: %v", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("Delete error holds the token: %v", err)
	}
}
