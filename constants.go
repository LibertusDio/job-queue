package jobqueue

import "errors"

type commonerror struct {
	INVALID_JD     error
	INVALID_JOB    error
	INVALID_WORKER error
	INVALID_PL     error
	NOT_FOUND      error
}

var CommonError = commonerror{
	INVALID_JD:     errors.New("invalid job description"),
	INVALID_JOB:    errors.New("invalid job"),
	INVALID_WORKER: errors.New("invalid worker"),
	INVALID_PL:     errors.New("invalid production line"),
	NOT_FOUND:      errors.New("not found"),
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

const ContextJobKey = "job"
