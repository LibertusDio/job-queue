package main

import (
	"context"
	"fmt"
	"time"

	jobqueue "github.com/LibertusDio/job-queue"
	gormmidl "github.com/LibertusDio/job-queue/middlewares"
	gormstore "github.com/LibertusDio/job-queue/store/gorm"
	"github.com/LibertusDio/job-queue/utils"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func main() {
	logger := utils.DumpLogger{}

	gormDB, err := gorm.Open(
		mysql.Open("root:mysql@tcp(127.0.0.1:3306)/jobs?charset=utf8mb4&parseTime=True&loc=Local"),
		&gorm.Config{},
	)
	if err != nil {
		logger.Error(err.Error())
	}

	gormDB = gormDB.Debug()

	store, err := gormstore.NewGormStore("jobs", "db", gormDB, logger)
	if err != nil {
		logger.Error(err.Error())
	}

	queuecfg := &jobqueue.QueueConfig{
		JobDescription: map[string]jobqueue.JobDescription{
			"foo": {
				Title:      "foo",
				TTL:        20,
				Concurrent: 1,
				Priority:   1,
				MaxRetry:   2,
				Secure:     true,
				Schedule:   false,
			},
			"bar": {
				Title:      "bar",
				TTL:        20,
				Concurrent: 10,
				Priority:   1,
				MaxRetry:   2,
				Secure:     false,
				Schedule:   false,
			},
		},
		CleanError: false,
		Concurrent: 10,
		BreakTime:  1000,
		RampTime:   300,
	}
	queue := jobqueue.NewForeman(queuecfg, store, logger)
	queue.AddMiddleware(gormmidl.GormTransactionMiddleware("db", gormDB))
	queue.AddWorker("foo", func(ctx context.Context) error {
		job, ok := ctx.Value(jobqueue.ContextJobKey).(*jobqueue.Job)
		time.Sleep(2 * time.Second)
		if !ok {
			return jobqueue.JobError.INVALID_JOB
		}

		job.Status = jobqueue.JobStatus.DONE
		job.Message = "ok"
		return nil
	})

	queue.AddWorker("bar", func(ctx context.Context) error {
		job, ok := ctx.Value(jobqueue.ContextJobKey).(*jobqueue.Job)
		time.Sleep(2 * time.Second)
		if !ok {
			return jobqueue.JobError.INVALID_JOB
		}

		job.Status = jobqueue.JobStatus.RETRY
		job.Message = "timeout"
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

	err = queue.AddJob(context.WithValue(context.Background(), "db", gormDB), &jobqueue.Job{
		Title: "foo",
		JobID: "1",
	})
	if err != nil {
		logger.Error("Add job: " + err.Error())
	}

	err = queue.AddJob(context.WithValue(context.Background(), "db", gormDB), &jobqueue.Job{
		Title: "foo",
		JobID: "1",
	})
	if err != nil {
		logger.Error("Add job: " + err.Error())
	}
	err = queue.AddJob(context.WithValue(context.Background(), "db", gormDB), &jobqueue.Job{
		Title: "foo",
		JobID: "2",
	})
	if err != nil {
		logger.Error("Add job: " + err.Error())
	}
	err = queue.AddJob(context.WithValue(context.Background(), "db", gormDB), &jobqueue.Job{
		Title: "bar",
		JobID: "1",
	})
	if err != nil {
		logger.Error("Add job: " + err.Error())
	}
	err = queue.AddJob(context.WithValue(context.Background(), "db", gormDB), &jobqueue.Job{
		Title: "bar",
		JobID: "2",
	})
	if err != nil {
		logger.Error("Add job: " + err.Error())
	}
	err = queue.AddJob(context.WithValue(context.Background(), "db", gormDB), &jobqueue.Job{
		Title: "foo",
		JobID: "3",
	})
	if err != nil {
		logger.Error("Add job: " + err.Error())
	}
	time.Sleep(30 * time.Second)
	queue.Strike(5)
	time.Sleep(5 * time.Second)
}
