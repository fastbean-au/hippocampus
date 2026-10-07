// Package db is Hippocampus's storage layer: one DB type speaking three SQL dialects - SQLite (the
// default, embedded and single-instance), PostgreSQL and MySQL - selected by storage.driver.
//
// Nearly all query and consolidation logic is shared. What differs between the dialects is confined
// to dialect.go (plus metadata.go and search_dialect.go), and a test refuses dialect checks
// anywhere else. The schema is one versioned migration list for every dialect (schema.go); a store
// records its version, and a build refuses to open a store newer than it understands.
//
// Store is the interface the service depends on. Server is the reverse dependency: the
// consolidation scans call it to ask whether a row should go, so the decay maths stays in the
// hippocampus package while the scans stay here, on a covering index that never reads a memory's
// body.
package db
