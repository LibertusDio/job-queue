package store

import (
	"context"
	"database/sql"
	"hash/fnv"
	"time"

	jobqueue "github.com/LibertusDio/job-queue"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// mysqlLockTimeoutSeconds bounds how long GET_LOCK waits for a contended
// (title, job_id) key before giving up; lock hold times here are meant to be
// a brief check-then-insert, never a full job execution.
const mysqlLockTimeoutSeconds = 5

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

// supportsRowLocking reports whether the underlying dialect understands
// `SELECT ... FOR UPDATE`. SQLite has no such clause (it serializes writers
// at the transaction level instead), so issuing it there is a SQL syntax error.
func (s gormstore) supportsRowLocking() bool {
	return s.db.Name() != "sqlite"
}

// jobLockKey identifies the (title, job_id) pair CreateJob/InjectJob must
// serialize on so their duplicate check and their insert are atomic even
// across separate processes/instances sharing the same database.
func jobLockKey(title, jobID string) string {
	return "jobqueue:" + title + ":" + jobID
}

// jobLockKeyHash reduces a lock key to an int64 for dialects (Postgres) whose
// advisory lock functions take an integer rather than an arbitrary string.
// A collision just means two unrelated keys occasionally serialize against
// each other - safe, only ever a minor unnecessary wait, never a missed lock.
func jobLockKeyHash(key string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return int64(h.Sum64())
}

// acquireJobLock takes a cross-instance named lock scoped to (title, job_id),
// so the duplicate-check-then-insert in CreateJob/InjectJob can't race against
// another process doing the same thing concurrently. It deliberately does not
// use a locking SELECT/FOR UPDATE over the jobs table: that would take
// gap/next-key locks on the same secondary indexes GetAndLockAvailableJob
// scans, which is what caused the deadlocks that the transaction "detach" was
// there to avoid. A named advisory lock lives in its own lock manager
// namespace, entirely separate from table/row locks, so it can't form a lock
// cycle with that scan no matter how it's called.
//
// On success it returns a release func the caller must invoke exactly once
// (regardless of dialect - it's a safe no-op where nothing needs releasing).
//
// MySQL's GET_LOCK is scoped to the DB session/connection, not to any
// transaction, so it must be released with an explicit RELEASE_LOCK on that
// same session once the caller's insert is known to be durable. Since that
// can be well after tx has committed (a *sql.Tx can't be used again once
// committed), the lock is acquired on its own dedicated *sql.Conn - entirely
// separate from tx - which this store keeps open until release. MySQL named
// locks are server-global, not tied to any particular connection's data, so
// the lock and the data transaction don't need to share a connection.
func (s gormstore) acquireJobLock(ctx context.Context, tx *gorm.DB, title, jobID string) (release func(), err error) {
	key := jobLockKey(title, jobID)
	noop := func() {}

	switch s.db.Name() {
	case "postgres":
		// pg_advisory_xact_lock auto-releases at transaction end (commit or
		// rollback, whoever calls it, even through nested savepoints), so
		// there is nothing to release afterward.
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", jobLockKeyHash(key)).Error; err != nil {
			return noop, err
		}
		return noop, nil

	case "mysql":
		sqlDB, err := s.db.DB()
		if err != nil {
			return noop, err
		}
		conn, err := sqlDB.Conn(ctx)
		if err != nil {
			return noop, err
		}

		var got sql.NullInt64
		if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", key, mysqlLockTimeoutSeconds).Scan(&got); err != nil {
			_ = conn.Close()
			return noop, err
		}
		if !got.Valid || got.Int64 != 1 {
			_ = conn.Close()
			return noop, jobqueue.StoreError.LOCK_TIMEOUT
		}

		return func() {
			if _, err := conn.ExecContext(context.Background(), "SELECT RELEASE_LOCK(?)", key); err != nil && s.logger != nil {
				s.logger.Error("[releaseJobLock] release error: " + err.Error())
			}
			_ = conn.Close()
		}, nil

	default:
		// SQLite (and anything else without a named-lock primitive): no-op.
		// SQLite already serializes writers at the transaction level, so the
		// check-then-insert below is already safe without an extra lock.
		return noop, nil
	}
}

// findActiveDuplicate is the actual duplicate predicate shared by
// CheckDuplicateJob's best-effort pre-check and CreateJob/InjectJob's
// authoritative, lock-protected recheck.
func (s gormstore) findActiveDuplicate(tx *gorm.DB, job jobqueue.Job) error {
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
	return nil
}

func (s gormstore) CreateJob(ctx context.Context, job jobqueue.Job) error {
	base, ok := ctx.Value(s.contextgormkey).(*gorm.DB)
	if !ok {
		return jobqueue.StoreError.INVALID_GORM_TX
	}

	if _, alreadyTx := base.Statement.ConnPool.(gorm.TxCommitter); alreadyTx {
		// Caller already owns an open transaction (sharing job creation with
		// its own writes) and controls when it commits - we release as soon
		// as this call returns rather than when that outer transaction
		// actually commits (MySQL only; see acquireJobLock's doc comment).
		release, err := s.acquireJobLock(ctx, base, job.Title, job.JobID)
		if err != nil {
			return err
		}
		defer release()

		if err := s.findActiveDuplicate(base, job); err != nil {
			return err
		}
		return base.Table(s.jobtablename).Create(&job).Error
	}

	// Plain, not-yet-transactional handle: own the whole unit of work so the
	// lock is released exactly when the insert is guaranteed durable.
	committed := false
	tx := base.Begin()
	defer func() {
		if !committed {
			e := tx.Rollback().Error
			if e != nil && s.logger != nil {
				s.logger.Error("[CreateJob] rollback error job: " + job.ID)
			}
		}
	}()

	release, err := s.acquireJobLock(ctx, tx, job.Title, job.JobID)
	if err != nil {
		return err
	}
	defer release()

	if err := s.findActiveDuplicate(tx, job); err != nil {
		return err
	}

	if err := tx.Table(s.jobtablename).Create(&job).Error; err != nil {
		return err
	}

	err = tx.Commit().Error
	if err != nil && s.logger != nil {
		s.logger.Error("[CreateJob] commit error job: " + job.ID)
	}
	committed = true
	return nil
}

// CheckDuplicateJob is a best-effort, non-authoritative pre-check: it lets
// callers fail fast on an obvious duplicate without paying for a lock
// acquisition and a transaction. It is NOT sufficient on its own to prevent
// duplicates under concurrent callers, since it runs in its own transaction,
// separate from the eventual insert - CreateJob and InjectJob re-run this
// same check under a cross-instance lock immediately before inserting, and
// that recheck is what actually prevents the race.
func (s gormstore) CheckDuplicateJob(job jobqueue.Job) error {
	committed := false
	tx := s.db.Begin()
	defer func() {
		if !committed {
			e := tx.Rollback().Error
			if e != nil && s.logger != nil {
				s.logger.Error("[CheckDuplicateJob] rollback error job: " + job.ID)
			}
		}
	}()

	if err := s.findActiveDuplicate(tx, job); err != nil {
		return err
	}

	err := tx.Commit().Error
	if err != nil && s.logger != nil {
		s.logger.Error("[CheckDuplicateJob] commit error job: " + job.ID)
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
		query = query.Where("title NOT IN ? ", ignorelist)
	}
	query = query.
		Where(subquery1.Where(" status IN ? ", []string{jobqueue.JobStatus.INIT, jobqueue.JobStatus.RETRY}).
			Or(subquery2.Where(" status = ? ", jobqueue.JobStatus.PROCESSING).
				Where(subquery3),
			),
		).
		Order("priority ASC").Order("updated_at ASC ").Limit(1)
	if s.supportsRowLocking() {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	err := query.First(&job).Error
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

	release, err := s.acquireJobLock(context.Background(), tx, job.Title, job.JobID)
	if err != nil {
		return err
	}
	defer release()

	if err := s.findActiveDuplicate(tx, job); err != nil {
		return err
	}

	err = tx.Table(s.jobtablename).Create(&job).Error
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
	q := tx.Table(s.schedulejobtablename).
		Where("schedule >= ? AND schedule <= ? ", from, to).
		Where(" (status = ? OR ( status = ? AND updated_at <= ? ))", jobqueue.JobStatus.INIT, jobqueue.JobStatus.PROCESSING, time.Now().Unix()-300).
		Order("priority ASC").
		Order("schedule ASC")
	if s.supportsRowLocking() {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	err := q.Find(&jobs).Error
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
