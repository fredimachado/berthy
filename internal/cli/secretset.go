package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"berthy/internal/agent"
	"berthy/internal/agentclient"
	"berthy/internal/cloudflare"
	"berthy/internal/manifest"
	"berthy/internal/registrycheck"

	"github.com/charmbracelet/x/term"
	"github.com/distribution/reference"
)

// maxSecretValue is the largest value a secret can have, in bytes.
const maxSecretValue = 65536

// cloudflareTokenTemplate is the start of Cloudflare's account API token form with its permissions filled in. Cloudflare
// reads the rest of the link as the token's permission groups and name.
const cloudflareTokenTemplate = "https://dash.cloudflare.com/?to=/:account/api-tokens&permissionGroupKeys="

// cloudflareTunnelWritePermission is Cloudflare's permission group ID for "Cloudflare Tunnel Write".
const cloudflareTunnelWritePermission = "c07321b023e944ff818fec44d8203567"

// cloudflareDNSPermission is Cloudflare's documented short key for the DNS permission.
const cloudflareDNSPermission = "dns"

// cloudflareURLEnv overrides the Cloudflare API base URL that the check asks. Tests and the end-to-end fixture set it.
const cloudflareURLEnv = "BERTHY_CLOUDFLARE_API_URL"

// secretGuide is what Berthy knows about a secret before it asks for its value at a terminal.
type secretGuide struct {
	// intro is printed before the prompt: what the secret is for and, when Berthy knows one, the link that creates it.
	intro []string
	// check tries the value from this machine before it is stored. nil when Berthy has nothing to try.
	check func(ctx context.Context, value string) checkResult
}

// checkResult is what trying a value found.
type checkResult struct {
	notes   []string // what was checked, or why something could not be; printed either way
	refusal string   // not empty: the service refused the value, so it is not stored; this says why
}

// cloudflarePermission is one permission group of a Cloudflare token link.
type cloudflarePermission struct {
	Key  string `json:"key"`
	Type string `json:"type"`
}

// guideFor works out the guide from the secret's name, the --registry host and the manifest.
// m is nil when no manifest was loaded.
func guideFor(m *manifest.Manifest, env, name, registry string) secretGuide {
	token := "berthy-" + env
	if m != nil {
		token = "berthy-" + m.Project + "-" + env
	}
	switch {
	case registry == "ghcr.io":
		link := "https://github.com/settings/tokens/new?" + url.Values{"scopes": {"read:packages"}, "description": {token}}.Encode()
		return secretGuide{intro: []string{
			name + " is the password Berthy uses to pull images from ghcr.io.",
			"Create a GitHub token (classic) that can only read packages:",
			"  " + link,
		}, check: registryCheck(m, registry)}
	case registry != "":
		return secretGuide{
			intro: []string{name + " is the password Berthy uses to pull images from " + registry + "."},
			check: registryCheck(m, registry),
		}
	case m != nil && m.Ingress != nil && m.Ingress.Cloudflare != nil && m.Ingress.Cloudflare.APITokenSecret == name:
		c := m.Ingress.Cloudflare
		g := cloudflareGuide(name, token, c.DNS == nil || *c.DNS)
		g.check = cloudflareCheck(m, env)
		return g
	}
	return secretGuide{}
}

// registryCheck returns the check for a registry password: the registry must let the credential pull every repository
// that an image app of the manifest takes from it. It returns nil when the manifest does not name the registry.
func registryCheck(m *manifest.Manifest, registry string) func(context.Context, string) checkResult {
	if m == nil || registry == "" {
		return nil
	}
	reg, ok := m.Registries[registry]
	if !ok {
		return nil
	}
	repos := imageRepos(m, registry)
	return func(ctx context.Context, value string) checkResult {
		if len(repos) == 0 {
			return checkResult{notes: []string{"not checked: no image app in the manifest pulls from " + registry}}
		}
		var res checkResult
		for _, repo := range repos {
			probe := registrycheck.Probe{Host: registry, Username: reg.Username, Password: value}
			err := probe.CanPull(ctx, repo)
			switch {
			case err == nil:
				res.notes = append(res.notes, "checked: "+registry+" lets this credential pull "+repo)
			case errors.Is(err, registrycheck.ErrDenied):
				res.refusal = err.Error()
				return res
			default:
				res.notes = append(res.notes, "not checked: "+registry+" could not be asked about "+repo+" from this machine ("+err.Error()+")")
			}
		}
		return res
	}
}

// imageRepos returns the sorted, distinct repositories of the manifest's image apps whose images are on registry.
func imageRepos(m *manifest.Manifest, registry string) []string {
	var repos []string
	for _, app := range m.Apps {
		if app.Image == "" {
			continue
		}
		named, err := reference.ParseNormalizedNamed(app.Image)
		if err != nil || reference.Domain(named) != registry {
			continue
		}
		repos = append(repos, reference.Path(named))
	}
	slices.Sort(repos)
	return slices.Compact(repos)
}

// roleHostnames returns the sorted, distinct hostnames that the manifest's roles publish.
func roleHostnames(m *manifest.Manifest) []string {
	var hosts []string
	for _, app := range m.Apps {
		for _, role := range app.Roles {
			hosts = append(hosts, role.Hostnames...)
		}
	}
	slices.Sort(hosts)
	return slices.Compact(hosts)
}

// cloudflareCheck returns the check for the managed ingress API token. Cloudflare must accept the token and let it read the
// account's tunnels. When DNS is on, the token must also see the zone of every published hostname.
func cloudflareCheck(m *manifest.Manifest, env string) func(context.Context, string) checkResult {
	c := m.Ingress.Cloudflare
	account := c.AccountID
	tunnel := m.TunnelName(env)
	dns := c.DNS == nil || *c.DNS
	hosts := roleHostnames(m)
	return func(ctx context.Context, value string) checkResult {
		base := os.Getenv(cloudflareURLEnv)
		if base != "" {
			// A misconfigured override must never receive the token, so it is refused before any request is made.
			if err := agent.ValidateAPIURL(cloudflareURLEnv, base); err != nil {
				return checkResult{refusal: err.Error()}
			}
		}
		cf := &cloudflare.Client{BaseURL: base, Token: value}
		var notes []string
		unreachable := func(err error) checkResult {
			return checkResult{notes: append(notes, "not checked: Cloudflare could not be asked from this machine ("+err.Error()+")")}
		}
		refuse := func(why string) checkResult {
			return checkResult{notes: notes, refusal: why}
		}
		if err := cf.VerifyToken(ctx, account); err != nil {
			if cloudflareRefused(err) {
				return refuse("Cloudflare does not accept this token: it is wrong, expired or disabled")
			}
			return unreachable(err)
		}
		if _, err := cf.FindTunnel(ctx, account, tunnel); err != nil {
			if cloudflareRefused(err) {
				return refuse("the token cannot read tunnels in account " + account + ": it needs Account > Cloudflare Tunnel > Edit")
			}
			return unreachable(err)
		}
		notes = append(notes, "checked: Cloudflare accepts the token, and it can read tunnels in account "+account)
		if !dns {
			return checkResult{notes: notes}
		}
		for _, host := range hosts {
			if _, err := cf.FindZone(ctx, account, host); err != nil {
				if cloudflareRefused(err) {
					return refuse("the token sees no zone for " + host + " in account " + account + ": it needs Zone > DNS > Edit on that zone")
				}
				return unreachable(err)
			}
			notes = append(notes, "checked: the token sees the zone of "+host)
		}
		return checkResult{notes: notes}
	}
}

// cloudflareRefused reports whether Cloudflare answered and refused the token or the request. Only an answer counts: an
// inactive token, a hostname whose zone the account does not hold, or an API error with a status below 500 other than 429
// (rate limiting). A server error, a rate limit, a connection failure, a timeout or a malformed answer means Cloudflare
// could not answer, so the caller reports the check as not made and the value is stored.
func cloudflareRefused(err error) bool {
	if errors.Is(err, cloudflare.ErrTokenInactive) || errors.Is(err, cloudflare.ErrZoneNotFound) {
		return true
	}
	var api *cloudflare.APIError
	return errors.As(err, &api) && api.Status < 500 && api.Status != http.StatusTooManyRequests
}

// cloudflareGuide is the guide for the managed ingress API token: a link to Cloudflare's account token form with the
// permissions the token needs filled in. The DNS permission is in the link only when the manifest keeps DNS on.
func cloudflareGuide(name, token string, dns bool) secretGuide {
	perms := []cloudflarePermission{{Key: cloudflareTunnelWritePermission, Type: "edit"}}
	if dns {
		perms = append(perms, cloudflarePermission{Key: cloudflareDNSPermission, Type: "edit"})
	}
	keys, _ := json.Marshal(perms) // a list of plain strings always marshals
	link := cloudflareTokenTemplate + url.QueryEscape(string(keys)) + "&name=" + url.QueryEscape(token)
	intro := []string{name + " is the Cloudflare API token for managed ingress."}
	if dns {
		intro = append(intro,
			"Create it with the permissions filled in (Account > Cloudflare Tunnel > Edit, Zone > DNS > Edit):",
			"  "+link,
			"Limit it to the zones that hold your hostnames.")
	} else {
		intro = append(intro,
			"Create it with the permission filled in (Account > Cloudflare Tunnel > Edit):",
			"  "+link)
	}
	return secretGuide{intro: intro}
}

// hiddenInput returns a function that reads one line from standard input without showing it.
// ok is false when standard input is not a terminal. It is a variable so tests can stand in for the terminal.
var hiddenInput = func() (read func() ([]byte, error), ok bool) {
	fd := os.Stdin.Fd()
	if !term.IsTerminal(fd) {
		return nil, false
	}
	return func() ([]byte, error) { return readHidden(fd) }, true
}

// readHidden reads one line from the terminal fd with echo off. term.ReadPassword turns echo back on only when it returns,
// so a Ctrl-C while it waits restores the terminal itself and exits with 130.
func readHidden(fd uintptr) ([]byte, error) {
	state, err := term.GetState(fd)
	if err != nil {
		return nil, err
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-interrupt:
			// The process exits next, so a failed restore has nowhere to be reported.
			_ = term.Restore(fd, state)
			fmt.Fprintln(os.Stderr)
			os.Exit(130)
		case <-done:
		}
	}()
	return term.ReadPassword(fd)
}

// storedSecret is one secret the agent holds, as GET secrets lists it: its name, and the registry whose password it is, or
// empty for a secret that is not a registry password.
type storedSecret struct {
	Name     string `json:"name"`
	Registry string `json:"registry"`
}

// secretValue returns the value `secret set` stores and the registry it is for. From a pipe it reads standard input to its
// end, as before, and the registry is the flag's. At a terminal it first asks the agent what it holds (stored; ok is false
// when the agent could not say, and has printed why), says when the value replaces a stored one, prints the guide, asks,
// and reads the value hidden. code is non-zero when there is no value to store.
func secretValue(f commandFlags, errout io.Writer, stored func() ([]storedSecret, bool)) (value, registry string, code int) {
	read, interactive := hiddenInput()
	if !interactive {
		data, e := io.ReadAll(io.LimitReader(os.Stdin, maxSecretValue+1))
		if e != nil || len(data) > maxSecretValue {
			fmt.Fprintln(errout, "secret input unavailable or too large")
			return "", "", 1
		}
		return strings.TrimSuffix(string(data), "\n"), f.registry, 0
	}
	name := f.pos[1]
	held, ok := stored()
	if !ok {
		return "", "", 1
	}
	registry = f.registry
	if i := slices.IndexFunc(held, func(s storedSecret) bool { return s.Name == name }); i >= 0 {
		prior := held[i]
		if prior.Registry != "" {
			fmt.Fprintf(errout, "%s is stored as the password for %s. The new value replaces it.\n", name, prior.Registry)
		} else {
			fmt.Fprintf(errout, "%s is already stored. The new value replaces it.\n", name)
		}
		if registry == "" {
			registry = prior.Registry
		}
	}
	m, err := manifest.Load(f.dir, f.env)
	if err != nil {
		m = nil
		// A directory with no berthy.yml is the common case and says nothing; a manifest that is there but does not load is said.
		if _, statErr := os.Stat(filepath.Join(f.dir, "berthy.yml")); statErr == nil {
			fmt.Fprintf(errout, "manifest not loaded: %v\n", err)
		}
	}
	g := guideFor(m, f.env, name, registry)
	for _, line := range g.intro {
		fmt.Fprintln(errout, line)
	}
	fmt.Fprintf(errout, "Value of %s (hidden): ", name)
	line, err := read()
	fmt.Fprintln(errout)
	if err != nil || len(line) > maxSecretValue {
		fmt.Fprintln(errout, "secret input unavailable or too large")
		return "", "", 1
	}
	if len(line) == 0 {
		fmt.Fprintln(errout, "no value entered")
		return "", "", 1
	}
	value = string(line)
	if g.check != nil {
		// Ctrl-C is not caught here: it ends the process before anything is stored, so an interrupted check stores nothing.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res := g.check(ctx, value)
		for _, n := range res.notes {
			fmt.Fprintln(errout, n)
		}
		if res.refusal != "" {
			fmt.Fprintln(errout, "not stored: "+res.refusal)
			return "", "", 1
		}
	}
	return value, registry, 0
}

// storedSecrets asks the agent which secrets it holds, at u. ok is false when the agent did not answer with the list, and the
// reason has been printed.
func storedSecrets(ctx context.Context, client *http.Client, u *url.URL, token string, errout io.Writer) ([]storedSecret, bool) {
	status, data, ok := askAgent(ctx, client, u, token, http.MethodGet, nil, errout)
	if !ok {
		return nil, false
	}
	if status != http.StatusOK {
		fmt.Fprintln(errout, agentclient.Message(status, data))
		return nil, false
	}
	var held []storedSecret
	// Only an array is the list: a JSON null decodes to a nil slice, which is not one.
	if json.Unmarshal(data, &held) != nil || held == nil {
		fmt.Fprintln(errout, "invalid agent response")
		return nil, false
	}
	return held, true
}

// secretSetReply is the agent's answer to a secret set. A registry password the agent stored names the registry, the running
// services that pull from it and what happened to each; services_error says why they could not be looked at.
type secretSetReply struct {
	Registry      string          `json:"registry"`
	Services      []secretService `json:"services"`
	ServicesError string          `json:"services_error"`
}

// secretService is what the agent did with one running service that pulls from the registry.
type secretService struct {
	Name    string `json:"name"`
	Updated bool   `json:"updated"`
	Error   string `json:"error"`
}

// printSecretSet says what the agent did with the services of a registry password it stored. It returns false, printing
// nothing, when the answer names no registry: that is a plain secret, which the generic printer prints.
func printSecretSet(out io.Writer, data []byte) bool {
	var reply secretSetReply
	if json.Unmarshal(data, &reply) != nil || reply.Registry == "" {
		return false
	}
	registry := terminalSafe(reply.Registry)
	fmt.Fprintln(out, "ok: true")
	retry := false
	switch {
	case reply.ServicesError != "":
		fmt.Fprintln(out, "services not updated: "+terminalSafe(reply.ServicesError))
		retry = true
	case len(reply.Services) == 0:
		fmt.Fprintln(out, "no running service pulls from "+registry+" yet")
	}
	for _, s := range reply.Services {
		if s.Updated {
			fmt.Fprintln(out, terminalSafe(s.Name)+": now pulls from "+registry+" with the new password")
			continue
		}
		fmt.Fprintln(out, terminalSafe(s.Name)+": not updated ("+terminalSafe(s.Error)+")")
		retry = true
	}
	if retry {
		fmt.Fprintln(out, "Run the command again to retry the services that were not updated.")
	}
	return true
}
