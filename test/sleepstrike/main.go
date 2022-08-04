package main

import (
	"context"
	"fmt"
	"time"

	jobqueue "github.com/LibertusDio/job-queue"
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
			Schedule:   false,
		}},
		CleanError: false,
		Concurrent: 10,
		BreakTime:  1,
		RampTime:   0,
	}

	queue := jobqueue.NewForeman(cfg, store{}, logger{})
	queue.AddWorker("test", func(ctx context.Context) error {
		time.Sleep(10 * time.Second)
		return nil
	})
	go func() { fmt.Println(queue.Serve()) }()
	time.Sleep(5 * time.Second)
	queue.Strike(20)
}

type store struct {
}

func (s store) CreateJob(ctx context.Context, job *jobqueue.Job) error {
	return nil
}
func (s store) GetAndLockAvailableJob(jd map[string]jobqueue.JobDescription) (*jobqueue.Job, error) {
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
func (s store) UpdateJob(ctx context.Context, job *jobqueue.Job) error {
	return nil
}
func (s store) UpdateJobResult(job *jobqueue.Job) error {
	return nil
}

type logger struct {
}

func (l logger) Debug(msg string) {
	fmt.Println("[DEBUG] " + msg)
}
func (l logger) Info(msg string) {
	fmt.Println("[INFO] " + msg)
}
func (l logger) Warn(msg string) {
	fmt.Println("[WARN] " + msg)
}
func (l logger) Error(msg string) {
	fmt.Println("[ERROR] " + msg)
}
func (l logger) Fatal(msg string) {
	fmt.Println("[FATAL] " + msg)
}
func (l logger) Panic(msg string) {
	fmt.Println("[PANIC] " + msg)
}
