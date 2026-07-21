package store

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jobqueue "github.com/LibertusDio/job-queue"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

//go:embed migration_mysql.sql
var migrationMySQL string

//go:embed migration_postgres.sql
var migrationPostgres string

//go:embed migration_sqlite.sql
var migrationSQLite string

// testEnv is one dialect's ready-to-use store + underlying *gorm.DB (tests
// need the raw DB handle to build contexts for CreateJob/CreateScheduleJob,
// and to assert on rows directly).
type testEnv struct {
	name  string
	store jobqueue.JobStorage
	db    *gorm.DB
}

// mysqlShared and postgresShared cache one connection+schema per dialect for
// the whole test binary run: recreating the schema (5 indexes each) per Test
// function against a real server is real, measurable overhead multiplied by
// every top-level test. testDialects instead clears rows between tests,
// which is enough since every test uses distinct job_id values.
var (
	mysqlSharedOnce sync.Once
	mysqlShared     *gorm.DB
	mysqlSharedErr  error

	postgresSharedOnce sync.Once
	postgresShared     *gorm.DB
	postgresSharedErr  error
)

// testDialects always includes SQLite (no external dependency - a fresh temp
// file per test) and adds MySQL/Postgres when JOBQUEUE_TEST_MYSQL_DSN /
// JOBQUEUE_TEST_POSTGRES_DSN are set (see store/gorm/docker-compose.test.yml
// for a way to stand up both locally). Dialects without a DSN are silently
// skipped rather than failing, so `go test ./...` works without Docker.
func testDialects(t *testing.T) []testEnv {
	t.Helper()
	var envs []testEnv
	envs = append(envs, openSQLite(t))

	if dsn := os.Getenv("JOBQUEUE_TEST_MYSQL_DSN"); dsn != "" {
		mysqlSharedOnce.Do(func() { mysqlShared, mysqlSharedErr = setupSharedDB(mysql.Open(dsn), migrationMySQL) })
		if mysqlSharedErr != nil {
			t.Fatalf("open mysql: %v", mysqlSharedErr)
		}
		resetRows(t, mysqlShared)
		envs = append(envs, newTestEnv(t, "mysql", mysqlShared))
	}

	if dsn := os.Getenv("JOBQUEUE_TEST_POSTGRES_DSN"); dsn != "" {
		postgresSharedOnce.Do(func() {
			postgresShared, postgresSharedErr = setupSharedDB(postgres.Open(dsn), migrationPostgres)
		})
		if postgresSharedErr != nil {
			t.Fatalf("open postgres: %v", postgresSharedErr)
		}
		resetRows(t, postgresShared)
		envs = append(envs, newTestEnv(t, "postgres", postgresShared))
	}

	return envs
}

func setupSharedDB(dialector gorm.Dialector, migration string) (*gorm.DB, error) {
	db, err := gorm.Open(dialector, &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		return nil, err
	}
	for _, stmt := range append(
		strings.Split("DROP TABLE IF EXISTS jobs; DROP TABLE IF EXISTS schedule_jobs;", ";"),
		strings.Split(migration, ";")...,
	) {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if err := db.Exec(stmt).Error; err != nil {
			return nil, fmt.Errorf("exec %q: %w", stmt, err)
		}
	}
	return db, nil
}

func resetRows(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.Exec("DELETE FROM jobs").Error; err != nil {
		t.Fatalf("clear jobs table: %v", err)
	}
	if err := db.Exec("DELETE FROM schedule_jobs").Error; err != nil {
		t.Fatalf("clear schedule_jobs table: %v", err)
	}
}

func openSQLite(t *testing.T) testEnv {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db") + "?_pragma=busy_timeout(5000)"
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// SQLite only ever allows one writer at a time; letting database/sql's
	// pool open several physical connections just means concurrent writers
	// hit SQLITE_BUSY against each other instead of queueing. Capping the
	// pool to one connection makes Go itself serialize access, which is more
	// reliable than busy_timeout alone under real concurrency.
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	if err := db.Exec(migrationSQLite).Error; err != nil {
		t.Fatalf("apply sqlite migration: %v", err)
	}
	// Unlike the shared MySQL/Postgres connections, this one is exclusive to
	// this test (fresh temp file), so it's ours to close afterward.
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.Close()
		}
	})
	return newTestEnv(t, "sqlite", db)
}

func newTestEnv(t *testing.T, name string, db *gorm.DB) testEnv {
	t.Helper()
	s, err := NewGormStore("jobs", "schedule_jobs", "db", db, nil)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return testEnv{name: name, store: s, db: db}
}

func newJob(title, jobID string) jobqueue.Job {
	return jobqueue.Job{
		ID:       uuid.NewString(),
		JobID:    jobID,
		Title:    title,
		Status:   jobqueue.JobStatus.INIT,
		Priority: 1,
	}
}

// TestInjectJob_ConcurrentDuplicatesRejected is the core regression test for
// the dedup race: many goroutines racing InjectJob for the same (title,
// job_id) must produce exactly one row, no matter the dialect.
func TestInjectJob_ConcurrentDuplicatesRejected(t *testing.T) {
	for _, env := range testDialects(t) {
		env := env
		t.Run(env.name, func(t *testing.T) {
			const n = 30
			var successes, duplicates, other int64
			var wg sync.WaitGroup
			start := time.Now()
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					err := env.store.InjectJob(newJob("foo", "dup-key"))
					switch err {
					case nil:
						atomic.AddInt64(&successes, 1)
					case jobqueue.JobError.DUPLICATE_JOB:
						atomic.AddInt64(&duplicates, 1)
					default:
						atomic.AddInt64(&other, 1)
						t.Errorf("unexpected error: %v", err)
					}
				}()
			}
			wg.Wait()
			elapsed := time.Since(start)

			if other != 0 {
				t.Fatalf("%d calls returned an unexpected error", other)
			}
			if successes != 1 {
				t.Fatalf("expected exactly 1 success, got successes=%d duplicates=%d", successes, duplicates)
			}
			if successes+duplicates != n {
				t.Fatalf("expected all %d calls accounted for, got successes=%d duplicates=%d", n, successes, duplicates)
			}
			// A leaked MySQL GET_LOCK would serialize every call behind the
			// 5s lock timeout instead of fast-failing on duplicate detection.
			if elapsed > 4*time.Second {
				t.Errorf("took %s for %d concurrent calls - suspiciously slow, possible lock leak", elapsed, n)
			}

			var count int64
			if err := env.db.Table("jobs").Where("job_id = ?", "dup-key").Count(&count).Error; err != nil {
				t.Fatalf("count rows: %v", err)
			}
			if count != 1 {
				t.Fatalf("expected exactly 1 row in jobs table, got %d", count)
			}
		})
	}
}

// TestCreateJob_ConcurrentDuplicatesRejected is the same race, but through
// CreateJob's plain-*gorm.DB path (the common AddJobTransaction usage).
func TestCreateJob_ConcurrentDuplicatesRejected(t *testing.T) {
	for _, env := range testDialects(t) {
		env := env
		t.Run(env.name, func(t *testing.T) {
			const n = 30
			var successes, duplicates, other int64
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					ctx := context.WithValue(context.Background(), "db", env.db)
					err := env.store.CreateJob(ctx, newJob("foo", "dup-key-create"))
					switch err {
					case nil:
						atomic.AddInt64(&successes, 1)
					case jobqueue.JobError.DUPLICATE_JOB:
						atomic.AddInt64(&duplicates, 1)
					default:
						atomic.AddInt64(&other, 1)
						t.Errorf("unexpected error: %v", err)
					}
				}()
			}
			wg.Wait()

			if other != 0 {
				t.Fatalf("%d calls returned an unexpected error", other)
			}
			if successes != 1 {
				t.Fatalf("expected exactly 1 success, got successes=%d duplicates=%d", successes, duplicates)
			}

			var count int64
			if err := env.db.Table("jobs").Where("job_id = ?", "dup-key-create").Count(&count).Error; err != nil {
				t.Fatalf("count rows: %v", err)
			}
			if count != 1 {
				t.Fatalf("expected exactly 1 row in jobs table, got %d", count)
			}
		})
	}
}

// TestCreateJob_CallerManagedTransactionCommit exercises CreateJob's other
// branch: ctx already carries an open transaction (the "share job creation
// with my own writes" pattern), and confirms it participates correctly.
func TestCreateJob_CallerManagedTransactionCommit(t *testing.T) {
	for _, env := range testDialects(t) {
		env := env
		t.Run(env.name, func(t *testing.T) {
			tx := env.db.Begin()
			ctx := context.WithValue(context.Background(), "db", tx)
			if err := env.store.CreateJob(ctx, newJob("foo", "caller-tx-commit")); err != nil {
				t.Fatalf("CreateJob: %v", err)
			}
			if err := tx.Commit().Error; err != nil {
				t.Fatalf("commit: %v", err)
			}

			var count int64
			if err := env.db.Table("jobs").Where("job_id = ?", "caller-tx-commit").Count(&count).Error; err != nil {
				t.Fatalf("count rows: %v", err)
			}
			if count != 1 {
				t.Fatalf("expected 1 row after commit, got %d", count)
			}
		})
	}
}

// TestCreateJob_CallerManagedTransactionRollback confirms CreateJob doesn't
// commit independently of the caller: rolling back the caller's transaction
// must leave no row behind.
func TestCreateJob_CallerManagedTransactionRollback(t *testing.T) {
	for _, env := range testDialects(t) {
		env := env
		t.Run(env.name, func(t *testing.T) {
			tx := env.db.Begin()
			ctx := context.WithValue(context.Background(), "db", tx)
			if err := env.store.CreateJob(ctx, newJob("foo", "caller-tx-rollback")); err != nil {
				t.Fatalf("CreateJob: %v", err)
			}
			if err := tx.Rollback().Error; err != nil {
				t.Fatalf("rollback: %v", err)
			}

			var count int64
			if err := env.db.Table("jobs").Where("job_id = ?", "caller-tx-rollback").Count(&count).Error; err != nil {
				t.Fatalf("count rows: %v", err)
			}
			if count != 0 {
				t.Fatalf("expected 0 rows after rollback, got %d", count)
			}

			// And the lock must have been released, not leaked - a second,
			// independent attempt at the same key should succeed promptly.
			done := make(chan error, 1)
			go func() { done <- env.store.InjectJob(newJob("foo", "caller-tx-rollback")) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("expected retry to succeed after rollback, got %v", err)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("retry after rollback did not complete - lock appears leaked")
			}
		})
	}
}

// TestDuplicate_AllowedAfterTerminalStatus checks the actual business rule:
// a (title, job_id) is only "in use" while INIT/RETRY/PROCESSING - once a job
// reaches a terminal status, the same key must be resubmittable.
func TestDuplicate_AllowedAfterTerminalStatus(t *testing.T) {
	for _, env := range testDialects(t) {
		env := env
		t.Run(env.name, func(t *testing.T) {
			job := newJob("foo", "resub-key")
			if err := env.store.InjectJob(job); err != nil {
				t.Fatalf("initial InjectJob: %v", err)
			}

			dup := newJob("foo", "resub-key")
			if err := env.store.InjectJob(dup); err != jobqueue.JobError.DUPLICATE_JOB {
				t.Fatalf("expected DUPLICATE_JOB while active, got %v", err)
			}

			job.Status = jobqueue.JobStatus.DONE
			if err := env.store.UpdateJobResult(job); err != nil {
				t.Fatalf("UpdateJobResult: %v", err)
			}

			resub := newJob("foo", "resub-key")
			if err := env.store.InjectJob(resub); err != nil {
				t.Fatalf("expected resubmission to succeed after terminal status, got %v", err)
			}
		})
	}
}

// TestGetAndLockAvailableJob_NoDoubleClaim fires many concurrent claimers at
// a fixed pool of jobs and verifies every job is claimed exactly once - the
// property GetAndLockAvailableJob's SELECT ... FOR UPDATE exists to provide.
func TestGetAndLockAvailableJob_NoDoubleClaim(t *testing.T) {
	for _, env := range testDialects(t) {
		env := env
		t.Run(env.name, func(t *testing.T) {
			const totalJobs = 20
			for i := 0; i < totalJobs; i++ {
				if err := env.store.InjectJob(newJob("foo", fmt.Sprintf("claim-%d", i))); err != nil {
					t.Fatalf("seed job %d: %v", i, err)
				}
			}

			jd := map[string]jobqueue.JobDescription{
				"foo": {Title: "foo", TTL: 60, Concurrent: totalJobs, Priority: 1, MaxRetry: 2},
			}

			const claimers = 8
			var wg sync.WaitGroup
			claimedIDs := make(chan string, totalJobs*2)
			for i := 0; i < claimers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						j, err := env.store.GetAndLockAvailableJob(jd)
						if err == jobqueue.JobError.NOT_FOUND {
							return
						}
						if err != nil {
							t.Errorf("claim error: %v", err)
							return
						}
						claimedIDs <- j.ID
					}
				}()
			}
			wg.Wait()
			close(claimedIDs)

			seen := make(map[string]bool, totalJobs)
			count := 0
			for id := range claimedIDs {
				if seen[id] {
					t.Fatalf("job %s claimed more than once", id)
				}
				seen[id] = true
				count++
			}
			if count != totalJobs {
				t.Fatalf("expected %d jobs claimed exactly once, got %d", totalJobs, count)
			}
		})
	}
}

// TestClaimAndComplete is a basic sanity round-trip: inject, claim, complete,
// and confirm the job is no longer claimable afterward.
func TestClaimAndComplete(t *testing.T) {
	for _, env := range testDialects(t) {
		env := env
		t.Run(env.name, func(t *testing.T) {
			if err := env.store.InjectJob(newJob("foo", "flow-key")); err != nil {
				t.Fatalf("InjectJob: %v", err)
			}

			jd := map[string]jobqueue.JobDescription{"foo": {Title: "foo", TTL: 30, Concurrent: 1, Priority: 1, MaxRetry: 2}}
			claimed, err := env.store.GetAndLockAvailableJob(jd)
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			if claimed.Status != jobqueue.JobStatus.PROCESSING {
				t.Fatalf("expected PROCESSING after claim, got %s", claimed.Status)
			}

			claimed.Status = jobqueue.JobStatus.DONE
			claimed.Result = "ok"
			if err := env.store.UpdateJobResult(*claimed); err != nil {
				t.Fatalf("UpdateJobResult: %v", err)
			}

			if _, err := env.store.GetAndLockAvailableJob(jd); err != jobqueue.JobError.NOT_FOUND {
				t.Fatalf("expected NOT_FOUND after completion, got %v", err)
			}
		})
	}
}
