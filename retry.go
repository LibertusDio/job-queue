package jobqueue

import (
	"errors"

	"github.com/LibertusDio/job-queue/internal/sqlerr"
)

// Server error codes for a lock conflict the database resolved by failing one
// of the transactions involved.
const (
	mysqlErrLockDeadlock    = 1213 // ER_LOCK_DEADLOCK
	mysqlErrLockWaitTimeout = 1205 // ER_LOCK_WAIT_TIMEOUT
	pgDeadlockDetected      = "40P01"
	pgLockNotAvailable      = "55P03"
)

// IsRetryable reports whether err is a deadlock or lock wait timeout: MySQL/
// MariaDB error 1213 or 1205, Postgres SQLSTATE 40P01 or 55P03, or the gorm
// store's StoreError.LOCK_TIMEOUT. These are an expected outcome of concurrent
// transactions touching the same rows and indexes (e.g. enqueueing while
// workers claim jobs), not a sign of a broken connection or a bad query, and
// running the transaction again normally succeeds.
//
// It recognizes go-sql-driver/mysql's *MySQLError and any error with a
// SQLState() string method (pgx's *pgconn.PgError), wrapped or not, without
// importing either driver.
//
// Retry the whole transaction, never just the failed statement: a MySQL
// deadlock has already rolled back everything the transaction did, and on
// Postgres any error aborts the transaction.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, StoreError.LOCK_TIMEOUT) {
		return true
	}
	if n, ok := sqlerr.MySQLNumber(err); ok {
		return n == mysqlErrLockDeadlock || n == mysqlErrLockWaitTimeout
	}
	if code, ok := sqlerr.SQLState(err); ok {
		return code == pgDeadlockDetected || code == pgLockNotAvailable
	}
	return false
}
