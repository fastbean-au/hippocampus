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
// because of repeated storage-level serialization conflicts - a MySQL InnoDB deadlock or a
// lock-wait timeout - that survived the transparent retries below. Callers map it (with errors.Is)
// to a gRPC Aborted status so a client sees a retryable conflict rather than an opaque Unknown, and
// the write is not silently lost.
var ErrWriteConflict = errors.New("write conflict")

// MySQL server error numbers for the two transient, retry-safe serialization conflicts: a detected
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

// writeRetry* bound the transparent retry of a transient write conflict: a handful of attempts with
// a short, jittered exponential backoff. Kept small so a genuinely contended write fails fast rather
// than stalling a request, while the common single-collision case clears on the first retry. The
// whole loop runs inside the operation's queryTimeout context, so it can never outlast it.
const (
	writeRetryMaxAttempts = 5
	writeRetryBaseBackoff = 2 * time.Millisecond
)

// IsWriteConflict reports whether err represents a transient storage-level serialization conflict
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

// isRetryableWriteError reports whether err is a transient MySQL serialization conflict that is safe
// to retry. Only MySQL errors match, so it is a no-op for the SQLite and Postgres drivers (SQLite is
// single-connection and Postgres's default READ COMMITTED does not deadlock a single INSERT).
func isRetryableWriteError(err error) bool {
	var myErr *mysql.MySQLError

	if !errors.As(err, &myErr) {
		return false
	}

	return myErr.Number == mysqlErrDeadlock || myErr.Number == mysqlErrLockWaitTimeout
}

// withWriteRetry runs fn, retrying it on a transient MySQL serialization conflict up to
// writeRetryMaxAttempts times with a short jittered backoff between attempts. It is a no-op wrapper
// for any driver other than MySQL. When the attempts are exhausted the final conflict is wrapped in
// ErrWriteConflict so the RPC layer can surface it as a retryable Aborted rather than an Unknown.
// The backoff waits respect ctx, so a cancelled or timed-out operation stops retrying immediately.
//
// Only safe for a single autocommit statement, whose failed attempt is rolled back whole: exec is
// the sole caller. Multi-statement transactions are not retried here - re-running them would need
// the whole transaction body replayed, not just one statement.
func (d *DB) withWriteRetry(ctx context.Context, fn func() error) error {
	if d.driver != driverMySQL {
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
