package deploy

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"berthy/internal/cloudflare"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// RouteWant is one hostname the environment's tunnel should route to a role's service.
type RouteWant struct{ Hostname, App, Role, Service string } // Service e.g. "http://myapp-api-web:8080"

// IngressChange is what the route and the DNS record of one hostname would do.
type IngressChange struct {
	Hostname string `json:"hostname"`
	App      string `json:"app,omitempty"`
	Role     string `json:"role,omitempty"`
	Route    string `json:"route"`              // add, change, remove, adopt, keep
	DNS      string `json:"dns,omitempty"`      // add, change, remove, adopt, keep; empty when DNS is not managed
	Target   string `json:"target,omitempty"`   // desired service
	Previous string `json:"previous,omitempty"` // previous service when Route is change/remove
}

// IngressPlan is what an ingress apply would change. Building it writes nothing.
type IngressPlan struct {
	Tunnel          string          `json:"tunnel"`
	TunnelID        string          `json:"tunnel_id,omitempty"`
	CreateTunnel    bool            `json:"create_tunnel,omitempty"`
	DNS             bool            `json:"dns"`
	Connector       *Plan           `json:"connector,omitempty"`
	RemoveConnector bool            `json:"remove_connector,omitempty"`
	Changes         []IngressChange `json:"changes"`
	Conflicts       []string        `json:"conflicts,omitempty"`
}

// dnsStep is the DNS work for one hostname. A step with remove set deletes the record of a hostname that is leaving
// the tunnel. The other steps belong to wanted hostnames.
type dnsStep struct {
	Want    RouteWant
	Action  string // add, change, adopt, keep, remove, or "" when DNS is off or nothing is to be done
	Zone    cloudflare.Zone
	Record  *cloudflare.DNSRecord  // the record to update, delete or adopt
	Replace []cloudflare.DNSRecord // foreign records deleted by --replace-existing before creating ours
	remove  bool
}

// ingressSync plans and applies the routes and DNS records of one environment against one tunnel.
type ingressSync struct {
	c    *Controller
	cf   *cloudflare.Client
	st   state.IngressState
	opID string
	base cloudflare.TunnelConfig // the configuration the last plan read; apply writes only when the new one differs
}

// mergeRoutes builds the tunnel's new rule list. Berthy's rules come first (sorted by hostname), every rule Berthy does
// not own keeps its relative order, and the final catch-all stays last. owned maps hostnames Berthy owns to their state
// rows; removable is the set of owned hostnames this run may remove. When there are conflicts the configuration is
// returned unchanged, so the caller writes nothing.
func mergeRoutes(cfg cloudflare.TunnelConfig, wants []RouteWant, owned map[string]state.IngressRoute, removable map[string]bool, replace bool) (cloudflare.TunnelConfig, []IngressChange, []string) {
	rules := cfg.Ingress
	catchAll := cloudflare.Rule{Service: "http_status:404"}
	if n := len(rules); n > 0 && rules[n-1].Hostname == "" && rules[n-1].Path == "" {
		catchAll = rules[n-1]
		rules = rules[:n-1]
	}
	sorted := sortedWants(wants)
	wanted := map[string]bool{}
	for _, w := range sorted {
		wanted[w.Hostname] = true
	}
	var changes []IngressChange
	var conflicts []string
	var berthy []cloudflare.Rule
	resolved := map[string]bool{}
	for _, w := range sorted {
		existing := rulesFor(rules, w.Hostname)
		_, isOwned := owned[w.Hostname]
		single := len(existing) == 1
		exact := single && existing[0].Path == "" && existing[0].Service == w.Service
		ch := IngressChange{Hostname: w.Hostname, App: w.App, Role: w.Role, Target: w.Service}
		rule := cloudflare.Rule{Hostname: w.Hostname, Service: w.Service}
		switch {
		case len(existing) == 0:
			ch.Route = "add"
		case isOwned && exact:
			ch.Route, rule = "keep", existing[0]
		case isOwned:
			ch.Route, ch.Previous = "change", servicesOf(existing)
		case exact:
			ch.Route, rule = "adopt", existing[0]
		case replace:
			ch.Route, ch.Previous = "change", servicesOf(existing)
		default:
			conflicts = append(conflicts, "route "+w.Hostname+" is used by another rule")
			continue
		}
		if ch.Route == "change" && single {
			rule.Extra = existing[0].Extra
		}
		berthy = append(berthy, rule)
		resolved[w.Hostname] = true
		changes = append(changes, ch)
	}
	removed := map[string]bool{}
	for _, h := range sortedKeys(owned) {
		if wanted[h] || !removable[h] {
			continue
		}
		removed[h] = true
		row := owned[h]
		changes = append(changes, IngressChange{Hostname: h, App: row.App, Role: row.Role, Route: "remove", Previous: servicesOf(rulesFor(rules, h))})
	}
	sortChanges(changes)
	if len(conflicts) > 0 {
		return cfg, changes, conflicts
	}
	out := berthy
	for _, r := range rules {
		if r.Hostname != "" && (resolved[r.Hostname] || removed[r.Hostname]) {
			continue
		}
		out = append(out, r)
	}
	out = append(out, catchAll)
	return cloudflare.TunnelConfig{Ingress: out, Extra: cfg.Extra}, changes, conflicts
}

// plan reads Cloudflare and state and returns what an apply would do: the plan, the tunnel configuration it would
// write, and the DNS steps. It changes nothing. removable decides which owned routes may be removed.
func (s *ingressSync) plan(ctx context.Context, wants []RouteWant, removable func(state.IngressRoute) bool, replace bool) (IngressPlan, cloudflare.TunnelConfig, []dnsStep, error) {
	routes, err := s.c.Store.IngressRoutes(ctx, "")
	if err != nil {
		return IngressPlan{}, cloudflare.TunnelConfig{}, nil, err
	}
	owned := map[string]state.IngressRoute{}
	removables := map[string]bool{}
	for _, r := range routes {
		owned[r.Hostname] = r
		removables[r.Hostname] = removable(r)
	}
	var cfg cloudflare.TunnelConfig
	target := ""
	if s.st.TunnelID != "" {
		if cfg, err = s.cf.GetConfig(ctx, s.st.AccountID, s.st.TunnelID); err != nil {
			return IngressPlan{}, cloudflare.TunnelConfig{}, nil, errCloudflareUnavailable
		}
		target = cloudflare.TunnelTarget(s.st.TunnelID)
	}
	s.base = cfg
	merged, changes, conflicts := mergeRoutes(cfg, wants, owned, removables, replace)
	wanted := map[string]bool{}
	dnsConflict := map[string]bool{}
	var steps []dnsStep
	for _, w := range sortedWants(wants) {
		wanted[w.Hostname] = true
		step := dnsStep{Want: w}
		if s.st.DNS {
			var prev *state.IngressRoute
			if r, ok := owned[w.Hostname]; ok {
				prev = &r
			}
			var msg string
			if step, msg, err = s.dnsFor(ctx, w, prev, target, replace); err != nil {
				return IngressPlan{}, cloudflare.TunnelConfig{}, nil, err
			}
			if msg != "" {
				conflicts = append(conflicts, msg)
				dnsConflict[w.Hostname] = true
			}
		}
		steps = append(steps, step)
	}
	for _, h := range sortedKeys(owned) {
		if wanted[h] || !removables[h] {
			continue
		}
		step, err := s.dnsRemoval(ctx, owned[h])
		if err != nil {
			return IngressPlan{}, cloudflare.TunnelConfig{}, nil, err
		}
		steps = append(steps, step)
	}
	actions := map[string]string{}
	for _, step := range steps {
		actions[step.Want.Hostname] = step.Action
	}
	kept := []IngressChange{}
	for _, ch := range changes {
		if dnsConflict[ch.Hostname] {
			continue
		}
		ch.DNS = actions[ch.Hostname]
		kept = append(kept, ch)
	}
	plan := IngressPlan{Tunnel: s.st.TunnelName, TunnelID: s.st.TunnelID, DNS: s.st.DNS, Changes: kept, Conflicts: conflicts}
	return plan, merged, steps, nil
}

// dnsFor decides the DNS record of one wanted hostname: add, change, adopt or keep it, or report the conflict that
// stops the run. target is the tunnel's address, and is empty while the tunnel does not exist yet.
func (s *ingressSync) dnsFor(ctx context.Context, w RouteWant, prev *state.IngressRoute, target string, replace bool) (dnsStep, string, error) {
	zone, err := s.cf.FindZone(ctx, s.st.AccountID, w.Hostname)
	if errors.Is(err, cloudflare.ErrZoneNotFound) {
		return dnsStep{Want: w}, "no Cloudflare zone for " + w.Hostname, nil
	}
	if err != nil {
		return dnsStep{Want: w}, "", errCloudflareUnavailable
	}
	records, err := s.cf.DNSRecords(ctx, zone.ID, w.Hostname)
	if err != nil {
		return dnsStep{Want: w}, "", errCloudflareUnavailable
	}
	step := dnsStep{Want: w, Zone: zone}
	env := s.c.Environment
	if prev != nil && prev.DNSRecordID != "" {
		for i := range records {
			if records[i].ID == prev.DNSRecordID && cloudflare.IsManaged(records[i], env) {
				step.Record = &records[i]
				step.Action = "change"
				if pointsAt(records[i], target) {
					step.Action = "keep"
				}
				return step, "", nil
			}
		}
	}
	switch {
	case len(records) == 0:
		step.Action = "add"
	case len(records) == 1 && pointsAt(records[0], target):
		step.Record = &records[0]
		step.Action = "adopt"
		if cloudflare.IsManaged(records[0], env) {
			step.Action = "keep"
		}
	case len(records) == 1 && records[0].Type == "CNAME" && cloudflare.IsManaged(records[0], env):
		step.Record = &records[0]
		step.Action = "change"
	case !replace:
		return dnsStep{Want: w}, "DNS name " + w.Hostname + " is used by another record", nil
	case len(records) == 1 && records[0].Type == "CNAME":
		step.Record = &records[0]
		step.Action = "change"
	default:
		step.Action = "add"
		step.Replace = records
	}
	return step, "", nil
}

// dnsRemoval plans the deletion of the record Berthy created for a hostname that is leaving the tunnel. A record that
// is missing, not managed, or not the one Berthy recorded is left alone.
func (s *ingressSync) dnsRemoval(ctx context.Context, row state.IngressRoute) (dnsStep, error) {
	step := dnsStep{Want: RouteWant{Hostname: row.Hostname, App: row.App, Role: row.Role, Service: row.Service}, Zone: cloudflare.Zone{ID: row.ZoneID}, remove: true}
	if row.DNSRecordID == "" || row.ZoneID == "" {
		return step, nil
	}
	records, err := s.cf.DNSRecords(ctx, row.ZoneID, row.Hostname)
	if notFound(err) {
		return step, nil
	}
	if err != nil {
		return step, errCloudflareUnavailable
	}
	for i := range records {
		if records[i].ID == row.DNSRecordID && cloudflare.IsManaged(records[i], s.c.Environment) {
			step.Action = "remove"
			step.Record = &records[i]
		}
	}
	return step, nil
}

// apply writes a plan. It writes the tunnel configuration when it changed, then the DNS records and recorded routes of
// the wanted hostnames, then the removals. It writes nothing when the plan has conflicts.
func (s *ingressSync) apply(ctx context.Context, plan IngressPlan, cfg cloudflare.TunnelConfig, steps []dnsStep) error {
	if len(plan.Conflicts) > 0 {
		return errIngressConflict
	}
	if s.st.TunnelID != "" {
		before, err := json.Marshal(s.base)
		if err != nil {
			return errIngressPublish
		}
		after, err := json.Marshal(cfg)
		if err != nil {
			return errIngressPublish
		}
		if !bytes.Equal(before, after) {
			if err = s.cf.PutConfig(ctx, s.st.AccountID, s.st.TunnelID, cfg); err != nil {
				return errIngressPublish
			}
		}
	}
	target := cloudflare.TunnelTarget(s.st.TunnelID)
	for i := range steps {
		st := &steps[i]
		if st.remove || st.Action == "" || st.Action == "keep" {
			continue
		}
		if target == "" {
			return errIngressPublish
		}
		switch st.Action {
		case "add":
			for _, r := range st.Replace {
				if err := s.cf.DeleteDNSRecord(ctx, st.Zone.ID, r.ID); err != nil && !notFound(err) {
					return errIngressPublish
				}
			}
			created, err := s.cf.CreateDNSRecord(ctx, st.Zone.ID, s.record(st.Want.Hostname, target))
			if err != nil {
				return errIngressPublish
			}
			st.Record = &created
		case "change", "adopt":
			if st.Record == nil {
				return errIngressPublish
			}
			if _, err := s.cf.UpdateDNSRecord(ctx, st.Zone.ID, st.Record.ID, s.record(st.Want.Hostname, target)); err != nil {
				return errIngressPublish
			}
		}
	}
	for _, st := range steps {
		if st.remove {
			continue
		}
		route := state.IngressRoute{Hostname: st.Want.Hostname, App: st.Want.App, Role: st.Want.Role, Service: st.Want.Service}
		if st.Action != "" {
			route.ZoneID = st.Zone.ID
		}
		if st.Record != nil {
			route.DNSRecordID = st.Record.ID
		}
		if err := s.c.Store.SetIngressRoute(ctx, s.opID, route); err != nil {
			return errIngressState
		}
	}
	for _, st := range steps {
		if !st.remove {
			continue
		}
		if st.Action == "remove" && st.Record != nil {
			if err := s.cf.DeleteDNSRecord(ctx, st.Zone.ID, st.Record.ID); err != nil && !notFound(err) {
				return errIngressPublish
			}
		}
		if err := s.c.Store.DeleteIngressRoute(ctx, s.opID, st.Want.Hostname); err != nil {
			return errIngressState
		}
	}
	return nil
}

// record is the DNS record Berthy writes for a hostname: a proxied CNAME to the tunnel, marked as Berthy's.
func (s *ingressSync) record(hostname, target string) cloudflare.DNSRecord {
	return cloudflare.DNSRecord{Type: "CNAME", Name: hostname, Content: target, Proxied: true, TTL: 1, Comment: cloudflare.RecordComment(s.c.Environment)}
}

// routeWants lists the routes of the given image apps: one for every hostname of every role. A role routes to its bound
// service when it is bound, and to <project>-<app>-<role> otherwise.
func (c *Controller) routeWants(ctx context.Context, m manifest.Manifest, apps []string) ([]RouteWant, error) {
	var out []RouteWant
	for _, name := range apps {
		app, ok := m.Apps[name]
		if !ok || app.Kind != "image" {
			continue
		}
		release, err := c.Store.Applied(ctx, name)
		if err != nil {
			return nil, err
		}
		bound := map[string]string{}
		for _, b := range release.Bindings {
			bound[b.Role] = b.Name
		}
		for role, r := range app.Roles {
			service := bound[role]
			if service == "" {
				service = m.Project + "-" + name + "-" + role
			}
			for _, host := range r.Hostnames {
				out = append(out, RouteWant{Hostname: host, App: name, Role: role, Service: fmt.Sprintf("http://%s:%d", service, r.Port)})
			}
		}
	}
	return sortedWants(out), nil
}

// deployedApps lists the image apps that have an applied release, in name order. An app that was never deployed is
// published by its first deploy, not by an ingress apply.
func (c *Controller) deployedApps(ctx context.Context, m manifest.Manifest) ([]string, error) {
	var out []string
	for _, name := range sortedKeys(m.Apps) {
		if m.Apps[name].Kind != "image" {
			continue
		}
		if _, err := c.Store.Applied(ctx, name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, err
		}
		out = append(out, name)
	}
	return out, nil
}

// wantsFor lists the routes a manifest asks for: the hostnames of the deployed image apps.
func (c *Controller) wantsFor(ctx context.Context, m manifest.Manifest) ([]RouteWant, error) {
	apps, err := c.deployedApps(ctx, m)
	if err != nil {
		return nil, err
	}
	return c.routeWants(ctx, m, apps)
}

// pointsAt reports whether r is a proxied CNAME to the tunnel's address.
func pointsAt(r cloudflare.DNSRecord, target string) bool {
	return target != "" && r.Type == "CNAME" && r.Content == target && r.Proxied
}

// notFound reports whether err is Cloudflare's answer that the object does not exist.
func notFound(err error) bool {
	var api *cloudflare.APIError
	return errors.As(err, &api) && api.Status == http.StatusNotFound
}

// rulesFor returns the rules of rules whose hostname is hostname, in order.
func rulesFor(rules []cloudflare.Rule, hostname string) []cloudflare.Rule {
	var out []cloudflare.Rule
	for _, r := range rules {
		if r.Hostname == hostname {
			out = append(out, r)
		}
	}
	return out
}

// servicesOf lists the distinct services of rules, in order, as one value for a change's previous service.
func servicesOf(rules []cloudflare.Rule) string {
	var services []string
	for _, r := range rules {
		seen := false
		for _, s := range services {
			if s == r.Service {
				seen = true
			}
		}
		if !seen {
			services = append(services, r.Service)
		}
	}
	return strings.Join(services, ", ")
}

// sortedWants returns a copy of wants ordered by hostname.
func sortedWants(wants []RouteWant) []RouteWant {
	out := append([]RouteWant(nil), wants...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Hostname < out[j].Hostname })
	return out
}

// sortChanges orders changes by hostname.
func sortChanges(changes []IngressChange) {
	sort.SliceStable(changes, func(i, j int) bool { return changes[i].Hostname < changes[j].Hostname })
}

// sortedKeys returns the keys of m in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
