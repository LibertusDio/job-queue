package main

import (
	"context"
	"fmt"
	"os"
	"time"

	jobqueue "github.com/LibertusDio/job-queue"
	gormmidl "github.com/LibertusDio/job-queue/middlewares"
	gormstore "github.com/LibertusDio/job-queue/store/gorm"
	"github.com/LibertusDio/job-queue/utils"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// Run store/gorm/migration_sqlite.sql against the same file before starting,
// e.g.: sqlite3 jobqueue-demo.db < store/gorm/migration_sqlite.sql
func main() {
	logger := utils.DumpLogger{}

	dbfile := "jobqueue-demo.db"
	os.Remove(dbfile) // start from a clean file for the demo

	gormDB, err := gorm.Open(
		sqlite.Open(dbfile),
		&gorm.Config{
			Logger: gormlogger.Default.LogMode(gormlogger.Silent),
		},
	)
	if err != nil {
		logger.Error(err.Error())
		return
	}

	migration, err := os.ReadFile("../../store/gorm/migration_sqlite.sql")
	if err != nil {
		logger.Error(err.Error())
		return
	}
	if err := gormDB.Exec(string(migration)).Error; err != nil {
		logger.Error(err.Error())
		return
	}

	store, err := gormstore.NewGormStore("jobs", "schedule_jobs", "db", gormDB, logger)
	if err != nil {
		logger.Error(err.Error())
		return
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
		if !ok {
			return jobqueue.JobError.INVALID_JOB
		}
		job.Status = jobqueue.JobStatus.DONE
		job.Message = "ok"
		return nil
	})

	go func() { logger.Error(fmt.Sprint(queue.Serve())) }()

	time.Sleep(2 * time.Second)

	err = queue.AddJobTransaction(context.WithValue(context.Background(), "db", gormDB), &jobqueue.Job{
		Title: "foo",
		JobID: "1",
	})
	if err != nil {
		logger.Error("Add job: " + err.Error())
	}

	time.Sleep(3 * time.Second)
	queue.Strike(2)
	time.Sleep(2 * time.Second)

	var job jobqueue.Job
	if err := gormDB.Table("jobs").Where("job_id = ?", "1").First(&job).Error; err != nil {
		logger.Error("verify job: " + err.Error())
		return
	}
	logger.Info(fmt.Sprintf("final job status: %s (message: %s)", job.Status, job.Message))
}
