package jobqueue

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	union "github.com/LibertusDio/union-go"
	"github.com/google/uuid"
)

// addJobLockShards sizes the in-process striped lock used to serialize
// AddJob/AddJobTransaction/AddJobWithSchedule calls for the same (title,
// job_id) pair within a single Foreman instance. It's a fixed size (rather
// than a map keyed by title+job_id) so it needs no cleanup/eviction; a
// collision between two different keys just means they occasionally
// serialize against each other unnecessarily, which is safe.
const addJobLockShards = 256

const (
	// storeRetries bounds the extra attempts made when a store call fails with
	// a deadlock or lock wait timeout (see IsRetryable). Those are expected
	// while jobs are enqueued concurrently and normally clear on the next try,
	// so they're retried right away, with a short jittered backoff starting at
	// storeRetryBackoff, rather than treated as a store failure.
	storeRetries      = 3
	storeRetryBackoff = 10 * time.Millisecond

	// errorBackoffMin/Max bound Serve's pause after any other fetch error
	// (lost connection, failover...). It doubles on each consecutive failure
	// and resets as soon as the store answers again.
	errorBackoffMin = 50 * time.Millisecond
	errorBackoffMax = 5 * time.Second

	defaultScheduleInterval = 59 * time.Second
)

type position struct {
	Func HandlerFunc
	Midl []MiddlewareFunc
}
type foreman struct {
	productionLine map[string]position
	cfg            *QueueConfig
	store          JobStorage
	workerSignal   map[string]chan bool
	working        atomic.Bool
	stop           chan struct{} // closed by Strike, wakes up pauses
	stopOnce       sync.Once
	signalLock     *sync.Mutex
	logger         Logger
	middleware     []MiddlewareFunc
	jobGovernor    Governor
	union          union.Union
	addJobLocks    [addJobLockShards]sync.Mutex
}

func NewForeman(config *QueueConfig, store JobStorage, logger Logger) Foreman {
	pl := make(map[string]position)
	f := &foreman{
		productionLine: pl,
		cfg:            config,
		store:          store,
		logger:         logger,
		stop:           make(chan struct{}),
		signalLock:     new(sync.Mutex),
		middleware:     []MiddlewareFunc{},
		workerSignal:   make(map[string]chan bool),
		jobGovernor:    NewLocalOndemandGovernor(config.BreakTime, config.RampTime, config.Concurrent, config.JobDescription),
		union:          union.NewGoroutineUnion(),
	}
	f.working.Store(true)
	return f
}

func (f *foreman) AddMiddleware(midl ...MiddlewareFunc) {
	f.middleware = append(f.middleware, midl...)
}

func (f *foreman) AddWorker(title string, workerfunc HandlerFunc, midl ...MiddlewareFunc) error {
	_, ok := f.cfg.JobDescription[title]
	if !ok {
		return JobError.INVALID_JD
	}

	f.productionLine[title] = position{Func: workerfunc, Midl: midl}
	return nil
}

// addJobLock returns the mutex serializing same-instance AddJob*/
// AddJobTransaction/AddJobWithSchedule calls for a given (title, job_id)
// pair. This is a same-process fast path only: it lets a duplicate
// submission on this instance fail without a DB round-trip, and reduces how
// many callers pile up simultaneously waiting on the store's cross-instance
// lock. It is not a substitute for that store-level lock (see
// store/gorm CheckDuplicateJob/CreateJob/InjectJob) - callers on other
// Foreman instances are unaffected by this mutex.
func (f *foreman) addJobLock(title, jobID string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(title + ":" + jobID))
	return &f.addJobLocks[h.Sum32()%addJobLockShards]
}
func (f *foreman) AddJobTransaction(ctx context.Context, job *Job) error {
	jd, ok := f.cfg.JobDescription[job.Title]
	if !ok {
		return JobError.INVALID_JOB
	}

	mu := f.addJobLock(job.Title, job.JobID)
	mu.Lock()
	defer mu.Unlock()

	if job.ID == "" {
		job.ID = uuid.NewString()
	}

	if job.Priority < 1 {
		job.Priority = jd.Priority
	}
	job.Status = JobStatus.INIT
	job.Try = 0
	job.UpdatedAt = time.Now().Unix()
	err := f.store.CheckDuplicateJob(*job)
	if err != nil {
		return err
	}

	err = f.store.CreateJob(ctx, *job)
	if err != nil {
		return err
	}
	return nil
}
func (f *foreman) AddJob(job *Job) error {
	jd, ok := f.cfg.JobDescription[job.Title]
	if !ok {
		return JobError.INVALID_JOB
	}

	mu := f.addJobLock(job.Title, job.JobID)
	mu.Lock()
	defer mu.Unlock()

	if job.ID == "" {
		job.ID = uuid.NewString()
	}

	if job.Priority < 1 {
		job.Priority = jd.Priority
	}
	job.Status = JobStatus.INIT
	job.Try = 0
	job.UpdatedAt = time.Now().Unix()
	err := f.store.CheckDuplicateJob(*job)
	if err != nil {
		return err
	}

	err = f.store.InjectJob(*job)
	if err != nil {
		return err
	}
	return nil
}

func (f *foreman) AddJobWithSchedule(ctx context.Context, job *Job, runat int64) error {
	jd, ok := f.cfg.JobDescription[job.Title]
	if !ok {
		return JobError.INVALID_JOB
	}

	mu := f.addJobLock(job.Title, job.JobID)
	mu.Lock()
	defer mu.Unlock()

	if job.ID == "" {
		job.ID = uuid.NewString()
	}

	if job.Priority < 1 {
		job.Priority = jd.Priority
	}
	job.Status = JobStatus.INIT
	job.Try = 0
	job.UpdatedAt = time.Now().Unix()

	// No CheckDuplicateJob here: an active job with the same job_id doesn't
	// block scheduling, so a recurring job can schedule its own next run from
	// its handler. CreateScheduleJob rejects a second pending schedule instead.
	sj := ScheduleJob{
		ID:        job.ID,
		JobID:     job.JobID,
		Title:     job.Title,
		Payload:   job.Payload,
		Priority:  job.Priority,
		Status:    job.Status,
		UpdatedAt: job.UpdatedAt,
		Schedule:  runat,
		ExecuteID: "",
	}

	return f.store.CreateScheduleJob(ctx, sj)
}

// pause sleeps for d, returning early once Strike is called.
func (f *foreman) pause(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-f.stop:
	}
}

// retryStore runs op, running it again up to storeRetries times while it fails
// with a deadlock or lock wait timeout. Any other result is returned as is.
func (f *foreman) retryStore(op func() error) error {
	for attempt := 0; ; attempt++ {
		err := op()
		if err == nil || attempt == storeRetries || !IsRetryable(err) {
			return err
		}
		f.logger.Warn("store lock conflict, retrying: " + err.Error())
		d := storeRetryBackoff << attempt
		f.pause(d + time.Duration(rand.Int63n(int64(d))))
	}
}

func nextErrorBackoff(d time.Duration) time.Duration {
	if d < errorBackoffMin {
		return errorBackoffMin
	}
	if d *= 2; d > errorBackoffMax {
		return errorBackoffMax
	}
	return d
}

// runScheduler fires due scheduled jobs every ScheduleInterval until done is
// closed.
func (f *foreman) runScheduler(done <-chan struct{}) {
	interval := defaultScheduleInterval
	if f.cfg.ScheduleInterval > 0 {
		interval = time.Duration(f.cfg.ScheduleInterval) * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if !f.working.Load() {
				return
			}
			f.fireScheduledJobs()
		}
	}
}

func (f *foreman) fireScheduledJobs() {
	jobs, err := f.store.GetScheduledJob(0, time.Now().Unix())
	if err == JobError.NOT_FOUND {
		f.logger.Info("empty schedule")
		return
	}
	if err != nil {
		f.logger.Error("fetching schedule error: " + err.Error())
		return
	}
	for _, job := range jobs {
		id := uuid.NewString()
		now := time.Now().Unix()
		j := Job{
			ID:        id,
			JobID:     job.JobID,
			Title:     job.Title,
			Payload:   job.Payload,
			Priority:  job.Priority,
			Status:    JobStatus.INIT,
			UpdatedAt: now,
		}
		err := f.retryStore(func() error { return f.store.InjectJob(j) })
		switch {
		case err == nil:
			job.Status = JobStatus.DONE
			job.ExecuteID = id
		case errors.Is(err, JobError.DUPLICATE_JOB), IsRetryable(err):
			// The job id is still active (typically a recurring job's previous
			// run hasn't finished), or the insert kept losing lock conflicts.
			// Leave the schedule pending for a later pass instead of dropping
			// this run; the store's TTL reclaim of stuck processing jobs keeps
			// it from waiting forever.
			job.Status = JobStatus.INIT
			job.Log = "deferred: " + err.Error()
		default:
			job.Status = JobStatus.ERROR
			job.Log = err.Error()
		}
		job.UpdatedAt = now
		err = f.retryStore(func() error { return f.store.UpdateScheduledJob(*job) })
		if err != nil {
			f.logger.Error("update schdule job error: " + err.Error())
		}
	}
}

func (f *foreman) Serve() error {
	// The scheduler lives exactly as long as this call: Serve doesn't return
	// until it has stopped, so calling Serve again never leaves two running.
	done := make(chan struct{})
	var scheduler sync.WaitGroup
	scheduler.Add(1)
	go func() {
		defer scheduler.Done()
		f.runScheduler(done)
	}()
	defer func() {
		close(done)
		scheduler.Wait()
	}()

	var backoff time.Duration
	for f.working.Load() {
		spawn, bl := f.jobGovernor.Spawn()
		if !spawn {
			continue
		}
		// get a job
		var j *Job
		err := f.retryStore(func() error {
			var err error
			j, err = f.store.GetAndLockAvailableJob(f.cfg.JobDescription, bl...)
			return err
		})
		if err != nil && err != JobError.NOT_FOUND {
			// Never fatal: the store can recover (reconnect, failover), and
			// returning would stop job processing for good.
			backoff = nextErrorBackoff(backoff)
			f.logger.Error("fetch job error:" + err.Error() + " (retrying in " + backoff.String() + ")")
			f.pause(backoff)
			continue
		}
		backoff = 0
		strikeChannel := make(chan bool, 1)

		if err == JobError.NOT_FOUND {
			f.logger.Info("no job")
			f.jobGovernor.NoJob()
			continue
		}
		f.jobGovernor.AddJob(j.Title)
		workerdata := union.WorkerData{
			InputData: j,
		}

		f.union.Execute(func(data union.WorkerData) {
			job := data.InputData.(*Job)
			f.signalLock.Lock()
			f.workerSignal[j.ID] = strikeChannel
			f.signalLock.Unlock()

			// get job support
			worker, ok := f.productionLine[job.Title]
			if !ok {
				job.Status = JobStatus.ERROR
				job.Message = JobError.INVALID_PL.Error()
				err = f.store.UpdateJobResult(*job)
				if err != nil {
					f.logger.Error("update job error:" + err.Error())
				}
				f.jobGovernor.DelJob(job.Title)
				f.signalLock.Lock()
				delete(f.workerSignal, job.ID)
				f.signalLock.Unlock()
				return
			}

			jd, ok := f.cfg.JobDescription[job.Title]
			if !ok {
				job.Status = JobStatus.ERROR
				job.Message = JobError.INVALID_JD.Error()
				err = f.store.UpdateJobResult(*job)
				if err != nil {
					f.logger.Error("update job error:" + err.Error())
				}
				f.jobGovernor.DelJob(job.Title)
				f.signalLock.Lock()
				delete(f.workerSignal, job.ID)
				f.signalLock.Unlock()
				return
			}

			// run job
			ctx, ctxCancel := context.WithTimeout(context.Background(), time.Duration(jd.TTL+2)*time.Second)

			// start worker
			go func(ctx context.Context, worker position) {
				defer ctxCancel()
				defer func() {
					if r := recover(); r != nil {
						job.Status = JobStatus.ERROR
						job.Result = fmt.Sprintf("panic: %v", r)
						f.logger.Error("panic in job: " + job.ID + ": " + fmt.Sprintf("%v", r))
					}
				}()
				f.logger.Debug("Start job: " + job.ID)
				ctx = context.WithValue(ctx, ContextJobKey, job)
				if err := f.applyMiddlewares(f.applyMiddlewares(worker.Func, worker.Midl...), f.middleware...)(ctx); err != nil {
					job.Result = err.Error()
					job.Status = JobStatus.ERROR
				}
			}(ctx, worker)
			//wait for job to complete or strike
			select {
			case <-strikeChannel:
				f.logger.Error("Strike job: " + job.ID)
				ctxCancel()
				f.jobGovernor.DelJob(job.Title)
				return
			case <-ctx.Done():
				// close(c)
				f.signalLock.Lock()
				delete(f.workerSignal, job.ID)
				f.signalLock.Unlock()
				if ctx.Err() == context.DeadlineExceeded {
					// timeout
					f.logger.Warn("Terminate job: " + job.ID)
					if job.Try >= jd.MaxRetry {
						job.Status = JobStatus.MAX_RETRY
					} else {
						job.Status = JobStatus.RETRY
					}
					job.Message = "job terminated at: " + time.Now().String()
				}
				if ctx.Err() == context.Canceled {
					// job done
					f.logger.Debug("Complete job: " + fmt.Sprintf("%v", job))
					if job.Status == JobStatus.RETRY {
						if job.Try >= jd.MaxRetry {
							job.Status = JobStatus.MAX_RETRY
						} else {
							job.Status = JobStatus.RETRY
						}
					}

				}

			}
			// update job status
			job.Try += 1
			job.Priority *= 2
			if err := f.retryStore(func() error { return f.store.UpdateJobResult(*job) }); err != nil {
				f.logger.Error("Update job: " + fmt.Sprintf("%v", job))
			}
			f.jobGovernor.DelJob(job.Title)
		}, workerdata)
	}
	return JobError.TERMINATING
}

func (f *foreman) Strike(ttl int) error {
	f.logger.Info("Stopping Foreman")
	f.working.Store(false)
	f.stopOnce.Do(func() { close(f.stop) })
	f.logger.Info("Waiting for graceful shutdown")
	time.Sleep(time.Duration(ttl) * time.Second)
	f.logger.Warn("Striking remaining job")
	f.signalLock.Lock()
	signals := f.workerSignal
	f.workerSignal = make(map[string]chan bool)
	f.signalLock.Unlock()
	for _, s := range signals {
		s <- true
	}
	return nil
}

func (f *foreman) applyMiddlewares(h HandlerFunc, m ...MiddlewareFunc) HandlerFunc {
	for i := len(m) - 1; i >= 0; i-- {
		h = m[i](h)
	}
	return h
}

func (f *foreman) GetCounter() int {
	return f.jobGovernor.GetCounter()
}
