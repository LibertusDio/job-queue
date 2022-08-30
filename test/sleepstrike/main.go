package main

import (
	"context"
	"fmt"
	"time"

	jobqueue "github.com/LibertusDio/job-queue"
	"github.com/LibertusDio/job-queue/utils"
	"github.com/google/uuid"
)

func main() {
	cfg := &jobqueue.QueueConfig{
		JobDescription: map[string]jobqueue.JobDescription{"test": {
			Title:      "test",
			TTL:        20,
			Concurrent: 10,
			Priority:   1,
			MaxRetry:   2,
			Secure:     false,
		}},
		CleanError: false,
		Concurrent: 10,
		BreakTime:  1000,
		RampTime:   300,
	}
	logger := utils.DumpLogger{}
	queue := jobqueue.NewForeman(cfg, store{}, logger)
	queue.AddWorker("test", func(ctx context.Context) error {
		time.Sleep(30 * time.Second)
		return nil
	})
	go func() { logger.Error(fmt.Sprint(queue.Serve())) }()
	go func() {
		for {
			time.Sleep(1 * time.Second)
			logger.Debug(fmt.Sprintf("Worker Counter: %v\r\n", queue.GetCounter()))
		}
	}()

	time.Sleep(5 * time.Second)
	queue.Strike(5)
	time.Sleep(5 * time.Second)
}

type store struct {
}

func (s store) CreateJob(ctx context.Context, job jobqueue.Job) error {
	return nil
}
func (s store) GetAndLockAvailableJob(jd map[string]jobqueue.JobDescription, ignorelist ...string) (*jobqueue.Job, error) {
	return &jobqueue.Job{
		ID:       uuid.NewString(),
		JobID:    uuid.NewString(),
		Title:    "test",
		Payload:  "",
		Try:      0,
		Priority: 1,
		Status:   jobqueue.JobStatus.INIT,
		Result:   "",
		Message:  "",
	}, nil
}
func (s store) GetJobByID(ctx context.Context) (*jobqueue.Job, error) {
	return nil, nil
}
func (s store) UpdateJob(ctx context.Context, job jobqueue.Job) error {
	return nil
}
func (s store) UpdateJobResult(job jobqueue.Job) error {
	return nil
}
func (s store) CheckDuplicateJob(ctx context.Context, job jobqueue.Job) error {
	return nil
}
func (s store) InjectJob(job jobqueue.Job) error {
	return nil
}
func (s store) CreateScheduleJob(ctx context.Context, job jobqueue.ScheduleJob) error {
	return nil
}
func (s store) GetScheduledJob(from, to int64) ([]*jobqueue.ScheduleJob, error) {
	return nil, jobqueue.JobError.NOT_FOUND
}
func (s store) UpdateScheduledJob(job jobqueue.ScheduleJob) error {
	return nil
}
