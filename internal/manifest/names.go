package manifest

import (
	"berthy/internal/dockername"
	"fmt"
	"maps"
)

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

// VolumeNameError reports a scoped volume name the engine cannot address.
func (m *Manifest) VolumeNameError(environment, volume string) string {
	name := m.VolumeName(environment, volume)
	if !dockername.Valid(name) {
		return fmt.Sprintf("volume name %s is not a valid Docker identifier; shorten the volume, project or environment", name)
	}
	return ""
}

// ComposeProject is the Compose project an app runs under: its project_name, or <project>-<environment>-<app>.
func (m *Manifest) ComposeProject(environment, app string) string {
	if a, ok := m.Apps[app]; ok && a.ProjectName != "" {
		return a.ProjectName
	}
	return m.Project + "-" + environment + "-" + app
}

// NetworkNameProblems finds roles that would answer to the same DNS name on the environment network.
// With a nil serviceName, only aliases are checked. Otherwise it supplies bound or adopted names when present.
// A non-empty targetApp reports only clashes involving that app.
func (m *Manifest) NetworkNameProblems(environment string, serviceName func(app, role string) string, targetApp string) Problems {
	type owner struct{ app, role string }
	seen := map[string]owner{}
	var problems Problems
	apps := maps.Clone(m.Apps)
	if m.Ingress != nil && m.Ingress.Cloudflare != nil {
		connector, _, err := m.ConnectorApp()
		if err == nil {
			if apps == nil {
				apps = map[string]App{}
			}
			apps[ConnectorAppName] = connector
		}
	}
	for _, app := range sortedKeys(apps) {
		a := apps[app]
		if a.Kind != "image" {
			continue
		}
		for _, role := range sortedKeys(a.Roles) {
			current := owner{app, role}
			service := m.ServiceName(environment, app, role)
			if serviceName != nil {
				if name := serviceName(app, role); name != "" {
					service = name
				}
			}
			reported := false
			names := []string{app + "-" + role}
			if serviceName != nil {
				names = append(names, service)
			}
			for _, name := range names {
				if previous, ok := seen[name]; ok && previous != current {
					if !reported && (targetApp == "" || app == targetApp || previous.app == targetApp) {
						path := "apps." + app + ".roles." + role
						if app == ConnectorAppName && m.Ingress != nil {
							path = "ingress.cloudflare"
						}
						problems.add(path, "network name %s is shared by roles %s/%s and %s/%s", name, previous.app, previous.role, app, role)
						reported = true
					}
				} else {
					seen[name] = current
				}
			}
		}
	}
	return problems
}
