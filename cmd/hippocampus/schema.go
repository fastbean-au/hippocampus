package main

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/db"
)

// reportSchemaVersion is the --schema-version CLI mode: read the configured store's schema version
// and exit without starting the server.
//
// It exists for the one question the startup log line cannot answer. The version is logged on every
// start, which serves an operator watching a running instance and is no use at all to one holding a
// store that will not start - which, after a refused downgrade, is exactly the situation. This reads
// the store without opening it for service: no lock, no DDL, and no version gate, so it answers for
// the store the gate refuses as readily as for one it admits.
//
// It is a flag on this binary rather than a `hippo` subcommand. The CLI is a network client and
// dials a running service; a stopped store has nothing to dial.
//
// The output is text rather than JSON. A caller scripting an upgrade wants the exit status, which is
// non-zero when the store is ahead of this build, and a human reading a support question wants the
// table.
func reportSchemaVersion(storageDriver string, storageDirectory string, postgresDSN string, mysqlDSN string) {
	target := storageDirectory

	switch storageDriver {

	case "postgres":
		target = postgresDSN

	case "mysql":
		target = mysqlDSN

	}

	report, err := db.InspectSchema(storageDriver, target)
	if err != nil {
		log.Fatalf("failed to read the schema version: %s", err.Error())
	}

	printSchemaReport(os.Stdout, report)

	// Non-zero, deliberately: this is what a deployment script gates on, and a store the running
	// build cannot open must not read as success. It shares the binary's one failure exit rather
	// than inventing a code of its own, so the message is what distinguishes it.
	if report.Ahead() {
		log.Fatalf(
			"this store cannot be opened by this build: it records schema version %d and this build understands up to %d",
			report.Version, report.Supported,
		)
	}
}

// printSchemaReport writes the report to w. Split from the mode above so a test can read it without
// capturing the process's stdout.
func printSchemaReport(w io.Writer, report *db.SchemaReport) {
	_, _ = fmt.Fprintf(w, "dialect:   %s\n", report.Dialect)

	if !report.HasLedger {
		// Version 0 with no ledger is not a fault: it is every store written before schema versions
		// were recorded, and saying "0" alone would read as one.
		_, _ = fmt.Fprintf(w, "version:   none recorded (written before schema versions existed)\n")
		_, _ = fmt.Fprintf(w, "supported: %d\n", report.Supported)
		_, _ = fmt.Fprintf(w, "\nStarting this build against it will apply %d migrations and record the result.\n", len(report.Pending))

		return
	}

	_, _ = fmt.Fprintf(w, "version:   %d\n", report.Version)
	_, _ = fmt.Fprintf(w, "supported: %d\n", report.Supported)

	switch {

	case report.Ahead():
		_, _ = fmt.Fprintf(w, "status:    AHEAD - written by a newer build; this one cannot open it\n")

	case len(report.Pending) > 0:
		_, _ = fmt.Fprintf(w, "status:    behind - %d migration(s) pending: %v\n", len(report.Pending), report.Pending)

	case report.Version < report.Supported:
		// A capability-gated migration is never recorded on a dialect it does not apply to, so a
		// perfectly current store legitimately reads below this build's newest version. Saying only
		// "current" beside two different numbers would look like a contradiction.
		_, _ = fmt.Fprintf(w, "status:    current - nothing pending; the newer versions this build declares do not apply to this dialect\n")

	default:
		_, _ = fmt.Fprintf(w, "status:    current\n")

	}

	if len(report.Applied) == 0 {
		return
	}

	_, _ = fmt.Fprintf(w, "\napplied migrations:\n")

	table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	for _, migration := range report.Applied {
		when := "unrecorded"
		if !migration.AppliedAt.IsZero() {
			when = migration.AppliedAt.UTC().Format("2006-01-02 15:04:05Z")
		}

		_, _ = fmt.Fprintf(table, "  %d\t%s\t%s\n", migration.Version, migration.Name, when)
	}

	_ = table.Flush()
}
