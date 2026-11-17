package manifest

import (
	"slices"
	"strings"
)

// checkDependsOn checks the depends_on of one app: each name is another app of the manifest, listed once. It returns false
// when one is not.
func (m *Manifest) checkDependsOn(p *Problems, name string, a App) bool {
	path := join("apps", name) + ".depends_on"
	ok := true
	seen := map[string]bool{}
	for _, dep := range a.DependsOn {
		switch _, known := m.Apps[dep]; {
		case dep == name:
			p.add(path, "apps.%s.depends_on: an app cannot depend on itself", name)
		case seen[dep]:
			p.add(path, "apps.%s.depends_on: duplicate app %q", name, dep)
		case !known:
			p.add(path, "apps.%s.depends_on: unknown app %q", name, dep)
		default:
			seen[dep] = true
			continue
		}
		ok = false
	}
	return ok
}

// checkDependsCycle refuses a depends_on cycle and names it: the path of apps, starting from the first by name that is on the
// cycle and ending where it began. Every depends_on name must already be an app of the manifest.
func (m *Manifest) checkDependsCycle(p *Problems) {
	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	var path []string
	var visit func(name string) []string
	visit = func(name string) []string {
		state[name] = visiting
		path = append(path, name)
		for _, dep := range m.Apps[name].DependsOn {
			switch state[dep] {
			case visiting:
				cycle := slices.Clone(path[slices.Index(path, dep):])
				first := slices.Index(cycle, slices.Min(cycle))
				return append(slices.Concat(cycle[first:], cycle[:first]), cycle[first])
			case 0:
				if found := visit(dep); found != nil {
					return found
				}
			}
		}
		path = path[:len(path)-1]
		state[name] = done
		return nil
	}
	for _, name := range sortedKeys(m.Apps) {
		if state[name] == 0 {
			if cycle := visit(name); cycle != nil {
				p.add(join("apps", cycle[0])+".depends_on", "depends_on cycle: %s", strings.Join(cycle, " -> "))
				return
			}
		}
	}
}

// DeployOrder returns every app in the order a deploy of all apps runs them: an app after the apps it depends on, and among
// apps that are ready, the first by name. The manifest is valid, so there is no cycle.
func (m *Manifest) DeployOrder() []string {
	waiting := map[string]int{}
	dependents := map[string][]string{}
	var ready []string
	for _, name := range sortedKeys(m.Apps) {
		waiting[name] = len(m.Apps[name].DependsOn)
		for _, dep := range m.Apps[name].DependsOn {
			dependents[dep] = append(dependents[dep], name)
		}
		if waiting[name] == 0 {
			ready = append(ready, name)
		}
	}
	order := make([]string, 0, len(m.Apps))
	for len(ready) > 0 {
		name := ready[0]
		ready = ready[1:]
		order = append(order, name)
		for _, dependent := range dependents[name] {
			if waiting[dependent]--; waiting[dependent] == 0 {
				ready = append(ready, dependent)
			}
		}
		slices.Sort(ready)
	}
	return order
}
