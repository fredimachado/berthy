package manifest

import "fmt"

// ServiceName is the Swarm service of a role.
func (m *Manifest) ServiceName(environment, app, role string) string {
	return m.Project + "-" + environment + "-" + app + "-" + role
}

// VolumeName is the Docker volume of a named volume mounted by an image app.
func (m *Manifest) VolumeName(environment, volume string) string {
	return m.Project + "-" + environment + "_" + volume
}

// MaxServiceName is the longest Swarm service name Docker accepts.
const MaxServiceName = 63

// ServiceNameError reports a service name that Docker will refuse.
func (m *Manifest) ServiceNameError(environment, app, role string) string {
	name := m.ServiceName(environment, app, role)
	if len(name) > MaxServiceName {
		return fmt.Sprintf("name %s is longer than 63 characters; shorten the project, environment, app or role", name)
	}
	return ""
}
