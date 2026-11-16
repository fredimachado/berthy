package api

import "time"

// JobRun is one run of a job as the API shows it. The command's output is never part of it.
type JobRun struct {
	ID        int64      `json:"id"`
	Job       string     `json:"job"`
	Trigger   string     `json:"trigger"`
	Scheduled *time.Time `json:"scheduled,omitempty"`
	Started   time.Time  `json:"started"`
	Finished  *time.Time `json:"finished,omitempty"`
	Status    string     `json:"status"`
	ExitCode  *int       `json:"exit_code,omitempty"`
	Detail    string     `json:"detail,omitempty"`
	Container string     `json:"container,omitempty"`
}

// JobStatus is a job of the last applied manifest with its newest run (of any status) and, for a scheduled job, its next
// occurrence.
type JobStatus struct {
	Name     string     `json:"name"`
	App      string     `json:"app"`
	Target   string     `json:"target,omitempty"` // the role or Compose service
	Schedule string     `json:"schedule,omitempty"`
	Timeout  string     `json:"timeout"`
	Next     *time.Time `json:"next,omitempty"`
	Last     *JobRun    `json:"last,omitempty"`
}
