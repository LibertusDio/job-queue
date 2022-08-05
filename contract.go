package jobqueue

import "context"

type JobStorage interface {
	CreateJob(ctx context.Context, job *Job) error
	GetAndLockAvailableJob(jd map[string]JobDescription) (*Job, error)
	// GetJobByID(ctx context.Context) (*Job, error)
	// UpdateJob(ctx context.Context, job *Job) error
	UpdateJobResult(job *Job) error
}

type MiddlewareFunc func(HandlerFunc) HandlerFunc
type HandlerFunc func(ctx context.Context) error

type Foreman interface {
	//AddWorker add a worker function for handler
	AddWorker(title string, worker HandlerFunc, midl ...MiddlewareFunc) error

	//AddJob add a new job to the queue
	AddJob(ctx context.Context, job *Job) error

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

type governor interface {
	AddJob()
	DelJob()
	NoJob()
	Spawn() bool
	GetCounter() int
}
