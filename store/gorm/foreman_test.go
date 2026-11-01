package store

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jobqueue "github.com/LibertusDio/job-queue"
	"github.com/LibertusDio/job-queue/internal/sqlerr"
	gormmidl "github.com/LibertusDio/job-queue/middlewares"

	"github.com/google/uuid"
)

// recLogger keeps the foreman's warnings and errors and prints them only if
// the test fails. It never writes to t directly, because the foreman's
// goroutines can still log after the test has finished.
type recLogger struct {
	mu   sync.Mutex
	msgs []string
}

func newRecLogger(t *testing.T) *recLogger {
	l := &recLogger{}
	t.Cleanup(func() {
		if t.Failed() {
			l.mu.Lock()
			defer l.mu.Unlock()
			for _, m := range l.msgs {
				t.Log(m)
			}
		}
	})
	return l
}

func (l *recLogger) add(level, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.msgs) < 200 {
		l.msgs = append(l.msgs, level+" "+msg)
	}
}

func (l *recLogger) Debug(string)     {}
func (l *recLogger) Info(string)      {}
func (l *recLogger) Warn(msg string)  { l.add("WARN", msg) }
func (l *recLogger) Error(msg string) { l.add("ERROR", msg) }
func (l *recLogger) Fatal(msg string) { l.add("FATAL", msg) }
func (l *recLogger) Panic(msg string) { l.add("PANIC", msg) }

// runningForeman is a foreman whose Serve runs in the background. It's always
// struck, and Serve has always returned, by the end of the test: one left
// running would claim the next test's rows on the shared MySQL/Postgres DBs.
type runningForeman struct {
	jobqueue.Foreman
	done chan struct{}
	err  error // Serve's result, valid once done is closed
}

func startForeman(t *testing.T, f jobqueue.Foreman) *runningForeman {
	r := &runningForeman{Foreman: f, done: make(chan struct{})}
	go func() {
		r.err = f.Serve()
		close(r.done)
	}()
	t.Cleanup(func() { r.stop(t) })
	return r
}

func (r *runningForeman) returned() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// stop strikes the foreman and returns what Serve returned.
func (r *runningForeman) stop(t *testing.T) error {
	t.Helper()
	_ = r.Strike(0)
	select {
	case <-r.done:
		return r.err
	case <-time.After(10 * time.Second):
		t.Errorf("Serve did not return within 10s of Strike")
		return nil
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// countJobs counts jobs rows for a polled waitFor condition. The deadline
// matters: if the code under test exhausts the connection pool (SQLite's has
// a single connection), an unbounded query would block forever instead of
// letting waitFor fail the test with a message.
func countJobs(env testEnv, query string, args ...interface{}) int64 {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var n int64
	env.db.WithContext(ctx).Table("jobs").Where(query, args...).Count(&n)
	return n
}

// lockStats tallies the errors each store call path ran into, by kind.
type lockStats struct {
	mu sync.Mutex
	n  map[string]int
}

func newLockStats() *lockStats { return &lockStats{n: map[string]int{}} }

func (s *lockStats) record(path string, err error) {
	if err == nil || err == jobqueue.JobError.NOT_FOUND || err == jobqueue.JobError.DUPLICATE_JOB {
		return
	}
	kind := "other error"
	n, _ := sqlerr.MySQLNumber(err)
	code, _ := sqlerr.SQLState(err)
	switch {
	case n == 1213, code == "40P01":
		kind = "deadlock"
	case jobqueue.IsRetryable(err):
		kind = "lock wait timeout"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n[kind]++
	s.n[kind+" in "+path]++
}

func (s *lockStats) get(kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n[kind]
}

func (s *lockStats) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.n) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(s.n))
	for k := range s.n {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%d", k, s.n[k])
	}
	return strings.Join(parts, ", ")
}

// countingStore records every error the foreman and the producers get back
// from the store, so the stress test can say whether deadlocks happened.
type countingStore struct {
	jobqueue.JobStorage
	stats *lockStats
}

func (s countingStore) CheckDuplicateJob(job jobqueue.Job) error {
	err := s.JobStorage.CheckDuplicateJob(job)
	s.stats.record("CheckDuplicateJob", err)
	return err
}

func (s countingStore) CreateJob(ctx context.Context, job jobqueue.Job) error {
	err := s.JobStorage.CreateJob(ctx, job)
	s.stats.record("CreateJob", err)
	return err
}

func (s countingStore) InjectJob(job jobqueue.Job) error {
	err := s.JobStorage.InjectJob(job)
	s.stats.record("InjectJob", err)
	return err
}

func (s countingStore) GetAndLockAvailableJob(jd map[string]jobqueue.JobDescription, ignorelist ...string) (*jobqueue.Job, error) {
	j, err := s.JobStorage.GetAndLockAvailableJob(jd, ignorelist...)
	s.stats.record("GetAndLockAvailableJob", err)
	return j, err
}

func (s countingStore) UpdateJobResult(job jobqueue.Job) error {
	err := s.JobStorage.UpdateJobResult(job)
	s.stats.record("UpdateJobResult", err)
	return err
}

// retryOnLockConflict runs op until it succeeds, retrying it (as the
// AddJobTransaction docs ask callers to) while it fails with IsRetryable.
func retryOnLockConflict(op func() error) error {
	for attempt := 1; ; attempt++ {
		err := op()
		if err == nil || !jobqueue.IsRetryable(err) || attempt == 20 {
			return err
		}
		time.Sleep(time.Duration(attempt) * 5 * time.Millisecond)
	}
}

// TestStress_ServeUnderConcurrentEnqueue: producers enqueue a burst of jobs -
// with AddJob, and with AddJobTransaction in batches inside their own
// transactions - while the foreman claims and runs them with concurrency 10.
// Serve must keep running through whatever deadlocks or lock wait timeouts
// that causes, and every job must run exactly once. The conflicts seen on the
// way are logged (go test -v). JOBQUEUE_STRESS_JOBS overrides the job count.
func TestStress_ServeUnderConcurrentEnqueue(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	total := 750
	if v := os.Getenv("JOBQUEUE_STRESS_JOBS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("bad JOBQUEUE_STRESS_JOBS %q", v)
		}
		total = n
	}
	const (
		workers   = 10
		producers = 10
		batchSize = 3 // jobs per caller transaction
	)

	for _, env := range testDialects(t) {
		env := env
		t.Run(env.name, func(t *testing.T) {
			stats := newLockStats()
			cfg := &jobqueue.QueueConfig{
				JobDescription: map[string]jobqueue.JobDescription{
					"stress": {Title: "stress", TTL: 300, Concurrent: workers, Priority: 1, MaxRetry: 0},
				},
				Concurrent: workers,
				BreakTime:  50,
				RampTime:   1,
			}
			f := jobqueue.NewForeman(cfg, countingStore{JobStorage: env.store, stats: stats}, newRecLogger(t))

			var runsMu sync.Mutex
			runs := make(map[string]int, total)
			if err := f.AddWorker("stress", func(ctx context.Context) error {
				job := ctx.Value(jobqueue.ContextJobKey).(*jobqueue.Job)
				runsMu.Lock()
				runs[job.JobID]++
				runsMu.Unlock()
				job.Status = jobqueue.JobStatus.DONE
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			rf := startForeman(t, f)

			ids := make(chan string, total)
			for i := 0; i < total; i++ {
				ids <- fmt.Sprintf("stress-%04d", i)
			}
			close(ids)

			start := time.Now()
			var wg sync.WaitGroup
			for p := 0; p < producers; p++ {
				wg.Add(1)
				go func(viaTransaction bool) {
					defer wg.Done()
					if !viaTransaction {
						for id := range ids {
							id := id
							if err := retryOnLockConflict(func() error {
								return f.AddJob(&jobqueue.Job{Title: "stress", JobID: id})
							}); err != nil {
								t.Errorf("AddJob %s: %v", id, err)
							}
						}
						return
					}
					for {
						var batch []string
						for id := range ids {
							if batch = append(batch, id); len(batch) == batchSize {
								break
							}
						}
						if len(batch) == 0 {
							return
						}
						if err := retryOnLockConflict(func() error {
							return addBatch(env, f, stats, batch)
						}); err != nil {
							t.Errorf("AddJobTransaction %v: %v", batch, err)
						}
					}
				}(p%2 == 1)
			}
			wg.Wait()
			enqueued := time.Since(start)

			var done int64
			waitFor(t, 5*time.Minute, "every job to finish", func() bool {
				if rf.returned() {
					return true
				}
				done = countJobs(env, "status = ?", jobqueue.JobStatus.DONE)
				return done == int64(total)
			})
			if rf.returned() {
				t.Fatalf("Serve returned with %d/%d jobs done: %v", done, total, rf.err)
			}
			elapsed := time.Since(start)
			if err := rf.stop(t); err != jobqueue.JobError.TERMINATING {
				t.Fatalf("Serve returned %v after Strike, want TERMINATING", err)
			}

			runsMu.Lock()
			defer runsMu.Unlock()
			for i := 0; i < total; i++ {
				id := fmt.Sprintf("stress-%04d", i)
				if runs[id] != 1 {
					t.Errorf("job %s ran %d times, want exactly once", id, runs[id])
				}
			}
			if len(runs) != total {
				t.Errorf("%d distinct jobs ran, want %d", len(runs), total)
			}

			t.Logf("%d jobs: enqueued in %s, all done in %s; deadlocks=%d, lock wait timeouts=%d; detail: %s",
				total, enqueued.Round(time.Millisecond), elapsed.Round(time.Millisecond),
				stats.get("deadlock"), stats.get("lock wait timeout"), stats)
		})
	}
}

// addBatch enqueues ids with AddJobTransaction inside one caller-owned
// transaction, like an app enqueueing jobs together with its own writes. On
// SQLite the test pool has a single connection, which that transaction would
// hold while AddJobTransaction's CheckDuplicateJob waits for another, so
// there each job goes through the plain handle instead.
func addBatch(env testEnv, f jobqueue.Foreman, stats *lockStats, ids []string) error {
	if env.name == "sqlite" {
		ctx := context.WithValue(context.Background(), "db", env.db)
		for _, id := range ids {
			if err := f.AddJobTransaction(ctx, &jobqueue.Job{Title: "stress", JobID: id}); err != nil {
				return err
			}
		}
		return nil
	}

	tx := env.db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	ctx := context.WithValue(context.Background(), "db", tx)
	for _, id := range ids {
		if err := f.AddJobTransaction(ctx, &jobqueue.Job{Title: "stress", JobID: id}); err != nil {
			tx.Rollback()
			return err
		}
	}
	err := tx.Commit().Error
	stats.record("caller Commit", err)
	return err
}

func newSchedule(title, jobID string, runat int64) jobqueue.ScheduleJob {
	return jobqueue.ScheduleJob{
		ID:        uuid.NewString(),
		JobID:     jobID,
		Title:     title,
		Priority:  1,
		Status:    jobqueue.JobStatus.INIT,
		UpdatedAt: time.Now().Unix(),
		Schedule:  runat,
	}
}

// TestCreateScheduleJob_DuplicateOnlyWhilePending pins down which schedules
// count as duplicates: a pending one does; an active job, or a schedule that
// already fired, doesn't.
func TestCreateScheduleJob_DuplicateOnlyWhilePending(t *testing.T) {
	for _, env := range testDialects(t) {
		env := env
		t.Run(env.name, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), "db", env.db)
			runat := time.Now().Add(time.Hour).Unix()

			if err := env.store.InjectJob(newJob("foo", "sched-key")); err != nil {
				t.Fatalf("InjectJob: %v", err)
			}
			first := newSchedule("foo", "sched-key", runat)
			if err := env.store.CreateScheduleJob(ctx, first); err != nil {
				t.Fatalf("expected an active job not to block scheduling, got %v", err)
			}
			if err := env.store.CreateScheduleJob(ctx, newSchedule("foo", "sched-key", runat)); err != jobqueue.JobError.DUPLICATE_JOB {
				t.Fatalf("expected DUPLICATE_JOB while a schedule is pending, got %v", err)
			}
			if err := env.store.CreateScheduleJob(ctx, newSchedule("foo", "other-key", runat)); err != nil {
				t.Fatalf("expected a different job_id to schedule fine, got %v", err)
			}

			first.Status = jobqueue.JobStatus.DONE
			if err := env.store.UpdateScheduledJob(first); err != nil {
				t.Fatalf("UpdateScheduledJob: %v", err)
			}
			if err := env.store.CreateScheduleJob(ctx, newSchedule("foo", "sched-key", runat)); err != nil {
				t.Fatalf("expected scheduling to succeed once the pending schedule fired, got %v", err)
			}
		})
	}
}

// TestCreateScheduleJob_ConcurrentDuplicatesRejected is the schedule version
// of the CreateJob/InjectJob dedup race tests: many callers scheduling the
// same (title, job_id) at once must leave exactly one pending schedule.
func TestCreateScheduleJob_ConcurrentDuplicatesRejected(t *testing.T) {
	for _, env := range testDialects(t) {
		env := env
		t.Run(env.name, func(t *testing.T) {
			const n = 30
			var successes, duplicates int64
			var wg sync.WaitGroup
			runat := time.Now().Add(time.Hour).Unix()
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					ctx := context.WithValue(context.Background(), "db", env.db)
					switch err := env.store.CreateScheduleJob(ctx, newSchedule("foo", "dup-sched", runat)); err {
					case nil:
						atomic.AddInt64(&successes, 1)
					case jobqueue.JobError.DUPLICATE_JOB:
						atomic.AddInt64(&duplicates, 1)
					default:
						t.Errorf("unexpected error: %v", err)
					}
				}()
			}
			wg.Wait()

			if successes != 1 || successes+duplicates != n {
				t.Fatalf("expected 1 success and %d duplicates, got successes=%d duplicates=%d", n-1, successes, duplicates)
			}
			var count int64
			if err := env.db.Table("schedule_jobs").Where("job_id = ?", "dup-sched").Count(&count).Error; err != nil {
				t.Fatalf("count rows: %v", err)
			}
			if count != 1 {
				t.Fatalf("expected exactly 1 row in schedule_jobs, got %d", count)
			}
		})
	}
}

// TestAddJobWithSchedule_RecurringJobReschedulesItself: a handler that
// schedules its own next run (same title + job_id) while it's processing must
// succeed, a second call for the pair while that schedule is pending must get
// DUPLICATE_JOB, and the chain must keep going from run to run.
func TestAddJobWithSchedule_RecurringJobReschedulesItself(t *testing.T) {
	for _, env := range testDialects(t) {
		env := env
		t.Run(env.name, func(t *testing.T) {
			const wantRuns = 3
			cfg := &jobqueue.QueueConfig{
				JobDescription: map[string]jobqueue.JobDescription{
					"recurring": {Title: "recurring", TTL: 30, Concurrent: 1, Priority: 1, MaxRetry: 0},
				},
				Concurrent:       2,
				BreakTime:        50,
				RampTime:         10,
				ScheduleInterval: 100,
			}
			f := jobqueue.NewForeman(cfg, env.store, newRecLogger(t))
			f.AddMiddleware(gormmidl.GormTransactionMiddleware("db", env.db))

			var mu sync.Mutex
			var runs int
			var results [][2]error // per run: first and second AddJobWithSchedule
			if err := f.AddWorker("recurring", func(ctx context.Context) error {
				job := ctx.Value(jobqueue.ContextJobKey).(*jobqueue.Job)
				mu.Lock()
				runs++
				n := runs
				mu.Unlock()
				if n < wantRuns {
					// Due right away, so it also comes due while this run is
					// still processing.
					next := func() error {
						return f.AddJobWithSchedule(ctx, &jobqueue.Job{Title: job.Title, JobID: job.JobID}, time.Now().Unix())
					}
					first := next()
					second := next()
					mu.Lock()
					results = append(results, [2]error{first, second})
					mu.Unlock()
				}
				job.Status = jobqueue.JobStatus.DONE
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			startForeman(t, f)

			if err := f.AddJob(&jobqueue.Job{Title: "recurring", JobID: "nightly-sync"}); err != nil {
				t.Fatalf("AddJob: %v", err)
			}

			check := func() (stop bool) {
				mu.Lock()
				defer mu.Unlock()
				for i, r := range results {
					if r[0] != nil {
						t.Fatalf("run %d: scheduling its own next run failed: %v", i+1, r[0])
					}
					if r[1] != jobqueue.JobError.DUPLICATE_JOB {
						t.Fatalf("run %d: second schedule while one is pending returned %v, want DUPLICATE_JOB", i+1, r[1])
					}
				}
				return false
			}
			waitFor(t, 30*time.Second, fmt.Sprintf("%d runs of the recurring job", wantRuns), func() bool {
				check()
				return countJobs(env, "job_id = ? AND status = ?", "nightly-sync", jobqueue.JobStatus.DONE) == wantRuns
			})
			check()

			var scheds []jobqueue.ScheduleJob
			if err := env.db.Table("schedule_jobs").Where("job_id = ?", "nightly-sync").Find(&scheds).Error; err != nil {
				t.Fatalf("read schedules: %v", err)
			}
			if len(scheds) != wantRuns-1 {
				t.Fatalf("expected %d schedule rows, got %d", wantRuns-1, len(scheds))
			}
			for _, s := range scheds {
				if s.Status != jobqueue.JobStatus.DONE || s.ExecuteID == "" {
					t.Errorf("schedule %s: status=%q execute_id=%q log=%q, want done with an execute_id", s.ID, s.Status, s.ExecuteID, s.Log)
				}
			}
		})
	}
}

// TestScheduledJob_FiresAfterActiveRunFinishes: a schedule that comes due
// while its job id is still processing must stay pending and fire once that
// run is over.
func TestScheduledJob_FiresAfterActiveRunFinishes(t *testing.T) {
	for _, env := range testDialects(t) {
		env := env
		t.Run(env.name, func(t *testing.T) {
			cfg := &jobqueue.QueueConfig{
				JobDescription: map[string]jobqueue.JobDescription{
					"fire": {Title: "fire", TTL: 30, Concurrent: 1, Priority: 1, MaxRetry: 0},
				},
				Concurrent:       2,
				BreakTime:        50,
				RampTime:         10,
				ScheduleInterval: 100,
			}
			f := jobqueue.NewForeman(cfg, env.store, newRecLogger(t))

			release := make(chan struct{})
			started := make(chan string, 4)
			var runs int32
			if err := f.AddWorker("fire", func(ctx context.Context) error {
				job := ctx.Value(jobqueue.ContextJobKey).(*jobqueue.Job)
				started <- job.ID
				if atomic.AddInt32(&runs, 1) == 1 {
					<-release
				}
				job.Status = jobqueue.JobStatus.DONE
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			startForeman(t, f)
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()

			if err := f.AddJob(&jobqueue.Job{Title: "fire", JobID: "sync-42"}); err != nil {
				t.Fatalf("AddJob: %v", err)
			}
			select {
			case <-started:
			case <-time.After(10 * time.Second):
				t.Fatal("first run never started")
			}

			// Due now, while the first run is still processing. Created
			// through the store so the setup doesn't depend on
			// AddJobWithSchedule's own duplicate rules.
			sched := newSchedule("fire", "sync-42", time.Now().Unix())
			if err := env.store.CreateScheduleJob(context.WithValue(context.Background(), "db", env.db), sched); err != nil {
				t.Fatalf("CreateScheduleJob: %v", err)
			}

			readSched := func() jobqueue.ScheduleJob {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				var s jobqueue.ScheduleJob
				if err := env.db.WithContext(ctx).Table("schedule_jobs").Where("id = ?", sched.ID).First(&s).Error; err != nil {
					t.Fatalf("read schedule: %v", err)
				}
				return s
			}
			waitFor(t, 10*time.Second, "the scheduler to try firing the schedule", func() bool {
				s := readSched()
				return s.Log != "" || s.Status == jobqueue.JobStatus.ERROR || s.Status == jobqueue.JobStatus.DONE
			})
			if s := readSched(); s.Status != jobqueue.JobStatus.INIT && s.Status != jobqueue.JobStatus.PROCESSING {
				t.Fatalf("schedule fired while its job was running ended up %q (log %q), want it left pending", s.Status, s.Log)
			}
			select {
			case id := <-started:
				t.Fatalf("second run %s started while the first was still processing", id)
			default:
			}

			close(release)
			var secondID string
			select {
			case secondID = <-started:
			case <-time.After(10 * time.Second):
				t.Fatalf("the deferred schedule never fired after the first run finished (schedule: %+v)", readSched())
			}
			waitFor(t, 10*time.Second, "the schedule to be marked done", func() bool {
				return readSched().Status == jobqueue.JobStatus.DONE
			})
			if s := readSched(); s.ExecuteID != secondID {
				t.Fatalf("schedule execute_id=%q, want the second run's job id %q", s.ExecuteID, secondID)
			}
			waitFor(t, 10*time.Second, "both runs to be saved as done", func() bool {
				return countJobs(env, "job_id = ? AND status = ?", "sync-42", jobqueue.JobStatus.DONE) == 2
			})
		})
	}
}
