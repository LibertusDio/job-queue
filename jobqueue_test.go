package jobqueue_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	jobqueue "github.com/LibertusDio/job-queue"
)

// MySQLError and pgError have the shape IsRetryable looks for in
// go-sql-driver/mysql's *MySQLError and pgx's *pgconn.PgError. The store/gorm
// tests check it against the real drivers' errors.
type MySQLError struct {
	Number  uint16
	Message string
}

func (e *MySQLError) Error() string { return fmt.Sprintf("Error %d: %s", e.Number, e.Message) }

type pgError struct{ code string }

func (e *pgError) Error() string    { return "SQLSTATE " + e.code }
func (e *pgError) SQLState() string { return e.code }

type nopLogger struct{}

func (nopLogger) Debug(string) {}
func (nopLogger) Info(string)  {}
func (nopLogger) Warn(string)  {}
func (nopLogger) Error(string) {}
func (nopLogger) Fatal(string) {}
func (nopLogger) Panic(string) {}

type fetchResult struct {
	job *jobqueue.Job
	err error
}

// fakeStore hands out a scripted sequence of GetAndLockAvailableJob results
// (then NOT_FOUND forever, or failForever once the script runs out) and
// records what the foreman does with them.
type fakeStore struct {
	mu            sync.Mutex
	script        []fetchResult
	failForever   error
	fetches       int
	updated       []jobqueue.Job
	scheduleCalls int
}

func (s *fakeStore) GetAndLockAvailableJob(map[string]jobqueue.JobDescription, ...string) (*jobqueue.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetches++
	if len(s.script) == 0 {
		if s.failForever != nil {
			return nil, s.failForever
		}
		return nil, jobqueue.JobError.NOT_FOUND
	}
	r := s.script[0]
	s.script = s.script[1:]
	return r.job, r.err
}

func (s *fakeStore) UpdateJobResult(job jobqueue.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updated = append(s.updated, job)
	return nil
}

func (s *fakeStore) GetScheduledJob(int64, int64) ([]*jobqueue.ScheduleJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scheduleCalls++
	return nil, jobqueue.JobError.NOT_FOUND
}

func (s *fakeStore) CheckDuplicateJob(jobqueue.Job) error                          { return nil }
func (s *fakeStore) CreateJob(context.Context, jobqueue.Job) error                 { return nil }
func (s *fakeStore) InjectJob(jobqueue.Job) error                                  { return nil }
func (s *fakeStore) CreateScheduleJob(context.Context, jobqueue.ScheduleJob) error { return nil }
func (s *fakeStore) UpdateScheduledJob(jobqueue.ScheduleJob) error                 { return nil }

func (s *fakeStore) counts() (fetches, scheduleCalls int, updated []jobqueue.Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetches, s.scheduleCalls, append([]jobqueue.Job(nil), s.updated...)
}

func testConfig() *jobqueue.QueueConfig {
	return &jobqueue.QueueConfig{
		JobDescription: map[string]jobqueue.JobDescription{
			"foo": {Title: "foo", TTL: 10, Concurrent: 1, Priority: 1, MaxRetry: 1},
		},
		Concurrent: 1,
		BreakTime:  10,
		RampTime:   1,
	}
}

func serve(f jobqueue.Foreman) <-chan error {
	errc := make(chan error, 1)
	go func() { errc <- f.Serve() }()
	return errc
}

func strikeAndWait(t *testing.T, f jobqueue.Foreman, errc <-chan error) {
	t.Helper()
	_ = f.Strike(0)
	select {
	case err := <-errc:
		if err != jobqueue.JobError.TERMINATING {
			t.Fatalf("Serve returned %v after Strike, want TERMINATING", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s of Strike")
	}
}

// TestServe_KeepsServingAfterFetchErrors checks Serve keeps running through a
// deadlock and a generic error while fetching, then runs the next job.
func TestServe_KeepsServingAfterFetchErrors(t *testing.T) {
	store := &fakeStore{script: []fetchResult{
		{err: &MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock; try restarting transaction"}},
		{err: errors.New("invalid connection")},
		{job: &jobqueue.Job{ID: "id-1", JobID: "1", Title: "foo", Status: jobqueue.JobStatus.PROCESSING}},
	}}
	f := jobqueue.NewForeman(testConfig(), store, nopLogger{})
	ran := make(chan string, 1)
	if err := f.AddWorker("foo", func(ctx context.Context) error {
		job := ctx.Value(jobqueue.ContextJobKey).(*jobqueue.Job)
		job.Status = jobqueue.JobStatus.DONE
		ran <- job.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	errc := serve(f)
	select {
	case id := <-ran:
		if id != "id-1" {
			t.Fatalf("ran job %q, want id-1", id)
		}
	case err := <-errc:
		t.Fatalf("Serve returned %v instead of retrying the fetch", err)
	case <-time.After(5 * time.Second):
		t.Fatal("job never ran")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, _, updated := store.counts(); len(updated) == 1 {
			if updated[0].Status != jobqueue.JobStatus.DONE {
				t.Fatalf("job saved with status %q, want done", updated[0].Status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job result never saved")
		}
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case err := <-errc:
		t.Fatalf("Serve returned %v while it should still be serving", err)
	default:
	}
	strikeAndWait(t, f, errc)
}

// TestServe_BacksOffOnPersistentFetchErrors checks a store that keeps failing
// (say, the database is down) isn't hammered in a tight loop.
func TestServe_BacksOffOnPersistentFetchErrors(t *testing.T) {
	store := &fakeStore{failForever: errors.New("dial tcp: connection refused")}
	cfg := testConfig()
	cfg.BreakTime, cfg.RampTime = 1, 1
	f := jobqueue.NewForeman(cfg, store, nopLogger{})

	errc := serve(f)
	time.Sleep(500 * time.Millisecond)
	select {
	case err := <-errc:
		t.Fatalf("Serve returned %v on a store error", err)
	default:
	}
	strikeAndWait(t, f, errc)

	// The backoff runs 50+100+200ms in that window, so about 4 attempts;
	// without one, the 1ms governor sleep allows hundreds.
	if fetches, _, _ := store.counts(); fetches > 10 {
		t.Fatalf("%d fetch attempts in 500ms against a failing store, want a backoff", fetches)
	}
}

// TestServe_SchedulerStopsWithServe checks the scheduler goroutine doesn't
// outlive Serve, which would mean a second one runs if Serve is called again.
func TestServe_SchedulerStopsWithServe(t *testing.T) {
	store := &fakeStore{}
	cfg := testConfig()
	cfg.ScheduleInterval = 10
	f := jobqueue.NewForeman(cfg, store, nopLogger{})

	errc := serve(f)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, calls, _ := store.counts(); calls >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scheduler never ran")
		}
		time.Sleep(5 * time.Millisecond)
	}
	strikeAndWait(t, f, errc)

	_, before, _ := store.counts()
	time.Sleep(100 * time.Millisecond)
	if _, after, _ := store.counts(); after != before {
		t.Fatalf("scheduler polled %d more times after Serve returned", after-before)
	}
}

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("invalid connection"), false},
		{jobqueue.JobError.NOT_FOUND, false},
		{jobqueue.JobError.DUPLICATE_JOB, false},
		{jobqueue.StoreError.LOCK_TIMEOUT, true},
		{&MySQLError{Number: 1213, Message: "Deadlock found"}, true},
		{&MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"}, true},
		{&MySQLError{Number: 1062, Message: "Duplicate entry"}, false},
		{fmt.Errorf("claim: %w", &MySQLError{Number: 1213}), true},
		{&pgError{code: "40P01"}, true},
		{&pgError{code: "55P03"}, true},
		{&pgError{code: "23505"}, false},
		{fmt.Errorf("claim: %w", &pgError{code: "40P01"}), true},
	}
	for _, c := range cases {
		if got := jobqueue.IsRetryable(c.err); got != c.want {
			t.Errorf("IsRetryable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
