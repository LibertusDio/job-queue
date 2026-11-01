package jobqueue

import "context"

type JobStorage interface {
	CheckDuplicateJob(job Job) error
	CreateJob(ctx context.Context, job Job) error
	GetAndLockAvailableJob(jd map[string]JobDescription, ignorelist ...string) (*Job, error)
	UpdateJobResult(job Job) error

	InjectJob(Job) error
	// CreateScheduleJob stores a schedule, returning JobError.DUPLICATE_JOB if
	// one with the same title + job_id is already pending (status init).
	// Active jobs with that job_id must not block it.
	CreateScheduleJob(ctx context.Context, job ScheduleJob) error
	GetScheduledJob(from, to int64) ([]*ScheduleJob, error)
	UpdateScheduledJob(ScheduleJob) error
}

type MiddlewareFunc func(HandlerFunc) HandlerFunc
type HandlerFunc func(ctx context.Context) error

type Foreman interface {
	//AddWorker add a worker function for handler
	AddWorker(title string, worker HandlerFunc, midl ...MiddlewareFunc) error

	//AddJobTransaction add a new job to the queue. ctx is handed to JobStorage.CreateJob; with
	//store/gorm it carries the caller's *gorm.DB, and when that is an open transaction the job
	//is created inside it, so it only exists if that transaction commits.
	//
	//If it fails inside the caller's transaction with anything but a JobError (such as
	//DUPLICATE_JOB, which leaves the transaction intact), treat the whole transaction as
	//failed: return the error, roll back, and retry the transaction from the start if
	//IsRetryable(err). Do not log it and carry on. On MySQL/MariaDB a deadlock has already
	//rolled back everything the transaction wrote before this call, yet the transaction stays
	//usable: later statements and the final Commit still succeed, persisting only the writes
	//made after the failure. On Postgres any error aborts the transaction.
	AddJobTransaction(ctx context.Context, job *Job) error

	//AddJob add a new job to the queue without context, dont recommend but handy for multiple context
	AddJob(job *Job) error

	//AddJobWithSchedule add a new job to the queue to run at a schedule (unix seconds).
	//It returns JobError.DUPLICATE_JOB if a schedule for the same title + job_id is still
	//pending. An active job with that job_id doesn't block it, so a recurring job can
	//schedule its own next run from its handler. If the schedule comes due while that
	//job_id is still active, it stays pending and fires on a later scheduler pass.
	AddJobWithSchedule(ctx context.Context, job *Job, runat int64) error

	//Serve start the worker service. It blocks until Strike is called and then returns
	//JobError.TERMINATING; store errors while fetching jobs are logged and retried with
	//a backoff, never returned.
	Serve() error

	//Strike graceful shutdown service. Reject new jobs and wait for running one to finish. Force kill at TTL.
	Strike(ttl int) error // ttl in seconds

	//AddMiddleware add global middleware, order of adding matters
	AddMiddleware(midl ...MiddlewareFunc)

	//GetCounter return worker counter for debug purpose
	GetCounter() int
}

type Logger interface {
	Debug(msg string)
	Info(msg string)
	Warn(msg string)
	Error(msg string)
	Fatal(msg string)
	Panic(msg string)
}

type Governor interface {
	AddJob(string)
	DelJob(string)
	NoJob()
	Spawn() (bool, []string)
	GetCounter() int
}
