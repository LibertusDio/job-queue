package store

import (
	"context"
	"database/sql"
	"hash/fnv"
	"sync/atomic"
	"time"

	jobqueue "github.com/LibertusDio/job-queue"
	"github.com/LibertusDio/job-queue/internal/sqlerr"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// mysqlLockTimeoutSeconds bounds how long GET_LOCK waits for a contended
// (title, job_id) key before giving up; lock hold times here are meant to be
// a brief check-then-insert, never a full job execution.
const mysqlLockTimeoutSeconds = 5

// mysqlErrBinlogStmtModeRowEngine (ER_BINLOG_STMT_MODE_AND_ROW_ENGINE) is what
// MySQL returns for a write in a READ COMMITTED transaction when binary
// logging uses binlog_format=STATEMENT.
const mysqlErrBinlogStmtModeRowEngine = 1665

type gormstore struct {
	db                   *gorm.DB
	jobtablename         string
	schedulejobtablename string
	contextgormkey       string
	logger               jobqueue.Logger
	// claimAtDefaultIsolation is set once the server has refused a READ
	// COMMITTED claim (see beginClaim). It's a pointer so every copy of this
	// value-receiver store shares it.
	claimAtDefaultIsolation *atomic.Bool
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

		claimAtDefaultIsolation: new(atomic.Bool),
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

// findPendingSchedule is CreateScheduleJob's duplicate predicate: a (title,
// job_id) may have at most one schedule waiting to fire. Active jobs don't
// count, so a recurring job can schedule its next run while it's processing.
func (s gormstore) findPendingSchedule(tx *gorm.DB, job jobqueue.ScheduleJob) error {
	var sj jobqueue.ScheduleJob
	err := tx.Table(s.schedulejobtablename).
		Where(" title = ? ", job.Title).
		Where(" job_id = ? ", job.JobID).
		Where(" status = ? ", jobqueue.JobStatus.INIT).
		First(&sj).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return err
	}
	if err == nil {
		return jobqueue.JobError.DUPLICATE_JOB
	}
	return nil
}

// withJobLock runs fn (a duplicate check followed by an insert) while holding
// the cross-instance (title, job_id) lock from acquireJobLock.
//
// If base is already a transaction the caller owns it and controls when it
// commits, so fn runs inside it and the lock is released as soon as this call
// returns rather than when that outer transaction commits (MySQL only; see
// acquireJobLock's doc comment). Otherwise withJobLock owns the whole unit of
// work, so the lock is released exactly when the insert is durable.
func (s gormstore) withJobLock(ctx context.Context, base *gorm.DB, op, title, jobID string, fn func(tx *gorm.DB) error) error {
	if _, alreadyTx := base.Statement.ConnPool.(gorm.TxCommitter); alreadyTx {
		release, err := s.acquireJobLock(ctx, base, title, jobID)
		if err != nil {
			return err
		}
		defer release()
		return fn(base)
	}

	committed := false
	tx := base.Begin()
	defer func() {
		if !committed {
			e := tx.Rollback().Error
			if e != nil && s.logger != nil {
				s.logger.Error("[" + op + "] rollback error: " + title + ":" + jobID)
			}
		}
	}()

	release, err := s.acquireJobLock(ctx, tx, title, jobID)
	if err != nil {
		return err
	}
	defer release()

	if err := fn(tx); err != nil {
		return err
	}

	err = tx.Commit().Error
	committed = true
	if err != nil {
		if s.logger != nil {
			s.logger.Error("[" + op + "] commit error: " + title + ":" + jobID + ": " + err.Error())
		}
		return err
	}
	return nil
}

// CreateJob inserts job through the *gorm.DB found in ctx. When that is a
// transaction the caller opened, a database error leaves it unusable: on
// MySQL a deadlock (jobqueue.IsRetryable) has already rolled the whole
// transaction back, and on Postgres any error aborts it. The caller must roll
// back and retry the transaction, not carry on with it. DUPLICATE_JOB comes
// from a plain read and leaves the transaction as it was.
func (s gormstore) CreateJob(ctx context.Context, job jobqueue.Job) error {
	base, ok := ctx.Value(s.contextgormkey).(*gorm.DB)
	if !ok {
		return jobqueue.StoreError.INVALID_GORM_TX
	}

	return s.withJobLock(ctx, base, "CreateJob", job.Title, job.JobID, func(tx *gorm.DB) error {
		if err := s.findActiveDuplicate(tx, job); err != nil {
			return err
		}
		return tx.Table(s.jobtablename).Create(&job).Error
	})
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

// beginClaim opens GetAndLockAvailableJob's transaction. On MySQL/MariaDB it
// runs at READ COMMITTED: at the default REPEATABLE READ the claim's FOR
// UPDATE scan also gap-locks the whole range of pending rows, and concurrent
// job INSERTs deadlock against those gap locks. Postgres already defaults to
// READ COMMITTED and has no gap locks; SQLite has no row locks.
//
// MySQL rejects READ COMMITTED writes when binlog_format=STATEMENT, so after
// the first such error the store claims at the server's default level.
func (s gormstore) beginClaim() *gorm.DB {
	if s.db.Name() == "mysql" && !s.claimAtDefaultIsolation.Load() {
		return s.db.Begin(&sql.TxOptions{Isolation: sql.LevelReadCommitted})
	}
	return s.db.Begin()
}

// noteClaimError switches the store to claiming at the default isolation
// level if err shows the server won't run the claim at READ COMMITTED. With
// binlog_format=STATEMENT, MySQL raises it on the SELECT ... FOR UPDATE.
func (s gormstore) noteClaimError(err error) {
	if n, ok := sqlerr.MySQLNumber(err); !ok || n != mysqlErrBinlogStmtModeRowEngine {
		return
	}
	if !s.claimAtDefaultIsolation.Swap(true) && s.logger != nil {
		s.logger.Warn("[GetAndLockAvailableJob] binlog_format=STATEMENT rejects READ COMMITTED; claiming jobs at the default isolation level from now on")
	}
}

func (s gormstore) GetAndLockAvailableJob(jd map[string]jobqueue.JobDescription, ignorelist ...string) (*jobqueue.Job, error) {
	committed := false
	tx := s.beginClaim()
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
		s.noteClaimError(err)
		return nil, err
	}

	if err == gorm.ErrRecordNotFound {
		return nil, jobqueue.JobError.NOT_FOUND
	}

	job.UpdatedAt = time.Now().Unix()
	job.Status = jobqueue.JobStatus.PROCESSING

	err = tx.Table(s.jobtablename).Save(&job).Where(" id = ? ", job.ID).Error
	if err != nil {
		s.noteClaimError(err)
		return nil, err
	}

	err = tx.Commit().Error
	committed = true
	if err != nil {
		if s.logger != nil {
			s.logger.Error("[GetAndLockAvailableJob] commit error job: " + job.ID)
		}
		return nil, err
	}

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
	committed = true
	if err != nil {
		if s.logger != nil {
			s.logger.Error("[UpdateJobResult] commit error job: " + job.ID)
		}
		return err
	}
	return nil
}

func (s gormstore) InjectJob(job jobqueue.Job) error {
	return s.withJobLock(context.Background(), s.db, "InjectJob", job.Title, job.JobID, func(tx *gorm.DB) error {
		if err := s.findActiveDuplicate(tx, job); err != nil {
			return err
		}
		return tx.Table(s.jobtablename).Create(&job).Error
	})
}

// CreateScheduleJob inserts a schedule, rejecting it with DUPLICATE_JOB when
// one for the same (title, job_id) is already pending. It takes the same
// (title, job_id) lock as CreateJob/InjectJob, so concurrent callers on any
// instance can't both pass the check.
func (s gormstore) CreateScheduleJob(ctx context.Context, job jobqueue.ScheduleJob) error {
	base, ok := ctx.Value(s.contextgormkey).(*gorm.DB)
	if !ok {
		return jobqueue.StoreError.INVALID_GORM_TX
	}

	return s.withJobLock(ctx, base, "CreateScheduleJob", job.Title, job.JobID, func(tx *gorm.DB) error {
		if err := s.findPendingSchedule(tx, job); err != nil {
			return err
		}
		return tx.Table(s.schedulejobtablename).Create(&job).Error
	})
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
	committed = true
	if err != nil {
		if s.logger != nil {
			s.logger.Error("[GetScheduledJob] commit error " + err.Error())
		}
		return nil, err
	}
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
	committed = true
	if err != nil {
		if s.logger != nil {
			s.logger.Error("[UpdateScheduledJob] commit error job: " + job.ID)
		}
		return err
	}
	return nil
}
