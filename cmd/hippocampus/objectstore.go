package main

import (
	"context"
	"errors"
	"fmt"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/archive"
)

// objectStoreConfig is what initObjectStore needs to build the optional archive store.
type objectStoreConfig struct {
	S3        archive.S3Config
	Directory string
}

// setArchiveDefaults applies the archive.* defaults.
func setArchiveDefaults() {
	// Defaulted though the scheduled export is off unless intervalHours is set, as the tombstone caps
	// are: a bound that matters once the feature is on should not depend on the operator
	// remembering it. A week of daily archives.
	viper.SetDefault("archive.scheduledExport.keep", 7)
}

// archiveProblems checks the archive.* and s3.* keys.
func archiveProblems() []error {
	var problems []error

	// The scheduled export (TODO-3 item 158) needs an archive store to write to. Configured without
	// one it would start, log once, and take no backup ever - and a backup is the thing nobody checks
	// until the day it is needed, so this is refused rather than warned about.
	exportInterval := viper.GetInt("archive.scheduledExport.intervalHours")
	exportKeep := viper.GetInt("archive.scheduledExport.keep")

	switch {

	case exportInterval < 0:
		problems = append(problems, fmt.Errorf("archive.scheduledExport.intervalHours must not be negative, got %d", exportInterval))

	case exportKeep < 0:
		problems = append(problems, fmt.Errorf("archive.scheduledExport.keep must not be negative, got %d (0 keeps every archive)", exportKeep))

	case exportInterval > 0 && viper.GetString("archive.directory") == "" && viper.GetString("s3.bucket") == "":
		problems = append(problems, errors.New(
			"archive.scheduledExport.intervalHours is set but there is nowhere to write the exports: set archive.directory or s3.bucket",
		))

	}

	// Two object-store backends both configured. Only one can back Export/Import, so this is a
	// configuration whose author believes something the service does not do - and it is worth
	// refusing rather than resolving, because the two disagree about where the archives went, which
	// is the sort of thing found out when one is needed.
	if viper.GetString("s3.bucket") != "" && viper.GetString("archive.directory") != "" {
		problems = append(problems, fmt.Errorf("s3.bucket and archive.directory are both set: configure exactly one object store for Export/Import"))
	}

	return problems
}

// objectStoreConfigFromViper reads the s3.* keys and archive.directory.
func objectStoreConfigFromViper() objectStoreConfig {
	log.Trace("func() objectStoreConfigFromViper")

	return objectStoreConfig{
		S3: archive.S3Config{
			Endpoint:     viper.GetString("s3.endpoint"),
			Region:       viper.GetString("s3.region"),
			Bucket:       viper.GetString("s3.bucket"),
			UsePathStyle: viper.GetBool("s3.usePathStyle"),
		},
		Directory: viper.GetString("archive.directory"),
	}
}

// initObjectStore builds the optional object store backing the Export/Import RPCs. Nil when neither
// backend is configured, which makes those RPCs fail with FAILED_PRECONDITION rather than at
// startup: most deployments never touch the archive surface.
//
// s3.bucket takes precedence where both are somehow set, but configProblems refuses that pair
// outright, so this is a tie-break that should never be reached rather than a policy.
func initObjectStore(ctx context.Context, cfg objectStoreConfig) (archive.ObjectStore, error) {
	log.Trace("func() initObjectStore")

	switch {

	case cfg.S3.Bucket != "":
		// Credentials come from the standard AWS chain; s3.endpoint and s3.usePathStyle exist for
		// S3-compatible stores such as MinIO.
		log.Debug("initialising s3 object store")

		store, err := archive.NewS3Store(ctx, cfg.S3)
		if err != nil {
			return nil, fmt.Errorf("failed to initialise the s3 object store: %w", err)
		}

		log.Debug("s3 object store initialised")

		return store, nil

	case cfg.Directory != "":
		// The filesystem backend, for the deployment this product is actually shaped like: one
		// binary and a directory. It exists because the archive format is the only representation
		// that preserves a store's full state, and requiring a bucket to reach it put the offline
		// backup of a store that is designed to forget behind infrastructure.
		log.Debug("initialising filesystem object store")

		store, err := archive.NewFileStore(cfg.Directory)
		if err != nil {
			return nil, fmt.Errorf("failed to initialise the filesystem object store: %w", err)
		}

		log.Infof("archiving to the local directory '%s'", store.Directory())

		return store, nil

	}

	return nil, nil
}
