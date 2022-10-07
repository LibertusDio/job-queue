package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"time"

	jobqueue "github.com/LibertusDio/job-queue"
	gormmidl "github.com/LibertusDio/job-queue/middlewares"
	gormstore "github.com/LibertusDio/job-queue/store/gorm"
	"github.com/LibertusDio/job-queue/utils"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func main() {
	logger := utils.DumpLogger{}

	gormDB, err := gorm.Open(
		mysql.Open("root:mysql@tcp(127.0.0.1:3306)/jobs?charset=utf8mb4&parseTime=True&loc=Local"),
		&gorm.Config{
			Logger: gormlogger.Default.LogMode(gormlogger.Silent),
		},
	)
	if err != nil {
		logger.Error(err.Error())
	}

	// gormDB = gormDB.Debug()

	store, err := gormstore.NewGormStore("jobs", "schedule_jobs", "db", gormDB, logger)
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
			},
			"bar": {
				Title:      "bar",
				TTL:        20,
				Concurrent: 10,
				Priority:   1,
				MaxRetry:   2,
				Secure:     false,
			},
			"test_job_1": {
				Title:      "test_job_1",
				TTL:        20,
				Concurrent: 10,
				Priority:   1,
				MaxRetry:   5,
				Secure:     false,
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

	queue.AddWorker("test_job_1", TestJob1, gormmidl.GormTransactionMiddleware("db", gormDB))

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
	//
	rand.Seed(time.Now().UnixNano())
	min := 1
	max := 7

	for i := 1; i <= 10; i++ {
		rd := rand.Intn(max-min+1) + min
		type RequestTest struct {
			ID          int `json:"id"`
			Status      int `json:"status"`
			StatusFirst int `json:"status_first"`
			Retry       int `json:"retry"`
		}
		reqPayload := RequestTest{
			ID:          i,
			Status:      rd,
			StatusFirst: rd,
			Retry:       0,
		}
		textPayload, _ := json.Marshal(reqPayload)
		err = queue.AddJob(context.WithValue(context.Background(), "db", gormDB), &jobqueue.Job{
			Title:   "test_job_1",
			JobID:   fmt.Sprintf("%d", reqPayload.ID),
			Payload: string(textPayload),
		})
		if err != nil {
			logger.Error("Add job: " + err.Error())
		}
		reqPayload.ID++
	}
	//
	time.Sleep(6 * time.Minute)
	queue.Strike(5)
	time.Sleep(5 * time.Second)
}

func TestJob1(ctx context.Context) error {
	job, ok := ctx.Value(jobqueue.ContextJobKey).(*jobqueue.Job)
	if !ok {
		return jobqueue.JobError.INVALID_JOB
	}
	// update
	tx, ok := ctx.Value("db").(*gorm.DB)
	if !ok {
		return jobqueue.StoreError.INVALID_GORM_TX
	}

	type ActionRequest struct {
		ID          int `json:"id"`
		StatusFirst int `json:"status_first"`
		Status      int `json:"status"`
		CountRetry  int `json:"retry"`
	}

	req := ActionRequest{}
	err := json.Unmarshal([]byte(job.Payload), &req)
	if err != nil {
		return jobqueue.JobError.INVALID_JOB
	}
	req.CountRetry++
	switch req.Status {
	case 1: // timeout 5 times
		//
		text, _ := json.Marshal(req)
		// update
		job.Payload = string(text)
		err := tx.Table("jobs").Save(&job).Where(" id = ? ", job.ID).Error
		if err != nil {
			return jobqueue.JobError.INVALID_JOB
		}
		time.Sleep(30 * time.Second)
	case 2: // return error
		//
		text, _ := json.Marshal(req)
		// update
		job.Payload = string(text)
		err := tx.Table("jobs").Save(&job).Where(" id = ? ", job.ID).Error
		if err != nil {
			return jobqueue.JobError.INVALID_JOB
		}
		job.Status = jobqueue.JobStatus.ERROR
		job.Message = "lỗi"
		return nil
	case 3: // timeout 1 times and then return done
		//
		req.Status = 8
		text, _ := json.Marshal(req)
		// update
		job.Payload = string(text)
		err := tx.Table("jobs").Save(&job).Where(" id = ? ", job.ID).Error
		if err != nil {
			return jobqueue.JobError.INVALID_JOB
		}
		time.Sleep(30 * time.Second)
	case 4: // timeout 3 times and then return done
		if req.CountRetry == 3 {
			req.Status = 9
		}
		text, _ := json.Marshal(req)
		// update
		job.Payload = string(text)
		err := tx.Table("jobs").Save(&job).Where(" id = ? ", job.ID).Error
		if err != nil {
			return jobqueue.JobError.INVALID_JOB
		}
		time.Sleep(30 * time.Second)
		job.Status = jobqueue.JobStatus.DONE
		job.Message = "ok"
		return nil
	case 5: // timeout 4 times and then return error
		if req.CountRetry == 4 {
			req.Status = 2
		}
		text, _ := json.Marshal(req)
		// update
		job.Payload = string(text)
		err := tx.Table("jobs").Save(&job).Where(" id = ? ", job.ID).Error
		if err != nil {
			return jobqueue.JobError.INVALID_JOB
		}
		time.Sleep(30 * time.Second)
		job.Status = jobqueue.JobStatus.DONE
		job.Message = "ok"
		return nil
	case 6: // set status retry run max retry
		text, _ := json.Marshal(req)
		// update
		job.Payload = string(text)
		err := tx.Table("jobs").Save(&job).Where(" id = ? ", job.ID).Error
		if err != nil {
			return jobqueue.JobError.INVALID_JOB
		}
		job.Status = jobqueue.JobStatus.RETRY
		job.Message = "ok"
		return nil
	case 7: // set status retry 3 time then return done
		if req.CountRetry == 3 {
			req.Status = 10
		}
		text, _ := json.Marshal(req)
		// update
		job.Payload = string(text)
		err := tx.Table("jobs").Save(&job).Where(" id = ? ", job.ID).Error
		if err != nil {
			return jobqueue.JobError.INVALID_JOB
		}
		job.Status = jobqueue.JobStatus.RETRY
		job.Message = "ok"
		return nil
	case 8, 9, 10: // done
		text, _ := json.Marshal(req)
		// update
		job.Payload = string(text)
		err := tx.Table("jobs").Save(&job).Where(" id = ? ", job.ID).Error
		if err != nil {
			return jobqueue.JobError.INVALID_JOB
		}
		job.Status = jobqueue.JobStatus.DONE
		job.Message = "ok"
		return nil
	}
	job.Status = jobqueue.JobStatus.DONE
	job.Message = "ok"
	return nil
}
