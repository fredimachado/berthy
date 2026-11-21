package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"berthy/internal/cloudflare"
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// TunnelPlan is what a destroy does with the Cloudflare tunnel. Delete is true only when every condition holds; otherwise
// Reasons says which do not.
type TunnelPlan struct {
	Name    string   `json:"name,omitempty"`
	ID      string   `json:"id,omitempty"`
	Delete  bool     `json:"delete"`
	Reasons []string `json:"reasons,omitempty"`
}

// DestroyPlan is what destroying an environment removes and what it leaves. Building it changes nothing. The volumes each
// app would delete or keep are in the app's own plan.
type DestroyPlan struct {
	Apps            []RemovePlan    `json:"apps,omitempty"`
	Ingress         []IngressChange `json:"ingress,omitempty"` // routes and records no app's removal covers
	RemoveConnector bool            `json:"remove_connector,omitempty"`
	Helpers         []string        `json:"helpers,omitempty"`  // helper containers to remove
	OneOffs         []string        `json:"one_offs,omitempty"` // finished hook and migration containers to remove
	Images          []string        `json:"images,omitempty"`   // acquired images that nothing will use
	Tunnel          *TunnelPlan     `json:"tunnel,omitempty"`
	Blockers        []string        `json:"blockers,omitempty"`
}

const (
	errDestroyAdmin       Refusal = "destroying an environment requires administrator access"
	errDestroyConfirm     Refusal = "confirmation must name the environment"
	errDestroyInterrupted Refusal = "destroy interrupted; run destroy again"
	errDestroyPreflight   Refusal = "destroy preflight failed"
	errDestroyContainers  Refusal = "container removal failed"
	errDestroyImages      Refusal = "image removal failed"
	errDestroyState       Refusal = "tunnel record failed"
)

// destroyTunnelWait is how long a destroy waits for Cloudflare to report that the tunnel has no connections once the
// connector is gone. A variable so tests can shorten it.
var destroyTunnelWait = time.Minute

// maxTunnelReasons bounds the reasons listed for one tunnel.
const maxTunnelReasons = 10

// destroyEngine is the part of the Docker API a destroy uses for containers and images. The Engine of a Controller may
// implement it; without it a destroy has no containers or images to remove.
type destroyEngine interface {
	InventoryContainers(context.Context) ([]engine.InventoryContainer, error)
	InventoryImages(context.Context) ([]engine.InventoryImage, error)
	RemoveContainer(context.Context, string) error
	RemoveUnusedImage(context.Context, string) error
}

// destroyApp is one app a destroy removes, with the plan of its removal.
type destroyApp struct {
	req Request
	rm  *removal
}

// destruction is everything a destroy needs to know, read once.
type destruction struct {
	plan       DestroyPlan
	apps       []destroyApp
	containers []engine.InventoryContainer // helper and one-off containers that go
	services   []string                    // Swarm services the destroy deletes, whose task containers Docker removes a moment later
	tunnel     *state.IngressState         // the tunnel of managed ingress, if there is one, or one left behind
}

// destroyCheck is what a destroy decides before reading anything: the caller is an administrator and has named the
// environment.
func (c *Controller) destroyCheck(actor Actor, req Request) error {
	switch {
	case !actor.Admin:
		return errDestroyAdmin
	case c.Environment == "" || req.Confirm != c.Environment:
		return errDestroyConfirm
	case c.Store == nil:
		return errors.New("state unavailable")
	}
	return nil
}

func (c *Controller) planDestroy(ctx context.Context, actor Actor, req Request) (Plan, error) {
	d, err := c.destroyFor(ctx, actor, req)
	if err != nil {
		return Plan{}, err
	}
	return Plan{Roles: []string{}, Destroy: &d.plan}, nil
}

// containerName is a container's name without the slash Docker adds, or its short ID.
func containerName(ctr engine.InventoryContainer) string {
	if len(ctr.Names) > 0 {
		return strings.TrimPrefix(ctr.Names[0], "/")
	}
	return ctr.ID[:min(12, len(ctr.ID))]
}

// idle reports whether a container is stopped, so removing it interrupts nothing.
func idle(ctr engine.InventoryContainer) bool {
	return ctr.State == "exited" || ctr.State == "created" || ctr.State == "dead"
}

// destroyable classifies a container of this environment that a destroy may remove: the probe helper, in any state; a
// backup helper or a hook or migration container, only once it has stopped. It returns "" for anything else. The second
// result is whether the container is a helper (as opposed to a one-off).
func (c *Controller) destroyable(ctr engine.InventoryContainer) (string, bool) {
	if !c.id().Owns(ctr.Labels) {
		return "", false
	}
	switch kind := ctr.Labels["berthy.helper"]; {
	case kind == "probe":
		return "helper", true
	case kind == "archive" || kind == "restore":
		return "helper", idle(ctr)
	case ctr.Labels["berthy.oneoff"] != "":
		return "oneoff", idle(ctr)
	}
	return "", false
}

// destroyFor reads what destroying the environment would do. Anything that refuses it is a blocker of its plan, and a
// destroy with a blocker changes nothing. It changes nothing itself.
func (c *Controller) destroyFor(ctx context.Context, actor Actor, req Request) (*destruction, error) {
	if err := c.destroyCheck(actor, req); err != nil {
		return nil, err
	}
	d := &destruction{}
	applied, err := c.Store.AppliedApps(ctx)
	if err != nil {
		return nil, err
	}
	gone := pruneExclusions{containers: map[string]bool{}, services: map[string]bool{}, apps: map[string]bool{manifest.ConnectorAppName: true}}
	removedRoutes, removedRecords := map[string]bool{}, map[string]bool{}
	routeApps := map[string]bool{}
	for _, app := range applied {
		if app == manifest.ConnectorAppName {
			continue
		}
		appReq := Request{App: app, Volumes: req.Volumes, AllowRecreate: req.AllowRecreate}
		rm, err := c.removalFor(ctx, actor, appReq)
		if err != nil {
			return nil, err
		}
		d.apps = append(d.apps, destroyApp{req: appReq, rm: rm})
		d.plan.Apps = append(d.plan.Apps, rm.plan)
		d.plan.Blockers = append(d.plan.Blockers, rm.plan.Blockers...)
		gone.apps[app], routeApps[app] = true, true
		for _, lb := range rm.live {
			gone.services[lb.live.ID] = true
			d.services = append(d.services, lb.b.Name)
		}
		for _, ch := range rm.plan.Ingress {
			noteRemoved(ch, removedRoutes, removedRecords)
		}
	}
	// Managed ingress: what the apps' removals leave, and the connector.
	stored, err := c.Store.Ingress(ctx)
	hasIngress := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if hasIngress {
		d.plan.RemoveConnector = true
		d.tunnel = &stored
		bindings, err := c.Store.Bindings(ctx, manifest.ConnectorAppName)
		if err != nil {
			return nil, err
		}
		for _, b := range bindings {
			gone.services[b.ServiceID] = true
			d.services = append(d.services, b.Name)
		}
		t, err := c.ingressTargetFor(ctx, manifest.Manifest{})
		if err == nil {
			var ip IngressPlan
			if ip, _, _, err = c.newIngressSync(t, "", tunnelIDOf(t.tunnel)).plan(ctx, nil, everyRoute, false); err == nil {
				for _, ch := range ip.Changes {
					noteRemoved(ch, removedRoutes, removedRecords)
					if !routeApps[ch.App] {
						d.plan.Ingress = append(d.plan.Ingress, ch)
					}
				}
				d.plan.Blockers = append(d.plan.Blockers, ip.Conflicts...)
			}
		}
		var refusal Refusal
		if errors.As(err, &refusal) {
			d.plan.Blockers = append(d.plan.Blockers, string(refusal))
		} else if err != nil {
			return nil, err
		}
	} else if kept, err := c.Store.KeptTunnel(ctx); err == nil {
		d.tunnel = &kept
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	// Helpers, one-offs and images.
	if de, ok := c.Engine.(destroyEngine); ok {
		containers, err := de.InventoryContainers(ctx)
		if err != nil {
			return nil, errors.New("container inventory unavailable")
		}
		for _, ctr := range containers {
			kind, removable := c.destroyable(ctr)
			if kind == "" || !removable {
				continue
			}
			d.containers = append(d.containers, ctr)
			gone.containers[ctr.ID] = true
			if kind == "helper" {
				d.plan.Helpers = append(d.plan.Helpers, containerName(ctr))
			} else {
				d.plan.OneOffs = append(d.plan.OneOffs, containerName(ctr))
			}
		}
		if _, ok := c.Engine.(inventoryEngine); ok {
			candidates, err := c.prunePlanWithout(ctx, gone)
			if err != nil {
				return nil, errors.New(pruneFailure(err))
			}
			for _, i := range candidates {
				d.plan.Images = append(d.plan.Images, i.Ref)
			}
		}
	}
	if req.Tunnel {
		if d.plan.Tunnel, err = c.tunnelPlan(ctx, d.tunnel, hasIngress, removedRoutes, removedRecords); err != nil {
			return nil, err
		}
	}
	d.plan.Blockers = slices.Compact(d.plan.Blockers)
	return d, nil
}

// noteRemoved records the hostnames whose rule or record a change deletes, so the tunnel check does not count them.
func noteRemoved(ch IngressChange, routes, records map[string]bool) {
	if ch.Route == "remove" {
		routes[ch.Hostname] = true
	}
	if ch.DNS == "remove" {
		records[ch.Hostname] = true
	}
}

// tunnelPlan says whether the tunnel would be deleted, and if not, why. The rules and records that the destroy itself
// removes do not count. Whether the tunnel still has connections can be known only once the connector is gone, so it is
// checked then, unless Berthy has no connector for it.
func (c *Controller) tunnelPlan(ctx context.Context, st *state.IngressState, hasIngress bool, routes, records map[string]bool) (*TunnelPlan, error) {
	if st == nil {
		return &TunnelPlan{Reasons: []string{"no managed tunnel"}}, nil
	}
	plan := &TunnelPlan{Name: st.TunnelName, ID: st.TunnelID}
	cf, err := c.cloudflareClient(ctx, st.APITokenSecret)
	if err != nil {
		plan.Reasons = []string{string(errCloudflareSecret)}
		return plan, nil
	}
	tunnel, err := cf.GetTunnel(ctx, st.AccountID, st.TunnelID)
	switch {
	case notFound(err):
		plan.Reasons = []string{"the tunnel no longer exists"}
		return plan, nil
	case err != nil:
		plan.Reasons = []string{string(errCloudflareUnavailable)}
		return plan, nil
	}
	reasons, err := tunnelReasons(ctx, cf, *st, routes, records)
	if err != nil {
		plan.Reasons = []string{string(errCloudflareUnavailable)}
		return plan, nil
	}
	if !hasIngress && len(tunnel.Connections) > 0 {
		reasons = append(reasons, connectionReason(len(tunnel.Connections)))
	}
	plan.Reasons = reasons
	plan.Delete = len(reasons) == 0
	return plan, nil
}

func connectionReason(n int) string {
	if n == 1 {
		return "the tunnel still has 1 active connection"
	}
	return fmt.Sprintf("the tunnel still has %d active connections", n)
}

// tunnelReasons lists what stops a tunnel from being deleted: a rule with a hostname in its remotely managed
// configuration, and a DNS record in any zone of the account that points at it. Hostnames in routes and records are about
// to be removed, and do not count.
func tunnelReasons(ctx context.Context, cf *cloudflare.Client, st state.IngressState, routes, records map[string]bool) ([]string, error) {
	cfg, err := cf.GetConfig(ctx, st.AccountID, st.TunnelID)
	if err != nil {
		return nil, err
	}
	hosts := map[string]bool{}
	for _, r := range cfg.Ingress {
		if r.Hostname != "" && !routes[r.Hostname] {
			hosts[r.Hostname] = true
		}
	}
	var reasons []string
	for _, h := range sortedKeys(hosts) {
		reasons = append(reasons, "a rule for "+h+" remains")
	}
	zones, err := cf.Zones(ctx, st.AccountID)
	if err != nil {
		return nil, err
	}
	pointing := map[string]bool{}
	for _, z := range zones {
		found, err := cf.DNSRecordsByContent(ctx, z.ID, cloudflare.TunnelTarget(st.TunnelID))
		if err != nil {
			return nil, err
		}
		for _, r := range found {
			if !records[r.Name] {
				pointing[r.Name] = true
			}
		}
	}
	for _, name := range sortedKeys(pointing) {
		reasons = append(reasons, "a DNS record for "+name+" points at the tunnel")
	}
	if len(reasons) > maxTunnelReasons {
		more := len(reasons) - maxTunnelReasons
		reasons = append(reasons[:maxTunnelReasons], fmt.Sprintf("and %d more", more))
	}
	return reasons, nil
}

// runDestroy removes everything Berthy made in the environment. Nothing changes when the plan has a blocker. Every step
// can be repeated, so an interrupted destroy is finished by running it again.
func (c *Controller) runDestroy(ctx context.Context, op state.Operation, actor Actor, req Request) state.Outcome {
	step := func(text string) {
		if data, err := json.Marshal(map[string]string{"step": text}); err == nil {
			_ = c.Store.AppendEvent(ctx, op.ID, state.Event{Kind: "step", Data: data})
		}
	}
	d, err := c.destroyFor(ctx, actor, req)
	if err != nil {
		return removeFailure(err, errDestroyPreflight)
	}
	for _, blocker := range d.plan.Blockers {
		_ = c.Store.AppendEvent(ctx, op.ID, state.Event{Kind: "blocked", Data: reasonEvent(blocker)})
	}
	if len(d.plan.Blockers) > 0 {
		return removeFailure(Refusal(d.plan.Blockers[0]), errDestroyPreflight)
	}
	// The tunnel is remembered before managed ingress is forgotten, so a later destroy can still find it.
	if d.tunnel != nil {
		if err = c.Store.SetKeptTunnel(ctx, op.ID, *d.tunnel); err != nil {
			return state.Outcome{State: "failed", Err: string(errDestroyState)}
		}
	}
	journal := 0
	for _, a := range d.apps {
		prefix := a.req.App + ": "
		out := c.removeApp(ctx, op.ID, a.req, a.rm, func(text string) { step(prefix + text) }, &journal)
		if out.State != "succeeded" {
			return out
		}
	}
	if d.plan.RemoveConnector {
		for _, ch := range d.plan.Ingress {
			text := "route " + ch.Hostname + ": " + ch.Route
			if ch.DNS != "" {
				text += ", DNS " + ch.DNS
			}
			step(text)
		}
		if out := c.runIngressRemoval(ctx, op, Request{}); out.State != "succeeded" {
			return out
		}
		step("removed the connector and managed ingress")
	}
	if out := c.removeContainersAndImages(ctx, d, step); out.State != "succeeded" {
		return out
	}
	if req.Tunnel {
		if err = c.finishTunnel(ctx, op.ID, step); err != nil {
			return state.Outcome{State: "failed", Err: string(errDestroyState)}
		}
	}
	return state.Outcome{State: "succeeded"}
}

// removeContainersAndImages removes the helper and one-off containers and the images
// Berthy acquired that nothing uses. It reports what it keeps.
func (c *Controller) removeContainersAndImages(ctx context.Context, d *destruction, step func(string)) state.Outcome {
	de, ok := c.Engine.(destroyEngine)
	if !ok {
		return state.Outcome{State: "succeeded"}
	}
	// Docker removes the task containers of a deleted service a moment later, and an image they use cannot go before they do.
	if err := waitTasksGone(ctx, de, d.services); err != nil {
		return state.Outcome{State: "failed", Err: string(errDestroyContainers)}
	}
	// Read again: a container may have started or stopped since the plan.
	containers, err := de.InventoryContainers(ctx)
	if err != nil {
		return state.Outcome{State: "failed", Err: string(errDestroyContainers)}
	}
	for _, ctr := range containers {
		kind, removable := c.destroyable(ctr)
		switch {
		case kind == "":
		case !removable:
			step("kept container " + containerName(ctr) + ": " + ctr.State)
		default:
			if err = de.RemoveContainer(ctx, ctr.ID); err != nil && !statusIs(err, 404) {
				return state.Outcome{State: "failed", Err: string(errDestroyContainers)}
			}
			step("removed container " + containerName(ctr))
		}
	}
	if _, ok := c.Engine.(inventoryEngine); ok {
		removed, failure := c.pruneNow(ctx)
		for _, ref := range removed {
			step("removed image " + ref)
		}
		if failure != "" {
			return state.Outcome{State: "failed", Err: failure}
		}
	}
	return state.Outcome{State: "succeeded"}
}

// waitTasksGone waits until no container is left that belongs to one of the deleted Swarm services. It gives up after
// removeVolumeWait and goes on; an image such a container still uses is then kept, as Docker would refuse to remove it.
func waitTasksGone(ctx context.Context, de destroyEngine, services []string) error {
	if len(services) == 0 {
		return nil
	}
	deadline := time.Now().Add(removeVolumeWait)
	for {
		containers, err := de.InventoryContainers(ctx)
		if err != nil {
			return err
		}
		left := false
		for _, ctr := range containers {
			left = left || slices.Contains(services, ctr.Labels["com.docker.swarm.service.name"])
		}
		if !left || !time.Now().Before(deadline) {
			return nil
		}
		if err = sleepContext(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
}

// statusIs reports whether err is a Docker answer with this status.
func statusIs(err error, code int) bool {
	var status *engine.StatusError
	return errors.As(err, &status) && status.Code == code
}

// finishTunnel deletes the tunnel that managed ingress left behind when it is clean, and says why it keeps it when it is
// not. A kept tunnel stays remembered, so a later destroy --tunnel can delete it once the reasons are gone. Cloudflare
// reporting that it cannot be asked is a reason to keep it, not a failure of the destroy.
func (c *Controller) finishTunnel(ctx context.Context, opID string, step func(string)) error {
	st, err := c.Store.KeptTunnel(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		step("no managed tunnel")
		return nil
	}
	if err != nil {
		return err
	}
	keep := func(reasons ...string) error {
		for _, r := range reasons {
			step("kept tunnel " + st.TunnelName + ": " + r)
		}
		return nil
	}
	cf, err := c.cloudflareClient(ctx, st.APITokenSecret)
	if err != nil {
		return keep(string(errCloudflareSecret))
	}
	tunnel, err := cf.GetTunnel(ctx, st.AccountID, st.TunnelID)
	if notFound(err) {
		step("tunnel " + st.TunnelName + " no longer exists")
		return c.Store.ClearKeptTunnel(ctx, opID)
	}
	if err != nil {
		return keep(string(errCloudflareUnavailable))
	}
	reasons, err := tunnelReasons(ctx, cf, st, nil, nil)
	if err != nil {
		return keep(string(errCloudflareUnavailable))
	}
	// The connector is gone, but Cloudflare takes a while to notice. A connection that stays is somebody else's.
	deadline := time.Now().Add(destroyTunnelWait)
	for len(tunnel.Connections) > 0 && time.Now().Before(deadline) {
		if sleepContext(ctx, min(time.Second, max(destroyTunnelWait/10, time.Millisecond))) != nil {
			break
		}
		if tunnel, err = cf.GetTunnel(ctx, st.AccountID, st.TunnelID); err != nil {
			return keep(string(errCloudflareUnavailable))
		}
	}
	if n := len(tunnel.Connections); n > 0 {
		reasons = append(reasons, connectionReason(n))
	}
	if len(reasons) > 0 {
		return keep(reasons...)
	}
	if err = cf.DeleteTunnel(ctx, st.AccountID, st.TunnelID); err != nil && !notFound(err) {
		return keep("Cloudflare refused to delete it")
	}
	step("deleted tunnel " + st.TunnelName)
	return c.Store.ClearKeptTunnel(ctx, opID)
}
