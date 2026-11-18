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

// NextClock is the next occurrence of the job on the clock of its Timezone, and false when it has none. A zone that is not
// stated or that this binary does not know reads as UTC.
func (j JobStatus) NextClock() (time.Time, bool) {
	if j.Next == nil {
		return time.Time{}, false
	}
	loc := time.UTC
	if j.Timezone != "" {
		if l, err := time.LoadLocation(j.Timezone); err == nil {
			loc = l
		}
	}
	return j.Next.In(loc), true
}

// JobStatus is a job of the running manifest with its newest run (of any status) and, for a scheduled job, its next
// occurrence. Next is the instant in UTC; NextLocal is the same instant on the clock of Timezone, the zone the manifest reads
// its schedules in.
type JobStatus struct {
	Name      string     `json:"name"`
	App       string     `json:"app"`
	Target    string     `json:"target,omitempty"` // the role or Compose service
	Schedule  string     `json:"schedule,omitempty"`
	Timezone  string     `json:"timezone,omitempty"` // the IANA name, "UTC" by default; set for a scheduled job
	Timeout   string     `json:"timeout"`
	Next      *time.Time `json:"next,omitempty"`
	NextLocal *time.Time `json:"next_local,omitempty"`
	Last      *JobRun    `json:"last,omitempty"`
}
