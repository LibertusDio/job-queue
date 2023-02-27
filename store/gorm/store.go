package store

import (
	"context"
	"time"

	jobqueue "github.com/LibertusDio/job-queue"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type gormstore struct {
	db                   *gorm.DB
	jobtablename         string
	schedulejobtablename string
	contextgormkey       string
	logger               jobqueue.Logger
}

func NewGormStore(jobtablename, schedulejobtablename string, contextgormkey string, db *gorm.DB, logger jobqueue.Logger) (jobqueue.JobStorage, error) {
	if jobtablename == "" {
		jobtablename = "jobs"
	}

	if schedulejobtablename == "" {
		schedulejobtablename = "schedule_jobs"
	}

	if contextgormkey == "" {
		contextgormkey = "db"
	}

	if db == nil {
		return nil, jobqueue.StoreError.INVALID_GORM_DB
	}

	return &gormstore{
		db:                   db,
		jobtablename:         jobtablename,
		schedulejobtablename: schedulejobtablename,
		contextgormkey:       contextgormkey,
		logger:               logger,
	}, nil
}

func (s gormstore) CreateJob(ctx context.Context, job jobqueue.Job) error {
	tx, ok := ctx.Value(s.contextgormkey).(*gorm.DB)
	if !ok {
		return jobqueue.StoreError.INVALID_GORM_TX
	}

	err := tx.Table(s.jobtablename).Create(&job).Error
	if err != nil {
		return err
	}
	return nil
}

func (s gormstore) CheckDuplicateJob(job jobqueue.Job) error {
	committed := false
	tx := s.db.Begin()
	defer func() {
		if !committed {
			e := tx.Rollback().Error
			if e != nil && s.logger != nil {
				s.logger.Error("[InjectJob] rollback error job: " + job.ID)
			}
		}
	}()

	var j jobqueue.Job
	err := tx.Table(s.jobtablename).
		Where(" title = ? ", job.Title).
		Where(" job_id = ? ", job.JobID).
		Where(" status IN ? ", []string{jobqueue.JobStatus.INIT, jobqueue.JobStatus.RETRY, jobqueue.JobStatus.PROCESSING}).
		First(&j).Error

	if err != nil && err != gorm.ErrRecordNotFound {
		return err
	}
	if err == nil {
		return jobqueue.JobError.DUPLICATE_JOB
	}

	err = tx.Commit().Error
	if err != nil && s.logger != nil {
		s.logger.Error("[InjectJob] commit error job: " + job.ID)
	}
	committed = true
	return nil
}

func (s gormstore) GetAndLockAvailableJob(jd map[string]jobqueue.JobDescription, ignorelist ...string) (*jobqueue.Job, error) {
	committed := false
	tx := s.db.Begin()
	defer func() {
		if !committed {
			e := tx.Rollback().Error
			if e != nil && s.logger != nil {
				s.logger.Error("[GetAndLockAvailableJob] rollback error ")
			}
		}
	}()

	var job jobqueue.Job
	subquery1 := tx.Table(s.jobtablename)
	subquery2 := tx.Table(s.jobtablename)
	subquery3 := tx.Table(s.jobtablename)
	for k, v := range jd {
		subquery3 = subquery3.Or(" title = ? AND updated_at <= ? ", k, time.Now().Unix()-int64(v.TTL))
	}

	query := tx.Table(s.jobtablename)
	if len(ignorelist) > 0 {
		query = query.Not("title NOT IN ? ", ignorelist)
	}
	err := query.
		Where(subquery1.Where(" status IN ? ", []string{jobqueue.JobStatus.INIT, jobqueue.JobStatus.RETRY}).
			Or(subquery2.Where(" status = ? ", jobqueue.JobStatus.PROCESSING).
				Where(subquery3),
			),
		).
		Order("priority ASC").Order("updated_at ASC ").Limit(1).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		First(&job).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, err
	}

	if err == gorm.ErrRecordNotFound {
		return nil, jobqueue.JobError.NOT_FOUND
	}

	job.UpdatedAt = time.Now().Unix()
	job.Status = jobqueue.JobStatus.PROCESSING

	err = tx.Table(s.jobtablename).Save(&job).Where(" id = ? ", job.ID).Error
	if err != nil {
		return nil, err
	}

	err = tx.Commit().Error
	if err != nil && s.logger != nil {
		s.logger.Error("[GetAndLockAvailableJob] commit error job: " + job.ID)
	}
	committed = true

	return &job, nil
}

func (s gormstore) UpdateJobResult(job jobqueue.Job) error {
	committed := false
	tx := s.db.Begin()
	defer func() {
		if !committed {
			e := tx.Rollback().Error
			if e != nil && s.logger != nil {
				s.logger.Error("[UpdateJobResult] rollback error job: " + job.ID)
			}
		}
	}()
	err := tx.Table(s.jobtablename).Save(job).Where(" id = ? ", job.ID).Error
	if err != nil {
		return err
	}
	err = tx.Commit().Error
	if err != nil && s.logger != nil {
		s.logger.Error("[UpdateJobResult] commit error job: " + job.ID)
	}
	committed = true
	return nil
}

func (s gormstore) InjectJob(job jobqueue.Job) error {
	committed := false
	tx := s.db.Begin()
	defer func() {
		if !committed {
			e := tx.Rollback().Error
			if e != nil && s.logger != nil {
				s.logger.Error("[InjectJob] rollback error job: " + job.ID)
			}
		}
	}()

	err := tx.Table(s.jobtablename).Create(&job).Error
	if err != nil {
		return err
	}

	err = tx.Commit().Error
	if err != nil && s.logger != nil {
		s.logger.Error("[InjectJob] commit error job: " + job.ID)
	}
	committed = true
	return nil
}

func (s gormstore) CreateScheduleJob(ctx context.Context, job jobqueue.ScheduleJob) error {
	tx, ok := ctx.Value(s.contextgormkey).(*gorm.DB)
	if !ok {
		return jobqueue.StoreError.INVALID_GORM_TX
	}

	err := tx.Table(s.schedulejobtablename).Create(&job).Error
	if err != nil {
		return err
	}
	return nil
}

func (s gormstore) GetScheduledJob(from, to int64) ([]*jobqueue.ScheduleJob, error) {
	committed := false
	tx := s.db.Begin()
	defer func() {
		if !committed {
			e := tx.Rollback().Error
			if e != nil && s.logger != nil {
				s.logger.Error("[GetScheduledJob] rollback error: " + e.Error())
			}
		}
	}()
	var jobs []*jobqueue.ScheduleJob
	err := tx.Table(s.schedulejobtablename).
		Where("schedule >= ? AND schedule <= ? ", from, to).
		Where(" (status = ? OR ( status = ? AND updated_at <= ? ))", jobqueue.JobStatus.INIT, jobqueue.JobStatus.PROCESSING, time.Now().Unix()-300).
		Order("priority ASC").
		Order("schedule ASC").
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Find(&jobs).
		Error
	if err == gorm.ErrRecordNotFound {
		return nil, jobqueue.JobError.NOT_FOUND
	}
	if err != nil {
		return nil, err
	}

	for _, j := range jobs {
		j.Status = jobqueue.JobStatus.PROCESSING
		j.UpdatedAt = time.Now().Unix()
		e := tx.Table(s.schedulejobtablename).Where("id = ? ", j.ID).Save(&j).Error
		if e != nil {
			return nil, e
		}
	}

	err = tx.Commit().Error
	if err != nil && s.logger != nil {
		s.logger.Error("[GetScheduledJob] commit error " + err.Error())
	}
	committed = true
	return jobs, nil
}

func (s gormstore) UpdateScheduledJob(job jobqueue.ScheduleJob) error {
	committed := false
	tx := s.db.Begin()
	defer func() {
		if !committed {
			e := tx.Rollback().Error
			if e != nil && s.logger != nil {
				s.logger.Error("[UpdateScheduledJob] rollback error job: " + job.ID)
			}
		}
	}()
	err := tx.Table(s.schedulejobtablename).Save(job).Where(" id = ? ", job.ID).Error
	if err != nil {
		return err
	}
	err = tx.Commit().Error
	if err != nil && s.logger != nil {
		s.logger.Error("[UpdateScheduledJob] commit error job: " + job.ID)
	}
	committed = true
	return nil
}
