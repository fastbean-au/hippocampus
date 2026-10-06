package db

import (
	"context"
	"errors"
	"fmt"
	"os"

	log "github.com/sirupsen/logrus"
)

// ErrBackupUnsupported is returned by BackupTo on a dialect with no in-process online backup: the
// server dialects, whose own tools (pg_dump, mysqldump) are the right answer and which the service
// has no business imitating.
var ErrBackupUnsupported = errors.New("online backup is not available on this storage driver")

// BackupTo writes a consistent copy of the store to path, which must not already exist (TODO-3
// item 158).
//
// It exists because the documented alternative - copy the database file - is wrong for a store in
// WAL mode: the recent writes live in the -wal sidecar until a checkpoint, so a copy of the main file
// alone silently loses them, and copying the two separately is a race against the writer. VACUUM INTO
// reads one snapshot and writes a single compacted file with no sidecar, and it works on a read-only
// connection - so a backup taken through NewSQLiteReadOnly runs beside a live instance, taking no lock
// and writing nothing to the store it reads. The copy is a complete database: restoring it is putting
// it in place as hippocampus.db.
//
// An existing file is refused rather than overwritten, here and by SQLite: a backup that clobbers the
// previous one when a rotation script misfires is the wrong failure.
func (d *DB) BackupTo(ctx context.Context, path string) error {
	log.Trace("func() db.BackupTo")

	if !d.dialect().onlineBackup {
		return ErrBackupUnsupported
	}

	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("backup destination %q already exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking backup destination %q: %w", path, err)
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	if _, err := d.sql.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		log.Errorf("failed to back up the store to %s: %s", path, err.Error())

		return err
	}

	return nil
}
