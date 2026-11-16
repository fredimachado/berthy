package api

import "time"

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
	Started     time.Time  `json:"started"`
	Finished    *time.Time `json:"finished,omitempty"`
	Status      string     `json:"status"`
	Size        int64      `json:"size,omitempty"`
	SHA256      string     `json:"sha256,omitempty"`
	Detail      string     `json:"detail,omitempty"`
}
