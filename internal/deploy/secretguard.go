package deploy

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// Submitted holds the manifest and the Compose files supplied for its readers.
type Submitted struct {
	Manifest     manifest.Manifest
	ComposeFiles map[string]string
}

// GuardFinding names a reader and, when applicable, an existing volume.
type GuardFinding struct{ App, Volume string }

// GuardSecret refuses generation when a reader may hold an earlier value.
func (c *Controller) GuardSecret(ctx context.Context, name string, sub *Submitted) (*GuardFinding, error) {
	releases, err := c.Store.Releases(ctx)
	if err != nil {
		return nil, Refusal("the recorded releases cannot be read, so nothing is generated")
	}
	deployed := map[string]bool{}
	readers := map[string]bool{}
	for _, release := range releases {
		if release.App == manifest.ConnectorAppName {
			continue
		}
		m, err := manifest.DecodeRelease(release.Manifest, false)
		if err != nil {
			return nil, Refusal("the recorded releases cannot be read, so nothing is generated")
		}
		app, ok := m.Apps[release.App]
		if !ok {
			return nil, Refusal("the recorded releases cannot be read, so nothing is generated")
		}
		deployed[release.App] = true
		if slices.Contains(app.Env.SecretNames(), name) {
			readers[release.App] = true
		}
	}
	for _, app := range sortedKeys(readers) {
		return &GuardFinding{App: app}, nil
	}
	if sub == nil {
		return nil, nil
	}
	for _, app := range sub.Manifest.SecretReaders(name) {
		a := sub.Manifest.Apps[app]
		fail := func() (*GuardFinding, error) {
			return nil, Refusal(fmt.Sprintf("the state of app %s cannot be established, so nothing is generated", app))
		}
		project := ""
		if a.Kind == "compose" {
			project = sub.Manifest.ComposeProject(c.Environment, app)
			if c.Foreign != nil {
				foreign, err := c.Foreign(ctx)
				if err != nil {
					return fail()
				}
				if foreign.Claims[state.Claim{Kind: state.ClaimComposeProject, Name: project}] {
					return nil, composeClaimConflict("project")
				}
			}
		}
		attempted, err := c.Store.DeployAttempted(ctx, app)
		if err != nil {
			return fail()
		}
		if deployed[app] || attempted {
			return &GuardFinding{App: app}, nil
		}
		if c.Engine == nil {
			return fail()
		}
		var names []string
		if a.Kind == "compose" {
			ce, ok := c.Engine.(composeEngine)
			if !ok {
				return fail()
			}
			containers, err := ce.ComposeContainers(ctx, project)
			if err != nil {
				return fail()
			}
			if len(containers) > 0 {
				return &GuardFinding{App: app}, nil
			}
			pe, ok := c.Engine.(projectVolumeEngine)
			if !ok {
				return fail()
			}
			volumes, err := pe.ProjectVolumes(ctx, project)
			if err != nil {
				return fail()
			}
			if len(volumes) > 0 {
				sort.Strings(volumes)
				return &GuardFinding{App: app, Volume: volumes[0]}, nil
			}
			file, ok := sub.ComposeFiles[app]
			if !ok || manifest.CheckComposeFile([]byte(file)) != nil {
				return fail()
			}
			names, err = manifest.ComposeFixedVolumes([]byte(file))
			if err != nil {
				return fail()
			}
		} else {
			for _, role := range sortedKeys(a.Roles) {
				service := a.Roles[role].Service
				if service == "" {
					service = sub.Manifest.ServiceName(c.Environment, app, role)
				}
				_, err := c.Engine.InspectService(ctx, service)
				if err == nil {
					return &GuardFinding{App: app}, nil
				}
				if !statusIs(err, 404) {
					return fail()
				}
			}
			names = declaredVolumes(a, func(volume string) string { return sub.Manifest.VolumeName(c.Environment, volume) })
		}
		for _, volume := range names {
			if !engine.ValidName(volume) {
				return fail()
			}
		}
		if len(names) > 0 {
			if _, ok := c.Engine.(interface {
				VolumeExists(context.Context, string) (bool, error)
			}); !ok {
				return fail()
			}
			volumes, err := c.existingVolumes(ctx, names)
			if err != nil {
				return fail()
			}
			if len(volumes) > 0 {
				return &GuardFinding{App: app, Volume: volumes[0]}, nil
			}
		}
	}
	return nil, nil
}
