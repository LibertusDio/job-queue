CREATE TABLE jobs (
	id varchar(36) NOT NULL,
	job_id varchar(100) NOT NULL,
	title varchar(100) NOT NULL,
	payload LONGTEXT NOT NULL,
	try INT DEFAULT 0 NOT NULL,
	priority INT NOT NULL,
	status varchar(20) DEFAULT 'init' NOT NULL,
	`result` LONGTEXT NOT NULL,
	message varchar(100) DEFAULT 'ok' NOT NULL,
	updated_at BIGINT DEFAULT 0 NOT NULL,
	CONSTRAINT jobs_PK PRIMARY KEY (id)
)
ENGINE=InnoDB
DEFAULT CHARSET=utf8
COLLATE=utf8_general_ci;
CREATE INDEX jobs_job_id_IDX USING BTREE ON jobs (job_id);
CREATE INDEX jobs_title_IDX USING BTREE ON jobs (title);
CREATE INDEX jobs_priority_IDX USING BTREE ON jobs (priority);
CREATE INDEX jobs_status_IDX USING BTREE ON jobs (status);
CREATE INDEX jobs_updated_at_IDX USING BTREE ON jobs (updated_at);
