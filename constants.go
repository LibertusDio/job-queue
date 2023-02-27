package jobqueue

import "errors"

type joberror struct {
	INVALID_JD     error
	INVALID_JOB    error
	INVALID_WORKER error
	INVALID_PL     error
	NOT_FOUND      error
	TERMINATING    error
	DUPLICATE_JOB  error
}

var JobError = joberror{
	INVALID_JD:     errors.New("invalid job description"),
	INVALID_JOB:    errors.New("invalid job"),
	INVALID_WORKER: errors.New("invalid worker"),
	INVALID_PL:     errors.New("invalid production line"),
	NOT_FOUND:      errors.New("not found"),
	TERMINATING:    errors.New("terminating"),
	DUPLICATE_JOB:  errors.New("duplicate job"),
}

type storeerror struct {
	INVALID_GORM_TX error
	INVALID_GORM_DB error
}

var StoreError = storeerror{
	INVALID_GORM_TX: errors.New("invalid gorm transaction"),
	INVALID_GORM_DB: errors.New("invalid gorm session"),
}

type jobstatus struct {
	INIT       string
	PROCESSING string
	RETRY      string
	ERROR      string
	MAX_RETRY  string
	DONE       string
}

var JobStatus = jobstatus{
	INIT:       "init",
	PROCESSING: "processing",
	RETRY:      "retry", // controlled by process and foreman, might retry
	ERROR:      "error", // consider system error, no retry
	MAX_RETRY:  "max_retry",
	DONE:       "done",
}

const ContextJobKey = "job-key"
const ContextQueueKey = "queue-key"

const MaxTime int64 = 9223372036854775807
