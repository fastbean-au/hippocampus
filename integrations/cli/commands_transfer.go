package main

import (
	"github.com/spf13/pflag"
)

// transferCommands returns the commands that move records out of and into the store: export, import,
// transfer and the clear that follows them.
func transferCommands() map[string]command {
	return map[string]command{
		"export": {
			summary: "snapshot the store, or a selection of it, into an archive object",
			hint:    "[--clear] [--group G] [--metadata k=v] [--significance-max N]",
			flags: func(fs *pflag.FlagSet) {
				fs.Bool("clear", false, "delete the captured records after a successful upload")
				memorySelectionFlags(fs)
			},
			run: runExport,
		},
		"import": {
			summary: "import an archive object",
			hint:    "--object-key KEY",
			flags:   func(fs *pflag.FlagSet) { fs.String("object-key", "", "S3 object key of the archive (required)") },
			run:     runImport,
		},
		"import-batch": {
			summary: "upsert full-state rows from a JSON ImportBatchRequest file",
			hint:    "--file PATH ('-' for stdin)",
			flags: func(fs *pflag.FlagSet) {
				fs.String("file", "", "JSON ImportBatchRequest file, or '-' for stdin (required)")
			},
			run: runImportBatch,
		},
		"transfer": {
			summary: "stream the store, or a selection of it, into a centralised instance",
			hint:    "[--clear] [--group G] [--metadata k=v] [--significance-max N]",
			flags: func(fs *pflag.FlagSet) {
				fs.Bool("clear", false, "delete the captured records once the target has accepted them")
				memorySelectionFlags(fs)
			},
			run: runTransfer,
		},
		"clear": {
			summary: "delete exactly the records captured by an export/transfer run",
			hint:    "--manifest-id ID",
			flags: func(fs *pflag.FlagSet) {
				fs.String("manifest-id", "", "manifest id returned by export/transfer (required)")
			},
			run: runClear,
		},
	}
}
