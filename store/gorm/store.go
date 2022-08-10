package store

import (
	"context"
	"time"

	jobqueue "github.com/LibertusDio/job-queue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type gormstore struct {
	db             *gorm.DB
	tablename      string
	contextgormkey string
	logger         jobqueue.Logger
}

func NewGormStore(tablename string, contextgormkey string, db *gorm.DB, logger jobqueue.Logger) (jobqueue.JobStorage, error) {
	if tablename == "" {
		tablename = "jobs"
	}

	if contextgormkey == "" {
		contextgormkey = "db"
	}

	if db == nil {
		return nil, jobqueue.StoreError.INVALID_GORM_DB
	}

	return &gormstore{
		db:             db,
		tablename:      tablename,
		contextgormkey: contextgormkey,
		logger:         logger,
	}, nil
}

func (s gormstore) CreateJob(ctx context.Context, job *jobqueue.Job) error {
	tx, ok := ctx.Value(s.contextgormkey).(*gorm.DB)
	if !ok {
		return jobqueue.StoreError.INVALID_GORM_TX
	}

	err := tx.Table(s.tablename).Create(job).Error
	if err != nil {
		return err
	}
	return nil
}

func (s gormstore) CheckDuplicateJob(ctx context.Context, job *jobqueue.Job) error {
	tx, ok := ctx.Value(s.contextgormkey).(*gorm.DB)
	if !ok {
		return jobqueue.StoreError.INVALID_GORM_TX
	}

	var j jobqueue.Job
	err := tx.Table(s.tablename).
		Where(" title = ? ", job.Title).
		Where(" job_id = ? ", job.JobID).
		Where(" status IN ? ", []string{jobqueue.JobStatus.INIT, jobqueue.JobStatus.RETRY, jobqueue.JobStatus.PROCESSING}).
		First(&j).Error
	if err == gorm.ErrRecordNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	return jobqueue.JobError.DUPLICATE_JOB
}

func (s gormstore) GetAndLockAvailableJob(jd map[string]jobqueue.JobDescription) (*jobqueue.Job, error) {
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
	subquery := tx.Table(s.tablename)
	subquery2 := tx.Table(s.tablename)
	for k, v := range jd {
		subquery2 = subquery2.Or(" title = ? AND updated_at <= ? ", k, time.Now().Unix()-int64(v.TTL))
	}

	err := tx.Table(s.tablename).
		Where(" status IN ? ", []string{jobqueue.JobStatus.INIT, jobqueue.JobStatus.RETRY}).
		Or(subquery.Where(" status = ? ", jobqueue.JobStatus.PROCESSING).
			Where(subquery2),
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

	err = tx.Table(s.tablename).Save(&job).Where(" id = ? ", job.ID).Error
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

func (s gormstore) UpdateJobResult(job *jobqueue.Job) error {
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
	err := tx.Table(s.tablename).Save(job).Where(" id = ? ", job.ID).Error
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
