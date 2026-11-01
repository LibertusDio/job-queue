CREATE TABLE jobs (
	id varchar(36) NOT NULL,
	job_id varchar(100) NOT NULL,
	title varchar(100) NOT NULL,
	payload TEXT NOT NULL,
	try INT DEFAULT 0 NOT NULL,
	priority INT NOT NULL,
	status varchar(20) DEFAULT 'init' NOT NULL,
	result TEXT NOT NULL,
	message varchar(100) DEFAULT 'ok' NOT NULL,
	updated_at BIGINT DEFAULT 0 NOT NULL,
	CONSTRAINT jobs_PK PRIMARY KEY (id)
);
CREATE INDEX jobs_job_id_IDX ON jobs USING BTREE (job_id);
CREATE INDEX jobs_title_IDX ON jobs USING BTREE (title);
CREATE INDEX jobs_priority_IDX ON jobs USING BTREE (priority);
CREATE INDEX jobs_status_IDX ON jobs USING BTREE (status);
CREATE INDEX jobs_updated_at_IDX ON jobs USING BTREE (updated_at);


CREATE TABLE schedule_jobs (
	id varchar(36) NOT NULL,
	job_id varchar(100) NOT NULL,
	title varchar(100) NOT NULL,
	payload TEXT NOT NULL,
	priority INT NOT NULL,
	status varchar(20) DEFAULT 'init' NOT NULL,
	updated_at BIGINT DEFAULT 0 NOT NULL,
	schedule BIGINT DEFAULT 0 NOT NULL,
	execute_id varchar(36) NOT NULL,
	log TEXT NULL,
	CONSTRAINT schedule_jobs_PK PRIMARY KEY (id)
);
CREATE INDEX schedule_jobs_priority_IDX ON schedule_jobs USING BTREE (priority);
CREATE INDEX schedule_jobs_status_IDX ON schedule_jobs USING BTREE (status);
CREATE INDEX schedule_jobs_updated_at_IDX ON schedule_jobs USING BTREE (updated_at);
CREATE INDEX schedule_jobs_schedule_IDX ON schedule_jobs USING BTREE (schedule);
CREATE INDEX schedule_jobs_title_job_id_IDX ON schedule_jobs USING BTREE (title, job_id);
