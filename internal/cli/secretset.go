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
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"berthy/internal/agent"
	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/cloudflare"
	"berthy/internal/manifest"
	"berthy/internal/registrycheck"

	"github.com/charmbracelet/x/term"
)

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
	repos := m.ImageRepos(registry)
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
			if cloudflare.Refused(err) {
				return refuse("Cloudflare does not accept this token: it is wrong, expired or disabled")
			}
			return unreachable(err)
		}
		if _, err := cf.FindTunnel(ctx, account, tunnel); err != nil {
			if cloudflare.Refused(err) {
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
				if cloudflare.Refused(err) {
					return refuse("the token sees no zone for " + host + " in account " + account + ": it needs Zone > DNS > Edit on that zone")
				}
				return unreachable(err)
			}
			notes = append(notes, "checked: the token sees the zone of "+host)
		}
		return checkResult{notes: notes}
	}
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
var hiddenInput = func() (read func(context.Context) ([]byte, error), ok bool) {
	fd := os.Stdin.Fd()
	if !term.IsTerminal(fd) {
		return nil, false
	}
	return func(ctx context.Context) ([]byte, error) { return readHidden(ctx, fd) }, true
}

// readHidden changes the mode before arming cleanup. The reader that follows
// never changes the mode, so it cannot disable echo again after restoration.
func readHidden(ctx context.Context, fd uintptr) ([]byte, error) {
	state, err := term.GetState(fd)
	if err != nil {
		return nil, err
	}
	if err := setHiddenMode(fd); err != nil {
		return nil, err
	}
	restore := restoreOnCommandSignal(ctx, func() { _ = term.Restore(fd, state) }, func() { fmt.Fprintln(os.Stderr) })
	defer restore()
	return readHiddenLine(fd)
}

// readPasswordLine keeps the input behavior of term.ReadPassword after the
// platform code sets the mode. It consumes only one line.
func readPasswordLine(read func([]byte) (int, error), end byte) ([]byte, error) {
	var input []byte
	var b [1]byte
	for {
		n, err := read(b[:])
		if n <= 0 {
			if err == nil {
				err = io.EOF
			}
			if err == io.EOF && len(input) > 0 {
				err = nil
			}
			return input, err
		}
		switch b[0] {
		case end:
			return input, nil
		case '\r', '\n':
		case '\b':
			if len(input) > 0 {
				input = input[:len(input)-1]
			}
		default:
			input = append(input, b[0])
		}
	}
}

// storedSecret is one secret the agent holds, as GET secrets lists it: its name, and the registry whose password it is, or
// empty for a secret that is not a registry password.
type storedSecret struct {
	Name     string `json:"name"`
	Registry string `json:"registry"`
}

var pipeSignalAfterFunc = context.AfterFunc

// exitOnPipeSignal ends an unsubmitted secret read even when its producer keeps stdin open.
func exitOnPipeSignal(ctx context.Context) func() {
	exit := func() {
		if s, ok := context.Cause(ctx).(commandSignal); ok {
			exitProcess(128 + int(s.Signal))
		}
	}
	done := make(chan struct{})
	stop := pipeSignalAfterFunc(ctx, func() {
		defer close(done)
		exit()
	})
	return func() {
		if stop() {
			// EOF can disarm the callback after the signal cause was published.
			exit()
			return
		}
		<-done
	}
}

// secretValue returns the value `secret set` stores and the registry it is for. From a pipe it reads standard input to its
// end, as before, and the registry is the flag's. At a terminal it first asks the agent what it holds (stored; ok is false
// when the agent could not say, and has printed why), says when the value replaces a stored one, prints the guide, asks,
// and reads the value hidden. code is non-zero when there is no value to store.
func secretValue(ctx context.Context, f commandFlags, errout io.Writer, stored func() ([]storedSecret, bool), token string) (value, registry string, code int) {
	read, interactive := hiddenInput()
	if !interactive {
		stop := exitOnPipeSignal(ctx)
		data, e := io.ReadAll(io.LimitReader(os.Stdin, api.MaxSecretValue+1))
		stop()
		if ctx.Err() != nil {
			return "", "", 1
		}
		if e != nil || len(data) > api.MaxSecretValue {
			fmt.Fprintln(errout, "secret input unavailable or too large")
			return "", "", 1
		}
		value, ok := oneLine(data)
		if !ok {
			fmt.Fprintln(errout, "the secret must be one line: a piped value may end with one LF or CRLF")
			return "", "", 1
		}
		if len(value) == 0 {
			fmt.Fprintln(errout, "no value entered")
			return "", "", 1
		}
		return string(value), f.registry, 0
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
			fmt.Fprintf(errout, "%s is stored as the password for %s. The new value replaces it.\n", name, safeSecretText(prior.Registry, token))
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
	value, result := askSecret(ctx, g, fmt.Sprintf("Value of %s (hidden): ", name), func() ([]byte, error) { return read(ctx) }, errout, !f.json, token)
	if result == askEmpty {
		fmt.Fprintln(errout, "no value entered")
	}
	if result != askGot {
		return "", "", 1
	}
	return value, registry, 0
}

type askResult int

const (
	askGot askResult = iota
	askEmpty
	askRefused
	askFailed
)

// askSecret prints the guide, reads a hidden value and checks it.
func askSecret(ctx context.Context, g secretGuide, prompt string, read func() ([]byte, error), errout io.Writer, humanProgress bool, token string) (string, askResult) {
	for _, line := range g.intro {
		fmt.Fprintln(errout, safeSecretText(line, token))
	}
	fmt.Fprint(errout, prompt)
	line, err := read()
	fmt.Fprintln(errout)
	if err != nil || len(line) > api.MaxSecretValue {
		fmt.Fprintln(errout, "secret input unavailable or too large")
		return "", askFailed
	}
	if len(line) == 0 {
		return "", askEmpty
	}
	value := string(line)
	if g.check != nil {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		progress := startWait(errout, humanProgress, "checking secret")
		res := g.check(ctx, value)
		progress.Close()
		for _, n := range res.notes {
			fmt.Fprintln(errout, safeSecretText(n, value, wireSecret(value), token))
		}
		if res.refusal != "" {
			printRefusal(errout, "not stored: "+safeSecretText(res.refusal, value, wireSecret(value), token), humanProgress)
			return "", askRefused
		}
	}
	return value, askGot
}

func wireSecret(value string) string {
	encoded, _ := json.Marshal(value)
	var wire string
	_ = json.Unmarshal(encoded, &wire)
	return wire
}

// storedSecrets asks the agent which secrets it holds, at u. ok is false when the agent did not answer with the list, and the
// reason has been printed.
func storedSecrets(ctx context.Context, client *http.Client, u *url.URL, token string, out, errout io.Writer, human bool) ([]storedSecret, bool) {
	result, err := askAgent(ctx, client, u, token, http.MethodGet, nil)
	if err != nil {
		printAgentRequestError(out, errout, agentclient.Target{Base: u, Token: token}, err, !human)
		return nil, false
	}
	status, data, older := result.Status, result.Data, result.Older
	if status != http.StatusOK {
		printAgentRefusal(errout, safeSecretText(agentclient.Message(status, data), token), human, older)
		return nil, false
	}
	var rows []json.RawMessage
	if json.Unmarshal(data, &rows) != nil || rows == nil {
		fmt.Fprintln(errout, "invalid agent response")
		return nil, false
	}
	held := make([]storedSecret, 0, len(rows))
	seen := make(map[string]string, len(rows))
	for _, row := range rows {
		var item struct {
			Name     string          `json:"name"`
			Registry json.RawMessage `json:"registry"`
		}
		var registry string
		if string(row) == "null" || json.Unmarshal(row, &item) != nil || item.Name == "" || (len(item.Registry) != 0 && (string(item.Registry) == "null" || json.Unmarshal(item.Registry, &registry) != nil)) {
			fmt.Fprintln(errout, "invalid agent response")
			return nil, false
		}
		if prior, ok := seen[item.Name]; ok && prior != registry {
			fmt.Fprintln(errout, "invalid agent response")
			return nil, false
		}
		seen[item.Name] = registry
		held = append(held, storedSecret{Name: item.Name, Registry: registry})
	}
	return held, true
}

// secretSetReply is the agent's answer to a secret set. A registry password the agent stored names the registry, the running
// services that pull from it and what happened to each; services_error says why they could not be looked at.
type secretSetReply struct {
	Ok            bool            `json:"ok"`
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

// secretGenerateRequest is the request of secret generate, from its flags. A --bytes that is not a whole number is refused here,
// before anything is sent. The agent checks the range of --bytes and the format, so those rules are stated once.
func secretGenerateRequest(f commandFlags, errout io.Writer) (api.SecretGenerate, int) {
	req := api.SecretGenerate{Name: f.pos[1], Format: f.format, Replace: f.replaceGenerated, DryRun: f.dry}
	if slices.Contains(f.given, "--bytes") {
		n, err := strconv.Atoi(f.bytes)
		if err != nil {
			fmt.Fprintln(errout, "invalid --bytes")
			return req, 2
		}
		req.Bytes = &n
	}
	return req, 0
}

// printSecretGenerate says what a generate did, or what a dry run says it would do. It prints the name, the length and the format,
// and never a value, since the agent never sends one. A dry run for a stored secret says that a real run is refused without
// --replace, which is the one thing it would refuse.
func printSecretGenerate(out, errout io.Writer, data []byte, dry, replace bool) int {
	if dry {
		var plan api.SecretGeneratePlan
		if json.Unmarshal(data, &plan) != nil || plan.Name == "" {
			fmt.Fprintln(errout, "invalid agent response")
			return 1
		}
		name, size := terminalSafe(plan.Name), generatedSize(plan.Bytes, plan.Format)
		switch {
		case plan.App != "" && !replace && plan.Volume != "":
			fmt.Fprintf(out, "%s is not stored, but the volume %s of app %s exists: a real run is refused unless --replace is given\n", name, terminalSafe(plan.Volume), terminalSafe(plan.App))
		case plan.App != "" && !replace:
			fmt.Fprintf(out, "%s is not stored, but app %s was deployed here: a real run is refused unless --replace is given\n", name, terminalSafe(plan.App))
		case plan.Exists && !replace:
			fmt.Fprintf(out, "%s exists: a real run is refused unless --replace is given\n", name)
		case plan.Exists:
			fmt.Fprintf(out, "would replace %s (%s)\n", name, size)
		default:
			fmt.Fprintf(out, "would generate %s (%s)\n", name, size)
		}
		return 0
	}
	var res api.SecretGenerated
	if json.Unmarshal(data, &res) != nil || res.Name == "" {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	verb := "generated"
	if res.Replaced {
		verb = "replaced"
	}
	fmt.Fprintf(out, "%s %s (%s)\n", verb, terminalSafe(res.Name), generatedSize(res.Bytes, res.Format))
	return 0
}

// generatedSize says the length of a generated secret as the CLI prints it: bytes for hex and base64, and characters for alnum,
// whose length is in characters.
func generatedSize(n int, format string) string {
	if format == "alnum" {
		return fmt.Sprintf("%d characters, %s", n, format)
	}
	return fmt.Sprintf("%d bytes, %s", n, format)
}

// printSecretSet says what the agent did with the services of a registry password it stored. It returns false, printing
// nothing, when the answer names no registry: that is a plain secret, which the generic printer prints.
func printSecretSet(out io.Writer, data []byte, values ...string) bool {
	var reply secretSetReply
	if json.Unmarshal(data, &reply) != nil || reply.Registry == "" {
		return false
	}
	if printRegistrySet(out, reply, values...) {
		fmt.Fprintln(out, "Run the command again to retry the services that were not updated.")
	}
	return true
}

func printRegistrySet(out io.Writer, reply secretSetReply, values ...string) bool {
	registry := safeSecretText(reply.Registry, values...)
	fmt.Fprintln(out, "ok: true")
	retry := false
	switch {
	case reply.ServicesError != "":
		fmt.Fprintln(out, "services not updated: "+safeSecretText(reply.ServicesError, values...))
		retry = true
	case len(reply.Services) == 0:
		fmt.Fprintln(out, "no running service pulls from "+registry+" yet")
	}
	for _, s := range reply.Services {
		if s.Updated {
			fmt.Fprintln(out, safeSecretText(s.Name, values...)+": now pulls from "+registry+" with the new password")
			continue
		}
		fmt.Fprintln(out, safeSecretText(s.Name, values...)+": not updated ("+safeSecretText(s.Error, values...)+")")
		retry = true
	}
	return retry
}

// redactRegistryJSON changes only diagnostic strings; other fields and types remain agent-owned.
func redactRegistryJSON(data []byte, values ...string) []byte {
	return redactDiagnosticJSON(data, true, values...)
}
