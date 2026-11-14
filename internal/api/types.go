package api

type AppRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}
type App struct {
	Ref   AppRef `json:"ref"`
	Roles []Role `json:"roles"`
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
}
type Check struct {
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Critical bool   `json:"critical"`
	Detail   string `json:"detail"`
}
