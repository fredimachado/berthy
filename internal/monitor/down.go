package monitor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"berthy/internal/api"
)

const (
	// DownConfirm is how long a role may run fewer replicas than it should before its down check is critical.
	DownConfirm = 2 * time.Minute
	// DownQuietAfter is how long after an operation on an app its down checks are not judged.
	DownQuietAfter = 2 * time.Minute
	// downInterval is how often each down check runs. The watch is fed by the inventory the monitor rebuilds.
	downInterval = 30 * time.Second
)

// downRecord is what the watch keeps about one role of an app, or one Compose service: how many replicas it should run,
// how many run and are healthy, and since when it has been short.
type downRecord struct {
	app       api.AppRef
	role      string
	want      int       // replicas the role should run
	healthy   int       // replicas that run and are not unhealthy
	unhealthy int       // replicas that run and are unhealthy
	exited    bool      // the only replica is an exited container
	exitCode  *int      // the exit code of that container, when the inventory gave one
	since     time.Time // when the role was first seen short in a row; zero while it is not short, or while its app is quiet
	quiet     bool      // an operation on the app runs or has just ended, so the role is not judged
}

// DownWatch judges whether each role of an app runs the replicas it should, and each Compose service its containers. It
// is fed the same inventory as the restart watch, and keeps the roles it has seen in the watch's memory, so that a
// Compose service whose containers are gone counts as stopped.
type DownWatch struct {
	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	mu    sync.Mutex
	seen  time.Time              // when the last inventory was observed; zero before the first
	roles map[string]*downRecord // by RoleKey
}

// NewDownWatch returns an empty watch.
func NewDownWatch(now func() time.Time) *DownWatch {
	return &DownWatch{Now: now, roles: map[string]*downRecord{}}
}

// Observe records the roles of apps at now. quiet reports whether an app is not judged now, because an operation on it runs
// or has just ended; its roles then restart their clocks. A nil quiet means no app is quiet.
//
// A role seen before that is not in apps any more counts as stopped, with the replicas it had, while its app is observed and
// not quiet. A Compose service whose containers are gone is such a role, and so is a Compose project with none: the
// caller adds an app with no roles for a project of the manifest that has no containers.
func (w *DownWatch) Observe(now time.Time, apps []api.App, quiet func(api.AppRef) bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	isQuiet := func(ref api.AppRef) bool { return quiet != nil && quiet(ref) }
	fresh := map[string]*downRecord{}
	observed := map[api.AppRef]bool{}
	for _, app := range apps {
		observed[app.Ref] = true
		for _, role := range app.Roles {
			key := RoleKey(app.Ref, role.Name)
			fresh[key] = observeRole(now, app.Ref, role, w.roles[key], isQuiet(app.Ref))
		}
	}
	for key, old := range w.roles {
		if _, ok := fresh[key]; ok || !observed[old.app] || isQuiet(old.app) {
			continue
		}
		fresh[key] = observeRole(now, old.app, api.Role{Name: old.role}, old, false)
	}
	w.roles = fresh
	w.seen = now
}

// observeRole returns the record of one role at now. prev is the record of the last observation, or nil.
func observeRole(now time.Time, app api.AppRef, role api.Role, prev *downRecord, quiet bool) *downRecord {
	rec := &downRecord{app: app, role: role.Name, quiet: quiet}
	var replicas []api.Replica
	for _, r := range role.Replicas {
		// A finished one-shot is a job that is done: the service neither wants it running nor counts it as down.
		if !r.FinishedOneShot() {
			replicas = append(replicas, r)
		}
	}
	switch {
	case role.Desired != nil:
		rec.want = int(*role.Desired)
	case len(role.Replicas) > 0:
		// A Compose service has no desired count: the containers it has are the count it should run.
		rec.want = len(replicas)
	case prev != nil:
		// A Swarm service that is gone keeps the count it had.
		rec.want = prev.want
	}
	for _, r := range replicas {
		switch {
		case r.State != "running":
		case r.Health == "unhealthy":
			rec.unhealthy++
		default:
			rec.healthy++
		}
	}
	rec.exited = len(replicas) == 1 && replicas[0].State == "exited"
	if rec.exited {
		rec.exitCode = replicas[0].ExitCode
	}
	if rec.healthy < rec.want && !quiet {
		rec.since = now
		if prev != nil && !prev.since.IsZero() {
			rec.since = prev.since
		}
	}
	return rec
}

// result judges one role at now. Without a recent inventory the answer is unknown, not ok. A role that runs fewer replicas
// than it should is unknown for DownConfirm, and critical after that. Unknown never alerts, so a short role that has not
// been confirmed never sends a recovered message, and a critical one is not alerted again once it is confirmed.
func (w *DownWatch) result(now time.Time, key string) Result {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen.IsZero() || now.Sub(w.seen) > inventoryStale {
		return Result{State: Unknown, Detail: "no recent Docker inventory"}
	}
	rec := w.roles[key]
	if rec == nil {
		return Result{State: OK, Detail: "no replicas"}
	}
	if rec.quiet {
		return Result{State: Unknown, Detail: "operation on " + rec.app.Name + " running or just ended"}
	}
	if rec.healthy >= rec.want {
		if rec.want == 0 {
			return Result{State: OK, Detail: "no replicas wanted"}
		}
		return Result{State: OK, Detail: fmt.Sprintf("%d of %d replicas running", min(rec.healthy, rec.want), rec.want)}
	}
	short := shortfall(rec)
	age := now.Sub(rec.since)
	if age < DownConfirm {
		return Result{State: Unknown, Detail: short + ", not yet confirmed"}
	}
	return Result{State: Critical, Detail: fmt.Sprintf("%s for %dm", short, int(age/time.Minute))}
}

// shortfall says what was seen of a role that runs fewer replicas than it should.
func shortfall(rec *downRecord) string {
	switch {
	case rec.unhealthy > 0:
		return fmt.Sprintf("%d of %d replicas healthy (%d unhealthy)", rec.healthy, rec.want, rec.unhealthy)
	case rec.want == 1 && rec.exited && rec.exitCode != nil:
		return fmt.Sprintf("exited with code %d", *rec.exitCode)
	case rec.want == 1 && rec.exited:
		return "container exited"
	default:
		return fmt.Sprintf("%d of %d replicas running", rec.healthy, rec.want)
	}
}

// Checks returns the down check of every role the watch holds, including roles counted as stopped.
func (w *DownWatch) Checks() []Check {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]Check, 0, len(w.roles))
	for _, rec := range w.roles {
		out = append(out, w.Check(rec.app, rec.role))
	}
	return out
}

// Check returns the down check of one role of an app.
func (w *DownWatch) Check(ref api.AppRef, role string) Check {
	key := RoleKey(ref, role)
	return NewFunc(DownCheckName(ref, role), downInterval, 1, func(context.Context) Result {
		return w.result(w.now(), key)
	})
}

// DownCheckName is the name of the down check of one role: down:<app>/<role>. The app is named as the restart checks name it.
func DownCheckName(ref api.AppRef, role string) string { return "down:" + ref.Name + "/" + role }

func (w *DownWatch) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}
