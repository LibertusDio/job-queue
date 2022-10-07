package jobqueue

import (
	"context"
	"fmt"
	"sync"
	"time"

	union "github.com/LibertusDio/union-go"
	"github.com/google/uuid"
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
	working        *bool
	signalLock     *sync.Mutex
	logger         Logger
	middleware     []MiddlewareFunc
	jobGovernor    Governor
	union          union.Union
}

func NewForeman(config *QueueConfig, store JobStorage, logger Logger) Foreman {
	pl := make(map[string]position)
	working := true
	return &foreman{
		productionLine: pl,
		cfg:            config,
		store:          store,
		logger:         logger,
		signalLock:     new(sync.Mutex),
		middleware:     make([]MiddlewareFunc, 0),
		working:        &working,
		workerSignal:   make(map[string]chan bool),
		jobGovernor:    NewLocalOndemandGovernor(config.BreakTime, config.RampTime, config.Concurrent, config.JobDescription),
		union:          union.NewGoroutineUnion(),
	}
}

func (f foreman) AddMiddleware(midl ...MiddlewareFunc) {
	f.middleware = append(f.middleware, midl...)
}

func (f foreman) AddWorker(title string, workerfunc HandlerFunc, midl ...MiddlewareFunc) error {
	_, ok := f.cfg.JobDescription[title]
	if !ok {
		return JobError.INVALID_JD
	}

	f.productionLine[title] = position{Func: workerfunc, Midl: midl}
	return nil
}
func (f foreman) AddJob(ctx context.Context, job *Job) error {
	jd, ok := f.cfg.JobDescription[job.Title]
	if !ok {
		return JobError.INVALID_JOB
	}

	if job.ID == "" {
		job.ID = uuid.NewString()
	}

	if job.Priority < 1 {
		job.Priority = jd.Priority
	}
	job.Status = JobStatus.INIT
	job.Try = 0
	job.UpdatedAt = time.Now().Unix()
	err := f.store.CheckDuplicateJob(ctx, *job)
	if err != nil {
		return err
	}

	err = f.store.CreateJob(ctx, *job)
	if err != nil {
		return err
	}
	return nil
}

func (f foreman) AddJobWithSchedule(ctx context.Context, job *Job, runat int64) error {
	jd, ok := f.cfg.JobDescription[job.Title]
	if !ok {
		return JobError.INVALID_JOB
	}

	if job.ID == "" {
		job.ID = uuid.NewString()
	}

	if job.Priority < 1 {
		job.Priority = jd.Priority
	}
	job.Status = JobStatus.INIT
	job.Try = 0
	job.UpdatedAt = time.Now().Unix()
	err := f.store.CheckDuplicateJob(ctx, *job)
	if err != nil {
		return err
	}

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

	err = f.store.CreateScheduleJob(ctx, sj)
	if err != nil {
		return err
	}
	return nil
}

func (f foreman) Serve() error {
	// scheduler
	go func() {
		for *f.working {
			time.Sleep(59 * time.Second)
			jobs, err := f.store.GetScheduledJob(0, time.Now().Unix())
			if err == JobError.NOT_FOUND {
				f.logger.Info("empty schedule")
				continue
			}
			if err != nil {
				f.logger.Error("fetching schedule error: " + err.Error())
				continue
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
				err := f.store.InjectJob(j)
				if err != nil {
					job.Status = JobStatus.ERROR
					job.Log = err.Error()
					job.UpdatedAt = now
				} else {
					job.Status = JobStatus.DONE
					job.ExecuteID = id
					job.UpdatedAt = now
				}
				err = f.store.UpdateScheduledJob(*job)
				if err != nil {
					f.logger.Error("update schdule job error: " + err.Error())
				}
			}
		}
	}()

	for *f.working {
		spawn, bl := f.jobGovernor.Spawn()
		if !spawn {
			continue
		}
		// get a job
		j, err := f.store.GetAndLockAvailableJob(f.cfg.JobDescription, bl...)
		strikeChannel := make(chan bool, 1)

		if err != nil && err != JobError.NOT_FOUND {
			f.logger.Error("fetch job error:" + err.Error())
			return err
		}

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
			}

			jd, ok := f.cfg.JobDescription[job.Title]
			if !ok {
				job.Status = JobStatus.ERROR
				job.Message = JobError.INVALID_JD.Error()
				err = f.store.UpdateJobResult(*job)
				if err != nil {
					f.logger.Error("update job error:" + err.Error())
				}
			}

			// run job
			ctx, ctxCancel := context.WithTimeout(context.Background(), time.Duration(jd.TTL+2)*time.Second)

			// start worker
			go func(ctx context.Context, worker position) {
				defer ctxCancel()
				f.logger.Debug("Start job: " + job.ID)
				ctx = context.WithValue(ctx, ContextJobKey, job)
				if err := f.applyMiddlewares(f.applyMiddlewares(worker.Func, f.middleware...), worker.Midl...)(ctx); err != nil {
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
				close(strikeChannel)
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
			if err := f.store.UpdateJobResult(*job); err != nil {
				f.logger.Error("Update job: " + fmt.Sprintf("%v", job))
			}
			f.jobGovernor.DelJob(job.Title)
		}, workerdata)
	}
	return JobError.TERMINATING
}

func (f foreman) Strike(ttl int) error {
	f.logger.Info("Stopping Foreman")
	*f.working = false
	f.logger.Info("Waiting for graceful shutdown")
	time.Sleep(time.Duration(ttl) * time.Second)
	f.logger.Warn("Striking remaining job")
	for k, s := range f.workerSignal {
		f.signalLock.Lock()
		s <- true
		delete(f.workerSignal, k)
		f.signalLock.Unlock()
	}
	return nil
}

func (f foreman) applyMiddlewares(h HandlerFunc, m ...MiddlewareFunc) HandlerFunc {
	for i := len(m) - 1; i >= 0; i-- {
		h = m[i](h)
	}
	return h
}

func (f foreman) GetCounter() int {
	return f.jobGovernor.GetCounter()
}
