package db

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// TestWithWriteRetry_RetriesTransientDeadlock exposes the bug: a MySQL deadlock (error 1213) on a
// single-statement write used to propagate straight out as an opaque error, so the write was lost.
// withWriteRetry must instead re-run the statement and let a transient conflict clear.
func TestWithWriteRetry_RetriesTransientDeadlock(t *testing.T) {
	d := &DB{driver: driverMySQL}

	attempts := 0

	err := d.withWriteRetry(context.Background(), func() error {
		attempts++

		// Deadlock on the first two attempts, then succeed - the common single-collision case.
		if attempts < 3 {
			return &mysql.MySQLError{Number: mysqlErrDeadlock, Message: "Deadlock found"}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("expected the retry to eventually succeed, got: %v", err)
	}

	if attempts != 3 {
		t.Errorf("expected 3 attempts (two conflicts then success), got %d", attempts)
	}
}

// TestWithWriteRetry_ExhaustionWrapsErrWriteConflict verifies that a conflict that never clears is
// wrapped in ErrWriteConflict after the attempts are exhausted, so the RPC layer can map it to a
// retryable Aborted rather than an opaque Unknown.
func TestWithWriteRetry_ExhaustionWrapsErrWriteConflict(t *testing.T) {
	d := &DB{driver: driverMySQL}

	attempts := 0

	err := d.withWriteRetry(context.Background(), func() error {
		attempts++

		return &mysql.MySQLError{Number: mysqlErrLockWaitTimeout, Message: "Lock wait timeout"}
	})
	if !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("expected ErrWriteConflict after exhausting retries, got: %v", err)
	}

	if attempts != writeRetryMaxAttempts {
		t.Errorf("expected %d attempts, got %d", writeRetryMaxAttempts, attempts)
	}
}

// TestWithWriteRetry_NonRetryableReturnsImmediately verifies that an ordinary (non-conflict) error
// is returned on the first attempt, unwrapped and unretried.
func TestWithWriteRetry_NonRetryableReturnsImmediately(t *testing.T) {
	d := &DB{driver: driverMySQL}

	sentinel := errors.New("constraint violation")
	attempts := 0

	err := d.withWriteRetry(context.Background(), func() error {
		attempts++

		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the underlying error unchanged, got: %v", err)
	}

	if errors.Is(err, ErrWriteConflict) {
		t.Error("a non-conflict error must not be wrapped in ErrWriteConflict")
	}

	if attempts != 1 {
		t.Errorf("expected exactly 1 attempt for a non-retryable error, got %d", attempts)
	}
}

// TestWithWriteRetry_NonMySQLDriverDoesNotRetry verifies the wrapper is a pass-through for the other
// drivers: even a MySQLError-shaped error is not retried (it cannot arise there), so behaviour is
// unchanged for SQLite and Postgres.
func TestWithWriteRetry_NonMySQLDriverDoesNotRetry(t *testing.T) {
	d := &DB{driver: driverSQLite}

	attempts := 0

	err := d.withWriteRetry(context.Background(), func() error {
		attempts++

		return &mysql.MySQLError{Number: mysqlErrDeadlock, Message: "Deadlock found"}
	})
	if err == nil {
		t.Fatal("expected the error to pass through unchanged")
	}

	if errors.Is(err, ErrWriteConflict) {
		t.Error("a non-MySQL driver must not wrap errors in ErrWriteConflict")
	}

	if attempts != 1 {
		t.Errorf("expected exactly 1 attempt on a non-MySQL driver, got %d", attempts)
	}
}

// TestWithWriteRetry_StopsOnContextCancellation verifies that a cancelled context ends the retry
// loop promptly with the context error rather than sleeping out every remaining backoff.
func TestWithWriteRetry_StopsOnContextCancellation(t *testing.T) {
	d := &DB{driver: driverMySQL}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	attempts := 0

	err := d.withWriteRetry(ctx, func() error {
		attempts++

		return &mysql.MySQLError{Number: mysqlErrDeadlock, Message: "Deadlock found"}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}

	if attempts != 1 {
		t.Errorf("expected the loop to stop after the first attempt on a cancelled context, got %d", attempts)
	}
}

// TestIsWriteConflict verifies the exported classifier the RPC layer uses to map errors to Aborted:
// it must recognise both the ErrWriteConflict wrapper (single-statement exhaustion) and a raw MySQL
// deadlock/lock-wait error (the unwrapped form a multi-statement transfer transaction surfaces,
// which withWriteRetry deliberately never wraps), while leaving ordinary errors unmatched.
func TestIsWriteConflict(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "wrapped ErrWriteConflict", err: fmt.Errorf("db write: %w", ErrWriteConflict), want: true},
		{name: "raw deadlock", err: &mysql.MySQLError{Number: mysqlErrDeadlock, Message: "Deadlock found"}, want: true},
		{name: "raw lock-wait timeout", err: &mysql.MySQLError{Number: mysqlErrLockWaitTimeout, Message: "Lock wait timeout"}, want: true},
		{name: "unrelated mysql error", err: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}, want: false},
		{name: "ordinary error", err: errors.New("constraint violation"), want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsWriteConflict(tc.err); got != tc.want {
				t.Errorf("IsWriteConflict(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
