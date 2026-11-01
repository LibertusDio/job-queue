# job-queue

A small job queue for Go that keeps its jobs in your SQL database, through
GORM. Runs on MySQL 8, MariaDB 10.5, Postgres and SQLite, and on several
instances at once: each job runs on one worker, retries are counted, and jobs
can be scheduled for later.

```sh
go get github.com/LibertusDio/job-queue
```

Create the tables with the migration for your database:
[MySQL/MariaDB](store/gorm/migration_mysql.sql),
[Postgres](store/gorm/migration_postgres.sql),
[SQLite](store/gorm/migration_sqlite.sql).

## Quick start

```go
package main

import (
	"context"
	"os"
	"os/signal"

	jobqueue "github.com/LibertusDio/job-queue"
	"github.com/LibertusDio/job-queue/middlewares"
	gormstore "github.com/LibertusDio/job-queue/store/gorm"
	"github.com/LibertusDio/job-queue/utils"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func main() {
	logger := utils.DumpLogger{}
	db, err := gorm.Open(mysql.Open("user:pass@tcp(127.0.0.1:3306)/app?parseTime=true"), &gorm.Config{})
	if err != nil {
		panic(err)
	}

	// "db" is the context key the store reads the *gorm.DB from.
	store, err := gormstore.NewGormStore("jobs", "schedule_jobs", "db", db, logger)
	if err != nil {
		panic(err)
	}

	queue := jobqueue.NewForeman(&jobqueue.QueueConfig{
		JobDescription: map[string]jobqueue.JobDescription{
			"send-email": {Title: "send-email", TTL: 30, Concurrent: 5, Priority: 1, MaxRetry: 3},
		},
		Concurrent: 10,   // workers in total
		BreakTime:  1000, // max ms between polls when idle
		RampTime:   100,  // ms between polls while busy
	}, store, logger)

	// Runs each handler in a transaction, passed to it under "db".
	queue.AddMiddleware(middlewares.GormTransactionMiddleware("db", db))

	queue.AddWorker("send-email", func(ctx context.Context) error {
		job := ctx.Value(jobqueue.ContextJobKey).(*jobqueue.Job)
		if err := sendEmail(job.Payload); err != nil {
			job.Status = jobqueue.JobStatus.RETRY
			job.Result = err.Error()
			return nil
		}
		job.Status = jobqueue.JobStatus.DONE
		return nil
	})

	go queue.Serve() // blocks until Strike

	queue.AddJob(&jobqueue.Job{Title: "send-email", JobID: "welcome-42", Payload: `{"to":"a@example.com"}`})

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
	queue.Strike(30) // stop taking jobs, wait 30 s, cancel the rest
}

func sendEmail(payload string) error { return nil }
```

Runnable examples: [test/gorm](test/gorm) (MySQL),
[test/gorm-postgres](test/gorm-postgres), [test/gorm-sqlite](test/gorm-sqlite).

## Writing a worker

The handler gets the job from `ctx.Value(jobqueue.ContextJobKey)` and sets its
outcome on it:

| In the handler | Result |
|---|---|
| `job.Status = JobStatus.DONE` | Finished. |
| `job.Status = JobStatus.RETRY` | Runs again, up to `MaxRetry` times, then `max_retry`. |
| `job.Status = JobStatus.ERROR`, return an error, or panic | Failed, no retry. The error goes into `job.Result`. |
| Still running after `TTL` seconds | Cancelled and treated as `RETRY`. |

Always set a status. A job left `processing` is picked up again after its TTL.
Each run increments `job.Try` and doubles `job.Priority` (1 runs first), so
retries queue behind fresh jobs. Keep `job.Message` short (100 characters);
put details in `job.Result`.

## Adding jobs

A job is identified by `Title` + `JobID`. Adding one while the same pair is
`init`, `retry` or `processing` returns `JobError.DUPLICATE_JOB`; once it has
finished, the pair can be added again.

```go
err := queue.AddJob(&jobqueue.Job{Title: "send-email", JobID: "welcome-42"})

// Run later: runat is unix seconds.
err = queue.AddJobWithSchedule(context.WithValue(ctx, "db", db),
	&jobqueue.Job{Title: "reminder", JobID: order.ID}, time.Now().Add(time.Hour).Unix())
```

### In your own transaction

`AddJobTransaction` creates the job inside your transaction, so it exists only
if your writes commit. If it fails, return the error and roll back. Never log
it and carry on: on MySQL/MariaDB a deadlock has already rolled back your
earlier writes, and committing would keep only the writes made after it.
Deadlocks are normal under load, so retry the whole transaction when
`jobqueue.IsRetryable(err)`:

```go
for attempt := 1; ; attempt++ {
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&order).Error; err != nil {
			return err
		}
		return queue.AddJobTransaction(context.WithValue(ctx, "db", tx),
			&jobqueue.Job{Title: "invoice", JobID: order.ID})
	})
	if err == nil || !jobqueue.IsRetryable(err) || attempt == 5 {
		return err
	}
	time.Sleep(time.Duration(attempt) * 20 * time.Millisecond)
}
```

`DUPLICATE_JOB` is the exception: it leaves your transaction intact.

## Recurring jobs

A handler can schedule its own next run. Only one *pending* schedule per
`Title` + `JobID` is allowed (`DUPLICATE_JOB` otherwise), and the run in
progress doesn't count:

```go
queue.AddWorker("nightly-sync", func(ctx context.Context) error {
	job := ctx.Value(jobqueue.ContextJobKey).(*jobqueue.Job)
	// ... do the work ...
	next := time.Now().Add(24 * time.Hour).Unix()
	err := queue.AddJobWithSchedule(ctx, &jobqueue.Job{Title: job.Title, JobID: job.JobID}, next)
	if err != nil && err != jobqueue.JobError.DUPLICATE_JOB { // already scheduled is fine
		return err
	}
	job.Status = jobqueue.JobStatus.DONE
	return nil
})
```

The scheduler checks for due schedules every `ScheduleInterval` ms (59 s by
default). If a schedule comes due while the same job is still running, it
waits and fires after that run ends.

## Configuration

| `QueueConfig` | |
|---|---|
| `JobDescription` | One entry per job title (below). Jobs with other titles are rejected. |
| `Concurrent` | Workers in total. |
| `BreakTime`, `RampTime` | ms between polls: `RampTime` while jobs are coming in, doubling up to `BreakTime` while idle. |
| `ScheduleInterval` | ms between scheduler passes, default 59 s. |

| `JobDescription` | |
|---|---|
| `Title` | Same as the map key and `AddWorker` title. |
| `TTL` | Seconds a run may take. |
| `Concurrent` | Max running jobs with this title. |
| `Priority` | Default priority, 1 is highest. |
| `MaxRetry` | Retries allowed before `max_retry`. |

`CleanError` and `Secure` exist but aren't used yet.

## Errors and shutdown

`Serve` blocks until `Strike`, then returns `JobError.TERMINATING`. It never
stops because of database errors: deadlocks are retried, and other errors are
logged as `fetch job error:...` and retried with a backoff from 50 ms up to
5 s. Watch for that log line to spot a database outage.

`Strike(ttl)` stops taking new jobs, waits `ttl` seconds, then cancels the
jobs still running.

## Databases

- **MySQL/MariaDB:** the job claim runs at READ COMMITTED, which avoids
  most deadlocks with concurrent inserts. With `binlog_format=STATEMENT`
  MySQL refuses that; the store logs a warning and uses the server default.
- **Postgres:** no special setup.
- **SQLite:** one writer at a time, so best for a single process. Use
  `sqlDB.SetMaxOpenConns(1)`, as in [test/gorm-sqlite](test/gorm-sqlite).

### Upgrading from v1.0

Add the new index:

```sql
CREATE INDEX schedule_jobs_title_job_id_IDX ON schedule_jobs (title, job_id);
```

Behavior changes: `Serve` no longer returns on database errors.
`AddJobWithSchedule` now rejects only a second *pending* schedule, not a
running job with the same id. A schedule due while its job runs now waits
instead of being marked `error`.

## Running the tests

SQLite always runs. MySQL, MariaDB and Postgres run when their DSN is set:

```sh
docker compose -f store/gorm/docker-compose.test.yml up -d
export JOBQUEUE_TEST_MYSQL_DSN="root:jobqueue@tcp(127.0.0.1:3307)/jobqueue?charset=utf8mb4&parseTime=True&loc=Local"
export JOBQUEUE_TEST_MARIADB_DSN="root:jobqueue@tcp(127.0.0.1:3308)/jobqueue?charset=utf8mb4&parseTime=True&loc=Local"
export JOBQUEUE_TEST_POSTGRES_DSN="host=127.0.0.1 port=5544 user=postgres password=jobqueue dbname=jobqueue sslmode=disable"
go test ./...
docker compose -f store/gorm/docker-compose.test.yml down -v
```

`-short` skips the stress test (750 jobs, about 10 s per database).
The compose file turns off fsync for speed; don't copy its settings to real
databases.
