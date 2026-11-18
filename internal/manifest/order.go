package manifest

// The two ways an update replaces a role's old version with its new one. start-first starts the new version beside the old
// one. stop-first stops the old one before the new one starts, so two versions never run at once.
const (
	StartFirst = "start-first"
	StopFirst  = "stop-first"
)

// UpdateOrder is how an update replaces the role's old version, and why. The app's deploy.order decides whenever it is set.
// Otherwise a role that mounts a read-write volume is stopped first, because two versions would write the volume at once; the
// reason names the first such volume. Any other role starts first, with no reason. A read-only mount is not a write.
func (a App) UpdateOrder(role string) (order, reason string) {
	if a.Deploy.Order != "" {
		return a.Deploy.Order, "set by deploy.order"
	}
	if volume, ok := writableVolume(a.Roles[role]); ok {
		return StopFirst, "because it mounts volume " + volume
	}
	return StartFirst, ""
}

// writableVolume is the name of the first volume the role mounts read-write.
func writableVolume(r Role) (string, bool) {
	for _, raw := range r.Volumes {
		if m, err := ParseMount(raw); err == nil && !m.ReadOnly {
			return m.Source, true
		}
	}
	return "", false
}

// Warnings returns what is risky in a valid manifest but does not stop a deploy: a role that writes a read-write volume while
// two of its versions can run at once. Each one has the path of the setting that causes it.
func (m *Manifest) Warnings() Problems {
	var w Problems
	for _, name := range sortedKeys(m.Apps) {
		a := m.Apps[name]
		app := join("apps", name)
		for _, rn := range sortedKeys(a.Roles) {
			r := a.Roles[rn]
			volume, ok := writableVolume(r)
			if !ok {
				continue
			}
			if a.Deploy.Order == StartFirst {
				w.add(app+".deploy.order", "%s lets two versions of role %s write volume %s at once", StartFirst, rn, volume)
			}
			if r.Replicas != nil && *r.Replicas > 1 {
				w.add(app+".roles."+rn, "%d replicas share volume %s", *r.Replicas, volume)
			}
		}
	}
	return w
}
