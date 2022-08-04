package jobqueue

// Config models
type JobDescription struct {
	Title      string `mapstructure:"title" yaml:"title" json:"title"`
	TTL        int    `mapstructure:"ttl" yaml:"ttl" json:"ttl"`                      // seconds
	Concurrent int    `mapstructure:"concurrent" yaml:"concurrent" json:"concurrent"` // 0 is invalid
	Priority   int    `mapstructure:"priority" yaml:"priority" json:"priority"`       // 0 is invalid here, 1 is highest
	MaxRetry   int    `mapstructure:"max_retry" yaml:"max_retry" json:"max_retry"`    // number of retry
	Secure     bool   `mapstructure:"secure" yaml:"secure" json:"secure"`
	Schedule   bool   `mapstructure:"schedule" yaml:"schedule" json:"schedule"`
}

type QueueConfig struct {
	JobDescription map[string]JobDescription `mapstructure:"job_description" yaml:"job_description" json:"job_description"`
	CleanError     bool                      `yaml:"clean_error" mapstructure:"clean_error" json:"clean_error"` // will the GC clear maxed try job
	Concurrent     int                       `yaml:"concurrent" mapstructure:"concurrent" json:"concurrent"`    // number of workers can be run, will ignore JD.Concurrent if it >
	BreakTime      int                       `mapstructure:"break_time" yaml:"break_time" json:"break_time"`    // seconds, break between loop
	RampTime       int                       `mapstructure:"ramp_time" yaml:"ramp_time" json:"ramp_time"`
}

// system model

type Job struct {
	ID       string `mapstructure:"id" yaml:"id" json:"id"`
	JobID    string `mapstructure:"job_id" yaml:"job_id" json:"job_id"`
	Title    string `mapstructure:"title" yaml:"title" json:"title"`
	Payload  string `mapstructure:"payload" yaml:"payload" json:"payload"`
	Try      int    `mapstructure:"try" yaml:"try" json:"try"`
	Priority int    `mapstructure:"priority" yaml:"priority" json:"priority"`
	Status   string `mapstructure:"status" yaml:"status" json:"status"`
	Result   string `yaml:"result" mapstructure:"result" json:"result"`
	Message  string `yaml:"message" mapstructure:"message" json:"message"`
}
