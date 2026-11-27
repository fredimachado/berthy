package state

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"

	"berthy/internal/scope"
)

// HasAppliedApps reads the hosted environment's current applied rows without opening an owning Store.
func HasAppliedApps(ctx context.Context, stateDir string, id scope.ID) (bool, error) {
	if err := checkDir(stateDir); err != nil {
		return false, err
	}
	path := filepath.Join(stateDir, "state.db")
	if err := checkAppliedDatabase(path); err != nil {
		return false, err
	}
	uri := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&readonly_shm=1"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return false, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := checkOwner(ctx, tx, id.Project, id.Environment); err != nil {
		return false, err
	}
	var applied bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM applied WHERE environment = ?)`, id.Environment).Scan(&applied); err != nil {
		return false, err
	}
	return applied, nil
}

func checkAppliedDatabase(path string) error {
	if err := checkPrivateFile(path, false); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var header [100]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return err
	}
	if string(header[:16]) != "SQLite format 3\x00" {
		return errors.New("invalid SQLite header")
	}
	// SQLite may create a missing WAL even with mode=ro. Byte 19 is the database read format.
	walRequired := header[19] == 2
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := checkPrivateFile(path+suffix, !walRequired); err != nil {
			return err
		}
	}
	return checkPrivateFile(path+"-journal", true)
}
