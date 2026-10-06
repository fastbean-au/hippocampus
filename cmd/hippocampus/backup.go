package main

import (
	"context"
	"errors"
	"fmt"

	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/db"
)

// backupConfig is what the --backup mode needs, read from viper in main.
type backupConfig struct {
	StorageDriver    string
	StorageDirectory string
	Destination      string
}

// runBackup is the --backup CLI mode: a consistent copy of the SQLite store, taken through a
// read-only connection so it is safe beside a running instance (TODO-3 item 158). The store's own
// docs used to say "copy the file", which in WAL mode loses whatever has not been checkpointed.
//
// The server drivers are refused with the tool to use instead. A backup of a PostgreSQL or MySQL
// database is that database's job, done by tools that understand its transactions, roles and
// replication - not something this service should half-reimplement.
func runBackup(cfg backupConfig) error {
	switch cfg.StorageDriver {

	case "", "sqlite":
		// The default; handled below.

	case "postgres":
		return fmt.Errorf("--backup copies a SQLite store; back up PostgreSQL with pg_dump (or your provider's snapshots), " +
			"and see docs/operations.md")

	case "mysql":
		return fmt.Errorf("--backup copies a SQLite store; back up MySQL with mysqldump (or your provider's snapshots), " +
			"and see docs/operations.md")

	default:
		return fmt.Errorf("unknown storage.driver %q", cfg.StorageDriver)

	}

	if cfg.StorageDirectory == "" {
		return errors.New("storage.directory is empty, so there is no store file to back up (an in-memory store cannot be backed up)")
	}

	store, err := db.NewSQLiteReadOnly(cfg.StorageDirectory)
	if err != nil {
		return fmt.Errorf("opening the store in %q: %w", cfg.StorageDirectory, err)
	}

	defer func() { _ = store.Close() }()

	if err := store.BackupTo(context.Background(), cfg.Destination); err != nil {
		return err
	}

	log.WithFields(log.Fields{
		"source":      cfg.StorageDirectory,
		"destination": cfg.Destination,
	}).
		Info("backup written; restore it by putting it in place as hippocampus.db in storage.directory, with the service stopped")

	return nil
}
