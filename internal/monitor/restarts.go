package monitor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"berthy/internal/api"
)

const (
	// RestartWindow is how far back restarts and failed tasks are counted.
	RestartWindow = 10 * time.Minute
	// RestartLimit is how many restarts of one replica, or failed tasks of one role, inside the window make a loop.
	RestartLimit = 3
	// restartInterval is how often each restart check runs. The watch is fed by the inventory the monitor rebuilds.
	restartInterval = time.Minute
	// InventoryEvery is how often the agent reads the inventory the restart and down checks are fed with while Docker's event
	// feed is open. An event reads it sooner; the periodic read is the safety net for an event that never came.
	InventoryEvery = 3 * time.Minute
	// InventoryFallbackEvery is how often it reads the inventory while the feed is not open.
	InventoryFallbackEvery = time.Minute
	// inventoryStale is how old the last inventory may be before the checks report unknown: two reads of the slower cadence.
	inventoryStale = 2 * InventoryEvery
)

// observation is one restart count of a replica, seen at one time.
type observation struct {
	at    time.Time
	count int
}

// roleRecord is what the watch keeps about one role: the observations of each replica inside the window, and what the
// last inventory said about the role.
type roleRecord struct {
	replicas     map[string][]observation // by container ID, or task ID when there is no container
	current      int                      // replicas in the last inventory that report a restart count
	oom          bool                     // a replica in the last inventory was killed for memory
	taskFailures int                      // failed tasks of the role inside the window, as the last observation counted them
}

// RestartWatch judges restart loops. It keeps restart counts of replicas for RestartWindow, from the app inventory it is
// shown, and the failed Swarm tasks of each role. Swarm replaces a failed task with a new container whose restart count
// starts at zero, so for a Swarm role the failed tasks are what shows a loop. Until one RestartWindow has passed since
// the first inventory, a role with no issue found answers unknown so an agent restart cannot send a false recovery.
type RestartWatch struct {
	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	mu    sync.Mutex
	first time.Time              // when the first inventory was observed; zero before it
	seen  time.Time              // when the last inventory was observed; zero before the first
	roles map[string]*roleRecord // by RoleKey
}

// NewRestartWatch returns an empty watch.
func NewRestartWatch(now func() time.Time) *RestartWatch {
	return &RestartWatch{Now: now, roles: map[string]*roleRecord{}}
}

// RoleKey identifies one role of an app in the watch and in the failure counts Observe takes.
func RoleKey(ref api.AppRef, role string) string { return ref.Kind + "/" + ref.Name + "/" + role }

// RestartCheckName is the name of the restart check of one role: restarts:<app>/<role>. Unmanaged Swarm services carry
// the kind in their name, so they cannot collide with an app of the same name.
func RestartCheckName(ref api.AppRef, role string) string {
	app := ref.Name
	if ref.Kind == "swarm" {
		app = "swarm:" + ref.Name
	}
	return "restarts:" + app + "/" + role
}

// Observe records the replicas of apps at now, and the failed tasks of each role. failures is keyed by RoleKey; a role
// missing from it has none. The wiring calls it with each successful inventory.
func (w *RestartWatch) Observe(now time.Time, apps []api.App, failures map[string]int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	fresh := map[string]*roleRecord{}
	for _, app := range apps {
		for _, role := range app.Roles {
			key := RoleKey(app.Ref, role.Name)
			rec := &roleRecord{replicas: map[string][]observation{}, taskFailures: failures[key]}
			if old := w.roles[key]; old != nil {
				for id, obs := range old.replicas {
					rec.replicas[id] = obs
				}
			}
			for _, rep := range role.Replicas {
				if rep.OOMKilled != nil && *rep.OOMKilled {
					rec.oom = true
				}
				id := rep.ContainerID
				if id == "" {
					id = rep.TaskID
				}
				if id == "" || rep.RestartCount == nil {
					continue
				}
				rec.current++
				rec.record(id, observation{at: now, count: *rep.RestartCount})
			}
			prune(rec, now)
			fresh[key] = rec
		}
	}
	w.roles = fresh
	if w.first.IsZero() {
		w.first = now
	}
	w.seen = now
}

// record adds an observation of a replica. The same read can be shown to the watch again at a later rebuild: it is one
// observation, not several.
func (r *roleRecord) record(id string, o observation) {
	obs := r.replicas[id]
	if n := len(obs); n > 0 && obs[n-1].at.Equal(o.at) {
		obs[n-1] = o
		return
	}
	r.replicas[id] = append(obs, o)
}

// prune keeps the observations inside the window, and the last one at or before its start as the baseline that growth
// is measured from. A replica with nothing inside the window is dropped.
func prune(rec *roleRecord, now time.Time) {
	cutoff := now.Add(-RestartWindow)
	for id, obs := range rec.replicas {
		last := -1
		for i, o := range obs {
			if !o.at.After(cutoff) {
				last = i
			}
		}
		if last > 0 {
			obs = obs[last:]
		}
		if len(obs) == 0 || obs[len(obs)-1].at.Before(cutoff) {
			delete(rec.replicas, id)
			continue
		}
		rec.replicas[id] = obs
	}
}

// result judges one role at now. Without a recent inventory the answer is unknown, not ok.
func (w *RestartWatch) result(now time.Time, key string) Result {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen.IsZero() || now.Sub(w.seen) > inventoryStale {
		return Result{State: Unknown, Detail: "no recent Docker inventory"}
	}
	rec := w.roles[key]
	if rec == nil {
		return Result{State: OK, Detail: "no replicas"}
	}
	var issues []string
	worst := 0
	for _, obs := range rec.replicas {
		if growth := obs[len(obs)-1].count - obs[0].count; growth > worst {
			worst = growth
		}
	}
	minutes := int(RestartWindow / time.Minute)
	if worst >= RestartLimit {
		issues = append(issues, fmt.Sprintf("a replica restarted %d times in %d minutes", worst, minutes))
	}
	if rec.taskFailures >= RestartLimit {
		issues = append(issues, fmt.Sprintf("%d task failures in %dm", rec.taskFailures, minutes))
	}
	if rec.oom {
		issues = append(issues, "a replica was OOM killed")
	}
	if len(issues) > 0 {
		return Result{State: Warning, Detail: strings.Join(issues, "; ")}
	}
	if now.Sub(w.first) < RestartWindow {
		return Result{State: Unknown, Detail: "watching restarts since agent start"}
	}
	return Result{State: OK, Detail: fmt.Sprintf("%d replicas, no restart loop", rec.current)}
}

// Check returns the restart check of one role of an app.
func (w *RestartWatch) Check(ref api.AppRef, role string) Check {
	key := RoleKey(ref, role)
	return NewFunc(RestartCheckName(ref, role), restartInterval, 1, func(context.Context) Result {
		return w.result(w.now(), key)
	})
}

func (w *RestartWatch) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}
