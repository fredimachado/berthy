// fixture-names records the Docker names a fixture's manifest may create.
package main

import (
	"fmt"
	"os"
	"sort"

	"berthy/internal/manifest"
)

func names(dir, project, environment, selected string) ([]string, error) {
	m, err := manifest.Load(dir, environment)
	if err != nil {
		return nil, err
	}
	if m.Project != project {
		return nil, fmt.Errorf("manifest belongs to another project")
	}
	apps := make([]string, 0, len(m.Apps))
	for name := range m.Apps {
		apps = append(apps, name)
	}
	sort.Strings(apps)
	var out []string
	for _, name := range apps {
		if selected != "" && name != selected {
			continue
		}
		app := m.Apps[name]
		if app.Kind == "compose" {
			out = append(out, "compose\t"+m.ComposeProject(environment, name))
			continue
		}
		if app.Kind != "image" {
			continue
		}
		roles := make([]string, 0, len(app.Roles))
		for role := range app.Roles {
			roles = append(roles, role)
		}
		sort.Strings(roles)
		for _, role := range roles {
			for _, raw := range app.Roles[role].Volumes {
				mount, err := manifest.ParseMount(raw)
				if err != nil {
					return nil, err
				}
				out = append(out, "volume\t"+m.VolumeName(environment, mount.Source))
			}
		}
	}
	return out, nil
}

func main() {
	if len(os.Args) != 5 {
		os.Exit(2)
	}
	entries, err := names(os.Args[1], os.Args[2], os.Args[3], os.Args[4])
	if err != nil {
		os.Exit(1)
	}
	for _, entry := range entries {
		fmt.Println(entry)
	}
}
