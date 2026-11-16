package api

import (
	"fmt"
	"time"
)

// BackupEntry is one backup in object storage, as `backup list` shows it. Key names the archive and is what restore takes.
type BackupEntry struct {
	Key      string    `json:"key"`
	Backup   string    `json:"backup"`
	App      string    `json:"app"`
	Volume   string    `json:"volume"`
	Size     int64     `json:"size"`
	SHA256   string    `json:"sha256"`
	Finished time.Time `json:"finished"`
}

// BackupRun is one run of a volume backup as the API shows it, whatever its outcome.
type BackupRun struct {
	ID          int64      `json:"id"`
	Backup      string     `json:"backup"`
	App         string     `json:"app"`
	Volume      string     `json:"volume"`
	Destination string     `json:"destination"`
	Key         string     `json:"key"`
	Trigger     string     `json:"trigger"`
	Scheduled   *time.Time `json:"scheduled,omitempty"` // the cron occurrence of a scheduled run
	Started     time.Time  `json:"started"`
	Finished    *time.Time `json:"finished,omitempty"`
	Status      string     `json:"status"`
	Size        int64      `json:"size,omitempty"`
	SHA256      string     `json:"sha256,omitempty"`
	Detail      string     `json:"detail,omitempty"`
}

// StateBackupRun is one run of the agent's state backup as the API shows it, whatever its outcome. Key names the snapshot.
type StateBackupRun struct {
	ID        int64      `json:"id"`
	Key       string     `json:"key"`
	Trigger   string     `json:"trigger"`
	Scheduled *time.Time `json:"scheduled,omitempty"` // the cron occurrence of a scheduled run
	Started   time.Time  `json:"started"`
	Finished  *time.Time `json:"finished,omitempty"`
	Status    string     `json:"status"`
	Size      int64      `json:"size,omitempty"`
	SHA256    string     `json:"sha256,omitempty"`
	Detail    string     `json:"detail,omitempty"`
}

// BackupStatus is a backup of the manifest as describe and ps show it for an app: when it last succeeded, how old that is and
// the outcome of its newest run.
type BackupStatus struct {
	Name        string     `json:"name"`
	Volume      string     `json:"volume"`
	Schedule    string     `json:"schedule,omitempty"`
	LastSuccess *time.Time `json:"last_success,omitempty"`
	Age         string     `json:"age,omitempty"` // how long ago the last success finished, such as "3h"
	Size        int64      `json:"size,omitempty"`
	LastStatus  string     `json:"last_status,omitempty"` // succeeded or failed; empty before any run
	Detail      string     `json:"detail,omitempty"`      // why the newest run failed
}

// HumanSize writes a byte count with a binary unit, such as "1.5 KiB".
func HumanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// HumanAge writes a duration as its largest whole unit: "42s", "5m", "3h" or "2d".
func HumanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(int(d/time.Second), 0))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}
