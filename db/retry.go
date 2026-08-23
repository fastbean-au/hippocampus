package db

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	log "github.com/sirupsen/logrus"
	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrWriteConflict wraps the error returned when a single-statement write could not complete
// because of repeated storage-level serialisation conflicts - a MySQL InnoDB deadlock or a
// lock-wait timeout - that survived the transparent retries below. Callers map it (with errors.Is)
// to a gRPC Aborted status so a client sees a retryable conflict rather than an opaque Unknown, and
// the write is not silently lost.
var ErrWriteConflict = errors.New("write conflict")

// MySQL server error numbers for the two transient, retry-safe serialisation conflicts: a detected
// deadlock (InnoDB rolls the losing transaction back whole) and a lock-wait timeout. Both leave no
// partial effect on an autocommit statement, so re-running it is safe. Under concurrency these
// surfaced as a gRPC Unknown, losing the write; see isRetryableWriteError.
const (
	mysqlErrLockWaitTimeout = 1205
	mysqlErrDeadlock        = 1213
)

// mysqlErrDupEntry is the MySQL server error number for a duplicate-key (unique or primary)
// violation; pgUniqueViolation is the PostgreSQL SQLSTATE for the same. SQLite reports it through
// the modernc extended result codes SQLITE_CONSTRAINT_UNIQUE / SQLITE_CONSTRAINT_PRIMARYKEY. Used
// by IsDuplicateKey so the RPC layer can map a duplicate create to codes.AlreadyExists.
const (
	mysqlErrDupEntry  = 1062
	pgUniqueViolation = "23505"
)

// PostgreSQL SQLSTATEs for the two transient serialisation conflicts, the class-40 rollbacks. A
// deadlock is detected and one transaction is rolled back whole; a serialisation failure is its
// sibling under a stricter isolation level. Both leave no partial effect, so the losing work is
// safe to re-run.
//
// These were absent until a concurrent write/recall load produced four PostgreSQL deadlocks in four
// and a half minutes, one of which reached the client as Internal and one of which failed an
// eviction pass. The comment on isRetryableWriteError used to reason that Postgres could not
// deadlock here; that holds for a single INSERT and not for the multi-statement paths, where the
// delete chokepoint takes memories -> memory_links -> memories while link creation takes
// memory_links -> memories. Opposite table orders deadlock however carefully the ids within each
// table are ordered.
const (
	pgDeadlockDetected     = "40P01"
	pgSerializationFailure = "40001"
)

// writeRetry* bound the transparent retry of a transient write conflict: a handful of attempts with
// a short, jittered exponential backoff. Kept small so a genuinely contended write fails fast rather
// than stalling a request, while the common single-collision case clears on the first retry. The
// whole loop runs inside the operation's queryTimeout context, so it can never outlast it.
const (
	writeRetryMaxAttempts = 5
	writeRetryBaseBackoff = 2 * time.Millisecond
)

// IsWriteConflict reports whether err represents a transient storage-level serialisation conflict
// that a client can safely retry, so the RPC layer can map it to a gRPC Aborted status. It matches
// both the ErrWriteConflict wrapper (a single-statement write whose transparent retries were
// exhausted) and a raw retryable MySQL deadlock/lock-wait error - the latter surfaces unwrapped from
// the multi-statement transfer transactions, which withWriteRetry deliberately does not retry.
func IsWriteConflict(err error) bool {
	return errors.Is(err, ErrWriteConflict) || isRetryableWriteError(err)
}

// IsDuplicateKey reports whether err is a unique- or primary-key constraint violation from any of
// the three drivers - the storage-layer signal that a client tried to create a row whose id already
// exists. The RPC layer maps it to codes.AlreadyExists rather than letting the raw driver text
// (which names the table and column, e.g. "UNIQUE constraint failed: memories.id") reach the client
// as an opaque Unknown; the constraint detail stays server-side.
func IsDuplicateKey(err error) bool {
	if err == nil {
		return false
	}

	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		return myErr.Number == mysqlErrDupEntry
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == pgUniqueViolation
	}

	var liteErr *sqlite.Error
	if errors.As(err, &liteErr) {
		code := liteErr.Code()

		return code == sqlite3.SQLITE_CONSTRAINT_UNIQUE || code == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
	}

	return false
}

// isRetryableWriteError reports whether err is a transient storage-level serialisation conflict that
// is safe to retry - a detected deadlock or a lock-wait/serialisation failure on either server
// driver. SQLite never matches: it has a single writer, so it cannot deadlock against itself.
func isRetryableWriteError(err error) bool {
	var myErr *mysql.MySQLError

	if errors.As(err, &myErr) {

		return myErr.Number == mysqlErrDeadlock || myErr.Number == mysqlErrLockWaitTimeout
	}

	var pgErr *pgconn.PgError

	if errors.As(err, &pgErr) {

		return pgErr.Code == pgDeadlockDetected || pgErr.Code == pgSerializationFailure
	}

	return false
}

// withWriteRetry runs fn, retrying it on a transient serialisation conflict up to
// writeRetryMaxAttempts times with a short jittered backoff between attempts. It is a no-op wrapper
// for SQLite, which has a single writer and cannot deadlock against itself. When the attempts are exhausted the final conflict is wrapped in
// ErrWriteConflict so the RPC layer can surface it as a retryable Aborted rather than an Unknown.
// The backoff waits respect ctx, so a cancelled or timed-out operation stops retrying immediately.
//
// Only safe for a single autocommit statement, whose failed attempt is rolled back whole: exec is
// the sole caller. Multi-statement transactions are not retried here - re-running them would need
// the whole transaction body replayed, not just one statement.
func (d *DB) withWriteRetry(ctx context.Context, fn func() error) error {
	if d.driver == driverSQLite {

		return fn()
	}

	var err error

	for attempt := range writeRetryMaxAttempts {
		err = fn()
		if err == nil {
			return nil
		}

		if !isRetryableWriteError(err) {
			return err
		}

		// Out of attempts: fall through to wrap the conflict so the caller can map it.
		if attempt == writeRetryMaxAttempts-1 {
			break
		}

		log.Tracef("db write hit a transient conflict, retrying (attempt %d/%d): %s", attempt+1, writeRetryMaxAttempts, err.Error())

		backoff := writeRetryBaseBackoff*time.Duration(1<<attempt) + time.Duration(rand.Int63n(int64(writeRetryBaseBackoff)))

		select {

		case <-ctx.Done():
			return ctx.Err()

		case <-time.After(backoff):
		}
	}

	return fmt.Errorf("write failed after %d attempts: %w: %v", writeRetryMaxAttempts, ErrWriteConflict, err)
}

// withTxRetry runs a whole multi-statement transaction, retrying it on a transient serialisation
// conflict. It is withWriteRetry's counterpart for the paths that cannot use it.
//
// The distinction matters and is the reason both exist. A deadlock aborts the ENTIRE transaction,
// not the statement that tripped it, so retrying one statement inside an already-aborted transaction
// only earns "current transaction is aborted". The body has to be replayed from BEGIN, which means
// fn must own its transaction: begin, do the work, commit, and roll back on any failure. Nothing
// from a failed attempt persists, so the replay starts from the same state the first attempt saw.
//
// fn must therefore be re-runnable. The two callers are: the delete chokepoint, which re-reads
// nothing it mutated and whose recall-race guard makes a second pass over the same ids idempotent;
// and link creation, which upserts, so re-applying an edge re-weights it rather than duplicating it.
//
// A caller that cannot satisfy that must not use this - a transaction with a side effect outside the
// database, or one whose input is consumed as it goes, would be replayed into a different outcome.
func (d *DB) withTxRetry(ctx context.Context, what string, fn func() error) error {
	if d.driver == driverSQLite {

		return fn()
	}

	var err error

	for attempt := range writeRetryMaxAttempts {
		err = fn()
		if err == nil {

			return nil
		}

		if !isRetryableWriteError(err) {

			return err
		}

		if attempt == writeRetryMaxAttempts-1 {

			break
		}

		log.Tracef("db transaction %q hit a transient conflict, retrying (attempt %d/%d): %s",
			what, attempt+1, writeRetryMaxAttempts, err.Error())

		backoff := writeRetryBaseBackoff*time.Duration(1<<attempt) + time.Duration(rand.Int63n(int64(writeRetryBaseBackoff)))

		select {

		case <-ctx.Done():

			return ctx.Err()

		case <-time.After(backoff):
		}
	}

	return fmt.Errorf("%s failed after %d attempts: %w: %v", what, writeRetryMaxAttempts, ErrWriteConflict, err)
}
