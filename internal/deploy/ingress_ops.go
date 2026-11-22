package deploy

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"berthy/internal/cloudflare"
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// maxConflictEvents bounds how many conflicts an operation records as events.
const maxConflictEvents = 10

// cloudflareClient builds an API client from the named workload secret.
func (c *Controller) cloudflareClient(ctx context.Context, tokenSecret string) (*cloudflare.Client, error) {
	if c.Store == nil {
		return nil, errCloudflareSecret
	}
	info, value, err := c.Store.Secret(ctx, tokenSecret)
	if err != nil || info.Registry != "" || len(value) == 0 {
		return nil, errCloudflareSecret
	}
	return &cloudflare.Client{BaseURL: c.CloudflareURL, Token: string(value)}, nil
}

// ingressTarget is the configuration an ingress run acts on: the manifest's, or the stored one when the run removes
// managed ingress.
type ingressTarget struct {
	removal    bool
	account    string
	secret     string
	tunnelName string
	dns        bool
	cf         *cloudflare.Client
	tunnel     *cloudflare.Tunnel // nil when the tunnel does not exist yet
}

// ingressTargetFor reads the Cloudflare side of an ingress run and checks it: the token works, and the tunnel with the
// configured name, when it exists, is remotely managed. It changes nothing.
func (c *Controller) ingressTargetFor(ctx context.Context, m manifest.Manifest) (ingressTarget, error) {
	if c.Store == nil {
		return ingressTarget{}, errors.New("state unavailable")
	}
	var t ingressTarget
	if m.Ingress == nil {
		st, err := c.Store.Ingress(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return t, errIngressNotConfigured
		}
		if err != nil {
			return t, errIngressPublish
		}
		t.removal = true
		t.account, t.secret, t.tunnelName, t.dns = st.AccountID, st.APITokenSecret, st.TunnelName, st.DNS
	} else {
		cfg := m.Ingress.Cloudflare
		t.account, t.secret, t.tunnelName = cfg.AccountID, cfg.APITokenSecret, m.TunnelName(c.Environment)
		t.dns = cfg.DNS == nil || *cfg.DNS
	}
	cf, err := c.cloudflareClient(ctx, t.secret)
	if err != nil {
		return t, err
	}
	t.cf = cf
	if err = cf.VerifyToken(ctx, t.account); err != nil {
		return t, errCloudflareToken
	}
	tunnel, err := cf.FindTunnel(ctx, t.account, t.tunnelName)
	if err != nil {
		return t, errCloudflareUnavailable
	}
	if tunnel != nil && !tunnel.RemoteConfig {
		return t, errTunnelNotRemote
	}
	t.tunnel = tunnel
	return t, nil
}

// newIngressSync returns the sync engine for an ingress run. tunnelID is empty while the tunnel does not exist.
func (c *Controller) newIngressSync(t ingressTarget, opID, tunnelID string) *ingressSync {
	return &ingressSync{c: c, cf: t.cf, opID: opID, st: state.IngressState{AccountID: t.account, APITokenSecret: t.secret, TunnelID: tunnelID, TunnelName: t.tunnelName, DNS: t.dns}}
}

// tunnelIDOf returns the tunnel's ID, or "" when there is no tunnel.
func tunnelIDOf(t *cloudflare.Tunnel) string {
	if t == nil {
		return ""
	}
	return t.ID
}

// everyRoute lets a run remove any route Berthy owns that the manifest no longer asks for.
func everyRoute(state.IngressRoute) bool { return true }

// applyRemovable decides which owned routes an ingress apply may remove. The manifest does not ask for the routes of an app
// it no longer names, but when that app still has an applied release its services keep running, so its routes stay until
// berthy remove takes the app down. The notes name each such app that owns a route. An app with no applied release has
// nothing left to serve, so its routes may go.
func (c *Controller) applyRemovable(ctx context.Context, m manifest.Manifest) (func(state.IngressRoute) bool, []string, error) {
	if c.Store == nil {
		return nil, nil, errors.New("state unavailable")
	}
	applied, err := c.Store.AppliedApps(ctx)
	if err != nil {
		return nil, nil, err
	}
	routes, err := c.Store.IngressRoutes(ctx, "")
	if err != nil {
		return nil, nil, err
	}
	owners := map[string]bool{}
	for _, r := range routes {
		owners[r.App] = true
	}
	kept := map[string]bool{}
	var notes []string
	for _, app := range applied {
		if _, named := m.Apps[app]; named || app == manifest.ConnectorAppName {
			continue
		}
		kept[app] = true
		if owners[app] {
			notes = append(notes, fmt.Sprintf("app %s is deployed but not in the manifest; run berthy remove %s", app, app))
		}
	}
	return func(r state.IngressRoute) bool { return !kept[r.App] }, notes, nil
}

// connectorRequest is the deploy request that runs the connector app, with the commit and checkout flag of from. withToken is
// false only before the tunnel token is stored, so the connector can still be planned.
func connectorRequest(m manifest.Manifest, from Request, environment string, withToken bool) (Request, error) {
	app, version, err := m.ConnectorApp()
	if err != nil {
		return Request{}, err
	}
	if !withToken {
		app.Env.Secret = nil
	}
	synth := manifest.Manifest{Project: m.Project, Network: m.NetworkName(environment), Protected: m.Protected, Registries: m.Registries, Apps: map[string]manifest.App{manifest.ConnectorAppName: app}, Connector: true}
	return Request{App: manifest.ConnectorAppName, Version: version, Commit: from.Commit, Modified: from.Modified, Manifest: synth, Kind: "deploy"}, nil
}

// tokenStored reports whether the connector's tunnel token is already in the secret store.
func (c *Controller) tokenStored(ctx context.Context) bool {
	_, _, err := c.Store.Secret(ctx, manifest.TunnelTokenSecret)
	return err == nil
}

// tokenMatches reports whether the stored tunnel token is token. The stored value is never returned.
func (c *Controller) tokenMatches(ctx context.Context, token string) bool {
	_, value, err := c.Store.Secret(ctx, manifest.TunnelTokenSecret)
	return err == nil && string(value) == token
}

// storeTunnelToken stores the connector's tunnel token for the operation that holds the lock, unless the stored value
// already matches. It reports whether it stored a different value. The token is never printed.
func (c *Controller) storeTunnelToken(ctx context.Context, opID, token string) (bool, error) {
	if c.tokenMatches(ctx, token) {
		return false, nil
	}
	if err := c.Store.PutOperationSecret(ctx, opID, manifest.TunnelTokenSecret, []byte(token)); err != nil {
		return false, err
	}
	return true, nil
}

// connectorCurrent reports whether the running connector already matches what this apply would deploy, so the rollout
// can be skipped. It requires the same pinned image as the connector's applied release, the same manifest that release
// stored (connector app, registries, network, project and protection), the tunnel token unchanged in this run, and for
// every bound role: the live service is still the bound one, runs the role's replicas, and was started with the stored
// tunnel token. Health is not checked here; status and doctor report it. Anything that cannot be read counts as not
// current, so the rollout runs. It changes nothing.
func (c *Controller) connectorCurrent(ctx context.Context, creq Request, image string, tokenChanged bool) bool {
	if tokenChanged || c.Engine == nil {
		return false
	}
	rel, err := c.Store.Applied(ctx, manifest.ConnectorAppName)
	if err != nil || rel.Digest != image {
		return false
	}
	prev, err := manifest.DecodeRelease(rel.Manifest, true)
	if err != nil {
		return false
	}
	stored, err := json.Marshal(prev)
	if err != nil {
		return false
	}
	wanted, err := json.Marshal(creq.Manifest)
	if err != nil || !bytes.Equal(stored, wanted) {
		return false
	}
	app := creq.Manifest.Apps[manifest.ConnectorAppName]
	bindings, err := c.Store.Bindings(ctx, manifest.ConnectorAppName)
	if err != nil || len(bindings) != len(app.Roles) {
		return false
	}
	_, token, err := c.Store.Secret(ctx, manifest.TunnelTokenSecret)
	if err != nil {
		return false
	}
	environment := manifest.TunnelTokenSecret + "=" + string(token)
	for _, b := range bindings {
		role, ok := app.Roles[b.Role]
		if !ok {
			return false
		}
		live, err := c.Engine.InspectService(ctx, b.Name)
		if err != nil || live.ID != b.ServiceID {
			return false
		}
		var spec struct {
			TaskTemplate struct {
				ContainerSpec struct{ Env []string }
			}
			Mode struct{ Replicated *struct{ Replicas int } }
		}
		if json.Unmarshal(live.Spec, &spec) != nil || spec.Mode.Replicated == nil {
			return false
		}
		replicas := 1
		if role.Replicas != nil {
			replicas = *role.Replicas
		}
		if spec.Mode.Replicated.Replicas != replicas || !slices.Contains(spec.TaskTemplate.ContainerSpec.Env, environment) {
			return false
		}
	}
	return true
}

// recordConflicts appends an event for each conflict, so the operator can see why the run stopped.
func (c *Controller) recordConflicts(ctx context.Context, opID string, conflicts []string) {
	for i, msg := range conflicts {
		if i == maxConflictEvents {
			break
		}
		data, err := json.Marshal(map[string]string{"reason": msg})
		if err != nil {
			continue
		}
		_ = c.Store.AppendEvent(ctx, opID, state.Event{Kind: "conflict", Data: data})
	}
}

// ingressFailure is the text an operation records for err: a public refusal as it is, anything else as fallback.
func ingressFailure(err error, fallback Refusal) string {
	var r Refusal
	if errors.As(err, &r) {
		return string(r)
	}
	return string(fallback)
}

// publishScope reports whether a deploy of app must publish: the app declares hostnames, or Berthy owns routes for it,
// which may need removing. The connector app never publishes. It also returns the routes Berthy owns for app.
func (c *Controller) publishScope(ctx context.Context, m manifest.Manifest, app string) (bool, []state.IngressRoute, error) {
	if app == manifest.ConnectorAppName {
		return false, nil, nil
	}
	if c.Store == nil {
		return false, nil, errors.New("state unavailable")
	}
	owned, err := c.Store.IngressRoutes(ctx, app)
	if err != nil {
		return false, nil, err
	}
	return hasHostnames(m.Apps[app]) || len(owned) > 0, owned, nil
}

// ingressPublish is the publish step of one deploy: the sync engine for the environment's tunnel, the routes the release
// wants, and the routes it may remove, which are the app's own.
type ingressPublish struct {
	sync      *ingressSync
	wants     []RouteWant
	removable func(state.IngressRoute) bool
}

// ownedBy lets a deploy of app remove the routes Berthy created for that app, and no other.
func ownedBy(app string) func(state.IngressRoute) bool {
	return func(r state.IngressRoute) bool { return r.App == app }
}

// publishSync returns the sync engine for a deploy's publish. It reads the stored ingress configuration, not the
// manifest's, so a deploy still reaches its app's routes after the manifest has dropped managed ingress. A manifest that
// names ingress must name the account it was applied to. When adds is true the publish adds or keeps routes, which only
// a running connector can serve, so the connector app must have an applied release. A publish that only removes routes
// needs no connector, so routes can always be taken down. It reads Cloudflare and changes nothing.
func (c *Controller) publishSync(ctx context.Context, m manifest.Manifest, opID string, adds bool) (*ingressSync, error) {
	st, err := c.Store.Ingress(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errIngressNotApplied
	}
	if err != nil {
		return nil, err
	}
	if adds {
		if _, err = c.Store.Applied(ctx, manifest.ConnectorAppName); err != nil {
			return nil, errIngressNotApplied
		}
	}
	if m.Ingress != nil && m.Ingress.Cloudflare != nil && m.Ingress.Cloudflare.AccountID != st.AccountID {
		return nil, errIngressChanged
	}
	cf, err := c.cloudflareClient(ctx, st.APITokenSecret)
	if err != nil {
		return nil, err
	}
	if err = cf.VerifyToken(ctx, st.AccountID); err != nil {
		return nil, errCloudflareToken
	}
	return &ingressSync{c: c, cf: cf, opID: opID, st: st}, nil
}

// publishCheck is the check step of a deploy. When the deploy publishes, it confirms before anything changes that managed
// ingress is applied, that the token works, and that the routes and records the release wants are free. A conflict is
// recorded on the operation and stops the deploy. It returns nil when the deploy publishes nothing.
func (c *Controller) publishCheck(ctx context.Context, opID string, m manifest.Manifest, app string, wants []RouteWant, replace bool) (*ingressPublish, error) {
	publishes, _, err := c.publishScope(ctx, m, app)
	if err != nil || !publishes {
		return nil, err
	}
	sy, err := c.publishSync(ctx, m, opID, len(wants) > 0)
	if err != nil {
		return nil, err
	}
	ing := &ingressPublish{sync: sy, wants: wants, removable: ownedBy(app)}
	plan, _, _, err := sy.plan(ctx, wants, ing.removable, replace)
	if err != nil {
		return nil, err
	}
	if len(plan.Conflicts) > 0 {
		c.recordConflicts(ctx, opID, plan.Conflicts)
		return nil, errIngressConflict
	}
	return ing, nil
}

// publishIngress is the publish step of a deploy. It runs once the new version is healthy. It plans again from fresh
// reads, then writes the routes and records. A nil publish does nothing.
func (c *Controller) publishIngress(ctx context.Context, opID string, ing *ingressPublish, replace bool) error {
	if ing == nil {
		return nil
	}
	plan, cfg, steps, err := ing.sync.plan(ctx, ing.wants, ing.removable, replace)
	if err != nil {
		return err
	}
	if len(plan.Conflicts) > 0 {
		c.recordConflicts(ctx, opID, plan.Conflicts)
		return errIngressConflict
	}
	return ing.sync.apply(ctx, plan, cfg, steps)
}

// dryRunPublish adds to plan the routes and records that a deploy's publish would change, when the deploy publishes.
// Nothing is written. A refusal that would stop the real publish becomes a blocker of the plan, not an error.
func (c *Controller) dryRunPublish(ctx context.Context, plan *Plan, m manifest.Manifest, app string, wants []RouteWant, replace bool) error {
	publishes, _, err := c.publishScope(ctx, m, app)
	if err != nil || !publishes {
		return err
	}
	sy, err := c.publishSync(ctx, m, "", len(wants) > 0)
	var ip IngressPlan
	if err == nil {
		ip, _, _, err = sy.plan(ctx, wants, ownedBy(app), replace)
	}
	if err != nil {
		var r Refusal
		if errors.As(err, &r) {
			plan.Blockers = append(plan.Blockers, string(r))
			return nil
		}
		return err
	}
	plan.Ingress = &ip
	plan.Blockers = append(plan.Blockers, ip.Conflicts...)
	return nil
}

// planIngress describes an ingress apply, or the removal of managed ingress, without writing anything. No tunnel is
// created, no route or record is written, and no connector is deployed.
func (c *Controller) planIngress(ctx context.Context, actor Actor, req Request) (Plan, error) {
	m := req.Manifest
	if err := m.Validate(); err != nil {
		return Plan{}, err
	}
	t, err := c.ingressTargetFor(ctx, m)
	if err != nil {
		return Plan{}, err
	}
	wants, err := c.wantsFor(ctx, m)
	if err != nil {
		return Plan{}, err
	}
	ing := c.newIngressSync(t, "", tunnelIDOf(t.tunnel))
	removable, notes := everyRoute, []string(nil)
	if !t.removal {
		if removable, notes, err = c.applyRemovable(ctx, m); err != nil {
			return Plan{}, err
		}
	}
	ip, _, _, err := ing.plan(ctx, wants, removable, req.ReplaceExisting)
	if err != nil {
		return Plan{}, err
	}
	ip.Notes = notes
	ip.CreateTunnel = !t.removal && t.tunnel == nil
	p := Plan{App: manifest.ConnectorAppName, Roles: []string{}, Blockers: ip.Conflicts}
	if t.removal {
		ip.RemoveConnector = true
	} else {
		creq, err := connectorRequest(m, req, c.Environment, c.tokenStored(ctx))
		if err != nil {
			return Plan{}, err
		}
		cp, err := c.Plan(ctx, actor, creq)
		if err != nil {
			return Plan{}, err
		}
		p.Roles, p.Image, p.Tag = cp.Roles, cp.Image, cp.Tag
		ip.Connector = &cp
		// The token is compared, never shown: an apply would store it only if it differs. A tunnel that does not exist
		// yet gets a new token, and a token that cannot be read stops the apply, so both count as a change.
		tokenChanged := true
		if t.tunnel != nil {
			token, err := t.cf.TunnelToken(ctx, t.account, t.tunnel.ID)
			if err != nil {
				p.Blockers = append(p.Blockers, errTunnelToken.Error())
			} else {
				tokenChanged = !c.tokenMatches(ctx, token)
			}
		}
		ip.ConnectorUnchanged = c.connectorCurrent(ctx, creq, cp.Image, tokenChanged)
	}
	p.Ingress = &ip
	return p, nil
}

// runIngress applies managed ingress to the environment, or removes it when the manifest has no ingress block. The
// checks run first, and nothing changes until they pass.
func (c *Controller) runIngress(ctx context.Context, op state.Operation, actor Actor, req Request) state.Outcome {
	fail := func(err error) state.Outcome {
		return state.Outcome{State: "failed", Err: ingressFailure(err, errIngressPublish)}
	}
	if req.ReplaceExisting && !actor.Admin {
		return fail(errReplaceAdmin)
	}
	m := req.Manifest
	if err := m.Validate(); err != nil {
		return fail(err)
	}
	if m.Ingress == nil {
		return c.runIngressRemoval(ctx, op, req)
	}
	if err := c.checkNetworkNames(ctx, req); err != nil {
		return fail(err)
	}
	t, err := c.ingressTargetFor(ctx, m)
	if err != nil {
		return fail(err)
	}
	wants, err := c.wantsFor(ctx, m)
	if err != nil {
		return fail(err)
	}
	ing := c.newIngressSync(t, op.ID, tunnelIDOf(t.tunnel))
	removable, notes, err := c.applyRemovable(ctx, m)
	if err != nil {
		return fail(err)
	}
	for _, note := range notes {
		if data, e := json.Marshal(map[string]string{"step": note}); e == nil {
			_ = c.Store.AppendEvent(ctx, op.ID, state.Event{Kind: "step", Data: data})
		}
	}
	plan, _, _, err := ing.plan(ctx, wants, removable, req.ReplaceExisting)
	if err != nil {
		return fail(err)
	}
	if len(plan.Conflicts) > 0 {
		c.recordConflicts(ctx, op.ID, plan.Conflicts)
		return fail(errIngressConflict)
	}
	// The connector must be deployable before anything is created in Cloudflare.
	preflight, err := connectorRequest(m, req, c.Environment, c.tokenStored(ctx))
	if err != nil {
		return fail(err)
	}
	cp, err := c.Plan(ctx, actor, preflight)
	if err != nil {
		return state.Outcome{State: "failed", Err: "deployment preflight failed"}
	}
	tunnel := t.tunnel
	if tunnel == nil {
		created, err := t.cf.CreateTunnel(ctx, t.account, t.tunnelName)
		if err != nil {
			return fail(errIngressPublish)
		}
		tunnel = &created
	}
	st := state.IngressState{AccountID: t.account, APITokenSecret: t.secret, TunnelID: tunnel.ID, TunnelName: t.tunnelName, DNS: t.dns}
	if err = c.Store.SetIngress(ctx, op.ID, st); err != nil {
		return fail(errIngressState)
	}
	ing.st = st
	token, err := t.cf.TunnelToken(ctx, t.account, tunnel.ID)
	if err != nil {
		return fail(errTunnelToken)
	}
	changed, err := c.storeTunnelToken(ctx, op.ID, token)
	if err != nil {
		return fail(errTunnelToken)
	}
	connector, err := connectorRequest(m, req, c.Environment, true)
	if err != nil {
		return fail(err)
	}
	// A connector that already matches is left running, so a repeat apply does not restart it. Otherwise it is rolled out.
	outcome := state.Outcome{State: "succeeded"}
	if c.connectorCurrent(ctx, connector, cp.Image, changed) {
		_ = c.Store.AppendEvent(ctx, op.ID, state.Event{Kind: "step", Data: json.RawMessage(`{"step":"connector unchanged"}`)})
	} else {
		outcome, _ = c.runDeploy(ctx, op, actor, connector, false)
		if outcome.State != "succeeded" {
			return outcome
		}
	}
	// The connector is running, so the routes can point at it. Plan again from fresh reads, then write.
	plan, cfg, steps, err := ing.plan(ctx, wants, removable, req.ReplaceExisting)
	if err == nil && len(plan.Conflicts) > 0 {
		c.recordConflicts(ctx, op.ID, plan.Conflicts)
		err = errIngressConflict
	}
	if err == nil {
		err = ing.apply(ctx, plan, cfg, steps)
	}
	if err != nil {
		// The release stays. The connector is running, and the next apply repairs the routes.
		outcome.State = "failed"
		outcome.Err = ingressFailure(err, errIngressPublish)
	}
	return outcome
}

// runIngressRemoval removes the routes and records Berthy created, deletes the connector and forgets the stored
// ingress. It never deletes the tunnel.
func (c *Controller) runIngressRemoval(ctx context.Context, op state.Operation, req Request) (outcome state.Outcome) {
	ctx, pending, owner := removalContext(ctx)
	defer c.finishRemoval(ctx, pending, owner, &outcome)
	fail := func(err error) state.Outcome {
		return state.Outcome{State: "failed", Err: ingressFailure(err, errIngressPublish)}
	}
	t, err := c.ingressTargetFor(ctx, req.Manifest)
	if err != nil {
		return fail(err)
	}
	ing := c.newIngressSync(t, op.ID, tunnelIDOf(t.tunnel))
	plan, cfg, steps, err := ing.plan(ctx, nil, everyRoute, false)
	if err != nil {
		return fail(err)
	}
	if err = ing.apply(ctx, plan, cfg, steps); err != nil {
		return fail(err)
	}
	if err = c.removeConnector(ctx); err != nil {
		return fail(errConnectorRemoval)
	}
	if err = c.Store.ForgetApp(ctx, op.ID, manifest.ConnectorAppName); err != nil {
		return fail(errIngressState)
	}
	// The tunnel token goes before the ingress state is cleared, so a failure here leaves the removal to be retried.
	if err = c.Store.DeleteOperationSecret(ctx, op.ID, manifest.TunnelTokenSecret); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fail(errTunnelTokenRemoval)
	}
	if err = c.Store.ClearIngress(ctx, op.ID); err != nil {
		return fail(errIngressState)
	}
	return state.Outcome{State: "succeeded"}
}

// removeConnector deletes the services bound to the connector app. Each must still carry Berthy's labels for this
// environment. A service that is already gone counts as removed.
func (c *Controller) removeConnector(ctx context.Context) error {
	bindings, err := c.Store.Bindings(ctx, manifest.ConnectorAppName)
	if err != nil {
		return err
	}
	if len(bindings) > 0 && c.Engine == nil {
		return errors.New("Docker unavailable")
	}
	var project string
	if len(bindings) > 0 {
		rel, err := c.Store.Applied(ctx, manifest.ConnectorAppName)
		if err != nil {
			return err
		}
		m, err := manifest.DecodeRelease(rel.Manifest, true)
		if err != nil {
			return err
		}
		project = m.Project
	}
	for _, b := range bindings {
		live, err := c.Engine.InspectService(ctx, b.Name)
		if serviceMissing(err) {
			c.settle(ctx, []state.Claim{{Kind: state.ClaimService, Name: b.Name}})
			continue
		}
		if err != nil {
			return err
		}
		var owner struct{ Labels map[string]string }
		if json.Unmarshal(live.Spec, &owner) != nil || owner.Labels["berthy.environment"] != c.Environment || owner.Labels["berthy.app"] != manifest.ConnectorAppName {
			return errors.New("connector service ownership changed")
		}
		if existing, ok := owner.Labels["berthy.project"]; ok && existing != project {
			return errors.New("connector service ownership changed")
		}
		if err = c.deleteBoundService(ctx, state.Intent{Name: b.Name, ServiceID: live.ID}); err != nil {
			return err
		}
		if _, err = c.Engine.InspectService(ctx, b.Name); !serviceMissing(err) {
			if err != nil {
				return err
			}
			return errors.New("connector service still present")
		}
		c.settle(ctx, []state.Claim{{Kind: state.ClaimService, Name: b.Name}})
	}
	return nil
}

// serviceMissing reports whether err is Docker's answer that the service does not exist.
func serviceMissing(err error) bool {
	var status *engine.StatusError
	return errors.As(err, &status) && status.Code == 404
}

// TunnelStatus is the tunnel as Cloudflare reports it.
type TunnelStatus struct {
	Name        string `json:"name"`
	ID          string `json:"id"`
	Status      string `json:"status,omitempty"`
	Connections int    `json:"connections"`
}

// RoleStatus is one bound connector role: its service, and how many of its replicas run healthy.
type RoleStatus struct {
	Role    string `json:"role"`
	Service string `json:"service"`
	Running int    `json:"running"`
	Desired int    `json:"desired"`
}

// RouteStatus is one stored route. DNS is managed, missing or off, and empty when Cloudflare could not be asked.
type RouteStatus struct {
	Hostname string `json:"hostname"`
	App      string `json:"app"`
	Role     string `json:"role"`
	Target   string `json:"target"`
	DNS      string `json:"dns,omitempty"`
}

// problemConnectorNotDeployed is the problem status reports when managed ingress is configured but the connector app has
// no applied release, so no connector runs for the tunnel.
const problemConnectorNotDeployed = "connector not deployed; run ingress apply"

// IngressStatus is what ingress status shows. Connector lists the bound connector roles, and is empty while the connector
// has no applied release; Problems then says why.
type IngressStatus struct {
	Configured bool          `json:"configured"`
	Tunnel     *TunnelStatus `json:"tunnel,omitempty"`
	Cloudflare string        `json:"cloudflare,omitempty"`
	Connector  []RoleStatus  `json:"connector"`
	Routes     []RouteStatus `json:"routes"`
	Problems   []string      `json:"problems,omitempty"`
}

// IngressStatus reports the tunnel as Cloudflare sees it, the connector's replicas and every stored route. A Cloudflare
// failure is reported in the result, not returned as an error.
func (c *Controller) IngressStatus(ctx context.Context) (IngressStatus, error) {
	if c.Store == nil {
		return IngressStatus{}, errors.New("state unavailable")
	}
	out := IngressStatus{Connector: []RoleStatus{}, Routes: []RouteStatus{}}
	st, err := c.Store.Ingress(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return IngressStatus{}, err
	}
	out.Configured = true
	out.Tunnel = &TunnelStatus{Name: st.TunnelName, ID: st.TunnelID}
	cf, clientErr := c.cloudflareClient(ctx, st.APITokenSecret)
	reachable := clientErr == nil
	if reachable {
		tunnel, err := cf.GetTunnel(ctx, st.AccountID, st.TunnelID)
		reachable = err == nil
		if reachable {
			out.Tunnel.Status, out.Tunnel.Connections = tunnel.Status, len(tunnel.Connections)
		}
	}
	out.Cloudflare = "unavailable"
	if reachable {
		out.Cloudflare = "available"
	}
	_, err = c.Store.Applied(ctx, manifest.ConnectorAppName)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		out.Problems = append(out.Problems, problemConnectorNotDeployed)
	case err != nil:
		return IngressStatus{}, err
	default:
		bindings, err := c.Store.Bindings(ctx, manifest.ConnectorAppName)
		if err != nil {
			return IngressStatus{}, err
		}
		for _, b := range bindings {
			running, desired, err := c.serviceReplicas(ctx, b)
			if err != nil {
				return IngressStatus{}, err
			}
			out.Connector = append(out.Connector, RoleStatus{Role: b.Role, Service: b.Name, Running: running, Desired: desired})
		}
	}
	routes, err := c.Store.IngressRoutes(ctx, "")
	if err != nil {
		return IngressStatus{}, err
	}
	for _, r := range routes {
		row := RouteStatus{Hostname: r.Hostname, App: r.App, Role: r.Role, Target: r.Service}
		switch {
		case !st.DNS:
			row.DNS = "off"
		case clientErr == nil:
			row.DNS = c.recordState(ctx, cf, r)
		}
		out.Routes = append(out.Routes, row)
	}
	return out, nil
}

// recordState reports whether the DNS record of a route is still there: managed, or missing. It is empty when
// Cloudflare cannot be asked.
func (c *Controller) recordState(ctx context.Context, cf *cloudflare.Client, r state.IngressRoute) string {
	if r.DNSRecordID == "" {
		return "missing"
	}
	records, err := cf.DNSRecords(ctx, r.ZoneID, r.Hostname)
	if err != nil {
		return ""
	}
	for _, rec := range records {
		if rec.ID == r.DNSRecordID && cloudflare.IsManaged(rec, c.id()) {
			return "managed"
		}
	}
	return "missing"
}

// serviceReplicas returns how many replicas of a bound service run healthy on its current generation, and how many it
// should run.
func (c *Controller) serviceReplicas(ctx context.Context, b state.Binding) (int, int, error) {
	if c.Engine == nil {
		return 0, 0, errors.New("Docker unavailable")
	}
	live, err := c.Engine.InspectService(ctx, b.Name)
	if err != nil || live.ID != b.ServiceID {
		return 0, 0, errors.New("bound service unavailable")
	}
	var spec struct {
		TaskTemplate struct {
			ContainerSpec struct{ Labels map[string]string }
		}
		Mode struct{ Replicated *struct{ Replicas int } }
	}
	if json.Unmarshal(live.Spec, &spec) != nil || spec.Mode.Replicated == nil {
		return 0, 0, errors.New("invalid bound service")
	}
	generation := spec.TaskTemplate.ContainerSpec.Labels["berthy.operation"]
	tasks, err := c.Engine.ServiceTasks(ctx, live.ID)
	if err != nil {
		return 0, 0, errors.New("task inspection failed")
	}
	slots := map[uint64]bool{}
	for _, t := range tasks {
		if t.ServiceID != live.ID || t.DesiredState != "running" || t.State != "running" || t.Labels["berthy.operation"] != generation || t.Slot == 0 || t.ContainerID == "" {
			continue
		}
		if health, err := c.Engine.InspectContainerHealth(ctx, t.ContainerID); err == nil && health == "healthy" {
			slots[t.Slot] = true
		}
	}
	return len(slots), spec.Mode.Replicated.Replicas, nil
}

// TunnelHealth reports whether managed ingress is configured and, if it is, the tunnel's status as Cloudflare reports
// it. The status is "unavailable" when Cloudflare cannot be asked.
func (c *Controller) TunnelHealth(ctx context.Context) (bool, string) {
	if c.Store == nil {
		return false, ""
	}
	st, err := c.Store.Ingress(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ""
	}
	if err != nil {
		return true, "unavailable"
	}
	cf, err := c.cloudflareClient(ctx, st.APITokenSecret)
	if err != nil {
		return true, "unavailable"
	}
	tunnel, err := cf.GetTunnel(ctx, st.AccountID, st.TunnelID)
	if err != nil || tunnel.Status == "" {
		return true, "unavailable"
	}
	return true, tunnel.Status
}
