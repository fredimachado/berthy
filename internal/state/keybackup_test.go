package state

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadSettledKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	key := bytes.Repeat([]byte{42}, 32)
	if err := os.WriteFile(path, key, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSettledKey(path)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatal("valid key refused", err)
	}
	if err := os.WriteFile(path+KeyNextSuffix, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSettledKey(path); !errors.Is(err, ErrRotationUnsettled) {
		t.Fatal("unsettled key accepted")
	}
}

func TestReadSettledKeyRefusesACommittedRotationUntilTheAgentSettlesIt(t *testing.T) {
	s, dir, path := keyedStore(t, randomKey(t))
	fillState(t, s)
	obstacle := path + ".prev.tmp"
	if err := os.Mkdir(obstacle, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(obstacle, "obstacle"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	rep, err := s.RotateKey(context.Background(), rotator, false)
	if !errors.Is(err, ErrKeyFiles) || s.KeyID() != rep.NewKeyID {
		t.Fatal("rotation did not commit", err)
	}
	if _, err = ReadSettledKey(path); !errors.Is(err, ErrRotationUnsettled) {
		t.Fatal("unfinished rotation accepted")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(dir, path, "prod"); err == nil {
		reopened.Close()
		t.Fatal("obstructed restart succeeded")
	}
	if _, err = ReadSettledKey(path); !errors.Is(err, ErrRotationUnsettled) {
		t.Fatal("obstructed restart accepted")
	}
	if err = os.RemoveAll(obstacle); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, path, "prod")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	raw, err := ReadSettledKey(path)
	if err != nil || keyIDOf(raw).String() != rep.NewKeyID {
		t.Fatal("settled key mismatch", err)
	}
}
