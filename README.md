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
