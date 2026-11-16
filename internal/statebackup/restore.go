package statebackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"berthy/internal/objectstore"
	"berthy/internal/state"
)

// RestoreOptions names a snapshot in an S3-compatible bucket and the place to rebuild it. The credentials come from the caller,
// which reads them from the environment: they never come from a flag.
type RestoreOptions struct {
	Endpoint, Region, Bucket string
	// Object is the key of the snapshot, the .db object, and its sidecar is the .json beside it.
	Object string
	// Out is the directory to write into. It is created when missing, and must otherwise be empty.
	Out string
	// KeyFile is the saved key that opens the snapshot.
	KeyFile              string
	AccessKey, SecretKey string
}

// RestoreReport describes a restored state. It never holds a secret value: only the names of the secrets.
type RestoreReport struct {
	Path          string // Out/state.db
	Environment   string
	SchemaVersion int // the schema the restored state runs with
	Size          int64
	SHA256        string
	Secrets       []string // names only, sorted
}

// ErrNoSnapshotFile is what a restore reports when the object is missing from the bucket.
var ErrNoSnapshotFile = Refusal("snapshot not found")

var environmentName = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// Restore downloads a snapshot and its sidecar, checks the download against the sidecar, and opens the result with the key to
// prove the key decrypts it. It works without the agent and its server. A failure removes what the restore wrote, so a retry
// into the same directory is possible. The result is a state directory an agent can run with: set its state_dir to Out and
// use the same key.
func Restore(ctx context.Context, o RestoreOptions) (RestoreReport, error) {
	var rep RestoreReport
	if !strings.HasSuffix(o.Object, snapshotSuffix) {
		return rep, ErrNotSnapshot
	}
	store, err := objectstore.New(objectstore.Config{Endpoint: o.Endpoint, Region: o.Region, Bucket: o.Bucket, AccessKey: o.AccessKey, SecretKey: o.SecretKey})
	if err != nil {
		return rep, err
	}
	sc, err := readRestoreSidecar(ctx, store, strings.TrimSuffix(o.Object, snapshotSuffix)+sidecarSuffix)
	if err != nil {
		return rep, err
	}
	created, err := prepareOutput(o.Out)
	if err != nil {
		return rep, err
	}
	fail := func(err error) (RestoreReport, error) {
		cleanupOutput(o.Out, created)
		return RestoreReport{}, err
	}
	rep.Path = filepath.Join(o.Out, "state.db")
	if err = download(ctx, store, o.Object, rep.Path, sc); err != nil {
		return fail(err)
	}
	st, err := state.Open(o.Out, o.KeyFile, sc.Environment)
	if err != nil {
		return fail(err)
	}
	rep.Environment, rep.Size, rep.SHA256 = sc.Environment, sc.Size, sc.SHA256
	rep.SchemaVersion, rep.Secrets, err = describe(ctx, st)
	// The store closes before a failure removes its files, which Windows would refuse while it is open.
	st.Close()
	if err != nil {
		return fail(err)
	}
	return rep, nil
}

// describe reads what a restored state holds: its schema version and the names of its secrets.
func describe(ctx context.Context, st *state.Store) (int, []string, error) {
	version, err := st.SchemaVersion(ctx)
	if err != nil {
		return 0, nil, err
	}
	secrets, err := st.Secrets(ctx)
	if err != nil {
		return 0, nil, err
	}
	var names []string
	for _, s := range secrets {
		names = append(names, s.Name)
	}
	return version, names, nil
}

// readRestoreSidecar reads the sidecar of a snapshot that is to be restored. A missing one is ErrNoSidecar.
func readRestoreSidecar(ctx context.Context, store objectstore.Store, key string) (Sidecar, error) {
	rc, _, err := store.Get(ctx, key)
	if errors.Is(err, objectstore.ErrNotFound) {
		return Sidecar{}, ErrNoSidecar
	}
	if err != nil {
		return Sidecar{}, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxSidecar+1))
	if err != nil {
		return Sidecar{}, err
	}
	var sc Sidecar
	if len(data) > maxSidecar || json.Unmarshal(data, &sc) != nil || sc.Format != SidecarFormat || !environmentName.MatchString(sc.Environment) ||
		sc.Size < 0 || !sha256Hex.MatchString(sc.SHA256) {
		return Sidecar{}, ErrInvalidSidecar
	}
	return sc, nil
}

// prepareOutput makes the directory a restore writes into. It returns whether the directory was created, so a failed restore
// can remove it. A directory that already holds anything is ErrNotEmpty and is left alone.
func prepareOutput(dir string) (bool, error) {
	created := false
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		if err = os.MkdirAll(dir, 0700); err != nil {
			return false, err
		}
		created = true
	case err != nil:
		return false, err
	case len(entries) > 0:
		return false, ErrNotEmpty
	}
	return created, os.Chmod(dir, 0700)
}

// cleanupOutput removes what a failed restore wrote: the whole directory when it created it, and otherwise the state files.
func cleanupOutput(dir string, created bool) {
	if created {
		os.RemoveAll(dir)
		return
	}
	for _, name := range []string{"state.db", "state.db-wal", "state.db-shm", "state.db-journal", "owner.lock"} {
		os.Remove(filepath.Join(dir, name))
	}
}

// download writes the object to path, which must not exist, and checks its size and SHA-256 against the sidecar.
func download(ctx context.Context, store objectstore.Store, key, path string, sc Sidecar) error {
	rc, _, err := store.Get(ctx, key)
	if errors.Is(err, objectstore.ErrNotFound) {
		return ErrNoSnapshotFile
	}
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), rc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != sc.Size || hex.EncodeToString(hash.Sum(nil)) != sc.SHA256 {
		return ErrChecksumMismatch
	}
	return nil
}
