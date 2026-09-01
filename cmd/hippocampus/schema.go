package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

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
// Two renderings, selected by --output. Text is the default and is what a human reading a support
// question wants; JSON is what a deployment script wants, and its `status` field is the value to
// branch on. The exit status carries the same verdict either way - non-zero when the store is ahead
// of this build - so a script that only needs the gate need not parse anything at all.
func reportSchemaVersion(cfg schemaVersionConfig) {
	// Diagnostics to stderr for the rest of this mode, because stdout is now a data channel: in
	// JSON mode a single log line on it makes the document unparseable, and initLogging has already
	// pointed logging at stdout by the time this runs. Nothing after this needs it back - the mode
	// renders and exits.
	log.SetOutput(os.Stderr)

	target := cfg.StorageDirectory

	switch cfg.StorageDriver {

	case "postgres":
		target = cfg.PostgresDSN

	case "mysql":
		target = cfg.MySQLDSN

	}

	report, err := db.InspectSchema(cfg.StorageDriver, target)
	if err != nil {
		log.Fatalf("failed to read the schema version: %s", err.Error())
	}

	if cfg.JSON {
		if err := printSchemaReportJSON(os.Stdout, report); err != nil {
			log.Fatalf("failed to render the schema report: %s", err.Error())
		}
	} else {
		printSchemaReport(os.Stdout, report)
	}

	// Non-zero, deliberately: this is what a deployment script gates on, and a store the running
	// build cannot open must not read as success. It shares the binary's one failure exit rather
	// than inventing a code of its own, so the message - on stderr, beside the report on stdout -
	// is what distinguishes it.
	if report.Ahead() {
		log.Fatalf(
			"this store cannot be opened by this build: it records schema version %d and this build understands up to %d",
			report.Version, report.Supported,
		)
	}
}

// schemaVersionConfig is what the --schema-version mode needs from the configuration. A struct
// rather than four positional strings, three of which are alternatives to one another.
type schemaVersionConfig struct {
	StorageDriver    string
	StorageDirectory string
	PostgresDSN      string
	MySQLDSN         string

	// JSON selects the machine-readable rendering.
	JSON bool
}

// Schema status values. They are the field a script branches on, so they are a small closed set and
// their spellings are part of the output's contract.
const (
	// schemaStatusCurrent: nothing pending. The store may still record a LOWER version than this
	// build's newest - a migration gated to another dialect is never recorded here - which is why
	// this is derived from what is pending rather than from the two numbers being equal.
	schemaStatusCurrent = "current"

	// schemaStatusBehind: migrations pending. Starting this build applies them.
	schemaStatusBehind = "behind"

	// schemaStatusAhead: written by a newer build. This one refuses to open it.
	schemaStatusAhead = "ahead"
)

// schemaStatus reduces a report to its status. Both renderings derive from this rather than each
// deciding for itself, so the word in the text output and the value in the JSON cannot disagree.
func schemaStatus(report *db.SchemaReport) string {
	switch {

	case report.Ahead():
		return schemaStatusAhead

	case len(report.Pending) > 0:
		return schemaStatusBehind

	default:
		return schemaStatusCurrent

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

	switch status := schemaStatus(report); {

	case status == schemaStatusAhead:
		_, _ = fmt.Fprintf(w, "status:    AHEAD - written by a newer build; this one cannot open it\n")

	case status == schemaStatusBehind:
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

// schemaReportJSON is the machine-readable rendering of a schema report.
//
// A projection rather than JSON tags on db.SchemaReport, following the same rule the MCP bridge
// follows for proto messages: the wire shape is this command's contract with whatever parses it, and
// the storage layer should not acquire one by being marshalled. It also lets the two things the Go
// struct cannot say directly be said - a derived status, and a timestamp that is absent rather than
// the zero time.
type schemaReportJSON struct {
	Dialect string `json:"dialect"`

	// Status is the field to branch on: "current", "behind" or "ahead". See schemaStatus.
	Status string `json:"status"`

	// Version is the highest recorded migration version, and 0 when none is recorded. HasLedger is
	// what separates that from a genuine version 0, which no build has ever written.
	Version   int  `json:"version"`
	Supported int  `json:"supported"`
	HasLedger bool `json:"has_ledger"`

	// Pending and Applied are always present, empty rather than null, so a consumer need not guard
	// for the difference.
	Pending []string               `json:"pending"`
	Applied []appliedMigrationJSON `json:"applied"`
}

// appliedMigrationJSON is one recorded migration.
type appliedMigrationJSON struct {
	Version int    `json:"version"`
	Name    string `json:"name"`

	// AppliedAt is RFC3339 in UTC, and null for a row recorded without a timestamp - which the zero
	// time would instead render as a date in year one, a value a consumer would have to know to
	// distrust.
	AppliedAt *string `json:"applied_at"`
}

// printSchemaReportJSON writes the machine-readable rendering.
func printSchemaReportJSON(w io.Writer, report *db.SchemaReport) error {
	out := schemaReportJSON{
		Dialect:   report.Dialect,
		Status:    schemaStatus(report),
		Version:   report.Version,
		Supported: report.Supported,
		HasLedger: report.HasLedger,
		Pending:   report.Pending,
		Applied:   make([]appliedMigrationJSON, 0, len(report.Applied)),
	}

	if out.Pending == nil {
		out.Pending = []string{}
	}

	for _, migration := range report.Applied {
		applied := appliedMigrationJSON{Version: migration.Version, Name: migration.Name}

		if !migration.AppliedAt.IsZero() {
			when := migration.AppliedAt.UTC().Format(time.RFC3339)
			applied.AppliedAt = &when
		}

		out.Applied = append(out.Applied, applied)
	}

	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")

	return encoder.Encode(out)
}
