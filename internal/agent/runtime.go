package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/jobs"
	"berthy/internal/metrics"
	"berthy/internal/scope"
	"berthy/internal/state"
	"berthy/internal/telemetry"
)

// Shared is what the agent has once and every hosted environment of the process uses.
type Shared struct {
	Engine    *engine.Engine
	Collector *metrics.Collector
	Log       func(string)
}

// Runtime is one hosted environment while the agent runs: its state, controller, reports, monitor, scheduler, backups and
// telemetry, and the handler of its API.
type Runtime struct {
	Config  Config
	Handler http.Handler

	log        func(string)
	deployment *deploy.Controller
	reports    *Reports
	monitoring *Monitoring
	scheduler  *jobs.Scheduler
	backups    *Backups
	exporter   *telemetry.Exporter
	// hosted is every hosted environment of this agent, set at Open. visible uses it to hide containers whose labels name
	// another one.
	hosted []scope.ID
	// peerClaims is the claims-only view of the other hosted environments, set at Open. Stats, the stats stream and export
	// read it once per sample. Nil, or an error from it, is fail-closed.
	peerClaims func(context.Context) (map[state.Claim]bool, error)

	// Set by Start. Close waits for each worker that was started.
	cancel                                                       context.CancelFunc
	reportsDone, recoveryDone, monitorDone, jobsDone, exportDone chan struct{}
	opsEnded                                                     bool
}

// OpenRuntime opens the environment's state and builds its controller, its workers and its handler. It starts nothing: no
// goroutine runs, nothing reads Docker and nothing is recovered until Start.
func OpenRuntime(sh Shared, c Config) (*Runtime, error) {
	if c.StateDir == "" || c.KeyFile == "" {
		return nil, errors.New("a hosted environment needs state_dir and key_file")
	}
	d, err := OpenDeployment(c, sh.Engine)
	if err != nil {
		return nil, err
	}
	d.Log = sh.Log
	r := &Runtime{Config: c, log: sh.Log, deployment: d}
	r.monitoring = NewMonitoring(c, sh.Engine, sh.Collector, d, sh.Log)
	r.reports = NewReports(c, d.Store, sh.Log)
	r.reports.wakeAppChecks = func() { r.monitoring.wakeChecks("drift:", "down:", "restarts:") }
	// The observer is set before recovery, so an operation that recovery ends is reported too.
	d.Observer = r.reports
	d.ReportMaintenance = r.logf
	// The checks at the end of a deploy reach each role's port from the overlay helper the monitor keeps.
	d.Reacher = r.monitoring
	r.scheduler = NewJobs(c, sh.Engine, d, sh.Log)
	r.backups = NewBackups(c, sh.Engine, d, r.scheduler, r.monitoring.HelperImage, sh.Log)
	r.exporter = NewTelemetry(c, sh.Collector, d, sh.Log)
	r.Handler = newDeploymentHandler(c, sh.Engine, sh.Collector, d, r.exporter, r.scheduler, r.backups, newLimits(), r.keep)
	return r, nil
}

// keep returns a filter over one claims snapshot, for one stats response, stream emission or export.
func (r *Runtime) keep() func(map[string]string) bool {
	own := r.ownClaims()
	foreign, ok := r.foreignClaims()
	return func(labels map[string]string) bool {
		return r.shown(labels, own, foreign, ok)
	}
}

// visible reports whether a container belongs in this environment's stats and export. Tests call it once per container;
// stats, the stream and export call keep so they read claims once per sample.
func (r *Runtime) visible(labels map[string]string) bool {
	return r.keep()(labels)
}

// shown is the filter for one claims snapshot.
func (r *Runtime) shown(labels map[string]string, own, foreign map[state.Claim]bool, ok bool) bool {
	if !ok {
		return r.ownContainer(labels, own)
	}
	if name := labels["com.docker.swarm.service.name"]; name != "" {
		claim := state.Claim{Kind: state.ClaimService, Name: name}
		if foreign[claim] {
			return false
		}
		if own[claim] {
			return true
		}
	}
	if name := labels["com.docker.compose.project"]; name != "" && foreign[state.Claim{Kind: state.ClaimComposeProject, Name: name}] {
		return false
	}
	id := r.Config.ID()
	if id.Owns(labels) {
		return true
	}
	for _, other := range r.hosted {
		if other != id && other.Owns(labels) {
			return false
		}
	}
	return true
}

func (r *Runtime) ownClaims() map[state.Claim]bool {
	out := map[state.Claim]bool{}
	if r.deployment == nil || r.deployment.Store == nil {
		return out
	}
	claims, err := r.deployment.Store.Claims(context.Background())
	if err != nil {
		return out
	}
	for _, c := range claims {
		out[c] = true
	}
	return out
}

func (r *Runtime) foreignClaims() (map[state.Claim]bool, bool) {
	if r.peerClaims == nil {
		return nil, false
	}
	f, err := r.peerClaims(context.Background())
	if err != nil {
		return nil, false
	}
	return f, true
}

func (r *Runtime) ownContainer(labels map[string]string, own map[state.Claim]bool) bool {
	if name := labels["com.docker.swarm.service.name"]; name != "" {
		return own[state.Claim{Kind: state.ClaimService, Name: name}]
	}
	if name := labels["com.docker.compose.project"]; name != "" {
		return own[state.Claim{Kind: state.ClaimComposeProject, Name: name}]
	}
	return r.Config.ID().Owns(labels)
}

// Start recovers the environment's operations and starts its workers under ctx. Requests are served only after it. When it
// fails, Close still releases what it started.
func (r *Runtime) Start(ctx context.Context) error {
	ctx, r.cancel = context.WithCancel(ctx)
	if err := r.recoverForStart(ctx); err != nil {
		return errors.New(StartupRecovery.String())
	}
	r.startWorkers(ctx)
	return nil
}

func (r *Runtime) recoverForStart(ctx context.Context) error {
	r.reportsDone = make(chan struct{})
	go func() {
		defer close(r.reportsDone)
		r.reports.Run(ctx)
	}()
	recoveryCtx, done := context.WithTimeout(ctx, 30*time.Second)
	err := r.deployment.Recover(recoveryCtx)
	done()
	if err != nil && !errors.Is(err, deploy.ErrRecoveryPending) {
		return err
	}
	// Recover reports the first move into recovery_required as success, so the lock is checked as well.
	if op, activeErr := r.deployment.Store.Active(ctx); activeErr == nil && op.State == "recovery_required" {
		r.logf(recoveryNotice(op.ID))
	} else if errors.Is(err, deploy.ErrRecoveryPending) {
		r.logf(recoveryNotice(""))
	}
	// A running agent owns no interactive session of an earlier process: their containers go, in whatever state they are.
	cleanupCtx, cleaned := context.WithTimeout(ctx, 30*time.Second)
	err = r.deployment.RemoveOrphanSessions(cleanupCtx)
	cleaned()
	if err != nil {
		r.logf("session and job container cleanup failed")
	}
	return nil
}

func (r *Runtime) startWorkers(ctx context.Context) {
	r.recoveryDone = make(chan struct{})
	go func() {
		defer close(r.recoveryDone)
		retryRecovery(ctx, 10*time.Second, r.deployment.Store.Active, r.deployment.Recover)
	}()
	r.monitorDone = make(chan struct{})
	go func() {
		defer close(r.monitorDone)
		r.monitoring.Run(ctx)
	}()
	r.jobsDone = make(chan struct{})
	go func() {
		defer close(r.jobsDone)
		r.scheduler.Run(ctx)
	}()
	r.backups.Start(ctx)
	if r.exporter != nil {
		r.exportDone = make(chan struct{})
		go func() {
			defer close(r.exportDone)
			r.exporter.Run(ctx)
		}()
	}
}

// CloseSessions closes the open interactive sessions: http.Server.Shutdown does not track a connection a handler took over.
func (r *Runtime) CloseSessions(ctx context.Context) { r.deployment.CloseSessions(ctx) }

// Close stops what Start started and then closes the state, last started first. It may follow a failed Start, and an
// OpenRuntime without Start.
func (r *Runtime) Close() {
	r.stop()
	r.wait()
	r.closeStore()
}

func (r *Runtime) stop() {
	if r.cancel != nil {
		r.cancel()
	}
}

func (r *Runtime) wait() {
	// The workers stop before the store closes. The last export is flushed after the monitor's stop, and the runs of the
	// scheduler and the backups end and are recorded.
	awaitWorker(r.exportDone)
	r.backups.Wait()
	awaitWorker(r.jobsDone)
	awaitWorker(r.monitorDone)
	awaitWorker(r.recoveryDone)
	// A running operation records its outcome and the reports hear of it. The agent no longer takes requests by now, so no
	// operation starts after this wait.
	if r.deployment != nil {
		waitCtx, cancelWait := context.WithTimeout(context.Background(), operationGrace)
		r.opsEnded = r.deployment.Drain(waitCtx) == nil
		cancelWait()
	}
	// The queued records and messages are sent before the store closes.
	r.waitReports()
}

func (r *Runtime) waitReports() {
	awaitWorker(r.reportsDone)
	if r.reports != nil {
		r.reports.Wait()
	}
}

func (r *Runtime) closeStore() {
	if r.deployment == nil || r.deployment.Store == nil {
		return
	}
	// The store is closed only when no operation is left running: one that is would write to a closed store. It is left open
	// otherwise, as it is when the agent is killed, and the next start recovers the operation.
	if r.opsEnded {
		r.deployment.Store.Close()
		return
	}
	r.logf("operations still running at exit: they are recovered at the next start")
}

// awaitWorker waits for a worker that Start started; done is nil for one that was not.
func awaitWorker(done chan struct{}) {
	if done != nil {
		<-done
	}
}

func (r *Runtime) logf(msg string) {
	if r.log != nil {
		r.log(msg)
	}
}

// operationGrace is how long a stopping agent lets a running operation record its outcome before it leaves it to the recovery of the
// next start.
const operationGrace = 30 * time.Second

func retryRecovery(ctx context.Context, interval time.Duration, active func(context.Context) (state.Operation, error), recover func(context.Context) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// A tick can be waiting when the context ends, and select picks between two ready cases at random.
			if ctx.Err() != nil {
				return
			}
			attempt, done := context.WithTimeout(ctx, 30*time.Second)
			op, err := active(attempt)
			if err == nil && op.State == "recovery_required" {
				_ = recover(attempt)
			}
			done()
		}
	}
}

// recoveryNotice is the line printed at startup while an operation waits for recovery. id is empty when it is unknown.
func recoveryNotice(id string) string {
	if id == "" {
		id = "OPERATION_ID"
	}
	return fmt.Sprintf(`deployment recovery required; check "berthy lock status" and resolve with "berthy recovery resolve %s -m REASON"`, id)
}
