package api

// MaxDeployDiffResponse limits the complete opt-in deploy field preview.
const MaxDeployDiffResponse = 64 << 20

type AppRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}
type App struct {
	Ref   AppRef      `json:"ref"`
	Roles []Role      `json:"roles"`
	Jobs  []JobStatus `json:"jobs,omitempty"`
	// ServiceID identifies the Swarm service in the Docker inventory snapshot.
	ServiceID string `json:"-"`
	// Backups are the backups of the manifest that belong to the app, with their ages.
	Backups []BackupStatus `json:"backups,omitempty"`
	// Drift lists, one line each, what runs differently from the applied release, such as "api/web: replicas 3, release says 2".
	// Scaled lists the roles that berthy scale set to another count, which is not drift.
	Drift  []string `json:"drift,omitempty"`
	Scaled []string `json:"scaled,omitempty"`
}
type Role struct {
	Name     string    `json:"name"`
	Desired  *uint64   `json:"desired,omitempty"`
	Running  int       `json:"running"`
	Replicas []Replica `json:"replicas"`
}
type Replica struct {
	ContainerID  string `json:"container_id,omitempty"`
	TaskID       string `json:"task_id,omitempty"`
	Image        string `json:"image"`
	State        string `json:"state"`
	Health       string `json:"health"`
	RestartCount *int   `json:"restart_count,omitempty"`
	OOMKilled    *bool  `json:"oom_killed,omitempty"`
	// ExitCode is the exit code of an exited Compose container, read from its status. Nil for a running replica, or when the
	// status does not say.
	ExitCode *int `json:"exit_code,omitempty"`
	// Local is true when the container was inspected on this node. It is not part of the API.
	Local bool `json:"-"`
}

// FinishedOneShot reports whether r is a Compose container that exited with code 0: a one-shot that finished. Neither a
// deploy nor the down check wants it running.
func (r Replica) FinishedOneShot() bool {
	return r.State == "exited" && r.ExitCode != nil && *r.ExitCode == 0
}

type DoctorReport struct {
	Checks []Check `json:"checks"`
	KeyID  string  `json:"key_id,omitempty"`
}

type Check struct {
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Critical bool   `json:"critical"`
	Detail   string `json:"detail"`
}
