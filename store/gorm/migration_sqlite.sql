CREATE TABLE jobs (
	id TEXT NOT NULL,
	job_id TEXT NOT NULL,
	title TEXT NOT NULL,
	payload TEXT NOT NULL,
	try INTEGER DEFAULT 0 NOT NULL,
	priority INTEGER NOT NULL,
	status TEXT DEFAULT 'init' NOT NULL,
	result TEXT NOT NULL,
	message TEXT DEFAULT 'ok' NOT NULL,
	updated_at INTEGER DEFAULT 0 NOT NULL,
	CONSTRAINT jobs_PK PRIMARY KEY (id)
);
CREATE INDEX jobs_job_id_IDX ON jobs (job_id);
CREATE INDEX jobs_title_IDX ON jobs (title);
CREATE INDEX jobs_priority_IDX ON jobs (priority);
CREATE INDEX jobs_status_IDX ON jobs (status);
CREATE INDEX jobs_updated_at_IDX ON jobs (updated_at);


CREATE TABLE schedule_jobs (
	id TEXT NOT NULL,
	job_id TEXT NOT NULL,
	title TEXT NOT NULL,
	payload TEXT NOT NULL,
	priority INTEGER NOT NULL,
	status TEXT DEFAULT 'init' NOT NULL,
	updated_at INTEGER DEFAULT 0 NOT NULL,
	schedule INTEGER DEFAULT 0 NOT NULL,
	execute_id TEXT NOT NULL,
	log TEXT NULL,
	CONSTRAINT schedule_jobs_PK PRIMARY KEY (id)
);
CREATE INDEX schedule_jobs_priority_IDX ON schedule_jobs (priority);
CREATE INDEX schedule_jobs_status_IDX ON schedule_jobs (status);
CREATE INDEX schedule_jobs_updated_at_IDX ON schedule_jobs (updated_at);
CREATE INDEX schedule_jobs_schedule_IDX ON schedule_jobs (schedule);
CREATE INDEX schedule_jobs_title_job_id_IDX ON schedule_jobs (title, job_id);
