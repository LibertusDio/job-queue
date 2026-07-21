# job-queue
To manage small jobs

## Storage backends

`store/gorm` implements `JobStorage` on top of GORM and only depends on
`gorm.io/gorm` core, not any specific driver — pass in any `*gorm.DB` and it
works. It has been verified against:

- **MySQL** — schema: [store/gorm/migration_mysql.sql](store/gorm/migration_mysql.sql), example: [test/gorm](test/gorm)
- **Postgres** — schema: [store/gorm/migration_postgres.sql](store/gorm/migration_postgres.sql), example: [test/gorm-postgres](test/gorm-postgres)
- **SQLite** — schema: [store/gorm/migration_sqlite.sql](store/gorm/migration_sqlite.sql), example: [test/gorm-sqlite](test/gorm-sqlite) (uses the pure-Go `github.com/glebarez/sqlite` driver, so no CGO/C compiler is required)

SQLite has no `SELECT ... FOR UPDATE`; the store detects the dialect
(`db.Name() == "sqlite"`) and skips that clause there, relying on SQLite's
own transaction-level write serialization instead. Because of that, SQLite
is best suited to single-process/low-concurrency deployments — concurrent
claim attempts under real contention may surface as a "database is locked"
error rather than blocking.

## Duplicate-job locking

`CheckDuplicateJob` runs in its own short transaction, separate from the
`CreateJob`/`InjectJob` insert that follows it — on purpose, so the dedup
check never sits inside a long-lived job transaction and lock-waits against
`GetAndLockAvailableJob`'s claim scan (that combination is what used to cause
deadlocks). The tradeoff is that `CheckDuplicateJob` by itself cannot prevent
two concurrent callers from both passing the check and both inserting.

The actual guarantee comes from `CreateJob`/`InjectJob`, which take a
cross-instance named lock scoped to `(title, job_id)` before re-checking and
inserting:

- **Postgres**: `pg_advisory_xact_lock` — a separate lock-manager namespace
  from table/row locks, so it can't deadlock against the claim scan, and it
  auto-releases at transaction end (commit or rollback) regardless of who
  calls it. Fully race-free in every calling pattern.
- **MySQL**: `GET_LOCK`/`RELEASE_LOCK` — same deadlock-safety property, but
  since MySQL scopes the lock to the session/connection rather than the
  transaction, this store releases it explicitly right after its own insert.
  That's fully race-free for `InjectJob` and for `CreateJob` called with a
  plain (not already open) `*gorm.DB`, since the store then owns the whole
  transaction itself. If `CreateJob` is instead given a transaction the
  caller had already opened (e.g. to share job creation with its own writes)
  and holds open well beyond the insert before committing, there's a narrow
  window on MySQL specifically where a concurrent submission for the same
  key could still slip in — commit promptly in that pattern, or prefer
  Postgres, if that matters for your workload.
- **SQLite**: no named-lock primitive exists, but none is needed — SQLite
  already serializes writers at the transaction level.

`CheckDuplicateJob` itself is still called first by `Foreman.AddJob*` as a
cheap best-effort pre-check (fail fast without opening a second transaction),
but it is not what prevents the race — the lock-protected recheck inside
`CreateJob`/`InjectJob` is. Separately, `Foreman` also stripes an in-process
mutex over `(title, job_id)` around all `AddJob*` calls: it's a same-instance
optimization only (fewer redundant round-trips to the DB lock when the same
process double-submits), not a substitute for the DB-level lock, which is the
only thing that protects against concurrent calls across multiple instances.

## Running the store/gorm tests

[store/gorm/store_test.go](store/gorm/store_test.go) always runs against
SQLite (a fresh temp file per test, no setup needed). MySQL and Postgres are
included automatically when their DSN env vars are set, and silently skipped
otherwise:

```sh
docker compose -f store/gorm/docker-compose.test.yml up -d
# wait for both to report healthy: docker compose -f store/gorm/docker-compose.test.yml ps

export JOBQUEUE_TEST_MYSQL_DSN="root:jobqueue@tcp(127.0.0.1:3307)/jobqueue?charset=utf8mb4&parseTime=True&loc=Local"
export JOBQUEUE_TEST_POSTGRES_DSN="host=127.0.0.1 port=5544 user=postgres password=jobqueue dbname=jobqueue sslmode=disable"
go test ./store/gorm/... -v

docker compose -f store/gorm/docker-compose.test.yml down -v
```

The compose file relaxes MySQL's fsync durability (`innodb-flush-log-at-trx-commit=0`,
`sync-binlog=0`) purely for test-suite speed — real disk fsync under Docker
Desktop's virtualized filesystem was measured at ~400ms/commit, enough on its
own to make 30-way lock contention tests time out for reasons that have
nothing to do with the code under test. Never use these settings against data
you care about.

`-race` needs a C compiler, which this may not have on every machine (e.g.
Windows without a configured `gcc`). To run the race detector anyway, use a
Linux Go container on the same Docker network as the databases:

```sh
docker run --rm --network jobqueue-store-test_default \
  -v "$PWD:/src" -w /src \
  -e JOBQUEUE_TEST_MYSQL_DSN="root:jobqueue@tcp(jobqueue-store-test-mysql-1:3306)/jobqueue?charset=utf8mb4&parseTime=True&loc=Local" \
  -e JOBQUEUE_TEST_POSTGRES_DSN="host=jobqueue-store-test-postgres-1 port=5432 user=postgres password=jobqueue dbname=jobqueue sslmode=disable" \
  golang:1.24 go test -race ./store/gorm/... -v
```

This combination (real MySQL/Postgres via Docker, concurrent test, `-race`)
is what caught a real bug during development: the MySQL lock-release call was
originally issued on the same `*gorm.DB` handle that had just committed, and
a `*sql.Tx` can't be reused after `Commit()` - so `RELEASE_LOCK` silently
failed and every lock leaked after its first use, serializing all later
callers behind `GET_LOCK`'s timeout. Fixed by acquiring MySQL's lock on its
own dedicated `*sql.Conn`, decoupled from the data transaction's lifecycle,
so release always executes regardless of when the data transaction commits.
