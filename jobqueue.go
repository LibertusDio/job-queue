package jobqueue

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

type position struct {
	Func HandlerFunc
	Midl []MiddlewareFunc
}
type foreman struct {
	ProductionLine map[string]position
	Cfg            *QueueConfig
	Store          JobStorage
	WorkerCounter  int
	WorkerSignal   map[string]chan bool
	Working        *bool
	SignalLock     sync.Mutex
	Logger         Logger
	middleware     []MiddlewareFunc
}

func NewForeman(config *QueueConfig, store JobStorage, logger Logger) Foreman {
	pl := make(map[string]position)
	working := true
	return &foreman{
		ProductionLine: pl,
		Cfg:            config,
		Store:          store,
		Logger:         logger,
		WorkerCounter:  0,
		middleware:     make([]MiddlewareFunc, 0),
		Working:        &working,
		WorkerSignal:   make(map[string]chan bool),
	}
}

func (f foreman) AddMiddleware(midl ...MiddlewareFunc) {
	f.middleware = append(f.middleware, midl...)
}

func (f foreman) AddWorker(title string, workerfunc HandlerFunc, midl ...MiddlewareFunc) error {
	_, ok := f.Cfg.JobDescription[title]
	if !ok {
		return CommonError.INVALID_JD
	}

	f.ProductionLine[title] = position{Func: workerfunc, Midl: midl}
	return nil
}
func (f foreman) AddJob(ctx context.Context, job *Job) error {
	jd, ok := f.Cfg.JobDescription[job.Title]
	if !ok {
		return CommonError.INVALID_JOB
	}

	if job.ID == "" {
		job.ID = uuid.NewString()
	}

	if job.Priority < 1 {
		job.Priority = jd.Priority
	}

	err := f.Store.CreateJob(ctx, job)
	if err != nil {
		return err
	}
	return nil
}

func (f foreman) Serve() error {
	for *f.Working {
		if f.WorkerCounter >= f.Cfg.Concurrent {
			// TODO: change to governer
			time.Sleep(time.Duration(f.Cfg.RampTime) * time.Second)
			continue
		}
		f.WorkerCounter++
		go func() {
			defer func() { f.WorkerCounter-- }()

			// get a job
			job, err := f.Store.GetAndLockAvailableJob(f.Cfg.JobDescription)
			strikeChannel := make(chan bool)
			f.SignalLock.Lock()
			f.WorkerSignal[job.ID] = strikeChannel
			f.SignalLock.Unlock()
			if err != nil && err != CommonError.NOT_FOUND {
				time.Sleep(time.Duration(f.Cfg.BreakTime) * time.Second)
				return
			}

			// get job support
			worker, ok := f.ProductionLine[job.Title]
			if !ok {
				job.Status = JobStatus.ERROR
				job.Message = CommonError.INVALID_PL.Error()
				err = f.Store.UpdateJobResult(job)
				if err != nil {
					f.Logger.Error("update job error:" + err.Error())
				}
			}

			jd, ok := f.Cfg.JobDescription[job.Title]
			if !ok {
				job.Status = JobStatus.ERROR
				job.Message = CommonError.INVALID_JD.Error()
				err = f.Store.UpdateJobResult(job)
				if err != nil {
					f.Logger.Error("update job error:" + err.Error())
				}
			}

			// run job
			ctx, ctxCancel := context.WithTimeout(context.Background(), time.Duration(jd.TTL+2)*time.Second)

			// start worker
			go func(ctx context.Context, worker position) {
				defer ctxCancel()
				f.Logger.Debug("Start job: " + job.ID)
				ctx = context.WithValue(ctx, ContextJobKey, job)
				if err := f.applyMiddlewares(f.applyMiddlewares(worker.Func, f.middleware...), worker.Midl...)(ctx); err != nil {
					job.Result = err.Error()
					job.Status = JobStatus.ERROR
				}
			}(ctx, worker)
			//wait for job to complete or strike
			select {
			case <-strikeChannel:
				f.Logger.Error("Strike job: " + job.ID)
				ctxCancel()
				return
			case <-ctx.Done():
				// close(c)
				f.SignalLock.Lock()
				delete(f.WorkerSignal, job.ID)
				f.SignalLock.Unlock()
				close(strikeChannel)
				if ctx.Err() == context.DeadlineExceeded {
					// timeout
					f.Logger.Warn("Terminate job: " + job.ID)
					if job.Try >= jd.MaxRetry {
						job.Status = JobStatus.MAX_RETRY
					} else {
						job.Status = JobStatus.RETRY
					}
					job.Message = "job terminated at: " + time.Now().String()
				}
				if ctx.Err() == context.Canceled {
					// job done
					f.Logger.Debug("Complete job: " + fmt.Sprintf("%v", job))
				}

			}
			// update job status
			job.Try += 1
			job.Priority *= 2
			if err := f.Store.UpdateJobResult(job); err != nil {
				f.Logger.Error("Update job: " + fmt.Sprintf("%v", job))
			}
		}()
		time.Sleep(time.Duration(f.Cfg.RampTime) * time.Second)
	}
	return CommonError.TERMINATING
}
func (f foreman) Strike(ttl int) error {
	*f.Working = false
	for k, s := range f.WorkerSignal {
		f.SignalLock.Lock()
		s <- true
		delete(f.WorkerSignal, k)
		f.SignalLock.Unlock()
	}
	time.Sleep(time.Duration(ttl) * time.Second)
	return nil
}

func (f foreman) applyMiddlewares(h HandlerFunc, m ...MiddlewareFunc) HandlerFunc {
	for i := len(m) - 1; i >= 0; i-- {
		h = m[i](h)
	}
	return h
}
