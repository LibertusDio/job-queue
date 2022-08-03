package jobqueue

import "context"

type JobStorage interface {
	CreateJob(ctx context.Context, job *Job) error
	GetAndLockAvailableJob(jd map[string]JobDescription) (*Job, error)
	GetJobByID(ctx context.Context) (*Job, error)
	UpdateJob(ctx context.Context, job *Job) error
	UpdateJobResult(job *Job) error
}

type MiddlewareFunc func(HandlerFunc) HandlerFunc
type HandlerFunc func(ctx context.Context) error

type Foreman interface {
	AddWorker(title string, worker HandlerFunc, midl ...MiddlewareFunc) error
	AddJob(ctx context.Context, job *Job) error
	Serve() error
	Strike(ttl int) error // ttl in seconds
	AddMiddleware(midl ...MiddlewareFunc)
}

type Logger interface {
	Debug(msg string)
	Info(msg string)
	Warn(msg string)
	Error(msg string)
	Fatal(msg string)
	Panic(msg string)
}
