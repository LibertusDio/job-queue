package jobqueue

import "context"

type JobStorage interface {
	CheckDuplicateJob(ctx context.Context, job Job) error
	CreateJob(ctx context.Context, job Job) error
	GetAndLockAvailableJob(jd map[string]JobDescription, ignorelist ...string) (*Job, error)
	UpdateJobResult(job Job) error

	InjectJob(Job) error
	CreateScheduleJob(ctx context.Context, job ScheduleJob) error
	GetScheduledJob(from, to int64) ([]*ScheduleJob, error)
	UpdateScheduledJob(ScheduleJob) error
}

type MiddlewareFunc func(HandlerFunc) HandlerFunc
type HandlerFunc func(ctx context.Context) error

type Foreman interface {
	//AddWorker add a worker function for handler
	AddWorker(title string, worker HandlerFunc, midl ...MiddlewareFunc) error

	//AddJob add a new job to the queue
	AddJob(ctx context.Context, job *Job) error

	//AddJobWithSchedule add a new job to the queue to run at a schedule
	AddJobWithSchedule(ctx context.Context, job *Job, runat int64) error

	//Serve start the worker service
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
